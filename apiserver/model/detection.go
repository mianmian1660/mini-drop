// ============================================================
// model/detection.go — 检测→触发深度诊断：数据模型
// ============================================================
// 对应 docs/detection-trigger-pipeline-design.md 的三张表：
//   SentinelRule    哨兵规则（信号 + 固定阈值 + 冷却期）
//   DetectionState  滚动基线/冷却期缓存（按规则维度，一条规则一行）
//   DetectionEvent  每一次判异的审计记录（触发的和被闸门跳过的都记）
// MVP 阶段：仅支持固定阈值（FloorValue），不做滚动中位数/MAD——
// RecentValues 字段先建好列，供后续迭代往里追加数据，本阶段暂不使用。
// ============================================================

package model

import (
	"time"

	"gorm.io/gorm"
)

// SentinelRule 哨兵规则：持续采集某个信号超过固定阈值时，自动触发一次深度诊断。
type SentinelRule struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	SID  string `gorm:"column:sid;uniqueIndex;size:64" json:"sid"`
	Name string `gorm:"column:name;size:256" json:"name"`
	// TargetIP + Signal + Metric 唯一确定"盯哪台机器的哪个信号"。
	TargetIP string `gorm:"column:target_ip;size:45;index" json:"target_ip"`
	// Signal 取值：sched_latency / io_latency / io_syscall_latency（MVP 仅启用 sched_latency）。
	Signal string `gorm:"column:signal;size:32" json:"signal"`
	// Metric 取值：p50 / p95 / p99。
	Metric string `gorm:"column:metric;size:16" json:"metric"`
	// FloorValue 固定阈值：观测值超过它就触发（MVP 判异逻辑，不做滚动基线）。
	FloorValue float64 `gorm:"column:floor_value" json:"floor_value"`
	// CooldownSeconds 冷却期：同一条规则再次触发前必须间隔的秒数，避免持续异常刷屏。
	// 不属于文档 MVP 范围内明确要求的能力，但没有它会导致单次异常在每个检测 tick
	// 都新建一个诊断任务；作为最基本的安全闸门提前实现，见设计文档 §5.1"单飞/去重"。
	CooldownSeconds int            `gorm:"column:cooldown_seconds;default:900" json:"cooldown_seconds"`
	Enabled         bool           `gorm:"column:enabled;default:true" json:"enabled"`
	UID             string         `gorm:"column:uid;size:64" json:"uid"`
	UserName        string         `gorm:"column:user_name;size:128" json:"user_name"`
	CreatedAt       time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt       gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
	// CanManage 非落库字段：当前请求方是否有权限删除/管理该规则，见 canManageOwner。
	CanManage bool `gorm:"-" json:"can_manage"`
}

// DetectionState 按规则缓存的滚动状态：目前只用 LastFiredAt 做冷却期判断；
// RecentValues 是为后续"滚动中位数+MAD"迭代预留的列，MVP 阶段不写入也不读取。
type DetectionState struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	RuleSID      string     `gorm:"column:rule_sid;uniqueIndex;size:64" json:"rule_sid"`
	RecentValues []byte     `gorm:"column:recent_values;type:jsonb" json:"recent_values,omitempty"`
	LastFiredAt  *time.Time `gorm:"column:last_fired_at" json:"last_fired_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// DetectionEvent 每一次判异的审计记录。Status 取值：
//
//	fired                 命中阈值，已创建诊断任务
//	skipped_cooldown      命中阈值，但仍在冷却期内
//	skipped_low_coverage  该窗口采样覆盖率过低，判异结果不可信，跳过
//	skipped_overlap       目标已有活跃诊断任务在跑，跳过（对齐 schedule.go 的单飞检查）
//	skipped_low_disk      磁盘预算不足，跳过（对齐 canStartCollection）
type DetectionEvent struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	RuleSID       string    `gorm:"column:rule_sid;size:64;index" json:"rule_sid"`
	EvaluatedAt   time.Time `gorm:"column:evaluated_at;index" json:"evaluated_at"`
	Signal        string    `gorm:"column:signal;size:32" json:"signal"`
	Metric        string    `gorm:"column:metric;size:16" json:"metric"`
	ObservedValue float64   `gorm:"column:observed_value" json:"observed_value"`
	FloorValue    float64   `gorm:"column:floor_value" json:"floor_value"`
	Status        string    `gorm:"column:status;size:32" json:"status"`
	// ChildTID 触发成功时指向创建出的 HotmethodTask.TID（同时也是该任务 MasterTaskTID 的值）。
	ChildTID string `gorm:"column:child_tid;size:64" json:"child_tid,omitempty"`
}
