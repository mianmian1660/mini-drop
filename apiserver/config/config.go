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
	Server            ServerConfig            `mapstructure:"server"`
	Database          DatabaseConfig          `mapstructure:"database"`
	GRPC              GRPCConfig              `mapstructure:"grpc"`
	Storage           StorageConfig           `mapstructure:"storage"`
	Retention         RetentionConfig         `mapstructure:"retention"`
	ContinuousBlock   ContinuousBlockConfig   `mapstructure:"continuous_block"`
	ContinuousParquet ContinuousParquetConfig `mapstructure:"continuous_parquet"`
	Blob              BlobConfig              `mapstructure:"blob"`
	StorageDisk       StorageDiskConfig       `mapstructure:"storage_disk"`
	Log               LogConfig               `mapstructure:"log"`
	Security          SecurityConfig          `mapstructure:"security"`
	Observability     ObservabilityConfig     `mapstructure:"observability"`
	Profile           ProfileConfig           `mapstructure:"profile"`
	AgentDiscovery    AgentDiscoveryConfig    `mapstructure:"agent_discovery"`
	SingleShot        SingleShotConfig        `mapstructure:"single_shot"`
}

// SingleShotConfig 阶段 4：单次采样最终存储模型的发布开关。
// 对应 Release B/C/D 的分阶段发布，全部默认关闭，部署侧显式开启：
//   - LayoutV2Enabled（SINGLE_SHOT_LAYOUT_V2_ENABLED）：Agent v2 路径
//     tasks/{tid}/attempts/{attempt_id}/raw/... 写入（apiserver 读取双格式）。
//   - GenerationsEnabled（ANALYSIS_GENERATIONS_ENABLED）：多代分析写入与
//     active 切换（关闭时等价于桥接版本单代行为）。
//   - ReanalyzeEnabled（ANALYSIS_REANALYZE_ENABLED）：人工重分析入口。
type SingleShotConfig struct {
	LayoutV2Enabled    bool `mapstructure:"layout_v2_enabled"`
	GenerationsEnabled bool `mapstructure:"generations_enabled"`
	ReanalyzeEnabled   bool `mapstructure:"reanalyze_enabled"`
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

	// ---- 存储阶段一：Artifact 生命周期闭环 ----
	// LifecycleMode 运行模式：observe（回填/重算/统计/候选日志，不做自动
	// 到期删除；用户主动删除任务仍正常执行）或 enforce（完整自动删除状态机）。
	// compose 通过 ${ARTIFACT_LIFECYCLE_MODE:-observe} 注入。
	LifecycleMode string `mapstructure:"lifecycle_mode"`
	// ReconcileIntervalSec 策略重算/回填周期（秒），默认 300。
	ReconcileIntervalSec int `mapstructure:"reconcile_interval_sec"`
	// ReconcileBatch 每轮重算的最大 Artifact 数，默认 2000。
	ReconcileBatch int `mapstructure:"reconcile_batch"`
	// NotBeforeProtectionHours 首次回填与策略缩短时给予的清理保护期，默认 24。
	NotBeforeProtectionHours int `mapstructure:"not_before_protection_hours"`
	// 各类别的保留时长（小时）。未单独配置时：
	//   raw_large / intermediate 回退到 RawRetentionHours（ARTIFACT_RAW_RETENTION_HOURS）
	//   result 回退到 ResultRetentionHours（ARTIFACT_RESULT_RETENTION_HOURS）
	//   raw_portable 默认 168（7 天）
	//   diagnostic 默认 72
	//   manifest 永不过期
	RawLargeHours     int `mapstructure:"raw_large_hours"`
	RawPortableHours  int `mapstructure:"raw_portable_hours"`
	IntermediateHours int `mapstructure:"intermediate_hours"`
	DiagnosticHours   int `mapstructure:"diagnostic_hours"`
	// SupersededResultHours 阶段 4：被新代次替换的旧代 RESULT/INTERMEDIATE
	// 保留时长（小时），默认 72（ARTIFACT_SUPERSEDED_RESULT_HOURS）。
	SupersededResultHours int `mapstructure:"superseded_result_hours"`
	// ManifestPermanent 为 true 时 manifest 类永不过期（默认 true）。
	ManifestPermanent bool `mapstructure:"manifest_permanent"`
}

// BlobConfig 控制阶段二物理 Blob 存储。
// 扩展发布顺序（Release A → B → C）：先只部署兼容 Reader（所有开关关闭），
// 再开回填与压缩迁移，最后 24h 宽限期过后开旧对象 GC。
type BlobConfig struct {
	// BackfillEnabled 开启全量元数据回填：按有效引用 distinct object_key 创建
	// storage_blobs 并回填三张表的 blob_id。历史对象不搬迁、不重新计算内容哈希。
	BackfillEnabled bool `mapstructure:"backfill_enabled"`
	// MigrationEnabled 开启共享符号压缩迁移（kallsyms/ELF/SVG/folded ≥4KiB 文本）。
	MigrationEnabled bool `mapstructure:"migration_enabled"`
	// GCEnabled 开启 storage_object_gc 延迟删除（必须在兼容 Reader 上线且
	// 旧对象保留满宽限期后才允许打开）。
	GCEnabled bool `mapstructure:"gc_enabled"`
	// MinCompressBytes 迁移/新写入压缩阈值：大于等于该字节数的文本结果才压缩，默认 4096。
	MinCompressBytes int64 `mapstructure:"min_compress_bytes"`
	// GCSafeGraceHours 迁移入队到允许删除的宽限期（小时），默认 24。
	GCSafeGraceHours int `mapstructure:"gc_safe_grace_hours"`
	// MigrationBatch 每轮迁移/回填的最大对象数，默认 50。
	MigrationBatch int `mapstructure:"migration_batch"`
	// MigrationIntervalSec 迁移 worker 扫描周期（秒），默认 60。
	MigrationIntervalSec int `mapstructure:"migration_interval_sec"`
	// 迁移对象类型开关（MigrationEnabled=true 时按这些子开关分阶段执行）。
	MigrateKallsyms bool `mapstructure:"migrate_kallsyms"`
	MigrateELF      bool `mapstructure:"migrate_elf"`
	MigrateResults  bool `mapstructure:"migrate_results"`
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

// ContinuousParquetConfig 控制阶段五的 Continuous Parquet Block v2。
// 保留阶段三 gzip JSON Block v1 作为兼容基线与回退源；v2 是独立链路
// （continuous/v2/...），写入 Parquet 文件并登记到目录账本
// （continuous_parquet_blocks / block_files / block_members）。
//
// Mode 取值（CONTINUOUS_PARQUET_MODE，默认 off）：
//   - off：不写入 v2，不启动 v2 compactor；查询完全走 v1。Parquet reader
//     仍然可用（迁移重放/兼容读取），additive migration 正常应用。
//   - shadow：v2 双写 + 每完成小时自动对账；v1 仍是唯一查询源。
//   - prefer：按 coverage map 优先 v2，缺口回退 v1，继续双写。
//   - enforce：停止生成 v1 小时块（分钟 JSON 仅作 staging）；既有 v1
//     保留 24h 回滚窗口后按 200 对象/批分批删除。
//
// 对象布局固定为：
//
//	continuous/v2/tenant=default/date=YYYY-MM-DD/hour=HH/
//	  signal=cpu|metrics|histogram|db/resolution=raw|5m|1h/{block-id}-{part}.parquet
//
// raw/5m/1h 默认保留 24h/7d/30d；Continuous 硬配额 4 GiB（目标水位 3.6 GiB），
// staging、v1 fallback 与 v2 共用同一配额池。
type ContinuousParquetConfig struct {
	// Mode 运行模式：off | shadow | prefer | enforce。
	Mode string `mapstructure:"mode"`
	// Tenant 单租户固定为 default（当前系统只支持单租户）。
	Tenant string `mapstructure:"tenant"`
	// RawRetentionHours / Res5mRetentionHours / Res1hRetentionHours：
	// raw/5m/1h 分区保留时长（小时），默认 24 / 168 / 720。
	RawRetentionHours   int `mapstructure:"raw_retention_hours"`
	Res5mRetentionHours int `mapstructure:"res5m_retention_hours"`
	Res1hRetentionHours int `mapstructure:"res1h_retention_hours"`
	// QuotaBytes Continuous 全部存储（staging + v1 fallback + v2）硬配额，
	// 默认 4 GiB（CONTINUOUS_QUOTA_BYTES）。
	QuotaBytes int64 `mapstructure:"quota_bytes"`
	// QuotaTargetBytes 目标水位，默认 3.6 GiB（CONTINUOUS_QUOTA_TARGET_BYTES）。
	QuotaTargetBytes int64 `mapstructure:"quota_target_bytes"`
	// StagingMinutesRetention 分钟 JSON staging（未压缩 batch）最长保留时长
	// （分钟），默认 120（约 2 小时，v2 raw 块生成后即可清理）。
	StagingMinutesRetention int `mapstructure:"staging_minutes_retention"`
	// RowGroupTargetBytes 单 row group 目标大小（字节），默认 16 MiB。
	RowGroupTargetBytes int64 `mapstructure:"row_group_target_bytes"`
	// MaxPartBytes 单 Parquet 文件目标上限（字节），默认 128 MiB；超出时
	// 同一 logical block 拆分为多个 part。
	MaxPartBytes int64 `mapstructure:"max_part_bytes"`
	// ShadowBackfillHours shadow 模式启动时回填最近 N 个完整小时，默认 1。
	ShadowBackfillHours int `mapstructure:"shadow_backfill_hours"`
	// ReconcileIntervalSec 每小时对账/补偿 worker 扫描周期（秒），默认 300。
	ReconcileIntervalSec int `mapstructure:"reconcile_interval_sec"`
	// BlockIntervalSec v2 block 生成扫描周期（秒），默认 300。
	BlockIntervalSec int `mapstructure:"block_interval_sec"`
	// V1RollbackWindowHours enforce 删除 v1 前的回滚窗口（小时），默认 24。
	V1RollbackWindowHours int `mapstructure:"v1_rollback_window_hours"`
	// V1DeleteBatch 删除 v1 块/对象每批数量，默认 200。
	V1DeleteBatch int `mapstructure:"v1_delete_batch"`
	// MinFreeReserve 除 min_free_bytes 外为 v2 构建/迁移保留的额外空间
	// （字节），默认 512 MiB（CONTINUOUS_PARQUET_MIN_FREE_RESERVE）。
	MinFreeReserve int64 `mapstructure:"min_free_reserve"`
	// RequiredFreeExtraBytes 在 required_free 公式中额外保留的空间，默认 0。
	RequiredFreeExtraBytes int64 `mapstructure:"required_free_extra_bytes"`
	// ForecastWindowHours 每小时采集量预测窗口（用于 required_free 公式），默认 2。
	ForecastWindowHours int `mapstructure:"forecast_window_hours"`
	// RecoverHysteresisBytes 空间恢复到 required_free + 该值且连续两次
	// 60s 检查通过后才自动恢复采集，默认 128 MiB。
	RecoverHysteresisBytes int64 `mapstructure:"recover_hysteresis_bytes"`
	// RecoveryChecks 连续通过次数，默认 2。
	RecoveryChecks int `mapstructure:"recovery_checks"`
	// FineRowGCMode 阶段六细粒度行 GC 模式：off | observe | enforce，默认 off。
	//   - off：不统计不清理（阶段六未开启时的默认行为）。
	//   - observe：只统计候选与阻塞原因，不删除。
	//   - enforce：对小事务/固定批次清理 window/batch 元数据与 staging 对象。
	// 环境变量 CONTINUOUS_FINE_ROW_GC_MODE。
	FineRowGCMode string `mapstructure:"fine_row_gc_mode"`
	// HotMetadataRetentionMinutes 热 window/batch 元数据保留时长（分钟），
	// 默认 120（2 小时）。早于此时间的 window/batch 才允许被细粒度 GC 清理。
	// 环境变量 CONTINUOUS_HOT_METADATA_RETENTION_MINUTES。
	HotMetadataRetentionMinutes int `mapstructure:"hot_metadata_retention_minutes"`
	// FineRowGCBatch 每轮细粒度 GC 最多处理的行数，默认 1000。
	// 环境变量 CONTINUOUS_FINE_ROW_GC_BATCH。
	FineRowGCBatch int `mapstructure:"fine_row_gc_batch"`
	// CoverageRetentionHours coverage segment 保留时长（小时），默认 720（30 天）。
	// 环境变量 CONTINUOUS_COVERAGE_RETENTION_HOURS。
	CoverageRetentionHours int `mapstructure:"coverage_retention_hours"`
	// MigrationFailureRetryLimit 迁移失败重试上限：连续达到该次数且跨越至少
	// 30 分钟后标记 quarantined，默认 3。
	// 环境变量 CONTINUOUS_MIGRATION_FAILURE_RETRY_LIMIT。
	MigrationFailureRetryLimit int `mapstructure:"migration_failure_retry_limit"`
	// MigrationReceiptRetentionHours migration receipt 保留时长（小时），默认
	// 72。超过该时长的 batch receipt，若对应 profile_batches.bid 已不存在，则
	// 在 Parquet 维护周期内被回收（passed/revoked 均可回收），防止无界增长。
	// 环境变量 CONTINUOUS_MIGRATION_RECEIPT_RETENTION_HOURS。
	MigrationReceiptRetentionHours int `mapstructure:"migration_receipt_retention_hours"`
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
	v.SetDefault("retention.lifecycle_mode", "observe")
	v.SetDefault("retention.reconcile_interval_sec", 300)
	v.SetDefault("retention.reconcile_batch", 2000)
	v.SetDefault("retention.not_before_protection_hours", 24)
	v.SetDefault("retention.raw_large_hours", 24)
	v.SetDefault("retention.raw_portable_hours", 168)
	v.SetDefault("retention.intermediate_hours", 24)
	v.SetDefault("retention.diagnostic_hours", 72)
	v.SetDefault("retention.superseded_result_hours", 72)
	v.SetDefault("retention.manifest_permanent", true)
	v.SetDefault("continuous_block.enabled", false)
	v.SetDefault("continuous_block.window_sec", 3600)
	v.SetDefault("continuous_block.compaction_delay_sec", 600)
	v.SetDefault("continuous_block.compaction_interval_sec", 300)
	v.SetDefault("continuous_parquet.mode", "off")
	v.SetDefault("continuous_parquet.tenant", "default")
	v.SetDefault("continuous_parquet.raw_retention_hours", 24)
	v.SetDefault("continuous_parquet.res5m_retention_hours", 168)
	v.SetDefault("continuous_parquet.res1h_retention_hours", 720)
	v.SetDefault("continuous_parquet.quota_bytes", int64(4<<30))
	v.SetDefault("continuous_parquet.quota_target_bytes", int64(3600*1024*1024))
	v.SetDefault("continuous_parquet.staging_minutes_retention", 120)
	v.SetDefault("continuous_parquet.row_group_target_bytes", int64(16<<20))
	v.SetDefault("continuous_parquet.max_part_bytes", int64(128<<20))
	v.SetDefault("continuous_parquet.shadow_backfill_hours", 1)
	v.SetDefault("continuous_parquet.reconcile_interval_sec", 300)
	v.SetDefault("continuous_parquet.block_interval_sec", 300)
	v.SetDefault("continuous_parquet.v1_rollback_window_hours", 24)
	v.SetDefault("continuous_parquet.v1_delete_batch", 200)
	v.SetDefault("continuous_parquet.min_free_reserve", int64(512<<20))
	v.SetDefault("continuous_parquet.required_free_extra_bytes", int64(0))
	v.SetDefault("continuous_parquet.forecast_window_hours", 2)
	v.SetDefault("continuous_parquet.recover_hysteresis_bytes", int64(128<<20))
	v.SetDefault("continuous_parquet.recovery_checks", 2)
	v.SetDefault("continuous_parquet.migration_receipt_retention_hours", 72)
	v.SetDefault("blob.backfill_enabled", false)
	v.SetDefault("blob.migration_enabled", false)
	v.SetDefault("blob.gc_enabled", false)
	v.SetDefault("blob.min_compress_bytes", 4096)
	v.SetDefault("blob.gc_safe_grace_hours", 24)
	v.SetDefault("blob.migration_batch", 50)
	v.SetDefault("blob.migration_interval_sec", 60)
	v.SetDefault("blob.migrate_kallsyms", true)
	v.SetDefault("blob.migrate_elf", true)
	v.SetDefault("blob.migrate_results", true)
	v.SetDefault("storage_disk.path", "/tmp")
	v.SetDefault("storage_disk.warning_free_bytes", uint64(2<<30))
	v.SetDefault("storage_disk.critical_free_bytes", uint64(1<<30))
	v.SetDefault("storage_disk.min_free_bytes", uint64(512<<20))
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("log.output", "stdout")
	v.SetDefault("security.environment", "development")
	v.SetDefault("security.allow_insecure_transport", true)
	v.SetDefault("observability.metrics_enabled", true)
	v.SetDefault("profile.enabled", false)
	v.SetDefault("profile.timeout_sec", 5)
	v.SetDefault("agent_discovery.extra_ips", "")
	v.SetDefault("single_shot.layout_v2_enabled", false)
	v.SetDefault("single_shot.generations_enabled", false)
	v.SetDefault("single_shot.reanalyze_enabled", false)

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
	if envLifecycleMode := os.Getenv("ARTIFACT_LIFECYCLE_MODE"); envLifecycleMode != "" {
		v.Set("retention.lifecycle_mode", envLifecycleMode)
	}
	if envReconcileInterval := os.Getenv("ARTIFACT_LIFECYCLE_RECONCILE_INTERVAL_SEC"); envReconcileInterval != "" {
		v.Set("retention.reconcile_interval_sec", envReconcileInterval)
	}
	if envReconcileBatch := os.Getenv("ARTIFACT_LIFECYCLE_RECONCILE_BATCH"); envReconcileBatch != "" {
		v.Set("retention.reconcile_batch", envReconcileBatch)
	}
	if envNotBeforeProtection := os.Getenv("ARTIFACT_NOT_BEFORE_PROTECTION_HOURS"); envNotBeforeProtection != "" {
		v.Set("retention.not_before_protection_hours", envNotBeforeProtection)
	}
	if envRawLarge := os.Getenv("ARTIFACT_RAW_LARGE_HOURS"); envRawLarge != "" {
		v.Set("retention.raw_large_hours", envRawLarge)
	}
	if envRawPortable := os.Getenv("ARTIFACT_RAW_PORTABLE_HOURS"); envRawPortable != "" {
		v.Set("retention.raw_portable_hours", envRawPortable)
	}
	if envIntermediate := os.Getenv("ARTIFACT_INTERMEDIATE_HOURS"); envIntermediate != "" {
		v.Set("retention.intermediate_hours", envIntermediate)
	}
	if envDiagnostic := os.Getenv("ARTIFACT_DIAGNOSTIC_HOURS"); envDiagnostic != "" {
		v.Set("retention.diagnostic_hours", envDiagnostic)
	}
	if envSuperseded := os.Getenv("ARTIFACT_SUPERSEDED_RESULT_HOURS"); envSuperseded != "" {
		v.Set("retention.superseded_result_hours", envSuperseded)
	}
	if envLayoutV2 := os.Getenv("SINGLE_SHOT_LAYOUT_V2_ENABLED"); envLayoutV2 != "" {
		v.Set("single_shot.layout_v2_enabled", parseBoolEnv(envLayoutV2))
	}
	if envGenerations := os.Getenv("ANALYSIS_GENERATIONS_ENABLED"); envGenerations != "" {
		v.Set("single_shot.generations_enabled", parseBoolEnv(envGenerations))
	}
	if envReanalyze := os.Getenv("ANALYSIS_REANALYZE_ENABLED"); envReanalyze != "" {
		v.Set("single_shot.reanalyze_enabled", parseBoolEnv(envReanalyze))
	}
	if envManifestPermanent := os.Getenv("ARTIFACT_MANIFEST_PERMANENT"); envManifestPermanent != "" {
		v.Set("retention.manifest_permanent", parseBoolEnv(envManifestPermanent))
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
	if envPQMode := os.Getenv("CONTINUOUS_PARQUET_MODE"); envPQMode != "" {
		v.Set("continuous_parquet.mode", envPQMode)
	}
	if envPQTenant := os.Getenv("CONTINUOUS_PARQUET_TENANT"); envPQTenant != "" {
		v.Set("continuous_parquet.tenant", envPQTenant)
	}
	if envPQRaw := os.Getenv("CONTINUOUS_PARQUET_RAW_RETENTION_HOURS"); envPQRaw != "" {
		v.Set("continuous_parquet.raw_retention_hours", envPQRaw)
	}
	if envPQ5m := os.Getenv("CONTINUOUS_PARQUET_5M_RETENTION_HOURS"); envPQ5m != "" {
		v.Set("continuous_parquet.res5m_retention_hours", envPQ5m)
	}
	if envPQ1h := os.Getenv("CONTINUOUS_PARQUET_1H_RETENTION_HOURS"); envPQ1h != "" {
		v.Set("continuous_parquet.res1h_retention_hours", envPQ1h)
	}
	if envPQQuota := os.Getenv("CONTINUOUS_QUOTA_BYTES"); envPQQuota != "" {
		v.Set("continuous_parquet.quota_bytes", envPQQuota)
	}
	if envPQQuotaTarget := os.Getenv("CONTINUOUS_QUOTA_TARGET_BYTES"); envPQQuotaTarget != "" {
		v.Set("continuous_parquet.quota_target_bytes", envPQQuotaTarget)
	}
	if envPQStaging := os.Getenv("CONTINUOUS_STAGING_MINUTES_RETENTION"); envPQStaging != "" {
		v.Set("continuous_parquet.staging_minutes_retention", envPQStaging)
	}
	if envPQRG := os.Getenv("CONTINUOUS_PARQUET_ROW_GROUP_TARGET_BYTES"); envPQRG != "" {
		v.Set("continuous_parquet.row_group_target_bytes", envPQRG)
	}
	if envPQPart := os.Getenv("CONTINUOUS_PARQUET_MAX_PART_BYTES"); envPQPart != "" {
		v.Set("continuous_parquet.max_part_bytes", envPQPart)
	}
	if envPQBackfill := os.Getenv("CONTINUOUS_PARQUET_SHADOW_BACKFILL_HOURS"); envPQBackfill != "" {
		v.Set("continuous_parquet.shadow_backfill_hours", envPQBackfill)
	}
	if envPQReconcile := os.Getenv("CONTINUOUS_PARQUET_RECONCILE_INTERVAL_SEC"); envPQReconcile != "" {
		v.Set("continuous_parquet.reconcile_interval_sec", envPQReconcile)
	}
	if envPQInterval := os.Getenv("CONTINUOUS_PARQUET_BLOCK_INTERVAL_SEC"); envPQInterval != "" {
		v.Set("continuous_parquet.block_interval_sec", envPQInterval)
	}
	if envPQRollback := os.Getenv("CONTINUOUS_PARQUET_V1_ROLLBACK_WINDOW_HOURS"); envPQRollback != "" {
		v.Set("continuous_parquet.v1_rollback_window_hours", envPQRollback)
	}
	if envPQV1Del := os.Getenv("CONTINUOUS_PARQUET_V1_DELETE_BATCH"); envPQV1Del != "" {
		v.Set("continuous_parquet.v1_delete_batch", envPQV1Del)
	}
	if envPQReserve := os.Getenv("CONTINUOUS_PARQUET_MIN_FREE_RESERVE"); envPQReserve != "" {
		v.Set("continuous_parquet.min_free_reserve", envPQReserve)
	}
	if envPQExtra := os.Getenv("CONTINUOUS_PARQUET_REQUIRED_FREE_EXTRA_BYTES"); envPQExtra != "" {
		v.Set("continuous_parquet.required_free_extra_bytes", envPQExtra)
	}
	if envPQForecast := os.Getenv("CONTINUOUS_PARQUET_FORECAST_WINDOW_HOURS"); envPQForecast != "" {
		v.Set("continuous_parquet.forecast_window_hours", envPQForecast)
	}
	if envPQRecover := os.Getenv("CONTINUOUS_PARQUET_RECOVER_HYSTERESIS_BYTES"); envPQRecover != "" {
		v.Set("continuous_parquet.recover_hysteresis_bytes", envPQRecover)
	}
	if envPQChecks := os.Getenv("CONTINUOUS_PARQUET_RECOVERY_CHECKS"); envPQChecks != "" {
		v.Set("continuous_parquet.recovery_checks", envPQChecks)
	}
	if envFineGCMode := os.Getenv("CONTINUOUS_FINE_ROW_GC_MODE"); envFineGCMode != "" {
		v.Set("continuous_parquet.fine_row_gc_mode", envFineGCMode)
	}
	if envHotRet := os.Getenv("CONTINUOUS_HOT_METADATA_RETENTION_MINUTES"); envHotRet != "" {
		v.Set("continuous_parquet.hot_metadata_retention_minutes", envHotRet)
	}
	if envGCBatch := os.Getenv("CONTINUOUS_FINE_ROW_GC_BATCH"); envGCBatch != "" {
		v.Set("continuous_parquet.fine_row_gc_batch", envGCBatch)
	}
	if envCovRet := os.Getenv("CONTINUOUS_COVERAGE_RETENTION_HOURS"); envCovRet != "" {
		v.Set("continuous_parquet.coverage_retention_hours", envCovRet)
	}
	if envMigRetry := os.Getenv("CONTINUOUS_MIGRATION_FAILURE_RETRY_LIMIT"); envMigRetry != "" {
		v.Set("continuous_parquet.migration_failure_retry_limit", envMigRetry)
	}
	if envReceiptRet := os.Getenv("CONTINUOUS_MIGRATION_RECEIPT_RETENTION_HOURS"); envReceiptRet != "" {
		v.Set("continuous_parquet.migration_receipt_retention_hours", envReceiptRet)
	}
	if envBlobBackfill := os.Getenv("BLOB_BACKFILL_ENABLED"); envBlobBackfill != "" {
		v.Set("blob.backfill_enabled", parseBoolEnv(envBlobBackfill))
	}
	if envBlobMigration := os.Getenv("BLOB_MIGRATION_ENABLED"); envBlobMigration != "" {
		v.Set("blob.migration_enabled", parseBoolEnv(envBlobMigration))
	}
	if envBlobGC := os.Getenv("BLOB_GC_ENABLED"); envBlobGC != "" {
		v.Set("blob.gc_enabled", parseBoolEnv(envBlobGC))
	}
	if envBlobMinCompress := os.Getenv("BLOB_MIN_COMPRESS_BYTES"); envBlobMinCompress != "" {
		v.Set("blob.min_compress_bytes", envBlobMinCompress)
	}
	if envBlobGrace := os.Getenv("BLOB_GC_SAFE_GRACE_HOURS"); envBlobGrace != "" {
		v.Set("blob.gc_safe_grace_hours", envBlobGrace)
	}
	if envBlobBatch := os.Getenv("BLOB_MIGRATION_BATCH"); envBlobBatch != "" {
		v.Set("blob.migration_batch", envBlobBatch)
	}
	if envBlobInterval := os.Getenv("BLOB_MIGRATION_INTERVAL_SEC"); envBlobInterval != "" {
		v.Set("blob.migration_interval_sec", envBlobInterval)
	}
	if envMigrateKallsyms := os.Getenv("BLOB_MIGRATE_KALLSYMS"); envMigrateKallsyms != "" {
		v.Set("blob.migrate_kallsyms", parseBoolEnv(envMigrateKallsyms))
	}
	if envMigrateELF := os.Getenv("BLOB_MIGRATE_ELF"); envMigrateELF != "" {
		v.Set("blob.migrate_elf", parseBoolEnv(envMigrateELF))
	}
	if envMigrateResults := os.Getenv("BLOB_MIGRATE_RESULTS"); envMigrateResults != "" {
		v.Set("blob.migrate_results", parseBoolEnv(envMigrateResults))
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
	// 生命周期模式归一化（observe / enforce；其它值按 observe 处理，安全回滚）。
	mode := strings.ToLower(strings.TrimSpace(cfg.Retention.LifecycleMode))
	if mode != "enforce" && mode != "observe" {
		mode = "observe"
	}
	cfg.Retention.LifecycleMode = mode
	if cfg.Retention.ReconcileIntervalSec <= 0 {
		cfg.Retention.ReconcileIntervalSec = 300
	}
	if cfg.Retention.ReconcileBatch <= 0 {
		cfg.Retention.ReconcileBatch = 2000
	}
	if cfg.Retention.NotBeforeProtectionHours <= 0 {
		cfg.Retention.NotBeforeProtectionHours = 24
	}
	// 类别时长：未配置时用兼容回退（legacy env / 默认值）。
	if cfg.Retention.RawLargeHours <= 0 {
		cfg.Retention.RawLargeHours = cfg.Retention.RawRetentionHours
	}
	if cfg.Retention.RawPortableHours <= 0 {
		cfg.Retention.RawPortableHours = 168
	}
	if cfg.Retention.IntermediateHours <= 0 {
		cfg.Retention.IntermediateHours = cfg.Retention.RawRetentionHours
	}
	if cfg.Retention.DiagnosticHours <= 0 {
		cfg.Retention.DiagnosticHours = 72
	}
	if cfg.Retention.SupersededResultHours <= 0 {
		cfg.Retention.SupersededResultHours = 72
	}
	if cfg.Retention.ResultRetentionHours <= 0 {
		cfg.Retention.ResultRetentionHours = 720
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
	parquetMode := strings.ToLower(strings.TrimSpace(cfg.ContinuousParquet.Mode))
	switch parquetMode {
	case "off", "shadow", "prefer", "enforce":
	default:
		parquetMode = "off"
	}
	cfg.ContinuousParquet.Mode = parquetMode
	if strings.TrimSpace(cfg.ContinuousParquet.Tenant) == "" {
		cfg.ContinuousParquet.Tenant = "default"
	}
	if cfg.ContinuousParquet.RawRetentionHours <= 0 {
		cfg.ContinuousParquet.RawRetentionHours = 24
	}
	if cfg.ContinuousParquet.Res5mRetentionHours <= 0 {
		cfg.ContinuousParquet.Res5mRetentionHours = 168
	}
	if cfg.ContinuousParquet.Res1hRetentionHours <= 0 {
		cfg.ContinuousParquet.Res1hRetentionHours = 720
	}
	if cfg.ContinuousParquet.RawRetentionHours >= cfg.ContinuousParquet.Res5mRetentionHours ||
		cfg.ContinuousParquet.Res5mRetentionHours >= cfg.ContinuousParquet.Res1hRetentionHours {
		return fmt.Errorf("Continuous Parquet 保留期必须满足 raw < 5m < 1h")
	}
	if cfg.ContinuousParquet.QuotaBytes <= 0 {
		cfg.ContinuousParquet.QuotaBytes = 4 << 30
	}
	if cfg.ContinuousParquet.QuotaTargetBytes <= 0 {
		cfg.ContinuousParquet.QuotaTargetBytes = cfg.ContinuousParquet.QuotaBytes * 9 / 10
	}
	if cfg.ContinuousParquet.QuotaTargetBytes >= cfg.ContinuousParquet.QuotaBytes {
		return fmt.Errorf("Continuous Parquet 目标水位必须小于硬配额")
	}
	if cfg.ContinuousParquet.StagingMinutesRetention <= 0 {
		cfg.ContinuousParquet.StagingMinutesRetention = 120
	}
	if cfg.ContinuousParquet.RowGroupTargetBytes <= 0 {
		cfg.ContinuousParquet.RowGroupTargetBytes = 16 << 20
	}
	if cfg.ContinuousParquet.MaxPartBytes <= 0 {
		cfg.ContinuousParquet.MaxPartBytes = 128 << 20
	}
	if cfg.ContinuousParquet.MaxPartBytes < cfg.ContinuousParquet.RowGroupTargetBytes {
		return fmt.Errorf("Continuous Parquet max_part_bytes 必须不小于 row_group_target_bytes")
	}
	if cfg.ContinuousParquet.ShadowBackfillHours <= 0 {
		cfg.ContinuousParquet.ShadowBackfillHours = 1
	}
	if cfg.ContinuousParquet.ReconcileIntervalSec <= 0 {
		cfg.ContinuousParquet.ReconcileIntervalSec = 300
	}
	if cfg.ContinuousParquet.BlockIntervalSec <= 0 {
		cfg.ContinuousParquet.BlockIntervalSec = 300
	}
	if cfg.ContinuousParquet.V1RollbackWindowHours <= 0 {
		cfg.ContinuousParquet.V1RollbackWindowHours = 24
	}
	if cfg.ContinuousParquet.V1DeleteBatch <= 0 {
		cfg.ContinuousParquet.V1DeleteBatch = 200
	}
	if cfg.ContinuousParquet.ForecastWindowHours <= 0 {
		cfg.ContinuousParquet.ForecastWindowHours = 2
	}
	if cfg.ContinuousParquet.RecoveryChecks <= 0 {
		cfg.ContinuousParquet.RecoveryChecks = 2
	}
	if mode := strings.ToLower(strings.TrimSpace(cfg.ContinuousParquet.FineRowGCMode)); mode == "" {
		cfg.ContinuousParquet.FineRowGCMode = "off"
	} else {
		switch mode {
		case "off", "observe", "enforce":
			cfg.ContinuousParquet.FineRowGCMode = mode
		default:
			cfg.ContinuousParquet.FineRowGCMode = "off"
		}
	}
	if cfg.ContinuousParquet.HotMetadataRetentionMinutes <= 0 {
		cfg.ContinuousParquet.HotMetadataRetentionMinutes = 120
	}
	if cfg.ContinuousParquet.FineRowGCBatch <= 0 {
		cfg.ContinuousParquet.FineRowGCBatch = 1000
	}
	if cfg.ContinuousParquet.CoverageRetentionHours <= 0 {
		cfg.ContinuousParquet.CoverageRetentionHours = 720
	}
	if cfg.ContinuousParquet.MigrationFailureRetryLimit <= 0 {
		cfg.ContinuousParquet.MigrationFailureRetryLimit = 3
	}
	if cfg.Blob.MinCompressBytes <= 0 {
		cfg.Blob.MinCompressBytes = 4096
	}
	if cfg.Blob.GCSafeGraceHours <= 0 {
		cfg.Blob.GCSafeGraceHours = 24
	}
	if cfg.Blob.MigrationBatch <= 0 {
		cfg.Blob.MigrationBatch = 50
	}
	if cfg.Blob.MigrationIntervalSec <= 0 {
		cfg.Blob.MigrationIntervalSec = 60
	}
	if cfg.Retention.BatchLimit <= 0 {
		cfg.Retention.BatchLimit = 200
	}
	if strings.TrimSpace(cfg.StorageDisk.Path) == "" {
		cfg.StorageDisk.Path = "/tmp"
	}
	if cfg.StorageDisk.WarningFreeBytes == 0 {
		cfg.StorageDisk.WarningFreeBytes = 2 << 30
	}
	if cfg.StorageDisk.CriticalFreeBytes == 0 {
		cfg.StorageDisk.CriticalFreeBytes = 1 << 30
	}
	if cfg.StorageDisk.MinFreeBytes == 0 {
		cfg.StorageDisk.MinFreeBytes = 512 << 20
	}
	// 阈值链必须严格递减且为正；违反时直接拒绝启动，避免等级判定失真。
	if cfg.StorageDisk.WarningFreeBytes <= cfg.StorageDisk.CriticalFreeBytes ||
		cfg.StorageDisk.CriticalFreeBytes <= cfg.StorageDisk.MinFreeBytes {
		return fmt.Errorf("存储磁盘阈值配置非法：必须满足 warning_free_bytes(%d) > critical_free_bytes(%d) > min_free_bytes(%d) > 0",
			cfg.StorageDisk.WarningFreeBytes, cfg.StorageDisk.CriticalFreeBytes, cfg.StorageDisk.MinFreeBytes)
	}
	return nil
}
