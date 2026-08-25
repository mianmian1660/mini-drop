// ============================================================
// server/parquet_schema.go — 阶段五：Parquet v2 四类信号 schema
// ============================================================
// 写入参数固定（见 parquet_writer.go）：
//   - ZSTD 压缩
//   - 低基数字符串与 frame 字符串使用 Parquet dictionary encoding
//   - row group 目标 16 MiB
//   - 单文件目标 ≤128 MiB，超出拆 part
//   - 按 timestamp、session_id、process identity 排序
//   - 流式写入 MinIO，同步计算字节数与 SHA-256，不在宿主机生成大临时文件
//
// 四类信号：
//   - cpu：时间/session/service/agent、进程身份、backend、labels、
//     完整 frames、value、unit、profile type（一行 = 一个样本 × count）
//   - metrics：series 身份、metric、metric_kind（gauge/counter）、
//     value/unit；gauge 保存 min/max/sum/count/last，counter 保存
//     reset-aware delta
//   - histogram：signal/backend/unit、bucket low/high/count、
//     event_count、unavailable/reason；粗粒度层合并相同边界后重算分位数
//   - db：digest 按 instance/schema/digest 聚合 call/latency/rows；
//     lock wait 按 instance/table/waiting/blocking identity 聚合，
//     保存 occurrence count、最大等待时间和对应代表查询
// ============================================================

package server

import "sync/atomic"

// pqAvgRowBytesEstimate 平均行字节估算（写入前由 builder 设置，查询无关）。
var pqAvgRowBytesEstimate atomic.Int64

// pqRowGroupBoundary 记录单个 row group 的行范围与时间范围（查询选组用）。
type pqRowGroupBoundary struct {
	RowIndex int64 `json:"row_index"`
	MinTS    int64 `json:"min_ts"`
	MaxTS    int64 `json:"max_ts"`
}

// pqCPUFrame 结构化栈帧（与 ContinuousStackFrame 同构，含 parquet tag）。
type pqCPUFrame struct {
	Function         string `parquet:"function,dict"`
	Raw              string `parquet:"raw,dict"`
	File             string `parquet:"file,dict"`
	Line             int32  `parquet:"line"`
	Address          uint64 `parquet:"address"`
	MappingFile      string `parquet:"mapping_file,dict"`
	BuildID          string `parquet:"build_id,dict"`
	NormalizedOffset uint64 `parquet:"normalized_offset"`
	Resolved         bool   `parquet:"resolved"`
}

// pqCPURow 一行 = 一个 CPU/profile 样本（value = 样本计数）。
// 按 (timestamp, session_sid, pid, process_start_ms) 排序。
type pqCPURow struct {
	Timestamp      int64             `parquet:"timestamp"`
	SessionSID     string            `parquet:"session_sid,dict"`
	Service        string            `parquet:"service,dict"`
	Agent          string            `parquet:"agent,dict"`
	PID            int32             `parquet:"pid"`
	ProcessStartMs int64             `parquet:"process_start_ms"`
	Comm           string            `parquet:"comm,dict"`
	Exe            string            `parquet:"exe,dict"`
	Backend        string            `parquet:"backend,dict"`
	Runtime        string            `parquet:"runtime,dict"`
	Labels         map[string]string `parquet:"labels"`
	Frames         []pqCPUFrame      `parquet:"frames,list"`
	Value          uint64            `parquet:"value"`
	Unit           string            `parquet:"unit,dict"`
	ProfileType    string            `parquet:"profile_type,dict"`
	// ProfileID 阶段七：样本所属 profile 的幂等 ID（memray 等显式 profile）。
	// 查询侧按 (profile_id + pid + process_start_ms + exe) 跨窗口去重，
	// 与 v1 的 SeenProfileIDs 语义一致；旧 Parquet 文件无此列（空串），
	// 查询按无 profile 处理（不参与去重）。
	ProfileID     string `parquet:"profile_id,dict"`
	ProfileStatus string `parquet:"profile_status,dict"`
	ProfileReason string `parquet:"profile_reason,dict"`
}

func (r pqCPURow) pqSortKey() pqSortKey {
	return pqSortKey{Timestamp: r.Timestamp, SessionSID: r.SessionSID, PID: r.PID, ProcessStartMs: r.ProcessStartMs}
}

// pqMetricRow 一行 = 一个 (series, 采样点)。
// gauge：value/min/max/sum/count/last 填充；counter：value=累计值，
// delta=reset-aware 增量（回绕/重启后只计正向增量）。
type pqMetricRow struct {
	Timestamp      int64             `parquet:"timestamp"`
	SessionSID     string            `parquet:"session_sid,dict"`
	PID            int32             `parquet:"pid"`
	ProcessStartMs int64             `parquet:"process_start_ms"`
	Comm           string            `parquet:"comm,dict"`
	Exe            string            `parquet:"exe,dict"`
	Runtime        string            `parquet:"runtime,dict"`
	Metric         string            `parquet:"metric,dict"`
	MetricKind     string            `parquet:"metric_kind,dict"`
	Value          uint64            `parquet:"value"`
	Min            uint64            `parquet:"min"`
	Max            uint64            `parquet:"max"`
	Sum            uint64            `parquet:"sum"`
	Count          uint64            `parquet:"count"`
	Last           uint64            `parquet:"last"`
	Delta          uint64            `parquet:"delta"`
	Unit           string            `parquet:"unit,dict"`
	Labels         map[string]string `parquet:"labels"`
	RSSTruncated   int32             `parquet:"rss_truncated"`
}

func (r pqMetricRow) pqSortKey() pqSortKey {
	return pqSortKey{Timestamp: r.Timestamp, SessionSID: r.SessionSID, PID: r.PID, ProcessStartMs: r.ProcessStartMs}
}

// pqHistogramRow 一行 = 一个 histogram bucket。EventCount 只写在同一
// histogram 的第一条 bucket row，避免查询聚合时按 bucket 数量倍增。
// 粗粒度层合并相同 (low,high) 边界后重算 P50/P95/P99。
// 阶段三：增加完整进程身份（pid/process_start_ms/exe/comm），strict CO-RE
// 直方图可按实例过滤；旧 Parquet 文件无这些字段（零值），兼容 reader 只
// 允许 host 无过滤查询。
type pqHistogramRow struct {
	Timestamp      int64             `parquet:"timestamp"`
	SessionSID     string            `parquet:"session_sid,dict"`
	SignalType     string            `parquet:"signal_type,dict"`
	Backend        string            `parquet:"backend,dict"`
	Unit           string            `parquet:"unit,dict"`
	PID            int32             `parquet:"pid"`
	ProcessStartMs int64             `parquet:"process_start_ms"`
	Exe            string            `parquet:"exe,dict"`
	Comm           string            `parquet:"comm,dict"`
	BucketLow      float64           `parquet:"bucket_low"`
	BucketHigh     float64           `parquet:"bucket_high"`
	Count          uint64            `parquet:"count"`
	EventCount     uint64            `parquet:"event_count"`
	Min            float64           `parquet:"min"`
	Max            float64           `parquet:"max"`
	P50            float64           `parquet:"p50"`
	P95            float64           `parquet:"p95"`
	P99            float64           `parquet:"p99"`
	Unavailable    bool              `parquet:"unavailable"`
	Reason         string            `parquet:"reason,dict"`
	Labels         map[string]string `parquet:"labels"`
}

func (r pqHistogramRow) pqSortKey() pqSortKey {
	return pqSortKey{Timestamp: r.Timestamp, SessionSID: r.SessionSID, PID: r.PID, ProcessStartMs: r.ProcessStartMs}
}

// pqDBRow 一行 = 一个 db digest 聚合增量 或 lock wait 事件聚合。
// Kind: "digest" | "lock_wait"。
type pqDBRow struct {
	Timestamp             int64             `parquet:"timestamp"`
	SessionSID            string            `parquet:"session_sid,dict"`
	Kind                  string            `parquet:"kind,dict"`
	Instance              string            `parquet:"instance,dict"`
	SchemaName            string            `parquet:"schema_name,dict"`
	DigestText            string            `parquet:"digest_text,dict"`
	CallCount             uint64            `parquet:"call_count"`
	TotalLatencyUs        uint64            `parquet:"total_latency_us"`
	RowsExaminedTotal     uint64            `parquet:"rows_examined_total"`
	WaitingPID            int64             `parquet:"waiting_pid"`
	WaitingQuery          string            `parquet:"waiting_query"`
	BlockingPID           int64             `parquet:"blocking_pid"`
	BlockingQuery         string            `parquet:"blocking_query"`
	WaitSeconds           uint64            `parquet:"wait_seconds"`
	LockedTable           string            `parquet:"locked_table,dict"`
	OccurrenceCount       uint64            `parquet:"occurrence_count"`
	MaxWaitSeconds        uint64            `parquet:"max_wait_seconds"`
	MaxWaitRepresentative string            `parquet:"max_wait_representative"`
	Labels                map[string]string `parquet:"labels"`
}

func (r pqDBRow) pqSortKey() pqSortKey {
	return pqSortKey{Timestamp: r.Timestamp, SessionSID: r.SessionSID}
}

// pqSignalRows 是四类信号行类型的统一集合（写/读分派用）。
type pqSignalRows struct {
	CPU       []pqCPURow
	Metrics   []pqMetricRow
	Histogram []pqHistogramRow
	DB        []pqDBRow
}

// pqMetricKind 已知 metric 类型登记（未登记指标按 gauge 处理并记录告警）。
var knownMetricKinds = map[string]string{
	"rss_bytes":                           "gauge",
	"db_active_connections":               "gauge",
	"db_innodb_buffer_pool_hit_ratio_bps": "gauge",
	"db_questions_total":                  "counter",
}

// pqMetricKindFor 返回指标类型：登记过返回登记值；未登记按 gauge 处理。
func pqMetricKindFor(metric string) (kind string, registered bool) {
	kind, ok := knownMetricKinds[metric]
	if !ok {
		return "gauge", false
	}
	return kind, true
}
