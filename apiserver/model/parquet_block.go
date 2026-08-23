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
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
}

func (ContinuousParquetBlockMember) TableName() string { return "continuous_parquet_block_members" }
