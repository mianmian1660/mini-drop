// ============================================================
// model/symbol.go — 符号库账本表（Track 阶段三）
// ============================================================
// 落地 docs/symbolization-design.md §7.1 的数据模型：SymbolFile 以
// build_id 为主键去重存放符号文件（跨任务、跨 Agent 只存一份）；
// TaskBuildID 记录某个任务需要哪些符号，供 Analysis 直接查表，
// 不用重新解析 perf.data。
// ============================================================

package model

import "time"

// SymbolFile 状态：0 上传中 / 1 就绪 / 2 失败（对应 002_symbol_store.sql 的 SMALLINT 定义）
const (
	SymbolFileStatusUploading = 0
	SymbolFileStatusReady     = 1
	SymbolFileStatusFailed    = 2
)

// SymbolFile — build-id 索引的符号文件账本
type SymbolFile struct {
	BuildID    string     `gorm:"column:build_id;primaryKey;size:128" json:"build_id"`
	FileName   string     `gorm:"column:file_name;size:512" json:"file_name"`
	ObjectKey  string     `gorm:"column:object_key;size:512" json:"object_key"`
	SizeBytes  int64      `gorm:"column:size_bytes" json:"size_bytes"`
	SHA256     string     `gorm:"column:sha256;size:64" json:"sha256"`
	Status     int        `gorm:"column:status;default:0;index" json:"status"`
	CreatedAt  time.Time  `gorm:"column:created_at" json:"created_at"`
	LastUsedAt *time.Time `gorm:"column:last_used_at" json:"last_used_at"`
	// BlobID 指向 storage_blobs（阶段二）；NULL 表示尚未回填（兼容原 object_key）。
	BlobID *uint `gorm:"column:blob_id;index:idx_symbol_files_blob_id" json:"blob_id"`
}

// KernelSymbolFile — sha256 索引的 /proc/kallsyms 快照账本。
// 同一台机器同一内核的 kallsyms 内容通常稳定，按内容哈希去重后，
// 多个任务只保存 Artifact 引用，不再重复存 10MB 级快照。
type KernelSymbolFile struct {
	SHA256        string     `gorm:"column:sha256;primaryKey;size:64" json:"sha256"`
	ObjectKey     string     `gorm:"column:object_key;size:512" json:"object_key"`
	KernelRelease string     `gorm:"column:kernel_release;size:128" json:"kernel_release"`
	Hostname      string     `gorm:"column:hostname;size:256" json:"hostname"`
	TargetIP      string     `gorm:"column:target_ip;size:45;index" json:"target_ip"`
	SizeBytes     int64      `gorm:"column:size_bytes" json:"size_bytes"`
	Status        int        `gorm:"column:status;default:0;index" json:"status"`
	CreatedAt     time.Time  `gorm:"column:created_at" json:"created_at"`
	LastUsedAt    *time.Time `gorm:"column:last_used_at" json:"last_used_at"`
	// BlobID 指向 storage_blobs（阶段二）；NULL 表示尚未回填（兼容原 object_key）。
	BlobID *uint `gorm:"column:blob_id;index:idx_kernel_symbol_files_blob_id" json:"blob_id"`
}

// TaskBuildID — 某个任务需要哪些 build-id（tid, build_id 复合主键）
type TaskBuildID struct {
	TID     string `gorm:"column:tid;primaryKey;size:64" json:"tid"`
	BuildID string `gorm:"column:build_id;primaryKey;size:128" json:"build_id"`
	DSOPath string `gorm:"column:dso_path;size:1024" json:"dso_path"`
}
