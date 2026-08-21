package model

import (
	"time"

	"gorm.io/gorm"
)

const (
	ContinuousSessionStatusRunning = "running"
	ContinuousSessionStatusStopped = "stopped"
)

const (
	ContinuousBatchStatusReady  = "ready"
	ContinuousBatchStatusFailed = "failed"
)

type ContinuousSession struct {
	ID                   uint           `gorm:"primaryKey" json:"id"`
	SID                  string         `gorm:"column:sid;uniqueIndex;size:64" json:"sid"`
	Name                 string         `gorm:"column:name;size:256" json:"name"`
	TargetIP             string         `gorm:"column:target_ip;size:45;index" json:"target_ip"`
	Hostname             string         `gorm:"column:hostname;size:256" json:"hostname"`
	ServiceName          string         `gorm:"column:service_name;size:128;default:hotmethod" json:"service_name"`
	SampleRateHz         uint32         `gorm:"column:sample_rate_hz;default:19" json:"sample_rate_hz"`
	AggregationWindowSec uint32         `gorm:"column:aggregation_window_sec;default:10" json:"aggregation_window_sec"`
	UploadBatchSec       uint32         `gorm:"column:upload_batch_sec;default:60" json:"upload_batch_sec"`
	RetentionHours       uint32         `gorm:"column:retention_hours;default:24" json:"retention_hours"`
	Labels               []byte         `gorm:"column:labels;type:jsonb" json:"labels"`
	Capabilities         []byte         `gorm:"column:capabilities;type:jsonb" json:"capabilities"`
	Status               string         `gorm:"column:status;size:32;default:running;index" json:"status"`
	LastUploadAt         *time.Time     `gorm:"column:last_upload_at" json:"last_upload_at"`
	AgentClockOffsetMs   int64          `gorm:"column:agent_clock_offset_ms;default:0" json:"agent_clock_offset_ms"`
	AgentClockStatus     string         `gorm:"column:agent_clock_status;size:16;default:unknown" json:"agent_clock_status"`
	AgentClockObservedAt *time.Time     `gorm:"column:agent_clock_observed_at" json:"agent_clock_observed_at"`
	UID                  string         `gorm:"column:uid;size:64;index" json:"uid"`
	UserName             string         `gorm:"column:user_name;size:128" json:"user_name"`
	StartedAt            time.Time      `gorm:"column:started_at" json:"started_at"`
	StoppedAt            *time.Time     `gorm:"column:stopped_at" json:"stopped_at"`
	CreatedAt            time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt            time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt            gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

type ProfileBatch struct {
	ID                 uint      `gorm:"primaryKey" json:"id"`
	BID                string    `gorm:"column:bid;uniqueIndex;size:64" json:"bid"`
	SessionSID         string    `gorm:"column:session_sid;size:64;index" json:"session_sid"`
	TargetIP           string    `gorm:"column:target_ip;size:45;index" json:"target_ip"`
	ObjectKey          string    `gorm:"column:object_key;size:512" json:"object_key"`
	StartTime          time.Time `gorm:"column:start_time;index" json:"start_time"`
	EndTime            time.Time `gorm:"column:end_time;index" json:"end_time"`
	WindowCount        uint32    `gorm:"column:window_count" json:"window_count"`
	SampleCount        uint64    `gorm:"column:sample_count" json:"sample_count"`
	SchemaVersion      uint32    `gorm:"column:schema_version;default:1" json:"schema_version"`
	SignalTypes        []byte    `gorm:"column:signal_types;type:jsonb" json:"signal_types"`
	Backends           []byte    `gorm:"column:backends;type:jsonb" json:"backends"`
	Status             string    `gorm:"column:status;size:32;default:ready" json:"status"`
	ProfileFormat      string    `gorm:"column:profile_format;size:32;default:json" json:"profile_format"`
	BackendStatus      string    `gorm:"column:backend_status;size:32;default:ok" json:"backend_status"`
	BackendReason      string    `gorm:"column:backend_reason" json:"backend_reason"`
	AttemptedBackends  []byte    `gorm:"column:attempted_backends;type:jsonb" json:"attempted_backends"`
	SelectedBackend    string    `gorm:"column:selected_backend;size:64" json:"selected_backend"`
	SymbolRefs         []byte    `gorm:"column:symbol_refs;type:jsonb" json:"symbol_refs"`
	ReceivedAt         time.Time `gorm:"column:received_at" json:"received_at"`
	AgentClockOffsetMs int64     `gorm:"column:agent_clock_offset_ms;default:0" json:"agent_clock_offset_ms"`
	CreatedAt          time.Time `gorm:"column:created_at" json:"created_at"`
}

// ContinuousWindowSummary 是原始 ProfileWindow 过期硬删前的冷层降采样摘要：
// 按 session + signal_type + 小时分桶，只保留 TopN 函数的 self time 累加值，
// 不含调用栈/调用树结构——查询范围一旦落进纯冷层区间，火焰图/树形 diff 用
// 不了，只能回答"这段时间 TopN 热点函数是谁"。体积比原始窗口小几个数量级，
// 保留期由 config.Retention.ContinuousSummaryRetentionHours 单独控制（默认
// 7 天，比原始数据默认 24h 长一个数量级）。
type ContinuousWindowSummary struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	SessionSID  string    `gorm:"column:session_sid;size:64;uniqueIndex:idx_continuous_window_summary,priority:1" json:"session_sid"`
	SignalType  string    `gorm:"column:signal_type;size:64;uniqueIndex:idx_continuous_window_summary,priority:2" json:"signal_type"`
	BucketStart time.Time `gorm:"column:bucket_start;uniqueIndex:idx_continuous_window_summary,priority:3" json:"bucket_start"`
	BucketEnd   time.Time `gorm:"column:bucket_end" json:"bucket_end"`
	Unit        string    `gorm:"column:unit;size:32;default:samples" json:"unit"`
	SampleCount uint64    `gorm:"column:sample_count" json:"sample_count"`
	TopSelfJSON []byte    `gorm:"column:top_self_json;type:jsonb" json:"top_self_json"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

type ProfileWindow struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	SessionSID        string    `gorm:"column:session_sid;size:64;index:idx_profile_windows_session_time,priority:1" json:"session_sid"`
	BatchBID          string    `gorm:"column:batch_bid;size:64;index" json:"batch_bid"`
	WindowStart       time.Time `gorm:"column:window_start;index:idx_profile_windows_session_time,priority:2" json:"window_start"`
	WindowEnd         time.Time `gorm:"column:window_end;index" json:"window_end"`
	ObjectKey         string    `gorm:"column:object_key;size:512" json:"object_key"`
	SampleCount       uint64    `gorm:"column:sample_count" json:"sample_count"`
	SignalType        string    `gorm:"column:signal_type;size:64;default:cpu_profile;index" json:"signal_type"`
	SchemaVersion     uint32    `gorm:"column:schema_version;default:1" json:"schema_version"`
	Backend           string    `gorm:"column:backend;size:64" json:"backend"`
	Labels            []byte    `gorm:"column:labels;type:jsonb" json:"labels"`
	ProfileFormat     string    `gorm:"column:profile_format;size:32;default:json" json:"profile_format"`
	BackendStatus     string    `gorm:"column:backend_status;size:32;default:ok" json:"backend_status"`
	BackendReason     string    `gorm:"column:backend_reason" json:"backend_reason"`
	AttemptedBackends []byte    `gorm:"column:attempted_backends;type:jsonb" json:"attempted_backends"`
	SelectedBackend   string    `gorm:"column:selected_backend;size:64" json:"selected_backend"`
	SymbolRefs        []byte    `gorm:"column:symbol_refs;type:jsonb" json:"symbol_refs"`
	CreatedAt         time.Time `gorm:"column:created_at" json:"created_at"`
}
