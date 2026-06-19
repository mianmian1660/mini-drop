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
	"time"  // 时间类型

	"gorm.io/gorm"  // GORM 软删除
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
	ID          uint      `gorm:"primaryKey" json:"id"`
	Hostname    string    `gorm:"column:hostname;size:256" json:"hostname"`
	IPAddr      string    `gorm:"column:ip_addr;size:45;index" json:"ip_addr"`
	Online      bool      `gorm:"column:online;default:false" json:"online"`
	UID         string    `gorm:"column:uid;size:64" json:"uid"`
	GID         string    `gorm:"column:gid;size:64" json:"gid"`
	Version     string    `gorm:"column:version;size:32" json:"version"`
	Environment string    `gorm:"column:environment;size:64" json:"environment"`
	LastSeen    time.Time `gorm:"column:last_seen" json:"last_seen"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// ----------------------------------------------------------
// HotmethodTask — 任务表（最核心的表！）
// ----------------------------------------------------------
type HotmethodTask struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	TID            string         `gorm:"column:tid;uniqueIndex;size:64" json:"tid"`
	Name           string         `gorm:"column:name;size:256" json:"name"`
	Type           uint32         `gorm:"column:type;default:0" json:"type"`
	ProfilerType   uint32         `gorm:"column:profiler_type;default:0" json:"profiler_type"`
	TargetIP       string         `gorm:"column:target_ip;size:45" json:"target_ip"`
	RequestParams  []byte         `gorm:"column:request_params;type:jsonb" json:"request_params"`
	Status         int            `gorm:"column:status;default:0;index" json:"status"`
	StatusInfo     string         `gorm:"column:status_info;size:1024" json:"status_info"`
	AnalysisStatus int            `gorm:"column:analysis_status;default:0" json:"analysis_status"`
	UID            string         `gorm:"column:uid;size:64" json:"uid"`
	UserName       string         `gorm:"column:user_name;size:128" json:"user_name"`
	CreateTime     time.Time      `gorm:"column:create_time" json:"create_time"`
	BeginTime      *time.Time     `gorm:"column:begin_time" json:"begin_time"`
	EndTime        *time.Time     `gorm:"column:end_time" json:"end_time"`
	MasterTaskTID  string         `gorm:"column:master_task_tid;size:64" json:"master_task_tid"`
	DeletedAt      gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// ----------------------------------------------------------
// MultiTask — 组合任务表（一个任务包含多个子任务）
// ----------------------------------------------------------
type MultiTask struct {
	ID             uint   `gorm:"primaryKey" json:"id"`
	TID            string `gorm:"column:tid;uniqueIndex;size:64" json:"tid"`
	SubTIDs        []byte `gorm:"column:sub_tids;type:jsonb" json:"sub_tids"`
	Type           uint32 `gorm:"column:type" json:"type"`
	Status         int    `gorm:"column:status" json:"status"`
	AnalysisStatus int    `gorm:"column:analysis_status" json:"analysis_status"`
	TriggerType    uint32 `gorm:"column:trigger_type" json:"trigger_type"`
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
	ID           uint           `gorm:"primaryKey" json:"id"`
	SID          string         `gorm:"column:sid;uniqueIndex;size:64" json:"sid"`
	Name         string         `gorm:"column:name;size:256" json:"name"`
	CronExpr     string         `gorm:"column:cron_expr;size:128" json:"cron_expr"`       // cron 表达式
	TaskType     uint32         `gorm:"column:task_type;default:0" json:"task_type"`
	ProfilerType uint32         `gorm:"column:profiler_type;default:0" json:"profiler_type"`
	TargetIP     string         `gorm:"column:target_ip;size:45" json:"target_ip"`
	RequestParams []byte        `gorm:"column:request_params;type:jsonb" json:"request_params"`
	Enabled      bool           `gorm:"column:enabled;default:true" json:"enabled"`
	LastRunAt    *time.Time     `gorm:"column:last_run_at" json:"last_run_at"`
	NextRunAt    *time.Time     `gorm:"column:next_run_at" json:"next_run_at"`
	UID          string         `gorm:"column:uid;size:64" json:"uid"`
	UserName     string         `gorm:"column:user_name;size:128" json:"user_name"`
	CreatedAt    time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt    time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt    gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// ----------------------------------------------------------
// AutoMigrate：一键创建/更新所有表
// 调用这个方法，GORM 会检查每张表是否存在：
//   不存在 → 创建
//   存在但字段不同 → 自动加列
//   不会删除已有的列（安全）
// ----------------------------------------------------------
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&UserInfo{},
		&AgentInfo{},
		&HotmethodTask{},
		&MultiTask{},
		&Group{},
		&GroupMember{},
		&AnalysisSuggestion{},
		&ScheduleTask{},
	)
}
