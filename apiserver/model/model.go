// ============================================================
// model (数据模型) 小白版注释
// ============================================================
// 这个文件定义了数据库的 7 张表结构
// GORM 会根据这些 struct 自动创建/更新数据库表
// 每个 struct 对应一张表，每个字段对应一列
//
// Go 语法小课堂：
//   type XXX struct { ... }  = 定义一个结构体
//   字段后面的 `gorm:"..."`   = GORM 标签，告诉 GORM 怎么处理这个字段
//   json:"..."               = JSON 序列化时的字段名
// ============================================================

package model

import (
	"time" // 时间类型

	"gorm.io/gorm" // GORM 软删除
)

// ----------------------------------------------------------
// UserInfo — 用户表
// ----------------------------------------------------------
type UserInfo struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	UID    string `gorm:"column:uid;uniqueIndex;size:64" json:"uid"`
	Name   string `gorm:"column:name;size:128" json:"name"`
	Groups []byte `gorm:"column:groups;type:jsonb" json:"groups"`
	Key    string `gorm:"column:key;size:256" json:"key"`
}

// ----------------------------------------------------------
// AgentInfo — Agent 表（记录所有接入的 Agent）
// ----------------------------------------------------------
type AgentInfo struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	Hostname       string    `gorm:"column:hostname;size:256" json:"hostname"`
	IPAddr         string    `gorm:"column:ip_addr;size:45;index" json:"ip_addr"`
	Online         bool      `gorm:"column:online;default:false" json:"online"`
	AgentID        string    `gorm:"column:agent_id;size:128;index" json:"agent_id"`
	UID            string    `gorm:"column:uid;size:64" json:"uid"`
	GID            string    `gorm:"column:gid;size:64" json:"gid"`
	Version        string    `gorm:"column:version;size:32" json:"version"`
	Environment    string    `gorm:"column:environment;size:64" json:"environment"`
	Capabilities   []byte    `gorm:"column:capabilities;type:jsonb" json:"capabilities"`
	Labels         []byte    `gorm:"column:labels;type:jsonb" json:"labels"`
	ResourceBudget []byte    `gorm:"column:resource_budget;type:jsonb" json:"resource_budget"`
	SupportedOS    string    `gorm:"column:supported_os;size:64" json:"supported_os"`
	Status         string    `gorm:"column:status;size:32;default:online" json:"status"`
	LastSeen       time.Time `gorm:"column:last_seen" json:"last_seen"`
	CreatedAt      time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// ----------------------------------------------------------
// AgentAuditLog — Agent 在线/离线/恢复审计表
// ----------------------------------------------------------
type AgentAuditLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	IPAddr    string    `gorm:"column:ip_addr;size:45;index" json:"ip_addr"`
	Hostname  string    `gorm:"column:hostname;size:256" json:"hostname"`
	Event     string    `gorm:"column:event;size:64" json:"event"`
	Reason    string    `gorm:"column:reason;size:1024" json:"reason"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

// ----------------------------------------------------------
// HotmethodTask — 任务表（最核心的表！）
// ----------------------------------------------------------
type HotmethodTask struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	TID          string `gorm:"column:tid;uniqueIndex;size:64" json:"tid"`
	Name         string `gorm:"column:name;size:256" json:"name"`
	TaskKind     string `gorm:"column:task_kind;size:64;index" json:"task_kind"`
	RequestID    string `gorm:"column:request_id;size:64;index" json:"request_id"`
	Type         uint32 `gorm:"column:type;default:0" json:"type"`
	ProfilerType uint32 `gorm:"column:profiler_type;default:0" json:"profiler_type"`
	// TargetIP 参与 A5 新增的 idx_tasks_target_status 联合索引（target_ip, status, create_time），
	// 支持"某目标机器下特定状态任务"这类查询（新复刻指南 9.4 节）。
	TargetIP      string `gorm:"column:target_ip;size:45;index:idx_tasks_target_status,priority:1" json:"target_ip"`
	RequestParams []byte `gorm:"column:request_params;type:jsonb" json:"request_params"`
	// Status 原有的单列 index 保留（现有查询很多按 status 单独过滤），
	// 再加入 idx_tasks_target_status 联合索引的第二列。
	Status         int    `gorm:"column:status;default:0;index;index:idx_tasks_target_status,priority:2" json:"status"`
	StatusInfo     string `gorm:"column:status_info;size:1024" json:"status_info"`
	AnalysisStatus int    `gorm:"column:analysis_status;default:0" json:"analysis_status"`
	// UID 同时属于两个联合索引：A2 的幂等键唯一索引、A5 新增的 idx_tasks_uid_created
	// （uid, create_time），支持 ListTasks 里"按 uid 过滤 + 按创建时间排序"的查询模式。
	UID      string `gorm:"column:uid;size:64;uniqueIndex:idx_task_uid_idempotency;index:idx_tasks_uid_created,priority:1" json:"uid"`
	UserName string `gorm:"column:user_name;size:128" json:"user_name"`
	// IdempotencyKey 幂等键（A2）：和 UID 组成联合唯一索引。
	// 未提供时为 nil（SQL NULL），多条 NULL 之间不冲突；提供了就必须在同一用户下唯一，
	// 重复请求会命中已有任务而不是新建一条（新复刻指南 4.2 节）。
	IdempotencyKey *string `gorm:"column:idempotency_key;size:128;uniqueIndex:idx_task_uid_idempotency" json:"idempotency_key,omitempty"`
	// CreateTime 同时是 idx_tasks_uid_created 和 idx_tasks_target_status 两个联合索引的最后一列。
	CreateTime      time.Time  `gorm:"column:create_time;index:idx_tasks_uid_created,priority:2;index:idx_tasks_target_status,priority:3" json:"create_time"`
	BeginTime       *time.Time `gorm:"column:begin_time" json:"begin_time"`
	EndTime         *time.Time `gorm:"column:end_time" json:"end_time"`
	MasterTaskTID   string     `gorm:"column:master_task_tid;size:64" json:"master_task_tid"`
	DeadlineUnixMS  int64      `gorm:"column:deadline_unix_ms" json:"deadline_unix_ms"`
	ResourceBudget  []byte     `gorm:"column:resource_budget;type:jsonb" json:"resource_budget"`
	CancelRequested bool       `gorm:"column:cancel_requested;default:false;index" json:"cancel_requested"`
	CanceledAt      *time.Time `gorm:"column:canceled_at" json:"canceled_at"`
	// 存储阶段一：任务级 Artifact 固定（pin）。任务字段是 pin 的唯一当前状态来源：
	// 任务固定后新产生的 Artifact 自动受保护，无需批量复制 pin 状态。
	ArtifactsPinned    bool           `gorm:"column:artifacts_pinned;default:false" json:"artifacts_pinned"`
	ArtifactsPinnedAt  *time.Time     `gorm:"column:artifacts_pinned_at" json:"artifacts_pinned_at"`
	ArtifactsPinnedBy  string         `gorm:"column:artifacts_pinned_by;size:128" json:"artifacts_pinned_by"`
	ArtifactsPinReason string         `gorm:"column:artifacts_pin_reason;size:256" json:"artifacts_pin_reason"`
	DeletedAt          gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
	CanManage          bool           `gorm:"-" json:"can_manage"`
}

// ----------------------------------------------------------
// TaskStatusEvent — 任务状态迁移审计表
// ----------------------------------------------------------
type TaskStatusEvent struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// TID 原有单列 index 保留，再加入 A5 新增的 idx_task_events_tid_created 联合索引，
	// 支持 fetchTaskStatusEvents 里"按 tid 过滤 + 按时间排序"的查询模式（9.4 节）。
	TID          string    `gorm:"column:tid;size:64;index;index:idx_task_events_tid_created,priority:1" json:"tid"`
	FromStatus   int       `gorm:"column:from_status" json:"from_status"`
	ToStatus     int       `gorm:"column:to_status" json:"to_status"`
	Reason       string    `gorm:"column:reason;size:1024" json:"reason"`
	Source       string    `gorm:"column:source;size:64" json:"source"`
	Sequence     int64     `gorm:"column:sequence;index" json:"sequence"`
	AttemptID    uint      `gorm:"column:attempt_id;index" json:"attempt_id"`
	SourceModule string    `gorm:"column:source_module;size:64" json:"source_module"`
	Payload      []byte    `gorm:"column:payload;type:jsonb" json:"payload"`
	CreatedAt    time.Time `gorm:"column:created_at;index:idx_task_events_tid_created,priority:2" json:"created_at"`
}

// ----------------------------------------------------------
// MultiTask — 组合任务表（一个任务包含多个子任务）
// ----------------------------------------------------------
type MultiTask struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	TID               string    `gorm:"column:tid;uniqueIndex;size:64" json:"tid"`
	SubTIDs           []byte    `gorm:"column:sub_tids;type:jsonb" json:"sub_tids"`
	Type              uint32    `gorm:"column:type" json:"type"`
	Status            int       `gorm:"column:status" json:"status"`
	AnalysisStatus    int       `gorm:"column:analysis_status" json:"analysis_status"`
	TriggerType       uint32    `gorm:"column:trigger_type" json:"trigger_type"`
	AggregationPolicy string    `gorm:"column:aggregation_policy;size:32;default:ALL_REQUIRED" json:"aggregation_policy"`
	Quorum            int       `gorm:"column:quorum;default:0" json:"quorum"`
	DAG               []byte    `gorm:"column:dag;type:jsonb" json:"dag"`
	Summary           []byte    `gorm:"column:summary;type:jsonb" json:"summary"`
	CreatedAt         time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// ----------------------------------------------------------
// Group — 用户组表
// ----------------------------------------------------------
type Group struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	GID       string    `gorm:"column:gid;uniqueIndex;size:64" json:"gid"`
	Name      string    `gorm:"column:name;size:128" json:"name"`
	OwnerID   string    `gorm:"column:owner_id;size:64" json:"owner_id"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// ----------------------------------------------------------
// GroupMember — 组成员关系表（多对多）
// ----------------------------------------------------------
type GroupMember struct {
	GID string `gorm:"column:gid;primaryKey;size:64" json:"gid"`
	UID string `gorm:"column:uid;primaryKey;size:64" json:"uid"`
}

// ----------------------------------------------------------
// AnalysisSuggestion — 分析建议表
// ----------------------------------------------------------
type AnalysisSuggestion struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	TID          string `gorm:"column:tid;size:64;index" json:"tid"`
	Func         string `gorm:"column:func;size:512" json:"func"`
	Suggestion   string `gorm:"column:suggestion;type:text" json:"suggestion"`
	AISuggestion string `gorm:"column:ai_suggestion;type:text" json:"ai_suggestion"`
	Status       int    `gorm:"column:status;default:0" json:"status"`
}

// ----------------------------------------------------------
// ScheduleTask — 定时任务表（W5）
// ----------------------------------------------------------
type ScheduleTask struct {
	ID            uint           `gorm:"primaryKey" json:"id"`
	SID           string         `gorm:"column:sid;uniqueIndex;size:64" json:"sid"`
	Name          string         `gorm:"column:name;size:256" json:"name"`
	CronExpr      string         `gorm:"column:cron_expr;size:128" json:"cron_expr"` // cron 表达式
	TaskKind      string         `gorm:"column:task_kind;size:64" json:"task_kind"`
	TaskType      uint32         `gorm:"column:task_type;default:0" json:"task_type"`
	ProfilerType  uint32         `gorm:"column:profiler_type;default:0" json:"profiler_type"`
	TargetIP      string         `gorm:"column:target_ip;size:45" json:"target_ip"`
	RequestParams []byte         `gorm:"column:request_params;type:jsonb" json:"request_params"`
	Enabled       bool           `gorm:"column:enabled;default:true" json:"enabled"`
	LastRunAt     *time.Time     `gorm:"column:last_run_at" json:"last_run_at"`
	NextRunAt     *time.Time     `gorm:"column:next_run_at" json:"next_run_at"`
	UID           string         `gorm:"column:uid;size:64" json:"uid"`
	UserName      string         `gorm:"column:user_name;size:128" json:"user_name"`
	CreatedAt     time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt     time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt     gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
	CanManage     bool           `gorm:"-" json:"can_manage"`
}

// ScheduleTrigger — 定时任务触发表。
// 同一个 schedule 在同一个 scheduled_at 只允许触发一次，解决多 API 实例或
// cron 重入造成的重复子任务问题。
type ScheduleTrigger struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	ScheduleID  string    `gorm:"column:schedule_id;size:64;uniqueIndex:idx_schedule_trigger_once,priority:1" json:"schedule_id"`
	ScheduledAt time.Time `gorm:"column:scheduled_at;uniqueIndex:idx_schedule_trigger_once,priority:2" json:"scheduled_at"`
	ChildTID    string    `gorm:"column:child_tid;size:64;index" json:"child_tid"`
	Status      string    `gorm:"column:status;size:32;default:claimed" json:"status"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// ----------------------------------------------------------
// AutoMigrate：一键创建/更新所有表
// 调用这个方法，GORM 会检查每张表是否存在：
//
//	不存在 → 创建
//	存在但字段不同 → 自动加列
//	不会删除已有的列（安全）
//
// ----------------------------------------------------------
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&UserInfo{},
		&AgentInfo{},
		&AgentAuditLog{},
		&HotmethodTask{},
		&TaskStatusEvent{},
		&MultiTask{},
		&Group{},
		&GroupMember{},
		&AnalysisSuggestion{},
		&ScheduleTask{},
		&ScheduleTrigger{},
		&TaskAttempt{},
		&Artifact{},
		&AnalysisJob{},
		&Outbox{},
		&SchemaMigration{},
		&ContinuousSession{},
		&ContinuousProcessSnapshot{},
		&ContinuousAgentState{},
		&ProfileBatch{},
		&ProfileWindow{},
		&ContinuousWindowSummary{},
		&ContinuousProfileBlock{},
		&SymbolFile{},
		&KernelSymbolFile{},
		&TaskBuildID{},
		&StorageBlob{},
		&StorageObjectGC{},
		&StorageMigrationFailure{},
	)
}
