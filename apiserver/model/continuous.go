package model

import (
	"time"

	"gorm.io/gorm"
)

const (
	ContinuousSessionStatusRunning = "running"
	ContinuousSessionStatusStopped = "stopped"
	ContinuousSessionStatusLegacy  = "legacy"
)

const (
	ContinuousDesiredStateRunning = "running"
	ContinuousDesiredStateStopped = "stopped"
)

const (
	ContinuousObservedStatePending  = "pending"
	ContinuousObservedStateRunning  = "running"
	ContinuousObservedStateWaiting  = "waiting"
	ContinuousObservedStateDegraded = "degraded"
	ContinuousObservedStateStopping = "stopping"
	ContinuousObservedStateStopped  = "stopped"
	ContinuousObservedStateError    = "error"
	ContinuousObservedStateOffline  = "offline"
)

const (
	ContinuousBatchStatusReady  = "ready"
	ContinuousBatchStatusFailed = "failed"
)

const (
	ContinuousBlockStatusActive     = "active"
	ContinuousBlockStatusSuperseded = "superseded"
	ContinuousBlockStatusDeleting   = "deleting"
)

type ContinuousSession struct {
	ID                   uint   `gorm:"primaryKey" json:"id"`
	SID                  string `gorm:"column:sid;uniqueIndex;size:64" json:"sid"`
	Name                 string `gorm:"column:name;size:256" json:"name"`
	TargetIP             string `gorm:"column:target_ip;size:45;index" json:"target_ip"`
	Hostname             string `gorm:"column:hostname;size:256" json:"hostname"`
	ServiceName          string `gorm:"column:service_name;size:128;default:hotmethod" json:"service_name"`
	SampleRateHz         uint32 `gorm:"column:sample_rate_hz;default:19" json:"sample_rate_hz"`
	AggregationWindowSec uint32 `gorm:"column:aggregation_window_sec;default:10" json:"aggregation_window_sec"`
	UploadBatchSec       uint32 `gorm:"column:upload_batch_sec;default:60" json:"upload_batch_sec"`
	RetentionHours       uint32 `gorm:"column:retention_hours;default:24" json:"retention_hours"`
	Labels               []byte `gorm:"column:labels;type:jsonb" json:"labels"`
	Capabilities         []byte `gorm:"column:capabilities;type:jsonb" json:"capabilities"`
	Status               string `gorm:"column:status;size:32;default:running;index" json:"status"`
	Scope                string `gorm:"column:scope;size:16;default:host;index" json:"scope"`
	SelectorExe          string `gorm:"column:selector_exe;size:4096;index" json:"selector_exe"`
	SelectorMode         string `gorm:"column:selector_mode;size:32;default:all_instances" json:"selector_mode"`
	Signals              []byte `gorm:"column:signals;type:jsonb" json:"signals"`
	// RequestedSignals 是 signals 的显式字符串数组镜像（阶段一）：Reconcile
	// 下发 assignment 时按字符串数组返回，避免 GORM 对 jsonb 的 []byte 自动
	// 编码导致 Agent 侧解析歧义。写入时由服务端在创建/更新 Session 时维护。
	RequestedSignals     []byte         `gorm:"column:requested_signals;type:jsonb" json:"requested_signals"`
	DesiredState         string         `gorm:"column:desired_state;size:16;default:running;index" json:"desired_state"`
	ObservedState        string         `gorm:"column:observed_state;size:16;default:pending;index" json:"observed_state"`
	ObservedAt           *time.Time     `gorm:"column:observed_at;index" json:"observed_at"`
	ActiveProcesses      []byte         `gorm:"column:active_processes;type:jsonb" json:"active_processes"`
	ContinuityMode       string         `gorm:"column:continuity_mode;size:16;default:degraded" json:"continuity_mode"`
	AllowDegraded        bool           `gorm:"column:allow_degraded;default:false" json:"allow_degraded"`
	DegradationReason    string         `gorm:"column:degradation_reason;type:text" json:"degradation_reason"`
	LastError            string         `gorm:"column:last_error;type:text" json:"last_error"`
	StopRequestedAt      *time.Time     `gorm:"column:stop_requested_at" json:"stop_requested_at"`
	Revision             uint64         `gorm:"column:revision;default:1" json:"revision"`
	AgentID              string         `gorm:"column:agent_id;size:128;index" json:"agent_id"`
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
	CanManage            bool           `gorm:"-" json:"can_manage"`
	SampleCount          uint64         `gorm:"-" json:"sample_count"`
}

// ContinuousProcessSnapshot stores only the latest process inventory reported by
// an Agent. Command-line arguments are deliberately excluded.
type ContinuousProcessSnapshot struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	TargetIP       string    `gorm:"column:target_ip;size:45;uniqueIndex:idx_continuous_process_identity,priority:1" json:"target_ip"`
	AgentID        string    `gorm:"column:agent_id;size:128;index" json:"agent_id"`
	PID            int       `gorm:"column:pid;uniqueIndex:idx_continuous_process_identity,priority:2" json:"pid"`
	ProcessStartMs int64     `gorm:"column:process_start_ms;uniqueIndex:idx_continuous_process_identity,priority:3" json:"process_start_ms"`
	Comm           string    `gorm:"column:comm;size:256" json:"comm"`
	Exe            string    `gorm:"column:exe;size:4096;index" json:"exe"`
	RSSBytes       uint64    `gorm:"column:rss_bytes" json:"rss_bytes"`
	ObservedAt     time.Time `gorm:"column:observed_at;index" json:"observed_at"`
}

type ContinuousAgentState struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	TargetIP      string    `gorm:"column:target_ip;size:45;uniqueIndex" json:"target_ip"`
	AgentID       string    `gorm:"column:agent_id;size:128;index" json:"agent_id"`
	StrictCapable bool      `gorm:"column:strict_capable;default:false" json:"strict_capable"`
	Capabilities  []byte    `gorm:"column:capabilities;type:jsonb" json:"capabilities"`
	Revision      uint64    `gorm:"column:revision;default:0" json:"revision"`
	ObservedAt    time.Time `gorm:"column:observed_at;index" json:"observed_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at" json:"updated_at"`
}

type ProfileBatch struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	BID           string    `gorm:"column:bid;uniqueIndex;size:64" json:"bid"`
	SessionSID    string    `gorm:"column:session_sid;size:64;index" json:"session_sid"`
	TargetIP      string    `gorm:"column:target_ip;size:45;index" json:"target_ip"`
	ObjectKey     string    `gorm:"column:object_key;size:512" json:"object_key"`
	StartTime     time.Time `gorm:"column:start_time;index" json:"start_time"`
	EndTime       time.Time `gorm:"column:end_time;index" json:"end_time"`
	WindowCount   uint32    `gorm:"column:window_count" json:"window_count"`
	SampleCount   uint64    `gorm:"column:sample_count" json:"sample_count"`
	SchemaVersion uint32    `gorm:"column:schema_version;default:1" json:"schema_version"`
	// 阶段一（协议 v3）新字段：collector generation 唯一标识物理采集器实例，
	// batch_sequence 单调递增，content_sha256 为 batch 内容摘要（幂等校验），
	// signal_counts 分信号计数。v3 起 batch 层 sample_count 废弃并写 0，所有
	// 计数改读 signal_counts。
	CollectorGeneration string    `gorm:"column:collector_generation;size:64;default:''" json:"collector_generation"`
	BatchSequence       uint64    `gorm:"column:batch_sequence;default:0" json:"batch_sequence"`
	ContentSHA256       string    `gorm:"column:content_sha256;size:64;default:''" json:"content_sha256"`
	SignalCounts        []byte    `gorm:"column:signal_counts;type:jsonb" json:"signal_counts"`
	SignalTypes         []byte    `gorm:"column:signal_types;type:jsonb" json:"signal_types"`
	Backends            []byte    `gorm:"column:backends;type:jsonb" json:"backends"`
	Status              string    `gorm:"column:status;size:32;default:ready" json:"status"`
	ProfileFormat       string    `gorm:"column:profile_format;size:32;default:json" json:"profile_format"`
	BackendStatus       string    `gorm:"column:backend_status;size:32;default:ok" json:"backend_status"`
	BackendReason       string    `gorm:"column:backend_reason" json:"backend_reason"`
	AttemptedBackends   []byte    `gorm:"column:attempted_backends;type:jsonb" json:"attempted_backends"`
	SelectedBackend     string    `gorm:"column:selected_backend;size:64" json:"selected_backend"`
	SymbolRefs          []byte    `gorm:"column:symbol_refs;type:jsonb" json:"symbol_refs"`
	ReceivedAt          time.Time `gorm:"column:received_at" json:"received_at"`
	AgentClockOffsetMs  int64     `gorm:"column:agent_clock_offset_ms;default:0" json:"agent_clock_offset_ms"`
	// BlockID 指向所属 continuous_profile_blocks 行；为空表示该 batch 尚未被
	// 压缩进小时块（热数据，仍读取原分钟对象）。
	BlockID string `gorm:"column:block_id;size:64;index" json:"block_id"`
	// SourceObjectKey 保存 compaction 前的原始分钟对象 key，compaction 成功
	// 后 object_key 改写为块 key；源对象删除失败时保留该列供 sweep 重试。
	SourceObjectKey string `gorm:"column:source_object_key;size:512" json:"source_object_key"`
	// CompactedAt 记录首次被压缩进块的时间（nil 表示尚未压缩）。
	CompactedAt *time.Time `gorm:"column:compacted_at" json:"compacted_at"`
	// PayloadBytes 记录原始 payload JSON 字节数，供 compactor 磁盘余量检查
	// 估算"本次输入大小"。
	PayloadBytes uint64    `gorm:"column:payload_bytes" json:"payload_bytes"`
	CreatedAt    time.Time `gorm:"column:created_at" json:"created_at"`
}

// ContinuousProfileBlock 是阶段三新增的块存储元数据行：每个 session 每小时
// 一个 gzip 压缩块对象（continuous-blocks/{session}/{YYYY}/{MM}/{DD}/{HH}/{block-id}.json.gz），
// 把同一 UTC 小时桶内的多个分钟 batch 合并存放。块内保留每个原始 batch 的
// 完整 payload（CPU profile / io-sched 延迟直方图 / db_snapshot 原结构），
// 查询侧按 object_key + batch_bid 去重加载，一个块只解压一次。
//
// 设计借鉴 Parca/Pyroscope 的块-压缩-索引-替换思路，但全部自研实现，
// 不部署、不依赖、不调用任何开源 profiling 后端。status 区分 active
// （当前可查）、superseded（被新版本块替换，对象在 15 分钟宽限后删除）
// 与 deleting（已解除查询引用，对象删除失败时由 sweep 重试）。
type ContinuousProfileBlock struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	BlockID       string     `gorm:"column:block_id;uniqueIndex;size:64" json:"block_id"`
	SessionSID    string     `gorm:"column:session_sid;size:64;index" json:"session_sid"`
	BucketStart   time.Time  `gorm:"column:bucket_start;index" json:"bucket_start"`
	BucketEnd     time.Time  `gorm:"column:bucket_end" json:"bucket_end"`
	ObjectKey     string     `gorm:"column:object_key;size:512" json:"object_key"`
	Compression   string     `gorm:"column:compression;size:16;default:gzip" json:"compression"`
	SchemaVersion uint32     `gorm:"column:schema_version;default:1" json:"schema_version"`
	Version       int        `gorm:"column:version;default:1" json:"version"`
	Status        string     `gorm:"column:status;size:16;default:active;index" json:"status"`
	BatchCount    int        `gorm:"column:batch_count" json:"batch_count"`
	SampleCount   uint64     `gorm:"column:sample_count" json:"sample_count"`
	BytesBefore   int64      `gorm:"column:bytes_before" json:"bytes_before"`
	BytesAfter    int64      `gorm:"column:bytes_after" json:"bytes_after"`
	SupersededAt  *time.Time `gorm:"column:superseded_at" json:"superseded_at"`
	ReplacedBy    string     `gorm:"column:replaced_by;size:64" json:"replaced_by"`
	CreatedAt     time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at" json:"updated_at"`
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
	ID            uint      `gorm:"primaryKey" json:"id"`
	SessionSID    string    `gorm:"column:session_sid;size:64;index:idx_profile_windows_session_time,priority:1" json:"session_sid"`
	BatchBID      string    `gorm:"column:batch_bid;size:64;index" json:"batch_bid"`
	WindowStart   time.Time `gorm:"column:window_start;index:idx_profile_windows_session_time,priority:2" json:"window_start"`
	WindowEnd     time.Time `gorm:"column:window_end;index" json:"window_end"`
	ObjectKey     string    `gorm:"column:object_key;size:512" json:"object_key"`
	SampleCount   uint64    `gorm:"column:sample_count" json:"sample_count"`
	SignalType    string    `gorm:"column:signal_type;size:64;default:cpu_profile;index" json:"signal_type"`
	SchemaVersion uint32    `gorm:"column:schema_version;default:1" json:"schema_version"`
	// 阶段一（协议 v3）新字段：window_id 为逻辑窗口稳定 ID（唯一约束
	// session_sid+window_id+signal_type 的幂等键），collector_generation /
	// target_fingerprint 标识产生窗口的采集器与目标集，content_sha256 窗口
	// 内容摘要（冲突检测），signal_counts 分信号计数。
	WindowID            string    `gorm:"column:window_id;size:128;default:''" json:"window_id"`
	CollectorGeneration string    `gorm:"column:collector_generation;size:64;default:''" json:"collector_generation"`
	TargetFingerprint   string    `gorm:"column:target_fingerprint;size:256;default:''" json:"target_fingerprint"`
	ContentSHA256       string    `gorm:"column:content_sha256;size:64;default:''" json:"content_sha256"`
	SignalCounts        []byte    `gorm:"column:signal_counts;type:jsonb" json:"signal_counts"`
	Backend             string    `gorm:"column:backend;size:64" json:"backend"`
	Labels              []byte    `gorm:"column:labels;type:jsonb" json:"labels"`
	ProfileFormat       string    `gorm:"column:profile_format;size:32;default:json" json:"profile_format"`
	BackendStatus       string    `gorm:"column:backend_status;size:32;default:ok" json:"backend_status"`
	BackendReason       string    `gorm:"column:backend_reason" json:"backend_reason"`
	AttemptedBackends   []byte    `gorm:"column:attempted_backends;type:jsonb" json:"attempted_backends"`
	SelectedBackend     string    `gorm:"column:selected_backend;size:64" json:"selected_backend"`
	SymbolRefs          []byte    `gorm:"column:symbol_refs;type:jsonb" json:"symbol_refs"`
	// 阶段三（协议 v4）：每信号采集状态（collected/target_idle/no_events/
	// unavailable/failed/unknown）、reason、lost events、物理/生效采样率、
	// 身份不完整被丢弃的样本数。旧行默认为 unknown/0。
	SignalStatus          string `gorm:"column:signal_status;size:16;default:unknown" json:"signal_status"`
	SignalStatusReason    string `gorm:"column:signal_status_reason;size:512;default:''" json:"signal_status_reason"`
	SignalLostEvents      uint64 `gorm:"column:signal_lost_events;default:0" json:"signal_lost_events"`
	PhysicalSampleRateHz  int    `gorm:"column:physical_sample_rate_hz;default:0" json:"physical_sample_rate_hz"`
	EffectiveSampleRateHz int    `gorm:"column:effective_sample_rate_hz;default:0" json:"effective_sample_rate_hz"`
	IdentityUnavailable   uint64 `gorm:"column:identity_unavailable_count;default:0" json:"identity_unavailable_count"`
	CreatedAt             time.Time `gorm:"column:created_at" json:"created_at"`
}

// ContinuousRepairAudit 阶段一历史重复修复审计表：每次 repair run 把"同一逻辑
// 窗口被排除的 batch"记成一条只读审计记录（保留/排除双方 batch、摘要、原因与
// 时间），原始对象暂不立即删除，保证修复可审计、可回放、可对账。逻辑窗口键
// logical_window_key 由 session+signal+起止时间规范化生成；window_id 为新协议
// 稳定 ID（历史 v2 行为空）。
type ContinuousRepairAudit struct {
	ID                    uint      `gorm:"primaryKey" json:"id"`
	RunID                 string    `gorm:"column:run_id;size:64;index" json:"run_id"`
	Tenant                string    `gorm:"column:tenant;size:64;default:default" json:"tenant"`
	SessionSID            string    `gorm:"column:session_sid;size:64;index" json:"session_sid"`
	LogicalWindowKey      string    `gorm:"column:logical_window_key;size:512" json:"logical_window_key"`
	WindowID              string    `gorm:"column:window_id;size:128;default:''" json:"window_id"`
	SignalType            string    `gorm:"column:signal_type;size:32" json:"signal_type"`
	WindowStart           time.Time `gorm:"column:window_start" json:"window_start"`
	WindowEnd             time.Time `gorm:"column:window_end" json:"window_end"`
	KeptBatchBID          string    `gorm:"column:kept_batch_bid;size:64" json:"kept_batch_bid"`
	ExcludedBatchBID      string    `gorm:"column:excluded_batch_bid;size:64" json:"excluded_batch_bid"`
	KeptContentSHA256     string    `gorm:"column:kept_content_sha256;size:64" json:"kept_content_sha256"`
	ExcludedContentSHA256 string    `gorm:"column:excluded_content_sha256;size:64" json:"excluded_content_sha256"`
	Reason                string    `gorm:"column:reason;size:128" json:"reason"`
	CreatedAt             time.Time `gorm:"column:created_at" json:"created_at"`
}
