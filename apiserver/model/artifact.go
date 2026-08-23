// ============================================================
// model/artifact.go — 产物元数据表（Track A1）
// ============================================================
// 任务产生的每一个文件（perf.data / flamegraph.svg / top.json ...）一条记录。
// 数据库只存对象存储的 key，不存长期可访问 URL（URL 一律用临时签名生成）。
// 对应新复刻指南第 2.4 节。
//
// 存储阶段一（Artifact 生命周期闭环）扩展：
//   - Retention 列复用为 canonical retention_class（不建重复字段）
//   - expires_at / retention_not_before / retention_policy_version 支撑保留策略
//   - deleting / deleted 状态 + delete_* 字段支撑清理状态机
//   - 删除成功后保留 tombstone 行（object_key / size / sha256 / 删除原因永久保留），
//     不允许被普通 upsert 直接复活
// ============================================================

package model

import "time"

// Artifact 类型：RAW（原始采样文件）/ INTERMEDIATE（中间产物）/
// RESULT（分析结果）/ LOG（任务日志）/ MANIFEST（产物清单）
const (
	ArtifactKindRaw          = "RAW"
	ArtifactKindIntermediate = "INTERMEDIATE"
	ArtifactKindResult       = "RESULT"
	ArtifactKindLog          = "LOG"
	ArtifactKindManifest     = "MANIFEST"
)

// Artifact 状态：uploading（上传中）/ ready（可用）/ failed（上传失败）/
// deleting（清理中，等待删除对象）/ deleted（已删除，保留墓碑）
const (
	ArtifactStatusUploading = "uploading"
	ArtifactStatusReady     = "ready"
	ArtifactStatusFailed    = "failed"
	ArtifactStatusDeleting  = "deleting"
	ArtifactStatusDeleted   = "deleted"
)

// 保留类别（retention_class，写入 retention 列）
const (
	RetentionClassRawLarge     = "raw_large"
	RetentionClassRawPortable  = "raw_portable"
	RetentionClassIntermediate = "intermediate"
	RetentionClassDiagnostic   = "diagnostic"
	RetentionClassResult       = "result"
	RetentionClassManifest     = "manifest"
	RetentionClassUnknown      = ""
)

// 主动删除原因
const (
	DeleteReasonTaskDeleted    = "task_deleted"
	DeleteReasonExpired        = "expired"
	DeleteReasonStaleUploading = "stale_uploading"
)

// Artifact — 一份任务产物的元数据
type Artifact struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// TaskTID 原有单列 index 保留，再加入 A5 新增的 idx_artifacts_task_kind 联合索引
	// （task_tid, kind），对应新复刻指南 9.4 节原文列出的同名索引。
	TaskTID   string `gorm:"column:task_tid;size:64;index;index:idx_artifacts_task_kind,priority:1;uniqueIndex:uidx_artifacts_task_kind_key,priority:1" json:"task_tid"`
	AttemptID uint   `gorm:"column:attempt_id;index" json:"attempt_id"` // 关联 TaskAttempt.ID，0 表示尚未关联到具体尝试
	Kind      string `gorm:"column:kind;size:32;index:idx_artifacts_task_kind,priority:2;uniqueIndex:uidx_artifacts_task_kind_key,priority:2" json:"kind"`
	ObjectKey string `gorm:"column:object_key;size:512;uniqueIndex:uidx_artifacts_task_kind_key,priority:3" json:"object_key"` // 如 "tid/perf.data"
	// Size 含义固定为对象存储实际字节数（压缩后字节数，若有 compression）。
	Size        int64  `gorm:"column:size" json:"size"`
	SHA256      string `gorm:"column:sha256;size:64" json:"sha256"`
	ETag        string `gorm:"column:etag;size:128" json:"etag"`
	Hash        string `gorm:"column:hash;size:128" json:"hash"`
	ManifestKey string `gorm:"column:manifest_key;size:512" json:"manifest_key"`
	// Retention 复用为 canonical retention_class（raw_large/raw_portable/...）。
	Retention   string    `gorm:"column:retention;size:64" json:"retention_class"`
	ContentType string    `gorm:"column:content_type;size:128" json:"content_type"`
	Status      string    `gorm:"column:status;size:32;default:ready;index:idx_artifacts_ready_expiry,priority:1;index:idx_artifacts_deleting_retry,priority:1" json:"status"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`

	// ---- 存储阶段一：生命周期字段 ----
	// ExpiresAt 有效到期时间；非终态任务的 Artifact 为 NULL（暂不计算到期）。
	ExpiresAt *time.Time `gorm:"column:expires_at;index:idx_artifacts_ready_expiry,priority:2" json:"expires_at"`
	// RetentionPolicyVersion 当前生效的策略版本；reconciler 发现不一致时重算。
	RetentionPolicyVersion string `gorm:"column:retention_policy_version;size:64" json:"retention_policy_version"`
	// RetentionTaskState 记录计算期限时任务所处的状态类别；任务从 active 进入
	// done/diagnostic 后，即使配置版本未变化，reconciler 也会重新计算期限。
	RetentionTaskState string `gorm:"column:retention_task_state;size:32" json:"retention_task_state"`
	// RetentionNotBefore 最早允许清理的时间（迁移回填/策略缩短的 24h 保护期）。
	RetentionNotBefore *time.Time `gorm:"column:retention_not_before" json:"retention_not_before"`
	// LogicalSize 未压缩逻辑字节数（可选，压缩产物记录解压后大小）。
	LogicalSize int64  `gorm:"column:logical_size" json:"logical_size"`
	Compression string `gorm:"column:compression;size:32" json:"compression"` // gzip / zstd / ""（未压缩）
	// DeleteReason 删除原因（expired / task_deleted / stale_uploading）；tombstone 永久保留。
	DeleteReason string `gorm:"column:delete_reason;size:128" json:"delete_reason"`
	// DeleteAttempts 删除尝试次数；失败按 1m→5m→30m→2h→6h 退避重试。
	DeleteAttempts int `gorm:"column:delete_attempts;default:0" json:"delete_attempts"`
	// NextDeleteAttemptAt 下次重试时间（deleting 状态时有效）。
	NextDeleteAttemptAt *time.Time `gorm:"column:next_delete_attempt_at;index:idx_artifacts_deleting_retry,priority:2" json:"next_delete_attempt_at"`
	LastDeleteError     string     `gorm:"column:last_delete_error;size:1024" json:"last_delete_error"`
	// DeletedAt tombstone 时间；非 NULL 表示对象已删除，行保留。
	DeletedAt *time.Time `gorm:"column:deleted_at" json:"deleted_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// IsDeleted 判断是否为墓碑行。
func (a *Artifact) IsDeleted() bool {
	return a == nil || a.DeletedAt != nil || a.Status == ArtifactStatusDeleted
}
