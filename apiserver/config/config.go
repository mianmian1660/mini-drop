// ============================================================
// config — 配置加载模块
// 使用 Viper 读取 apiserver.yaml，并支持环境变量覆盖
// ============================================================

package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config 是全局配置结构体，包含所有运行时配置
type Config struct {
	Server          ServerConfig          `mapstructure:"server"`
	Database        DatabaseConfig        `mapstructure:"database"`
	GRPC            GRPCConfig            `mapstructure:"grpc"`
	Storage         StorageConfig         `mapstructure:"storage"`
	Retention       RetentionConfig       `mapstructure:"retention"`
	ContinuousBlock ContinuousBlockConfig `mapstructure:"continuous_block"`
	StorageDisk     StorageDiskConfig     `mapstructure:"storage_disk"`
	Log             LogConfig             `mapstructure:"log"`
	Security        SecurityConfig        `mapstructure:"security"`
	Observability   ObservabilityConfig   `mapstructure:"observability"`
	Profile         ProfileConfig         `mapstructure:"profile"`
	AgentDiscovery  AgentDiscoveryConfig  `mapstructure:"agent_discovery"`
}

// StorageDiskConfig protects the host filesystem used by containers and
// temporary capture files. It intentionally is separate from object storage.
//
// 阈值语义（全部是"剩余可用字节数"的硬下界）：
//   - WarningFreeBytes：剩余低于该值 → level=warning（只告警，不拒收）
//   - CriticalFreeBytes：剩余低于该值 → level=critical（严重告警，不拒收）
//   - MinFreeBytes：剩余低于该值 → level=emergency（拒绝新采集）
//
// 必须满足 warning_free_bytes > critical_free_bytes > min_free_bytes > 0。
type StorageDiskConfig struct {
	Path              string `mapstructure:"path"`
	WarningFreeBytes  uint64 `mapstructure:"warning_free_bytes"`
	CriticalFreeBytes uint64 `mapstructure:"critical_free_bytes"`
	MinFreeBytes      uint64 `mapstructure:"min_free_bytes"`
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Port int    `mapstructure:"port"`
	Mode string `mapstructure:"mode"`
}

// DatabaseConfig 数据库连接配置
type DatabaseConfig struct {
	DSN                string `mapstructure:"dsn"`
	MaxOpenConns       int    `mapstructure:"max_open_conns"`
	MaxIdleConns       int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetimeSec int    `mapstructure:"conn_max_lifetime_sec"`
}

// GRPCConfig gRPC 客户端配置
type GRPCConfig struct {
	Addr         string `mapstructure:"addr"`
	TimeoutSec   int    `mapstructure:"timeout_sec"`
	MTLSCertFile string `mapstructure:"mtls_cert_file"`
	MTLSKeyFile  string `mapstructure:"mtls_key_file"`
	MTLSCAFile   string `mapstructure:"mtls_ca_file"`
}

// StorageConfig 对象存储配置
type StorageConfig struct {
	Endpoint         string `mapstructure:"endpoint"`
	PublicEndpoint   string `mapstructure:"public_endpoint"` // 浏览器可访问的地址（预签名URL用）
	AccessKey        string `mapstructure:"access_key"`
	SecretKey        string `mapstructure:"secret_key"`
	UseSSL           bool   `mapstructure:"use_ssl"`
	Bucket           string `mapstructure:"bucket"`
	PresignExpireSec int    `mapstructure:"presign_expire_sec"`
}

// RetentionConfig controls automatic cleanup of task artifacts.
type RetentionConfig struct {
	Enabled              bool `mapstructure:"enabled"`
	RawRetentionHours    int  `mapstructure:"raw_retention_hours"`
	ResultRetentionHours int  `mapstructure:"result_retention_hours"`
	CleanupIntervalSec   int  `mapstructure:"cleanup_interval_sec"`
	BatchLimit           int  `mapstructure:"batch_limit"`
	// ContinuousSummaryRetentionHours 控制 Native Continuous Profiling 冷层
	// 摘要（ContinuousWindowSummary）的保留时长。原始窗口/样本过期后（由
	// 每个 session 自己的 RetentionHours 决定）不是直接硬删，而是先降采样
	// 成这张摘要表，摘要本身再按这个全局配置单独过期——默认比原始数据
	// 保留期长一个数量级（7 天），因为摘要体积小很多。
	ContinuousSummaryRetentionHours int `mapstructure:"continuous_summary_retention_hours"`
}

// ContinuousBlockConfig 控制阶段三的持续采集块存储（compactor）：
// 把同一 UTC 小时桶内的分钟 batch 合并为 gzip 压缩块。所有参数都支持
// 环境变量覆盖（CONTINUOUS_BLOCK_*），并保持"未启用时完全等价于旧的
// 每分钟一个 JSON 对象"的行为——默认 disabled，由部署显式开启。
type ContinuousBlockConfig struct {
	// Enabled 开启 compactor 与块读取。关闭时查询继续读旧分钟对象，
	// 存量块对象仍可被读取（加载器按 key 后缀自动识别）。
	Enabled bool `mapstructure:"enabled"`
	// WindowSec 块覆盖的 UTC 小时桶长度（秒），默认 3600。
	WindowSec int `mapstructure:"window_sec"`
	// CompactionDelaySec 小时桶"已结束至少这么久"才允许 compaction，
	// 给迟到的 batch 留出合并窗口，默认 600。
	CompactionDelaySec int `mapstructure:"compaction_delay_sec"`
	// CompactionIntervalSec compactor 扫描周期（秒），默认 300。
	CompactionIntervalSec int `mapstructure:"compaction_interval_sec"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// SecurityConfig 控制生产/开发安全边界。开发环境默认允许 insecure，
// 生产环境必须显式配置可信通道或明确承认 insecure 例外。
type SecurityConfig struct {
	Environment            string `mapstructure:"environment"`
	AllowInsecureTransport bool   `mapstructure:"allow_insecure_transport"`
}

// ObservabilityConfig 控制运行时可观测性入口。
type ObservabilityConfig struct {
	MetricsEnabled bool `mapstructure:"metrics_enabled"`
}

// ProfileConfig controls Native Continuous Profiling query defaults.
type ProfileConfig struct {
	Enabled    bool `mapstructure:"enabled"`
	TimeoutSec int  `mapstructure:"timeout_sec"`
}

// AgentDiscoveryConfig 控制 apiserver 主动探测哪些 Agent IP。
// ExtraIPs 使用逗号分隔，适合通过环境变量 AGENT_DISCOVERY_EXTRA_IPS
// 放入公网服务器地址，例如 111.230.29.115。这里不保存 SSH 密码，也不负责部署，
// 只是在 drop_agent 已经连上 drop_server 后，帮助首页把远端主机发现出来。
type AgentDiscoveryConfig struct {
	ExtraIPs string `mapstructure:"extra_ips"`
}

// Load 加载配置文件并返回 Config 结构体
// 优先使用环境变量覆盖 YAML 中的值
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// 设置配置文件路径
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		// 默认搜索路径
		v.SetConfigName("apiserver")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("/etc/apiserver/")
	}

	// 支持环境变量覆盖（如 PG_DSN, DROP_GRPC 等）
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 读取配置文件（文件不存在不报错，可以用纯环境变量运行）
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
		// 配置文件不存在，不影响（Docker 里全部走环境变量）
	}

	cfg := &Config{}

	// 绑定默认值
	v.SetDefault("server.port", 8191)
	v.SetDefault("server.mode", "release")
	v.SetDefault("database.dsn", "host=localhost user=postgres password=dev dbname=drop sslmode=disable")
	v.SetDefault("database.max_open_conns", 100)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.conn_max_lifetime_sec", 3600)
	v.SetDefault("grpc.addr", "localhost:50051")
	v.SetDefault("grpc.timeout_sec", 5)
	v.SetDefault("storage.endpoint", "localhost:9000")
	v.SetDefault("storage.public_endpoint", "localhost:9000")
	v.SetDefault("storage.access_key", "drop")
	v.SetDefault("storage.secret_key", "dropdrop")
	v.SetDefault("storage.use_ssl", false)
	v.SetDefault("storage.bucket", "drop-data")
	v.SetDefault("storage.presign_expire_sec", 900)
	v.SetDefault("retention.enabled", true)
	v.SetDefault("retention.raw_retention_hours", 24)
	v.SetDefault("retention.result_retention_hours", 720)
	v.SetDefault("retention.cleanup_interval_sec", 300)
	v.SetDefault("retention.batch_limit", 200)
	v.SetDefault("retention.continuous_summary_retention_hours", 168)
	v.SetDefault("continuous_block.enabled", false)
	v.SetDefault("continuous_block.window_sec", 3600)
	v.SetDefault("continuous_block.compaction_delay_sec", 600)
	v.SetDefault("continuous_block.compaction_interval_sec", 300)
	v.SetDefault("storage_disk.path", "/tmp")
	v.SetDefault("storage_disk.warning_free_bytes", uint64(8<<30))
	v.SetDefault("storage_disk.critical_free_bytes", uint64(4<<30))
	v.SetDefault("storage_disk.min_free_bytes", uint64(1<<30))
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("log.output", "stdout")
	v.SetDefault("security.environment", "development")
	v.SetDefault("security.allow_insecure_transport", true)
	v.SetDefault("observability.metrics_enabled", true)
	v.SetDefault("profile.enabled", false)
	v.SetDefault("profile.timeout_sec", 5)
	v.SetDefault("agent_discovery.extra_ips", "")

	// 环境变量覆盖（优先级最高）
	// PG_DSN → database.dsn
	if envDSN := os.Getenv("PG_DSN"); envDSN != "" {
		v.Set("database.dsn", envDSN)
	}
	if envGRPC := os.Getenv("DROP_GRPC"); envGRPC != "" {
		v.Set("grpc.addr", envGRPC)
	}
	if envCert := os.Getenv("GRPC_MTLS_CERT_FILE"); envCert != "" {
		v.Set("grpc.mtls_cert_file", envCert)
	}
	if envKey := os.Getenv("GRPC_MTLS_KEY_FILE"); envKey != "" {
		v.Set("grpc.mtls_key_file", envKey)
	}
	if envCA := os.Getenv("GRPC_MTLS_CA_FILE"); envCA != "" {
		v.Set("grpc.mtls_ca_file", envCA)
	}
	if envS3 := os.Getenv("S3_ENDPOINT"); envS3 != "" {
		v.Set("storage.endpoint", envS3)
		// 如果未单独配置 public_endpoint，默认用 S3_ENDPOINT
		if os.Getenv("S3_PUBLIC_ENDPOINT") == "" {
			v.Set("storage.public_endpoint", envS3)
		}
	}
	if envS3Pub := os.Getenv("S3_PUBLIC_ENDPOINT"); envS3Pub != "" {
		v.Set("storage.public_endpoint", envS3Pub)
	}
	if envAK := os.Getenv("S3_ACCESS_KEY"); envAK != "" {
		v.Set("storage.access_key", envAK)
	}
	if envSK := os.Getenv("S3_SECRET_KEY"); envSK != "" {
		v.Set("storage.secret_key", envSK)
	}
	if envRetention := os.Getenv("RETENTION_ENABLED"); envRetention != "" {
		v.Set("retention.enabled", parseBoolEnv(envRetention))
	}
	if envRawRetention := os.Getenv("ARTIFACT_RAW_RETENTION_HOURS"); envRawRetention != "" {
		v.Set("retention.raw_retention_hours", envRawRetention)
	}
	if envResultRetention := os.Getenv("ARTIFACT_RESULT_RETENTION_HOURS"); envResultRetention != "" {
		v.Set("retention.result_retention_hours", envResultRetention)
	}
	if envContinuousSummaryRetention := os.Getenv("CONTINUOUS_SUMMARY_RETENTION_HOURS"); envContinuousSummaryRetention != "" {
		v.Set("retention.continuous_summary_retention_hours", envContinuousSummaryRetention)
	}
	if envBlockEnabled := os.Getenv("CONTINUOUS_BLOCK_ENABLED"); envBlockEnabled != "" {
		v.Set("continuous_block.enabled", parseBoolEnv(envBlockEnabled))
	}
	if envBlockWindow := os.Getenv("CONTINUOUS_BLOCK_WINDOW_SEC"); envBlockWindow != "" {
		v.Set("continuous_block.window_sec", envBlockWindow)
	}
	if envBlockDelay := os.Getenv("CONTINUOUS_BLOCK_COMPACTION_DELAY_SEC"); envBlockDelay != "" {
		v.Set("continuous_block.compaction_delay_sec", envBlockDelay)
	}
	if envBlockInterval := os.Getenv("CONTINUOUS_BLOCK_COMPACTION_INTERVAL_SEC"); envBlockInterval != "" {
		v.Set("continuous_block.compaction_interval_sec", envBlockInterval)
	}
	if envCleanupInterval := os.Getenv("RETENTION_CLEANUP_INTERVAL_SEC"); envCleanupInterval != "" {
		v.Set("retention.cleanup_interval_sec", envCleanupInterval)
	}
	if envBatchLimit := os.Getenv("RETENTION_BATCH_LIMIT"); envBatchLimit != "" {
		v.Set("retention.batch_limit", envBatchLimit)
	}
	if envDiskPath := os.Getenv("STORAGE_DISK_PATH"); envDiskPath != "" {
		v.Set("storage_disk.path", envDiskPath)
	}
	if envWarningFree := os.Getenv("STORAGE_WARNING_FREE_BYTES"); envWarningFree != "" {
		v.Set("storage_disk.warning_free_bytes", envWarningFree)
	}
	if envCriticalFree := os.Getenv("STORAGE_CRITICAL_FREE_BYTES"); envCriticalFree != "" {
		v.Set("storage_disk.critical_free_bytes", envCriticalFree)
	}
	if envMinFree := os.Getenv("STORAGE_MIN_FREE_BYTES"); envMinFree != "" {
		v.Set("storage_disk.min_free_bytes", envMinFree)
	}
	if envPort := os.Getenv("PORT"); envPort != "" {
		v.Set("server.port", envPort)
	}
	if envName := os.Getenv("MINI_DROP_ENV"); envName != "" {
		v.Set("security.environment", envName)
	}
	if envInsecure := os.Getenv("ALLOW_INSECURE_TRANSPORT"); envInsecure != "" {
		v.Set("security.allow_insecure_transport", parseBoolEnv(envInsecure))
	}
	if envMetrics := os.Getenv("METRICS_ENABLED"); envMetrics != "" {
		v.Set("observability.metrics_enabled", parseBoolEnv(envMetrics))
	}
	if envProfileEnabled := os.Getenv("PROFILE_ENABLED"); envProfileEnabled != "" {
		v.Set("profile.enabled", parseBoolEnv(envProfileEnabled))
	}
	if envProfileTimeout := os.Getenv("PROFILE_TIMEOUT_SEC"); envProfileTimeout != "" {
		v.Set("profile.timeout_sec", envProfileTimeout)
	}
	if envAgentDiscoveryIPs := os.Getenv("AGENT_DISCOVERY_EXTRA_IPS"); envAgentDiscoveryIPs != "" {
		v.Set("agent_discovery.extra_ips", envAgentDiscoveryIPs)
	}

	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func parseBoolEnv(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func (cfg *Config) Validate() error {
	if cfg == nil {
		return fmt.Errorf("配置为空")
	}
	env := strings.ToLower(strings.TrimSpace(cfg.Security.Environment))
	if env == "" {
		env = "development"
	}
	cfg.Security.Environment = env
	hasMTLS := cfg.GRPC.MTLSCertFile != "" && cfg.GRPC.MTLSKeyFile != "" && cfg.GRPC.MTLSCAFile != ""
	if env == "production" && !cfg.Security.AllowInsecureTransport && !hasMTLS {
		return fmt.Errorf("生产环境必须配置 gRPC mTLS 证书，或显式设置 ALLOW_INSECURE_TRANSPORT=true 承认 insecure 通道")
	}
	if cfg.Retention.RawRetentionHours <= 0 {
		cfg.Retention.RawRetentionHours = 24
	}
	if cfg.Retention.ResultRetentionHours <= 0 {
		cfg.Retention.ResultRetentionHours = 720
	}
	if cfg.Retention.CleanupIntervalSec <= 0 {
		cfg.Retention.CleanupIntervalSec = 300
	}
	if cfg.Retention.ContinuousSummaryRetentionHours <= 0 {
		cfg.Retention.ContinuousSummaryRetentionHours = 168
	}
	if cfg.ContinuousBlock.WindowSec <= 0 {
		cfg.ContinuousBlock.WindowSec = 3600
	}
	if cfg.ContinuousBlock.CompactionDelaySec <= 0 {
		cfg.ContinuousBlock.CompactionDelaySec = 600
	}
	if cfg.ContinuousBlock.CompactionIntervalSec <= 0 {
		cfg.ContinuousBlock.CompactionIntervalSec = 300
	}
	if cfg.Retention.BatchLimit <= 0 {
		cfg.Retention.BatchLimit = 200
	}
	if strings.TrimSpace(cfg.StorageDisk.Path) == "" {
		cfg.StorageDisk.Path = "/tmp"
	}
	if cfg.StorageDisk.WarningFreeBytes == 0 {
		cfg.StorageDisk.WarningFreeBytes = 8 << 30
	}
	if cfg.StorageDisk.CriticalFreeBytes == 0 {
		cfg.StorageDisk.CriticalFreeBytes = 4 << 30
	}
	if cfg.StorageDisk.MinFreeBytes == 0 {
		cfg.StorageDisk.MinFreeBytes = 1 << 30
	}
	// 阈值链必须严格递减且为正；违反时直接拒绝启动，避免等级判定失真。
	if cfg.StorageDisk.WarningFreeBytes <= cfg.StorageDisk.CriticalFreeBytes ||
		cfg.StorageDisk.CriticalFreeBytes <= cfg.StorageDisk.MinFreeBytes {
		return fmt.Errorf("存储磁盘阈值配置非法：必须满足 warning_free_bytes(%d) > critical_free_bytes(%d) > min_free_bytes(%d) > 0",
			cfg.StorageDisk.WarningFreeBytes, cfg.StorageDisk.CriticalFreeBytes, cfg.StorageDisk.MinFreeBytes)
	}
	return nil
}
