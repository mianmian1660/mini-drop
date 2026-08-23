package model

import (
	"time"
)

// ============================================================
// 阶段五：Continuous Parquet Block v2 目录账本
// ============================================================
// 保留阶段三 gzip JSON Block v1（continuous_profile_blocks）作为兼容基线与
// 回退源；v2 是独立链路，逻辑分区（tenant + UTC 小时 + signal + resolution）
// 只能有一个 active 版本，物理文件通过 block_files 分片登记。
//
// 状态机（连续变量见 ContinuousParquetBlockStatus*）：
//
//	building → validating → active
//	                  ↘ failed
//	active → superseded → deleting → deleted（deleted 为墓碑行，保留元数据）
//
// 对象布局：
//
//	continuous/v2/{tenant}/date=YYYY-MM-DD/hour=HH/
//	  signal={cpu|metrics|histogram|db}/resolution={raw|5m|1h}/{block-id}-{part}.parquet
// ============================================================

const (
	ContinuousParquetSignalCPU       = "cpu"
	ContinuousParquetSignalMetrics   = "metrics"
	ContinuousParquetSignalHistogram = "histogram"
	ContinuousParquetSignalDB        = "db"
)

const (
	ContinuousParquetResolutionRaw = "raw"
	ContinuousParquetResolution5m  = "5m"
	ContinuousParquetResolution1h  = "1h"
)

// 合法分辨率集合（按粒度从小到大）。
var ContinuousParquetResolutions = []string{
	ContinuousParquetResolutionRaw,
	ContinuousParquetResolution5m,
	ContinuousParquetResolution1h,
}

const (
	// 状态机常量。deleted 行作为墓碑永久保留（object_key/sha256/delete_reason），
	// 供孤儿对账与历史扫描排除。
	ContinuousParquetStatusBuilding   = "building"
	ContinuousParquetStatusValidating = "validating"
	ContinuousParquetStatusActive     = "active"
	ContinuousParquetStatusFailed     = "failed"
	ContinuousParquetStatusSuperseded = "superseded"
	ContinuousParquetStatusDeleting   = "deleting"
	ContinuousParquetStatusDeleted    = "deleted"
)

const (
	// 校验状态。
	ContinuousParquetValidationPending = "pending"
	ContinuousParquetValidationPassed  = "passed"
	ContinuousParquetValidationFailed  = "failed"
	ContinuousParquetValidationSkipped = "skipped"
)

// ContinuousParquetBlock 是 v2 逻辑块：一个 (tenant, bucket_start, signal,
// resolution) 分区 + 一个版本。active 版本是查询唯一来源；其它版本处于
// building/validating/failed/superseded/deleting/deleted 之一。
type ContinuousParquetBlock struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	BlockID     string    `gorm:"column:block_id;uniqueIndex;size:64" json:"block_id"`
	Tenant      string    `gorm:"column:tenant;size:64;default:default" json:"tenant"`
	BucketStart time.Time `gorm:"column:bucket_start;index" json:"bucket_start"`
	BucketEnd   time.Time `gorm:"column:bucket_end" json:"bucket_end"`
	SignalType  string    `gorm:"column:signal_type;size:32;index" json:"signal_type"`
	Resolution  string    `gorm:"column:resolution;size:16;index" json:"resolution"`
	// Version 同分区内递增；每次重写（迟到 batch / 保留移除 / 降采样源变更）+1。
	Version    int    `gorm:"column:version;default:1" json:"version"`
	Status     string `gorm:"column:status;size:16;default:building;index" json:"status"`
	Validation string `gorm:"column:validation;size:16;default:pending" json:"validation"`
	// SourceBlockID raw 块为 ""；5m/1h 指向来源分辨率块（降采样 lineage）。
	SourceBlockID string `gorm:"column:source_block_id;size:64" json:"source_block_id"`
	// MemberRefs 指 block_members 中的 lineage 记录（由成员表外键体现），
	// 这里保留冗余计数便于快速校验。
	MemberCount  int    `gorm:"column:member_count" json:"member_count"`
	RowCount     int64  `gorm:"column:row_count" json:"row_count"`
	ValueTotal   uint64 `gorm:"column:value_total" json:"value_total"`
	SampleTotal  uint64 `gorm:"column:sample_total" json:"sample_total"`
	SessionCount int    `gorm:"column:session_count" json:"session_count"`
	ProcessCount int    `gorm:"column:process_count" json:"process_count"`
	BytesTotal   int64  `gorm:"column:bytes_total" json:"bytes_total"`
	// FirstRowTime / LastRowTime 时间范围（min/max 时间一致性的对账依据）。
	FirstRowTime time.Time `gorm:"column:first_row_time" json:"first_row_time"`
	LastRowTime  time.Time `gorm:"column:last_row_time" json:"last_row_time"`
	// RowGroupBoundaries 每 row group 的起始行索引 + min/max 时间（查询选组用）。
	RowGroupBoundaries []byte     `gorm:"column:row_group_boundaries;type:jsonb" json:"row_group_boundaries"`
	SupersededAt       *time.Time `gorm:"column:superseded_at" json:"superseded_at"`
	ReplacedBy         string     `gorm:"column:replaced_by;size:64" json:"replaced_by"`
	DeleteReason       string     `gorm:"column:delete_reason;size:64" json:"delete_reason"`
	CreatedAt          time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at" json:"updated_at"`
	// ReconcileStatus 阶段六对账状态：pending|passed|failed|quarantined。
	// prefer/enforce 查询与细粒度 GC 只接受 validation='passed' AND
	// reconcile_status='passed' 的块。
	ReconcileStatus string `gorm:"column:reconcile_status;size:16;default:pending" json:"reconcile_status"`
	// ReconciledAt 最近一次对账完成时间。
	ReconciledAt *time.Time `gorm:"column:reconciled_at" json:"reconciled_at"`
	// ReconcileError 最近一次对账错误描述（failed/quarantined 时填充）。
	ReconcileError string `gorm:"column:reconcile_error;type:text" json:"reconcile_error"`
	// TombstoneAt 墓碑时间（status=deleted）。列名保持 deleted_at，避免 gorm
	// 软删语义（字段名+类型匹配才会触发软删；这里显式用 *time.Time + 别名）。
	TombstoneAt *time.Time `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (ContinuousParquetBlock) TableName() string { return "continuous_parquet_blocks" }

// ContinuousParquetBlockFile 是逻辑块下的物理 shard（part）。
type ContinuousParquetBlockFile struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	BlockID       string     `gorm:"column:block_id;size:64;index;uniqueIndex:uq_cpq_files_block_part,priority:1" json:"block_id"`
	PartIndex     int        `gorm:"column:part_index;uniqueIndex:uq_cpq_files_block_part,priority:2" json:"part_index"`
	ObjectKey     string     `gorm:"column:object_key;uniqueIndex;size:768" json:"object_key"`
	SizeBytes     int64      `gorm:"column:size_bytes" json:"size_bytes"`
	SHA256        string     `gorm:"column:sha256;size:64" json:"sha256"`
	RowGroupCount int        `gorm:"column:row_group_count" json:"row_group_count"`
	RowCount      int64      `gorm:"column:row_count" json:"row_count"`
	CreatedAt     time.Time  `gorm:"column:created_at" json:"created_at"`
	TombstoneAt   *time.Time `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (ContinuousParquetBlockFile) TableName() string { return "continuous_parquet_block_files" }

// ContinuousParquetBlockMember 是 lineage 账本：
//   - raw 块：一行对应一个来源（batch 或 window）；
//   - 5m/1h 块：一行对应一个来源块（来源块 lineage）。
type ContinuousParquetBlockMember struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	BlockID string `gorm:"column:block_id;size:64;index;uniqueIndex:uq_cpq_members_source,priority:1" json:"block_id"`
	// SourceKind: "batch" | "window" | "block"。
	SourceKind string `gorm:"column:source_kind;size:16;uniqueIndex:uq_cpq_members_source,priority:2" json:"source_kind"`
	// SourceRef: batch 为 bid，window 为 window_id/时间窗，block 为来源 block_id。
	SourceRef   string    `gorm:"column:source_ref;size:128;uniqueIndex:uq_cpq_members_source,priority:3" json:"source_ref"`
	StartTime   time.Time `gorm:"column:start_time" json:"start_time"`
	EndTime     time.Time `gorm:"column:end_time" json:"end_time"`
	SampleCount uint64    `gorm:"column:sample_count" json:"sample_count"`
	ValueTotal  uint64    `gorm:"column:value_total" json:"value_total"`
	// SessionSID 来源 session（删除 batch 后仍可审计 Block 来源）。
	SessionSID string `gorm:"column:session_sid;size:64" json:"session_sid"`
	// RowCount 该来源对块的贡献行数（按信号行计）。
	RowCount  int64     `gorm:"column:row_count" json:"row_count"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

func (ContinuousParquetBlockMember) TableName() string { return "continuous_parquet_block_members" }

// ============================================================
// 阶段六：细粒度目录模型
// ============================================================

// 对账状态常量。
const (
	ContinuousParquetReconcilePending     = "pending"
	ContinuousParquetReconcilePassed      = "passed"
	ContinuousParquetReconcileFailed      = "failed"
	ContinuousParquetReconcileQuarantined = "quarantined"
)

// 迁移失败记录状态常量。
const (
	ContinuousMigrationFailureRetrying    = "retrying"
	ContinuousMigrationFailureQuarantined = "quarantined"
	ContinuousMigrationFailureResolved    = "resolved"
	ContinuousMigrationFailurePurged      = "purged"
)

// ContinuousCoverageSegment 阶段六：精确覆盖区间。由 Parquet raw Block
// 激活时按 session/信号/小时重建，连续或间隔 ≤5s 的 window 合并为一个
// segment，真实缺口原样保留。segment 独立于 raw Block 生命周期，保留
// 30 天（CONTINUOUS_COVERAGE_RETENTION_HOURS），避免 raw 降采样后丢失
// 精确缺口信息。
type ContinuousCoverageSegment struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// Tenant 单租户固定 default。
	Tenant string `gorm:"column:tenant;size:64;default:default" json:"tenant"`
	// SessionSID + SignalType + SegmentStart + SegmentEnd 构成唯一键
	// （同一区间不允许重复 segment）。
	SessionSID   string    `gorm:"column:session_sid;size:64;uniqueIndex:uq_ccs_segment,priority:1" json:"session_sid"`
	SignalType   string    `gorm:"column:signal_type;size:32;uniqueIndex:uq_ccs_segment,priority:2" json:"signal_type"`
	SegmentStart time.Time `gorm:"column:segment_start;uniqueIndex:uq_ccs_segment,priority:3" json:"segment_start"`
	SegmentEnd   time.Time `gorm:"column:segment_end;uniqueIndex:uq_ccs_segment,priority:4" json:"segment_end"`
	// SampleCount 段内样本总数（区间/样本统计对账依据）。
	SampleCount uint64 `gorm:"column:sample_count" json:"sample_count"`
	// SourceBlock 生成该段的 raw Block ID（可追溯审计）。
	SourceBlock string `gorm:"column:source_block;size:64" json:"source_block"`
	// SourceVersion 生成该段的 Block 版本。
	SourceVersion int `gorm:"column:source_version;default:1" json:"source_version"`
	// Resolution 段来源分辨率（当前固定 raw）。
	Resolution string    `gorm:"column:resolution;size:16;default:raw" json:"resolution"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (ContinuousCoverageSegment) TableName() string { return "continuous_coverage_segments" }

// ContinuousMigrationFailure 阶段六：细粒度迁移/读取失败异常记录。
// 可恢复失败按后台重试；连续 3 次失败且跨越至少 30 分钟后标记
// quarantined；对象确实不存在时以该记录替代失效的细粒度元数据，
// 不伪造 Parquet 数据或覆盖率。
type ContinuousMigrationFailure struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// SourceKind: "batch" | "window" | "block" | "object"。
	SourceKind string `gorm:"column:source_kind;size:16;uniqueIndex:uq_cmf_source,priority:1" json:"source_kind"`
	// SourceRef: batch 为 bid，window 为 window id，block 为 block_id。
	SourceRef string `gorm:"column:source_ref;size:128;uniqueIndex:uq_cmf_source,priority:2" json:"source_ref"`
	// SessionSID 失败来源所属 session（可为空）。
	SessionSID string `gorm:"column:session_sid;size:64;index" json:"session_sid"`
	// ObjectKey 失败对象 key（缺失对象时用于审计）。
	ObjectKey string `gorm:"column:object_key;size:768" json:"object_key"`
	// ErrorType: missing_object | read_error | parse_error | source_mismatch | ...
	ErrorType string `gorm:"column:error_type;size:64" json:"error_type"`
	// ErrorMessage 错误描述。
	ErrorMessage string `gorm:"column:error_message;type:text" json:"error_message"`
	// FirstSeenAt / LastSeenAt 首次/最近出现时间。
	FirstSeenAt time.Time `gorm:"column:first_seen_at" json:"first_seen_at"`
	LastSeenAt  time.Time `gorm:"column:last_seen_at" json:"last_seen_at"`
	// RetryCount 已重试次数。
	RetryCount int `gorm:"column:retry_count;default:0" json:"retry_count"`
	// Status: retrying | quarantined | resolved | purged。
	Status    string    `gorm:"column:status;size:16;default:retrying" json:"status"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (ContinuousMigrationFailure) TableName() string { return "continuous_migration_failures" }
