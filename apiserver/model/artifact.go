// ============================================================
// model/artifact.go — 产物元数据表（Track A1）
// ============================================================
// 任务产生的每一个文件（perf.data / flamegraph.svg / top.json ...）一条记录。
// 数据库只存对象存储的 key，不存长期可访问 URL（URL 一律用临时签名生成）。
// 对应新复刻指南第 2.4 节。
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

// Artifact 状态：uploading（上传中）/ ready（可用）/ failed（上传失败）
const (
	ArtifactStatusUploading = "uploading"
	ArtifactStatusReady     = "ready"
	ArtifactStatusFailed    = "failed"
)

// Artifact — 一份任务产物的元数据
type Artifact struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// TaskTID 原有单列 index 保留，再加入 A5 新增的 idx_artifacts_task_kind 联合索引
	// （task_tid, kind），对应新复刻指南 9.4 节原文列出的同名索引。
	TaskTID     string    `gorm:"column:task_tid;size:64;index;index:idx_artifacts_task_kind,priority:1;uniqueIndex:uidx_artifacts_task_kind_key,priority:1" json:"task_tid"`
	AttemptID   uint      `gorm:"column:attempt_id;index" json:"attempt_id"` // 关联 TaskAttempt.ID，0 表示尚未关联到具体尝试
	Kind        string    `gorm:"column:kind;size:32;index:idx_artifacts_task_kind,priority:2;uniqueIndex:uidx_artifacts_task_kind_key,priority:2" json:"kind"`
	ObjectKey   string    `gorm:"column:object_key;size:512;uniqueIndex:uidx_artifacts_task_kind_key,priority:3" json:"object_key"` // 如 "tid/perf.data"
	Size        int64     `gorm:"column:size" json:"size"`
	SHA256      string    `gorm:"column:sha256;size:64" json:"sha256"`
	ContentType string    `gorm:"column:content_type;size:128" json:"content_type"`
	Status      string    `gorm:"column:status;size:32;default:ready" json:"status"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
}
