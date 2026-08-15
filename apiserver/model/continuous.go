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
	UID                  string         `gorm:"column:uid;size:64;index" json:"uid"`
	UserName             string         `gorm:"column:user_name;size:128" json:"user_name"`
	StartedAt            time.Time      `gorm:"column:started_at" json:"started_at"`
	StoppedAt            *time.Time     `gorm:"column:stopped_at" json:"stopped_at"`
	CreatedAt            time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt            time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt            gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

type ProfileBatch struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	BID         string    `gorm:"column:bid;uniqueIndex;size:64" json:"bid"`
	SessionSID  string    `gorm:"column:session_sid;size:64;index" json:"session_sid"`
	TargetIP    string    `gorm:"column:target_ip;size:45;index" json:"target_ip"`
	ObjectKey   string    `gorm:"column:object_key;size:512" json:"object_key"`
	StartTime   time.Time `gorm:"column:start_time;index" json:"start_time"`
	EndTime     time.Time `gorm:"column:end_time;index" json:"end_time"`
	WindowCount uint32    `gorm:"column:window_count" json:"window_count"`
	SampleCount uint64    `gorm:"column:sample_count" json:"sample_count"`
	Status      string    `gorm:"column:status;size:32;default:ready" json:"status"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
}

type ProfileWindow struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	SessionSID  string    `gorm:"column:session_sid;size:64;index:idx_profile_windows_session_time,priority:1" json:"session_sid"`
	BatchBID    string    `gorm:"column:batch_bid;size:64;index" json:"batch_bid"`
	WindowStart time.Time `gorm:"column:window_start;index:idx_profile_windows_session_time,priority:2" json:"window_start"`
	WindowEnd   time.Time `gorm:"column:window_end;index" json:"window_end"`
	ObjectKey   string    `gorm:"column:object_key;size:512" json:"object_key"`
	SampleCount uint64    `gorm:"column:sample_count" json:"sample_count"`
	Labels      []byte    `gorm:"column:labels;type:jsonb" json:"labels"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
}
