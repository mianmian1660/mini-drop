// ============================================================
// model/blob.go — 存储阶段二：逻辑引用 → 物理 Blob → MinIO 对象
// ============================================================
// 目标模型：
//
//	Artifact / SymbolFile / KernelSymbolFile（逻辑名称、权限、保留期、任务关系）
//	      │ blob_id
//	      ▼
//	StorageBlob（物理 key、存储/逻辑大小、双哈希、压缩格式、状态）
//	      │ object_key
//	      ▼
//	MinIO immutable object（内容寻址 CAS key）
//
// 设计要点：
//   - object_key 唯一：真实物理对象 key（既有历史对象回填时等于逻辑 key，
//     迁移/新写入后是 blobs/sha256/<ab>/<logical_sha256>/<format>-v<schema>.gz）。
//   - logical_sha256 可为空以兼容历史对象（不重新计算内容哈希）。
//   - 对非空 (logical_sha256, format, compression) 建部分唯一索引，实现内容寻址去重。
//   - 不持久化 ref_count：始终根据 artifacts/symbol_files/kernel_symbol_files 的
//     有效引用实时计算，避免计数漂移。
// ============================================================

package model

import "time"

// StorageBlob 状态：uploading / ready / deleting / deleted / failed
const (
	BlobStatusUploading = "uploading"
	BlobStatusReady     = "ready"
	BlobStatusDeleting  = "deleting"
	BlobStatusDeleted   = "deleted"
	BlobStatusFailed    = "failed"
)

// 压缩格式（compression 列）
const (
	CompressionNone = ""
	CompressionGzip = "gzip"
	CompressionZstd = "zstd"
)

// 内容格式（format 列）
const (
	BlobFormatPprof     = "pprof"
	BlobFormatKallsyms  = "kallsyms"
	BlobFormatELF       = "elf"
	BlobFormatSVG       = "svg"
	BlobFormatFolded    = "folded"
	BlobFormatJSON      = "json"
	BlobFormatMarkdown  = "markdown"
	BlobFormatPerfData  = "perf.data"
	BlobFormatCollapsed = "collapsed"
	BlobFormatUnknown   = ""
)

// BlobSchemaV1 阶段二引入的 schema 版本。
const BlobSchemaV1 = "1"

// StorageBlob — 一份物理对象元数据
type StorageBlob struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	ObjectKey string `gorm:"column:object_key;size:512;uniqueIndex:uidx_storage_blobs_object_key" json:"object_key"`
	// LogicalSHA256 未压缩规范内容的哈希；历史对象为 NULL（不重新计算）。
	// 用 *string：NULL 不参与内容寻址唯一索引（与 SQL 部分索引语义一致）。
	LogicalSHA256 *string `gorm:"column:logical_sha256;size:64;uniqueIndex:uidx_storage_blobs_content,priority:1" json:"logical_sha256"`
	// StoredSHA256 MinIO 中实际字节的哈希。
	StoredSHA256 string `gorm:"column:stored_sha256;size:64" json:"stored_sha256"`
	// StoredSize 实际存储字节数（压缩后）。
	StoredSize int64 `gorm:"column:stored_size;not null;default:0" json:"stored_size"`
	// LogicalSize 解压后的内容大小；历史对象无法确知时等于 stored_size。
	LogicalSize int64 `gorm:"column:logical_size;not null;default:0" json:"logical_size"`
	Format      string `gorm:"column:format;size:32;uniqueIndex:uidx_storage_blobs_content,priority:2" json:"format"`
	// SchemaVersion 内容 schema 版本（如 pprof v1）。
	SchemaVersion string `gorm:"column:schema_version;size:32" json:"schema_version"`
	Compression   string `gorm:"column:compression;size:32;uniqueIndex:uidx_storage_blobs_content,priority:3" json:"compression"`
	// ContentEncoding 透明 HTTP 编码（SVG/folded 等浏览器资源用 gzip 透明解码；
	// pprof 作为文件格式本身保持 gzip，此列为空）。
	ContentEncoding string `gorm:"column:content_encoding;size:32" json:"content_encoding"`
	ContentType     string `gorm:"column:content_type;size:128" json:"content_type"`
	Status          string `gorm:"column:status;size:32;not null;default:ready;index:idx_storage_blobs_status_retry,priority:1" json:"status"`
	// DeleteReason 对象删除原因（last_reference_expired 等），tombstone 保留。
	DeleteReason string `gorm:"column:delete_reason;size:128" json:"delete_reason"`
	// DeleteAttempts 删除尝试次数；失败按 1m→5m→30m→2h→6h 退避重试。
	DeleteAttempts int `gorm:"column:delete_attempts;not null;default:0" json:"delete_attempts"`
	// NextDeleteAttemptAt 下次删除重试时间。
	NextDeleteAttemptAt *time.Time `gorm:"column:next_delete_attempt_at;index:idx_storage_blobs_status_retry,priority:2" json:"next_delete_attempt_at"`
	LastDeleteError     string     `gorm:"column:last_delete_error;size:1024" json:"last_delete_error"`
	// VerifiedAt 最近一次回读校验时间（迁移影子写入后设置）。
	VerifiedAt *time.Time `gorm:"column:verified_at" json:"verified_at"`
	// DeletedAt 对象已删除的 tombstone 时间。
	DeletedAt *time.Time `gorm:"column:deleted_at" json:"deleted_at"`
	CreatedAt time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

// IsDeleted 判断 Blob 是否已为墓碑（对象已删除）。
func (b *StorageBlob) IsDeleted() bool {
	return b == nil || b.DeletedAt != nil || b.Status == BlobStatusDeleted
}

// StorageObjectGC 原因
const (
	GCMigrationReason       = "migration"
	GCLastReferenceReason   = "last_reference"
	GCOverrideReason        = "override"
)

// StorageObjectGC — 迁移后旧物理 key 的延迟 GC 队列。
// 新旧对象至少并存 24 小时（not_before = 入队时间 + 宽限期），并且
// 必须在兼容 Reader 已上线后才允许删除（宽限期由部署节奏保证）。
type StorageObjectGC struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	ObjectKey string `gorm:"column:object_key;size:512;uniqueIndex:uidx_storage_object_gc_key" json:"object_key"`
	Reason    string `gorm:"column:reason;size:64" json:"reason"`
	// NotBefore 最早允许删除的时间（迁移入队时间 + 24h 宽限期）。
	NotBefore *time.Time `gorm:"column:not_before" json:"not_before"`
	// DeleteAttempts 删除尝试次数；失败按 1m→5m→30m→2h→6h 退避重试。
	DeleteAttempts int `gorm:"column:delete_attempts;not null;default:0" json:"delete_attempts"`
	// NextDeleteAttemptAt 下次重试时间。
	NextDeleteAttemptAt *time.Time `gorm:"column:next_delete_attempt_at" json:"next_delete_attempt_at"`
	LastDeleteError     string     `gorm:"column:last_delete_error;size:1024" json:"last_delete_error"`
	// DeletedAt 删除成功时间（行保留作审计）。
	DeletedAt *time.Time `gorm:"column:deleted_at" json:"deleted_at"`
	CreatedAt time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}
