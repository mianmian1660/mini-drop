// ============================================================
// model/detection.go — 检测→触发深度诊断：数据模型
// ============================================================
// 对应 docs/detection-trigger-pipeline-design.md 的三张表：
//   SentinelRule    哨兵规则（信号 + 静态下限 + 滚动基线灵敏度 + 持续性判断 + 冷却期）
//   DetectionState  滚动基线（RecentValues，§10.2）+ 冷却期缓存（按规则维度，一条规则一行）
//   DetectionEvent  每一次判异的审计记录（触发的和被闸门跳过的都记）
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
	// Signal 取值：sched_latency / io_latency / io_syscall_latency（histogram 类，见 §4）
	// / db_snapshot（判异方式完全不同，见 §10.1，evaluateDBSnapshotRule 单独处理）。
	Signal string `gorm:"column:signal;size:32" json:"signal"`
	// Metric 取值：histogram 类信号用 p50/p95/p99；db_snapshot 信号用 lock_wait/digest
	// （见 §10.1，语义和取值方式与 histogram 类完全不同，不共用同一套判异逻辑）。
	Metric string `gorm:"column:metric;size:16" json:"metric"`
	// FloorValue 静态下限：观测值必须超过它才可能触发，避免"从0.1ms变到0.3ms"这种
	// 绝对值无意义的抖动报警（见 §4.1）。histogram 类信号还要再过 KFactor 的滚动基线判断；
	// db_snapshot 的 lock_wait 只看 FloorValue，digest 用 FloorValue 做环比判断的下限过滤
	// （见 §10.1）。
	FloorValue float64 `gorm:"column:floor_value" json:"floor_value"`
	// KFactor 判异灵敏度：histogram 类信号是滚动基线偏离倍数（见 §10.2/§4.1）：
	// score = |观测值-滚动中位数| / (1.4826*滚动MAD)，score 超过 KFactor 才算偏离正常波动，
	// 默认5（约等于5个"稳健标准差"），MAD=0（数据不足/零波动）时退化为只看 FloorValue。
	// db_snapshot 的 digest metric 复用这个字段做"环比上一窗口暴涨多少倍"的判断（§10.1），
	// lock_wait metric 不使用。
	KFactor float64 `gorm:"column:k_factor;default:5" json:"k_factor"`
	// CooldownSeconds 冷却期：同一条规则再次触发前必须间隔的秒数，避免持续异常刷屏。
	// 不属于文档 MVP 范围内明确要求的能力，但没有它会导致单次异常在每个检测 tick
	// 都新建一个诊断任务；作为最基本的安全闸门提前实现，见设计文档 §5.1"单飞/去重"。
	CooldownSeconds int `gorm:"column:cooldown_seconds;default:900" json:"cooldown_seconds"`
	// PersistenceWindows/PersistenceMinHits 持续性判断（见 docs/detection-trigger-pipeline-design.md
	// §10.3）：最近 PersistenceWindows 个窗口里，至少 PersistenceMinHits 个超过 FloorValue 才算命中，
	// 用于过滤单点抖动（一次网络抖动/GC 停顿）误判为异常。默认 1/1，等价于"只看最新一个窗口"，
	// 与升级前的 MVP 行为完全一致。
	PersistenceWindows int            `gorm:"column:persistence_windows;default:1" json:"persistence_windows"`
	PersistenceMinHits int            `gorm:"column:persistence_min_hits;default:1" json:"persistence_min_hits"`
	Enabled            bool           `gorm:"column:enabled;default:true" json:"enabled"`
	UID                string         `gorm:"column:uid;size:64" json:"uid"`
	UserName           string         `gorm:"column:user_name;size:128" json:"user_name"`
	CreatedAt          time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt          time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt          gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
	// CanManage 非落库字段：当前请求方是否有权限删除/管理该规则，见 canManageOwner。
	CanManage bool `gorm:"-" json:"can_manage"`
}

// DetectionState 按规则缓存的滚动状态：LastFiredAt 做冷却期判断；RecentValues 是最近
// detectionBaselineWindowSize（100）个窗口的观测值（json 数组），供 §10.2 的滚动中位数+MAD
// 判异使用，每次判异（无论是否触发）都会更新。
type DetectionState struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	RuleSID      string     `gorm:"column:rule_sid;uniqueIndex;size:64" json:"rule_sid"`
	RecentValues []byte     `gorm:"column:recent_values;type:jsonb" json:"recent_values,omitempty"`
	LastFiredAt  *time.Time `gorm:"column:last_fired_at" json:"last_fired_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// DetectionEvent 每一次判异的审计记录。Status 取值：
//
//	fired                   命中阈值，已创建诊断任务
//	fired_no_action         db_snapshot 信号命中阈值，但暂无可执行的诊断 TaskKind
//	                        （script_diagnostic 的 Runner 未接入），只记审计事件不建任务
//	                        （见 docs/detection-trigger-pipeline-design.md §10.1）
//	skipped_cooldown        命中阈值，但仍在冷却期内
//	skipped_low_coverage    该窗口采样覆盖率过低，判异结果不可信，跳过
//	skipped_low_persistence 最新窗口超阈值，但持续性不足（见 §10.3 PersistenceWindows/MinHits），判定为单点抖动
//	skipped_low_deviation   超过静态下限，但未偏离滚动基线（见 §10.2 KFactor），判定为正常波动
//	skipped_overlap         目标已有活跃诊断任务在跑，跳过（对齐 schedule.go 的单飞检查）
//	skipped_low_disk        磁盘预算不足，跳过（对齐 canStartCollection）
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
