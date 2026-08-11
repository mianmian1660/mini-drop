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
	Server        ServerConfig        `mapstructure:"server"`
	Database      DatabaseConfig      `mapstructure:"database"`
	GRPC          GRPCConfig          `mapstructure:"grpc"`
	Storage       StorageConfig       `mapstructure:"storage"`
	Log           LogConfig           `mapstructure:"log"`
	Security      SecurityConfig      `mapstructure:"security"`
	Observability ObservabilityConfig `mapstructure:"observability"`
	Profile       ProfileConfig       `mapstructure:"profile"`
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

// ProfileConfig controls the continuous profiling query proxy.
type ProfileConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	ParcaURL      string `mapstructure:"parca_url"`
	ParcaGRPCAddr string `mapstructure:"parca_grpc_addr"`
	ParcaUIURL    string `mapstructure:"parca_ui_url"`
	TimeoutSec    int    `mapstructure:"timeout_sec"`
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
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("log.output", "stdout")
	v.SetDefault("security.environment", "development")
	v.SetDefault("security.allow_insecure_transport", true)
	v.SetDefault("observability.metrics_enabled", true)
	v.SetDefault("profile.enabled", false)
	v.SetDefault("profile.parca_url", "")
	v.SetDefault("profile.parca_grpc_addr", "parca:7070")
	v.SetDefault("profile.parca_ui_url", "http://localhost:7070")
	v.SetDefault("profile.timeout_sec", 5)

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
	if envParca := os.Getenv("PARCA_URL"); envParca != "" {
		v.Set("profile.parca_url", envParca)
	}
	if envParcaGRPC := os.Getenv("PARCA_GRPC_ADDR"); envParcaGRPC != "" {
		v.Set("profile.parca_grpc_addr", envParcaGRPC)
	}
	if envParcaUI := os.Getenv("PARCA_UI_URL"); envParcaUI != "" {
		v.Set("profile.parca_ui_url", envParcaUI)
	}
	if envProfileTimeout := os.Getenv("PROFILE_TIMEOUT_SEC"); envProfileTimeout != "" {
		v.Set("profile.timeout_sec", envProfileTimeout)
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
	return nil
}
