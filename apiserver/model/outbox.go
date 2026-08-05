// ============================================================
// model/outbox.go — 事务性发件箱表（Track A5）
// ============================================================
// 新复刻指南第 9.6 节 Transactional Outbox：领域记录（HotmethodTask）和
// "需要下发"这件事在同一个数据库事务里落地，真正的下游调用（gRPC 下发）
// 交给后台 Dispatcher 异步执行，成功与否体现在 PublishedAt 是否被置位。
// ============================================================

package model

import "time"

// Outbox 聚合类型
const (
	OutboxAggregateTask = "task"
)

// Outbox 事件类型
const (
	OutboxEventDispatchTask = "dispatch_task"
)

// Outbox 状态：pending 等待领取、processing 已被当前进程领取、dead_letter 达到重试上限。
const (
	OutboxStatusPending    = "pending"
	OutboxStatusProcessing = "processing"
	OutboxStatusDeadLetter = "dead_letter"
)

// Outbox — 事务性发件箱记录
type Outbox struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	Aggregate     string     `gorm:"column:aggregate;size:64" json:"aggregate"`             // 聚合类型，如 "task"
	AggregateID   string     `gorm:"column:aggregate_id;size:64;index" json:"aggregate_id"` // 关联的 HotmethodTask.TID
	Event         string     `gorm:"column:event;size:64" json:"event"`                     // 事件类型，如 "dispatch_task"
	Payload       []byte     `gorm:"column:payload;type:jsonb" json:"payload"`              // 下发所需的完整参数（序列化后的 CreateTaskReq）
	Status        string     `gorm:"column:status;size:32;default:pending;index:idx_outbox_claim" json:"status"`
	Attempt       int        `gorm:"column:attempt;default:0" json:"attempt"`
	LastError     string     `gorm:"column:last_error;size:1024" json:"last_error"`
	NextAttemptAt *time.Time `gorm:"column:next_attempt_at;index:idx_outbox_unpublished" json:"next_attempt_at"`
	LockedAt      *time.Time `gorm:"column:locked_at;index:idx_outbox_claim" json:"locked_at"`
	LockedBy      string     `gorm:"column:locked_by;size:128" json:"locked_by"`
	DeadLetterAt  *time.Time `gorm:"column:dead_letter_at" json:"dead_letter_at"`
	PublishedAt   *time.Time `gorm:"column:published_at;index:idx_outbox_unpublished" json:"published_at"` // NULL = 尚未发布（待处理）
	CreatedAt     time.Time  `gorm:"column:created_at;index:idx_outbox_unpublished" json:"created_at"`
}
