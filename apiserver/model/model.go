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
	ID     uint   `gorm:"primaryKey"`              // 自增主键
	UID    string `gorm:"uniqueIndex;size:64"`     // 用户唯一 ID（类似工号）
	Name   string `gorm:"size:128"`                // 用户名
	Groups []byte `gorm:"type:jsonb"`              // 所属用户组（JSON 数组）
	Key    string `gorm:"size:256"`                // 鉴权密钥
}

// ----------------------------------------------------------
// AgentInfo — Agent 表（记录所有接入的 Agent）
// ----------------------------------------------------------
type AgentInfo struct {
	ID          uint      `gorm:"primaryKey"`
	Hostname    string    `gorm:"size:256"`        // 主机名
	IPAddr      string    `gorm:"size:45;index"`   // IP 地址（有索引，方便查询）
	Online      bool      `gorm:"default:false"`   // 是否在线
	UID         string    `gorm:"size:64"`         // Agent 唯一 ID
	GID         string    `gorm:"size:64"`         // 所属组 ID
	Version     string    `gorm:"size:32"`         // Agent 版本
	Environment string    `gorm:"size:64"`         // 环境（生产/测试）
	LastSeen    time.Time                         // 最后一次心跳时间
	CreatedAt   time.Time                         // 创建时间（GORM 自动维护）
	UpdatedAt   time.Time                         // 更新时间（GORM 自动维护）
}

// ----------------------------------------------------------
// HotmethodTask — 任务表（最核心的表！）
// ----------------------------------------------------------
type HotmethodTask struct {
	ID             uint           `gorm:"primaryKey"`
	TID            string         `gorm:"uniqueIndex;size:64"`  // 任务唯一 ID
	Name           string         `gorm:"size:256"`             // 任务名称
	Type           uint32         `gorm:"default:0"`            // 任务类型
	ProfilerType   uint32         `gorm:"default:0"`            // 采集器类型
	TargetIP       string         `gorm:"size:45"`              // 目标机器 IP
	RequestParams  []byte         `gorm:"type:jsonb"`           // 请求参数（JSON）
	Status         int            `gorm:"default:0;index"`      // 状态：0=待处理 1=执行中 2=成功 3=失败
	StatusInfo     string         `gorm:"size:1024"`            // 状态说明（如失败原因）
	AnalysisStatus int            `gorm:"default:0"`            // 分析状态：0=待分析 1=分析中 2=成功 3=失败
	UID            string         `gorm:"size:64"`              // 创建者用户 ID
	UserName       string         `gorm:"size:128"`             // 创建者用户名
	CreateTime     time.Time                                    // 创建时间
	BeginTime      *time.Time                                   // 开始执行时间（指针，可以为空）
	EndTime        *time.Time                                   // 结束时间
	MasterTaskTID  string         `gorm:"size:64"`              // 父任务 ID（如果是子任务）
	DeletedAt      gorm.DeletedAt `gorm:"index"`                // 软删除时间（不为空=已删除）
}

// ----------------------------------------------------------
// MultiTask — 组合任务表（一个任务包含多个子任务）
// ----------------------------------------------------------
type MultiTask struct {
	ID             uint   `gorm:"primaryKey"`
	TID            string `gorm:"uniqueIndex;size:64"`
	SubTIDs        []byte `gorm:"type:jsonb"`  // 子任务 ID 列表（JSON 数组）
	Type           uint32
	Status         int
	AnalysisStatus int
	TriggerType    uint32  // 触发方式：手动/定时/自动
}

// ----------------------------------------------------------
// Group — 用户组表
// ----------------------------------------------------------
type Group struct {
	ID      uint   `gorm:"primaryKey"`
	GID     string `gorm:"uniqueIndex;size:64"`  // 组唯一 ID
	Name    string `gorm:"size:128"`              // 组名
	OwnerID string `gorm:"size:64"`               // 组长 ID
}

// ----------------------------------------------------------
// GroupMember — 组成员关系表（多对多）
// ----------------------------------------------------------
type GroupMember struct {
	GID string `gorm:"primaryKey;size:64"`  // 组 ID（复合主键）
	UID string `gorm:"primaryKey;size:64"`  // 用户 ID（复合主键）
}

// ----------------------------------------------------------
// AnalysisSuggestion — 分析建议表
// ----------------------------------------------------------
type AnalysisSuggestion struct {
	ID           uint   `gorm:"primaryKey"`
	TID          string `gorm:"size:64;index"`   // 关联的任务 ID
	Func         string `gorm:"size:512"`        // 热点函数名
	Suggestion   string `gorm:"type:text"`       // 规则建议（中文）
	AISuggestion string `gorm:"type:text"`       // AI 建议
	Status       int    `gorm:"default:0"`       // 状态
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
	)
}
