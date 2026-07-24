// ============================================================
// model/task_attempt.go — 任务执行尝试表（Track A1）
// ============================================================
// 一个 HotmethodTask 可能被执行多次（掉线重下发 / 用户手动重试 / 采样成功但
// 上报失败后的恢复）。TaskAttempt 记录每一次具体执行，重试不会覆盖之前的
// 诊断证据。对应新复刻指南第 2.3 节。
// ============================================================

package model

import "time"

// TaskAttempt — 任务的一次具体执行尝试
type TaskAttempt struct {
	ID               uint       `gorm:"primaryKey" json:"id"`
	TaskTID          string     `gorm:"column:task_tid;size:64;index:idx_task_attempt_seq,unique" json:"task_tid"` // 关联 HotmethodTask.TID
	AttemptSeq       int        `gorm:"column:attempt_seq;index:idx_task_attempt_seq,unique" json:"attempt_seq"`   // 第几次尝试（1,2,3...）
	AgentIP          string     `gorm:"column:agent_ip;size:45" json:"agent_ip"`
	RunnerVersion    string     `gorm:"column:runner_version;size:32" json:"runner_version"`
	ExitCode         int        `gorm:"column:exit_code" json:"exit_code"`
	ErrorCode        string     `gorm:"column:error_code;size:64" json:"error_code"`
	ErrorMessage     string     `gorm:"column:error_message;size:1024" json:"error_message"`
	ResourceSnapshot []byte     `gorm:"column:resource_snapshot;type:jsonb" json:"resource_snapshot"` // CPU/内存/IO 快照
	BeginTime        *time.Time `gorm:"column:begin_time" json:"begin_time"`
	EndTime          *time.Time `gorm:"column:end_time" json:"end_time"`
	CreatedAt        time.Time  `gorm:"column:created_at" json:"created_at"`
}
