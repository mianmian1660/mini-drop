// ============================================================
// model/analysis_job.go — 异步分析作业表（Track A1）
// ============================================================
// 一次异步分析的租约状态，供分析 Worker 用
// `FOR UPDATE SKIP LOCKED` 领取（新复刻指南第 7.3 节的领取 SQL）。
// 采集状态（HotmethodTask.Status）和分析状态在这里彻底分离，
// 对应新复刻指南第 2.5 节。
//
// 阶段 4（单次采样最终存储模型）扩展：
//   - task_tid 单列唯一约束已删除（015 迁移），一个任务可有多条 AnalysisJob；
//     (task_tid, generation) 承担代次唯一性。
//   - attempt_id：本次分析使用的采集尝试；generation：任务内单调递增代次；
//     trigger：initial（采集通知自动创建）/ manual（人工重分析）；
//     requested_by：人工重分析请求者；superseded_at：被新代次替换的时间。
// ============================================================

package model

import "time"

// AnalysisJob 状态：pending（待处理）/ running（处理中）/
// success（成功）/ failed（不可重试失败）/ retry（可重试失败，等待重新领取）
const (
	AnalysisJobStatusPending = "pending"
	AnalysisJobStatusRunning = "running"
	AnalysisJobStatusSuccess = "success"
	AnalysisJobStatusFailed  = "failed"
	AnalysisJobStatusRetry   = "retry"
)

// AnalysisJob 触发方式：采集通知自动创建 / 人工重分析。
const (
	AnalysisJobTriggerInitial = "initial"
	AnalysisJobTriggerManual   = "manual"
)

// AnalysisJob — 一次异步分析作业（阶段 4：一个任务可有多代作业）
type AnalysisJob struct {
	ID                uint       `gorm:"primaryKey" json:"id"`
	TaskTID           string     `gorm:"column:task_tid;size:64;index:idx_analysis_jobs_task_attempt_status,priority:1" json:"task_tid"`
	Pipeline          string     `gorm:"column:pipeline;size:64" json:"pipeline"` // perf_flamegraph / bpf_histogram ...
	Status            string     `gorm:"column:status;size:32;default:pending;index:idx_analysis_job_claim" json:"status"`
	Attempt           int        `gorm:"column:attempt;default:0" json:"attempt"`
	MaxAttempts       int        `gorm:"column:max_attempts;default:3" json:"max_attempts"`
	LastError         string     `gorm:"column:last_error;size:1024" json:"last_error"`
	InputArtifactIDs  []byte     `gorm:"column:input_artifact_ids;type:jsonb" json:"input_artifact_ids"`
	OutputArtifactIDs []byte     `gorm:"column:output_artifact_ids;type:jsonb" json:"output_artifact_ids"`
	LeaseOwner        string     `gorm:"column:lease_owner;size:64" json:"lease_owner"`
	LeaseExpiresAt    *time.Time `gorm:"column:lease_expires_at" json:"lease_expires_at"`
	AnalyzerVersion   string     `gorm:"column:analyzer_version;size:32" json:"analyzer_version"`
	// ---- 阶段 4：多代分析 ----
	AttemptID     uint       `gorm:"column:attempt_id;index:idx_analysis_jobs_task_attempt_status,priority:2" json:"attempt_id"` // 输入 RAW 所属 TaskAttempt.ID
	Generation    int        `gorm:"column:generation;default:0" json:"generation"`                                              // 任务内单调递增（1 起）
	Trigger       string     `gorm:"column:trigger;size:32;default:initial" json:"trigger"`                                     // initial / manual
	RequestedBy   string     `gorm:"column:requested_by;size:128" json:"requested_by"`
	SupersededAt  *time.Time `gorm:"column:superseded_at" json:"superseded_at"`
	CreatedAt     time.Time  `gorm:"column:created_at;index:idx_analysis_job_claim" json:"created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at" json:"updated_at"`
}
