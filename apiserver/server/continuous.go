package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

const continuousMaxDBCount = uint64(1<<63 - 1)
const continuousMaxReasonableProfileSampleCount = uint64(1_000_000_000)

// errContinuousConflict 是窗口/批次内容冲突的哨兵错误：触发整个事务回滚，
// 由 IngestContinuousBatch 映射为 409 不可重试冲突。绝不允许 Agent 通过换
// ID（如旧版 cpb-retry-* rekey）绕过幂等约束。
var errContinuousConflict = errors.New("continuous batch/window content conflict")

// continuousSchemaVersionV3 是阶段一协议版本：v3 起启用窗口级 window_id 幂等、
// content_sha256 冲突检测与分信号 signal_counts；v1/v2 走兼容旧路径。
// continuousSchemaVersionV4 是阶段三协议版本：v4 在 v3 基础上新增窗口级
// signal_statuses（每信号采集状态）、physical/effective_sample_rate_hz、
// identity_unavailable_count，histogram 携带完整进程身份
// （pid/process_start_ms/exe/comm）。v4 结构按 v3 规则解析（字段增量），
// 旧 Agent 的 v3 批次继续按旧规则入库。
const continuousSchemaVersionV3 = uint32(3)
const continuousSchemaVersionV4 = uint32(4)

// continuousSummaryBucketDuration 冷层摘要按 1 小时对齐分桶，和原始数据
// 10s 窗口/60s batch 的粒度差好几个数量级——冷层本来就是拿精度换存储。
const continuousSummaryBucketDuration = time.Hour

// continuousSummaryTopLimit 每个 (session, signal_type, 小时桶) 摘要最多
// 保留的函数条数，防止某个桶函数基数很大时摘要本身也膨胀。
const continuousSummaryTopLimit = 50

// 查询跨度由当前 Session retention_hours 决定；匹配窗口超过上限时返回错误，
// 绝不静默截断。max_nodes 默认 5000，上限 20000。
const continuousMaxWindowCount = 20000
const continuousDefaultMaxNodes = 5000
const continuousMaxNodesCap = 20000

type CreateContinuousSessionReq struct {
	Name                 string                 `json:"name"`
	TargetIP             string                 `json:"target_ip" binding:"required"`
	Hostname             string                 `json:"hostname"`
	ServiceName          string                 `json:"service_name"`
	SampleRateHz         uint32                 `json:"sample_rate_hz"`
	AggregationWindowSec uint32                 `json:"aggregation_window_sec"`
	UploadBatchSec       uint32                 `json:"upload_batch_sec"`
	RetentionHours       uint32                 `json:"retention_hours"`
	Labels               map[string]interface{} `json:"labels"`
	Capabilities         map[string]interface{} `json:"capabilities"`
	Scope                string                 `json:"scope"`
	SelectorExe          string                 `json:"selector_exe"`
	SelectorMode         string                 `json:"selector_mode"`
	// SelectorParams 阶段六：selector 的结构化参数。按 selector_mode 使用：
	//   - pid_instance:      {pid, process_start_ms, exe}
	//   - exe_all_instances: {exe}
	//   - cgroup:            {cgroup}
	//   - container_id:      {container_id}
	// 兼容旧客户端：只传 selector_exe + selector_mode=all_instances 时归一化为
	// exe_all_instances。
	SelectorParams *ContinuousSelectorParams `json:"selector_params"`
	Signals        []string                  `json:"signals"`
	ContinuityMode string                    `json:"continuity_mode"`
	AllowDegraded  bool                      `json:"allow_degraded"`
}

// ContinuousSelectorParams 阶段六：selector 的结构化参数（与 Agent 侧
// ContinuousTargetProcess 的匹配身份对应）。进程实例身份统一使用
// pid + process_start_ms + exe 三元组，避免 PID 复用导致旧 Session 采集到
// 新进程。
type ContinuousSelectorParams struct {
	PID            int    `json:"pid"`
	ProcessStartMs int64  `json:"process_start_ms"`
	Exe            string `json:"exe"`
	Cgroup         string `json:"cgroup"`
	ContainerID    string `json:"container_id"`
}

type ContinuousBatchIngestReq struct {
	SessionSID    string    `json:"session_sid" binding:"required"`
	BatchID       string    `json:"batch_id"`
	TargetIP      string    `json:"target_ip"`
	ObjectKey     string    `json:"object_key"`
	StartTime     time.Time `json:"start_time" binding:"required"`
	EndTime       time.Time `json:"end_time" binding:"required"`
	WindowCount   uint32    `json:"window_count"`
	SampleCount   uint64    `json:"sample_count"`
	SchemaVersion uint32    `json:"schema_version"`
	// 阶段一（协议 v3）：collector_generation 标识物理采集器实例，
	// batch_sequence 单调递增，content_sha256 幂等摘要，signal_counts 分信号计数。
	CollectorGeneration string                   `json:"collector_generation"`
	BatchSequence       uint64                   `json:"batch_sequence"`
	ContentSHA256       string                   `json:"content_sha256"`
	SignalCounts        map[string]uint64        `json:"signal_counts"`
	SignalTypes         []string                 `json:"signal_types"`
	Backends            map[string]string        `json:"backends"`
	ProfileFormat       string                   `json:"profile_format"`
	BackendStatus       string                   `json:"backend_status"`
	BackendReason       string                   `json:"backend_reason"`
	AttemptedBackends   []string                 `json:"attempted_backends"`
	SelectedBackend     string                   `json:"selected_backend"`
	SymbolRefs          map[string]interface{}   `json:"symbol_refs"`
	Windows             []ContinuousWindowIngest `json:"windows"`
}

type ContinuousWindowIngest struct {
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	ObjectKey     string    `json:"object_key"`
	SampleCount   uint64    `json:"sample_count"`
	SignalType    string    `json:"signal_type"`
	SchemaVersion uint32    `json:"schema_version"`
	// 阶段一（协议 v3）：window_id 逻辑窗口稳定 ID（幂等键），collector_generation
	// / target_fingerprint 标识产生窗口的采集器与目标集，content_sha256 窗口
	// 内容摘要，signal_counts 分信号计数。
	WindowID            string                       `json:"window_id"`
	CollectorGeneration string                       `json:"collector_generation"`
	TargetFingerprint   string                       `json:"target_fingerprint"`
	ContentSHA256       string                       `json:"content_sha256"`
	SignalCounts        map[string]uint64            `json:"signal_counts"`
	Backend             string                       `json:"backend"`
	Labels              map[string]interface{}       `json:"labels"`
	ProfileFormat       string                       `json:"profile_format"`
	BackendStatus       string                       `json:"backend_status"`
	BackendReason       string                       `json:"backend_reason"`
	AttemptedBackends   []string                     `json:"attempted_backends"`
	SelectedBackend     string                       `json:"selected_backend"`
	SymbolRefs          map[string]interface{}       `json:"symbol_refs"`
	Samples             []ContinuousStackSample      `json:"samples"`
	Profiles            []ContinuousProfileIngest    `json:"profiles"`
	Histograms          []ContinuousHistogramIngest  `json:"histograms"`
	Metrics             []ContinuousMetricIngest     `json:"metrics"`
	DBSnapshots         []ContinuousDBSnapshotIngest `json:"db_snapshots"`
	RSSTruncated        int                          `json:"rss_truncated"`
	// 阶段三（协议 v4）：每信号采集状态（collected/target_idle/no_events/
	// unavailable/failed + reason + lost_events）、物理/生效采样率、身份不
	// 完整被丢弃的样本数。v3 批次这些字段为零值，查询按旧规则推断。
	SignalStatuses        map[string]ContinuousSignalStatus `json:"signal_statuses"`
	PhysicalSampleRateHz  int                               `json:"physical_sample_rate_hz"`
	EffectiveSampleRateHz int                               `json:"effective_sample_rate_hz"`
	IdentityUnavailable   uint64                            `json:"identity_unavailable_count"`
}

// ContinuousSignalStatus 对应 Agent 侧 SignalStatus（阶段三 v4）。
type ContinuousSignalStatus struct {
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	LostEvents uint64 `json:"lost_events"`
}

// ContinuousDBSnapshotIngest 对应 Agent 侧的 DBSnapshotSample（阶段二）。
// Kind 区分 "digest"（SQL 摘要增量）与 "lock_wait"（锁等待链），未用到的
// 字段为零值。DigestText 是数据库自身归一化后的占位符形式 SQL，不含原始参数。
type ContinuousDBSnapshotIngest struct {
	Kind          string    `json:"kind"`
	InstanceLabel string    `json:"instance_label"`
	Timestamp     time.Time `json:"timestamp"`

	SchemaName        string `json:"schema_name"`
	DigestText        string `json:"digest_text"`
	CallCount         uint64 `json:"call_count"`
	TotalLatencyUs    uint64 `json:"total_latency_us"`
	RowsExaminedTotal uint64 `json:"rows_examined_total"`

	WaitingPID    int64  `json:"waiting_pid"`
	WaitingQuery  string `json:"waiting_query"`
	BlockingPID   int64  `json:"blocking_pid"`
	BlockingQuery string `json:"blocking_query"`
	WaitSeconds   uint64 `json:"wait_seconds"`
	LockedTable   string `json:"locked_table"`
}

type ContinuousStackSample struct {
	Stack          []string               `json:"stack"`
	StackString    string                 `json:"stack_string"`
	Count          uint64                 `json:"count"`
	Comm           string                 `json:"comm"`
	PID            int                    `json:"pid"`
	ProcessStartMs int64                  `json:"process_start_ms"`
	Exe            string                 `json:"exe"`
	StackScope     string                 `json:"stack_scope"`
	Backend        string                 `json:"backend"`
	Runtime        string                 `json:"runtime"`
	Labels         map[string]interface{} `json:"labels"`
	// Frames 阶段五结构化栈（Agent 兼容期同时发送 stack+frames；v2-only
	// 且回滚窗口结束后仅发送 frames）。apiserver 优先使用 frames。
	Frames []ContinuousStackFrame `json:"frames,omitempty"`
	// ProfileID 阶段七：样本所属 profile 的幂等 ID（memray 等显式 profile
	// 载体）。由 continuousProfileSamplesForQuery/ForIngest 从
	// ContinuousProfileIngest 填充，供 v1/v2 查询按
	// (profile_id + pid + process_start_ms + exe) 跨窗口去重，并写入
	// Parquet 保留（v2 降采样/查询与 v1 同一 dedupe 语义）。
	ProfileID string `json:"profile_id,omitempty"`
}

type ContinuousProfileIngest struct {
	SignalType string                  `json:"signal_type"`
	Backend    string                  `json:"backend"`
	StackScope string                  `json:"stack_scope"`
	ProfileID  string                  `json:"profile_id"`
	Unit       string                  `json:"unit"`
	Samples    []ContinuousStackSample `json:"samples"`
	Labels     map[string]interface{}  `json:"labels"`
}

type ContinuousMetricIngest struct {
	Metric         string                 `json:"metric"`
	Timestamp      time.Time              `json:"timestamp"`
	PID            int                    `json:"pid"`
	ProcessStartMs int64                  `json:"process_start_ms"`
	Comm           string                 `json:"comm"`
	Exe            string                 `json:"exe"`
	Runtime        string                 `json:"runtime"`
	Value          uint64                 `json:"value"`
	Unit           string                 `json:"unit"`
	Labels         map[string]interface{} `json:"labels"`
}

type ProfileTimeseriesPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     uint64    `json:"value"`
}

type ProfileTimeseriesSeries struct {
	PID            int                      `json:"pid"`
	ProcessStartMs int64                    `json:"process_start_ms"`
	Comm           string                   `json:"comm"`
	Exe            string                   `json:"exe"`
	Runtime        string                   `json:"runtime"`
	Metric         string                   `json:"metric"`
	Unit           string                   `json:"unit"`
	Peak           uint64                   `json:"peak"`
	Points         []ProfileTimeseriesPoint `json:"points"`
}

type ContinuousHistogramIngest struct {
	SignalType  string                      `json:"signal_type"`
	Backend     string                      `json:"backend"`
	Unit        string                      `json:"unit"`
	EventCount  uint64                      `json:"event_count"`
	Buckets     []ContinuousHistogramBucket `json:"buckets"`
	Summary     ContinuousHistogramSummary  `json:"summary"`
	Labels      map[string]interface{}      `json:"labels"`
	Unavailable bool                        `json:"unavailable"`
	Reason      string                      `json:"reason"`
	// 阶段三（协议 v4）：histogram 完整进程身份（strict CO-RE 按 TGID 归属；
	// degraded 无法安全归属时 pid=0 且 unavailable）。v3 批次 pid 可能非零
	// 但无 start/exe，查询按旧规则处理。
	PID            int    `json:"pid"`
	ProcessStartMs int64  `json:"process_start_ms"`
	Exe            string `json:"exe"`
	Comm           string `json:"comm"`
}

type ContinuousHistogramBucket struct {
	Range string  `json:"range"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
	Count uint64  `json:"count"`
}

type ContinuousHistogramSummary struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type continuousStoredBatch struct {
	SessionSID          string                   `json:"session_sid"`
	BatchID             string                   `json:"batch_id"`
	TargetIP            string                   `json:"target_ip"`
	StartTime           time.Time                `json:"start_time"`
	EndTime             time.Time                `json:"end_time"`
	SchemaVersion       uint32                   `json:"schema_version"`
	CollectorGeneration string                   `json:"collector_generation,omitempty"`
	BatchSequence       uint64                   `json:"batch_sequence,omitempty"`
	ContentSHA256       string                   `json:"content_sha256,omitempty"`
	SignalCounts        map[string]uint64        `json:"signal_counts,omitempty"`
	SignalTypes         []string                 `json:"signal_types,omitempty"`
	Backends            map[string]string        `json:"backends,omitempty"`
	ProfileFormat       string                   `json:"profile_format,omitempty"`
	BackendStatus       string                   `json:"backend_status,omitempty"`
	BackendReason       string                   `json:"backend_reason,omitempty"`
	AttemptedBackends   []string                 `json:"attempted_backends,omitempty"`
	SelectedBackend     string                   `json:"selected_backend,omitempty"`
	SymbolRefs          map[string]interface{}   `json:"symbol_refs,omitempty"`
	Windows             []ContinuousWindowIngest `json:"windows"`
}

type continuousAggregate struct {
	Total                 float64
	Top                   map[string]*ProfileTopItem
	Root                  *continuousTreeNode
	LabelValue            map[string]map[string]bool
	ObjectKeys            []string
	Backends              map[string]bool
	SymbolStatus          string
	TotalFrameWeight      float64
	UnresolvedFrameWeight float64
	// 未解析帧拆分：模块已知（0x<addr> [module] / [module]，符号库可补）与
	// 无模块（裸地址 / [unknown]，多为 JIT 匿名内存，本质无解）分开统计，
	// 供前端分别展示成因。
	ModuleUnresolvedFrameWeight float64
	NoModuleFrameWeight         float64
	GoSymbolReady               bool
	GoSymbolPending             bool
	GoSymbolFailed              bool
	SymbolReasons               map[string]bool
	WindowCount                 int
	Unit                        string
	RuntimeDiagnostics          map[string]*runtimeDiagnosticAccumulator
	SeenProfileIDs              map[string]bool
	SeenProfileSamples          map[string]int64
}

type runtimeDiagnosticAccumulator struct {
	Modes    map[string]bool
	Detected map[string]ProfileRuntimeProcessDiagnostic
	Ready    map[string]ProfileRuntimeProcessDiagnostic
	Missing  map[string]ProfileRuntimeProcessDiagnostic
	Limited  int
	Reasons  map[string]bool
	// 阶段四：v2 语言诊断（language_status）。任一窗口携带 v2 时 HasV2 置位，
	// 输出优先采用 v2 口径；历史窗口继续走旧字段推导（兼容一个周期）。
	HasV2                        bool
	RuntimeDetection             string
	CollectorStatus              string
	SymbolStatusV2               string
	FrameWeight                  float64
	SemanticFrameWeight          float64
	UnresolvedFrameWeight        float64
	SemanticSampleWeight         float64
	TargetModuleFrameWeight      float64
	TargetModuleUnresolvedWeight float64
	V2SampleCount                float64
}

type continuousTreeNode struct {
	Name     string
	Value    float64
	Self     float64
	Children map[string]*continuousTreeNode
	Order    []*continuousTreeNode
}

func (s *APIServer) CreateContinuousSession(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	s.createContinuousSession(c, auth.UID, auth.Name)
}

func (s *APIServer) CreateInternalContinuousSession(c *gin.Context) {
	// Kept as an explicit compatibility response so an old Agent cannot silently
	// recreate the host-wide Sessions archived by migration 006.
	s.RespondHTTPError(c, http.StatusGone, ErrCodeTaskInvalidArgument,
		"Agent 自动创建持续采集任务已停用，请由用户在主机性能中心创建")
}

func (s *APIServer) createContinuousSession(c *gin.Context, ownerUID string, userName string) {
	var req CreateContinuousSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	req.TargetIP = strings.TrimSpace(req.TargetIP)
	if req.TargetIP == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "target_ip 不能为空")
		return
	}
	if err := applyContinuousDefaults(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, err.Error())
		return
	}
	if message := validateContinuousCreateRequest(req); message != "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, message)
		return
	}
	if ok, message, _ := s.canStartCollection(CollectionSourceContinuous); !ok {
		s.RespondHTTPError(c, http.StatusInsufficientStorage, ErrCodeStorageLowDisk, message)
		return
	}
	var agent model.AgentInfo
	if err := s.DB.Where("ip_addr = ?", req.TargetIP).Order("last_seen DESC").First(&agent).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "目标 Agent 不存在")
		return
	}
	if ownerUID != "" && !s.canReadAgent(agent, s.AuthContext(c)) {
		s.forbid(c)
		return
	}
	var agentState model.ContinuousAgentState
	agentStateErr := s.DB.Where("target_ip = ?", req.TargetIP).First(&agentState).Error
	agentReady := agentStateErr == nil && time.Since(agentState.ObservedAt) <= 30*time.Second
	if !agentReady {
		s.RespondHTTPError(c, http.StatusConflict, ErrCodeDependencyUnavailable, errContinuousAgentUnavailable.Error())
		return
	}
	strictCapable := agentState.StrictCapable
	if req.ContinuityMode == "strict" && !strictCapable && !req.AllowDegraded && ownerUID != "" {
		s.RespondHTTPError(c, http.StatusConflict, ErrCodeDependencyUnavailable,
			"目标 Agent 暂不具备严格连续采集能力；确认允许降级后才能创建")
		return
	}
	continuityMode := req.ContinuityMode
	degradationReason := ""
	observedState := model.ContinuousObservedStatePending
	if !strictCapable {
		continuityMode = "degraded"
		degradationReason = "strict persistent perf/CO-RE engine unavailable; using PID-scoped rolling fallback"
	}
	labels, _ := util.MarshalJSONB(req.Labels)
	caps, _ := util.MarshalJSONB(req.Capabilities)
	signals, _ := util.MarshalJSONB(req.Signals)
	requestedSignals, _ := util.MarshalJSONB(req.Signals)
	selectorParams, _ := util.MarshalJSONB(req.SelectorParams)
	now := time.Now()
	session := model.ContinuousSession{
		SID:                  "cps-" + util.GenTID()[4:],
		Name:                 firstNonEmpty(req.Name, "Native Continuous Profiling"),
		TargetIP:             req.TargetIP,
		Hostname:             req.Hostname,
		ServiceName:          firstNonEmpty(req.ServiceName, "hotmethod"),
		SampleRateHz:         req.SampleRateHz,
		AggregationWindowSec: req.AggregationWindowSec,
		UploadBatchSec:       req.UploadBatchSec,
		RetentionHours:       req.RetentionHours,
		Labels:               labels,
		Capabilities:         caps,
		Status:               model.ContinuousSessionStatusRunning,
		Scope:                req.Scope,
		SelectorExe:          req.SelectorExe,
		SelectorMode:         req.SelectorMode,
		SelectorParams:       selectorParams,
		Signals:              signals,
		RequestedSignals:     requestedSignals,
		DesiredState:         model.ContinuousDesiredStateRunning,
		ObservedState:        observedState,
		ActiveProcesses:      []byte(`[]`),
		ContinuityMode:       continuityMode,
		AllowDegraded:        req.AllowDegraded || req.ContinuityMode == "degraded",
		DegradationReason:    degradationReason,
		Revision:             0,
		AgentID:              firstNonEmpty(agentState.AgentID, agent.AgentID),
		UID:                  ownerUID,
		UserName:             userName,
		StartedAt:            now,
		CreatedAt:            now,
		UpdatedAt:            now,
		CanManage:            ownerUID != "",
	}
	var conflictSession *model.ContinuousSession
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// AgentInfo is guaranteed to exist and provides one stable row to lock even
		// when the host currently has zero active Sessions.
		var lockedAgent model.AgentInfo
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", agent.ID).First(&lockedAgent).Error; err != nil {
			return err
		}
		var lockedState model.ContinuousAgentState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("target_ip = ?", req.TargetIP).First(&lockedState).Error; err != nil {
			return errContinuousAgentUnavailable
		}
		if time.Since(lockedState.ObservedAt) > 30*time.Second {
			return errContinuousAgentUnavailable
		}
		var active []model.ContinuousSession
		if err := tx.Where("target_ip = ? AND desired_state = ?", req.TargetIP, model.ContinuousDesiredStateRunning).
			Find(&active).Error; err != nil {
			return err
		}
		if err := validateContinuousActiveSet(active, req); err != nil {
			conflictSession = findContinuousConflict(active, req)
			return err
		}
		nextRevision := lockedState.Revision + 1
		if err := tx.Model(&lockedState).Updates(map[string]interface{}{
			"revision": nextRevision, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		session.Revision = nextRevision
		return tx.Create(&session).Error
	})
	if err != nil {
		if errors.Is(err, errContinuousAgentUnavailable) {
			s.RespondHTTPError(c, http.StatusConflict, ErrCodeDependencyUnavailable, err.Error())
			return
		}
		if errors.Is(err, errContinuousModeConflict) || errors.Is(err, errContinuousHostLimitReached) ||
			errors.Is(err, errContinuousLimitReached) ||
			errors.Is(err, errContinuousDuplicateSelector) {
			data := gin.H{"conflict_type": continuousConflictType(err), "target_ip": req.TargetIP}
			if conflictSession != nil {
				data["existing_session"] = gin.H{
					"sid": conflictSession.SID, "name": conflictSession.Name, "scope": conflictSession.Scope,
					"selector_exe": conflictSession.SelectorExe, "uid": conflictSession.UID,
					"user_name": conflictSession.UserName,
				}
			}
			respondHTTPErrorWithData(c, http.StatusConflict, ErrCodeTaskInvalidArgument, err.Error(), data)
			return
		}
		s.Logger.Error("创建 ContinuousSession 失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建 ContinuousSession 失败")
		return
	}
	s.RespondOK(c, gin.H{"session": session})
}

func findContinuousConflict(active []model.ContinuousSession, req CreateContinuousSessionReq) *model.ContinuousSession {
	requestIdentity := continuousSelectorIdentity(model.ContinuousSession{
		SelectorMode:   req.SelectorMode,
		SelectorExe:    req.SelectorExe,
		SelectorParams: mustMarshalSelectorParams(req.SelectorParams),
	})
	for index := range active {
		session := &active[index]
		if req.Scope == "host" || session.Scope == "host" || session.Scope == "" ||
			continuousSelectorIdentity(*session) == requestIdentity {
			return session
		}
	}
	return nil
}

func continuousConflictType(err error) string {
	switch {
	case errors.Is(err, errContinuousHostLimitReached):
		return "host_limit"
	case errors.Is(err, errContinuousModeConflict):
		return "scope_conflict"
	case errors.Is(err, errContinuousDuplicateSelector):
		return "duplicate_selector"
	case errors.Is(err, errContinuousLimitReached):
		return "process_limit"
	default:
		return "continuous_conflict"
	}
}

func (s *APIServer) ListContinuousSessions(c *gin.Context) {
	auth := s.AuthContext(c)
	var sessions []model.ContinuousSession
	query := s.DB.Model(&model.ContinuousSession{})
	switch strings.ToLower(strings.TrimSpace(c.DefaultQuery("owner_filter", "all"))) {
	case "", "all":
	case "mine":
		query = query.Where("uid = ?", auth.UID)
	default:
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "owner_filter 仅支持 all/mine")
		return
	}
	if value := strings.TrimSpace(c.Query("target_ip")); value != "" {
		query = query.Where("target_ip = ?", value)
	}
	if value := strings.TrimSpace(c.Query("scope")); value != "" {
		query = query.Where("scope = ?", value)
	}
	if value := strings.TrimSpace(c.Query("desired_state")); value != "" {
		query = query.Where("desired_state = ?", value)
	}
	if value := strings.TrimSpace(c.Query("observed_state")); value != "" {
		query = query.Where("observed_state = ?", value)
	}
	if value := strings.TrimSpace(c.Query("keyword")); value != "" {
		like := "%" + value + "%"
		query = query.Where("name LIKE ? OR selector_exe LIKE ?", like, like)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 ContinuousSession 失败")
		return
	}
	page, pageSize := continuousPagination(c)
	query = query.Order(continuousSessionOrderSQL()).Offset((page - 1) * pageSize).Limit(pageSize)
	if err := query.Find(&sessions).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 ContinuousSession 失败")
		return
	}
	if sessions == nil {
		sessions = []model.ContinuousSession{}
	}
	sampleCounts, err := s.continuousSessionSampleCounts(sessions)
	if err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询持续采集样本数失败")
		return
	}
	for index := range sessions {
		markContinuousSessionOffline(&sessions[index], time.Now())
		sessions[index].CanManage = s.canManageOwner(sessions[index].UID, auth)
		sessions[index].SampleCount = sampleCounts[sessions[index].SID]
	}
	s.RespondOK(c, gin.H{"sessions": sessions, "total": total, "page": page, "page_size": pageSize})
}

func (s *APIServer) DeleteContinuousSession(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	session, ok := s.loadManageableContinuousSession(c, strings.TrimSpace(c.Param("sid")), auth)
	if !ok {
		return
	}
	if session.DesiredState != model.ContinuousDesiredStateStopped ||
		(session.ObservedState != model.ContinuousObservedStateStopped && session.ObservedState != model.ContinuousObservedStateError) {
		s.RespondHTTPError(c, http.StatusConflict, ErrCodeTaskInvalidArgument, "只能删除已经停止的持续采集任务")
		return
	}
	if !continuousLooksLikeTestSession(session.Name) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "为避免误删，只能清理明显的测试任务")
		return
	}
	if sampleCount := s.continuousSessionSampleCount(session.SID); sampleCount > 0 {
		s.RespondHTTPError(c, http.StatusConflict, ErrCodeTaskInvalidArgument, "该任务已有样本，不能作为无样本测试任务清理")
		return
	}
	var batches, windows, summaries int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("session_sid = ?", session.SID).Count(&batches).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "检查持续采集数据失败")
		return
	}
	if err := s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ?", session.SID).Count(&windows).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "检查持续采集窗口失败")
		return
	}
	if err := s.DB.Model(&model.ContinuousWindowSummary{}).Where("session_sid = ?", session.SID).Count(&summaries).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "检查持续采集摘要失败")
		return
	}
	if batches > 0 || windows > 0 || summaries > 0 {
		s.RespondHTTPError(c, http.StatusConflict, ErrCodeTaskInvalidArgument, "该任务已有采集记录，不能作为无样本测试任务清理")
		return
	}
	if err := s.DB.Delete(&session).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "清理持续采集任务失败")
		return
	}
	s.RespondOK(c, gin.H{"sid": session.SID, "deleted": true})
}

func (s *APIServer) continuousSessionSampleCount(sid string) uint64 {
	var total uint64
	if err := s.DB.Model(&model.ProfileWindow{}).
		Where("session_sid = ? AND (signal_type = ? OR signal_type = '')", sid, "cpu_profile").
		Select("COALESCE(SUM(sample_count), 0)").Scan(&total).Error; err == nil && total > 0 {
		return total
	}
	_ = s.DB.Model(&model.ProfileWindow{}).
		Where("session_sid = ?", sid).
		Select("COALESCE(SUM(sample_count), 0)").Scan(&total).Error
	if total > 0 {
		return total
	}
	_ = s.DB.Model(&model.ContinuousWindowSummary{}).
		Where("session_sid = ? AND (signal_type = ? OR signal_type = '')", sid, "cpu_profile").
		Select("COALESCE(SUM(sample_count), 0)").Scan(&total).Error
	return total
}

func (s *APIServer) continuousSessionSampleCounts(sessions []model.ContinuousSession) (map[string]uint64, error) {
	counts := make(map[string]uint64, len(sessions))
	if len(sessions) == 0 {
		return counts, nil
	}
	sids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if session.SID != "" {
			sids = append(sids, session.SID)
		}
	}
	if len(sids) == 0 {
		return counts, nil
	}
	type groupedCount struct {
		SessionSID string
		SignalType string
		Total      uint64
	}
	var windows []groupedCount
	if err := s.DB.Model(&model.ProfileWindow{}).
		Select("session_sid, signal_type, COALESCE(SUM(sample_count), 0) AS total").
		Where("session_sid IN ?", sids).
		Group("session_sid, signal_type").
		Scan(&windows).Error; err != nil {
		return nil, err
	}
	allWindowCounts := make(map[string]uint64, len(sids))
	cpuWindowCounts := make(map[string]uint64, len(sids))
	for _, row := range windows {
		allWindowCounts[row.SessionSID] += row.Total
		if row.SignalType == "cpu_profile" || row.SignalType == "" {
			cpuWindowCounts[row.SessionSID] += row.Total
		}
	}
	for sid, total := range cpuWindowCounts {
		if total > 0 {
			counts[sid] = total
		}
	}
	for sid, total := range allWindowCounts {
		if counts[sid] == 0 {
			counts[sid] = total
		}
	}
	var summaries []groupedCount
	if err := s.DB.Model(&model.ContinuousWindowSummary{}).
		Select("session_sid, signal_type, COALESCE(SUM(sample_count), 0) AS total").
		Where("session_sid IN ?", sids).
		Where("signal_type = ? OR signal_type = ''", "cpu_profile").
		Group("session_sid, signal_type").
		Scan(&summaries).Error; err != nil {
		return nil, err
	}
	for _, row := range summaries {
		if counts[row.SessionSID] == 0 {
			counts[row.SessionSID] = row.Total
		}
	}
	return counts, nil
}

func continuousLooksLikeTestSession(name string) bool {
	value := strings.ToLower(strings.TrimSpace(name))
	if value == "" {
		return false
	}
	for _, marker := range []string{"boundary-", "multilang-", "test", "smoke", "测试"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (s *APIServer) StopContinuousSession(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	sid := strings.TrimSpace(c.Param("sid"))
	session, ok := s.loadManageableContinuousSession(c, sid, auth)
	if !ok {
		return
	}
	now := time.Now()
	alreadyStopped := false
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		var current model.ContinuousSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", session.ID).First(&current).Error; err != nil {
			return err
		}
		session = current
		if current.DesiredState == model.ContinuousDesiredStateStopped {
			alreadyStopped = true
			return nil
		}
		nextRevision := current.Revision + 1
		var state model.ContinuousAgentState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("target_ip = ?", current.TargetIP).First(&state).Error; err == nil {
			nextRevision = state.Revision + 1
			if err := tx.Model(&state).Updates(map[string]interface{}{"revision": nextRevision, "updated_at": now}).Error; err != nil {
				return err
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Model(&current).Updates(map[string]interface{}{
			"desired_state": model.ContinuousDesiredStateStopped, "observed_state": model.ContinuousObservedStateStopping,
			"stop_requested_at": &now, "revision": nextRevision, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		session.DesiredState = model.ContinuousDesiredStateStopped
		session.ObservedState = model.ContinuousObservedStateStopping
		session.StopRequestedAt = &now
		session.Revision = nextRevision
		return nil
	}); err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "停止 ContinuousSession 失败")
		return
	}
	s.RespondOK(c, gin.H{"session": session, "already_stopped": alreadyStopped})
}

// UpdateContinuousSessionLabels 只更新 Labels 字段（承载 db_targets 等自由格式
// 配置），不改任何其它会话状态。给"数据库账号"页面用——db_targets 变化不会
// 热更新到正在跑的 Runtime（见 ReconcileDBSampler 的已知限制），前端要提示
// 用户改完需要停止并重建 Session 才生效。
type UpdateContinuousSessionLabelsReq struct {
	Labels map[string]interface{} `json:"labels"`
}

func (s *APIServer) UpdateContinuousSessionLabels(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	sid := strings.TrimSpace(c.Param("sid"))
	session, ok := s.loadManageableContinuousSession(c, sid, auth)
	if !ok {
		return
	}
	var req UpdateContinuousSessionLabelsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	labels, err := util.MarshalJSONB(req.Labels)
	if err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "labels 序列化失败: "+err.Error())
		return
	}
	now := time.Now()
	if err := s.DB.Model(&model.ContinuousSession{}).Where("id = ?", session.ID).
		Updates(map[string]interface{}{"labels": labels, "updated_at": now}).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "更新 labels 失败")
		return
	}
	session.Labels = labels
	session.UpdatedAt = now
	s.RespondOK(c, gin.H{"session": session})
}

func (s *APIServer) IngestContinuousBatch(c *gin.Context) {
	var req ContinuousBatchIngestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	// 阶段五：容量暂停时拒绝 batch 采集（GC/删除继续；Agent 侧此时应已
	// 进入 waiting/server_storage_pressure，这里是双保险）。
	if s.capacityHalted() {
		incCollectionRejectedLowDisk(CollectionSourceContinuous)
		s.RespondHTTPError(c, http.StatusInsufficientStorage, ErrCodeStorageLowDisk,
			"服务器存储压力：Continuous 采集已暂停（server_storage_pressure），请稍后重试")
		return
	}
	if !req.StartTime.Before(req.EndTime) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "batch 时间范围不合法")
		return
	}
	// 阶段一：schema_version 白名单。v1/v2 走兼容旧路径（升级过渡期遗留
	// spool 仍可排空），v3 起启用窗口级 window_id 幂等、content_sha256 冲突
	// 检测与分信号 signal_counts；v4 在 v3 基础上增加信号状态/采样率/身份
	// 字段（增量，解析规则与 v3 相同）。未知版本一律拒绝（不可重试）。
	if req.SchemaVersion == 0 {
		req.SchemaVersion = 1
	}
	if req.SchemaVersion > continuousSchemaVersionV4 {
		s.respondContinuousConflict(c, req, "不支持的 schema_version，仅接受 v4 及以下")
		return
	}
	isV3 := req.SchemaVersion >= continuousSchemaVersionV3
	if isV3 {
		if message := validateContinuousV3Batch(req); message != "" {
			s.respondContinuousConflict(c, req, message)
			return
		}
	}
	var session model.ContinuousSession
	if err := s.DB.Where("sid = ?", req.SessionSID).First(&session).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "ContinuousSession 不存在")
		return
	}
	if session.AgentID != "" && getRequestUID(c) != session.AgentID {
		s.forbid(c)
		return
	}
	// v3 为兼容旧 Agent 只强制四类核心信号；v4 的七类信号均为显式合同，
	// payload 和零计数 signal_statuses 都不得越过 Session 请求集合。
	if isV3 {
		signalSet := continuousSessionSignalSet(session)
		for signal := range continuousBatchSignalSet(req.Windows) {
			if req.SchemaVersion < continuousSchemaVersionV4 && !continuousCoreSignal(signal) {
				continue
			}
			if !signalSet[signal] {
				s.respondContinuousConflict(c, req, "窗口包含 Session 未请求的信号: "+signal)
				return
			}
		}
		if req.SchemaVersion >= continuousSchemaVersionV4 {
			if message := validateContinuousV4Windows(req.Windows); message != "" {
				s.respondContinuousConflict(c, req, message)
				return
			}
		}
	}
	if req.BatchID == "" {
		req.BatchID = "cpb-" + util.GenTID()[4:]
	}
	if req.TargetIP == "" {
		req.TargetIP = session.TargetIP
	} else if req.TargetIP != session.TargetIP {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "target_ip 与 ContinuousSession 不一致")
		return
	}
	if req.ObjectKey == "" {
		req.ObjectKey = continuousBatchObjectKey(req.SessionSID, req.BatchID)
	}
	if req.WindowCount == 0 {
		req.WindowCount = uint32(len(req.Windows))
	}
	receivedAt := time.Now()
	clockOffsetMS, clockStatus, clockObserved := continuousAgentClock(c, receivedAt)

	// 阶段一 v3：batch 级幂等。同 batch_id 重传必须携带相同 content_sha256；
	// 摘要不同 = 内容冲突（不可重试，禁止换 ID 绕过）。
	var existing model.ProfileBatch
	if err := s.DB.Where("bid = ?", req.BatchID).First(&existing).Error; err == nil {
		if existing.SessionSID != req.SessionSID || !existing.StartTime.Equal(req.StartTime) || !existing.EndTime.Equal(req.EndTime) {
			s.respondContinuousConflict(c, req, "batch_id 已被不同采集批次使用")
			return
		}
		if isV3 && existing.ContentSHA256 != "" && req.ContentSHA256 != "" && existing.ContentSHA256 != req.ContentSHA256 {
			s.respondContinuousConflict(c, req, "batch_id 内容摘要冲突：相同 ID 不同内容，禁止换 ID 重传")
			return
		}
		s.updateContinuousAgentClock(req.SessionSID, clockOffsetMS, clockStatus, clockObserved)
		s.respondContinuousBatchACK(c, req, true)
		return
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 ProfileBatch 失败")
		return
	}
	req.SignalTypes = normalizeContinuousSignalTypes(req)
	if req.Backends == nil {
		req.Backends = map[string]string{}
	}
	now := receivedAt

	// 阶段一：分信号计数由服务端权威重算（不信任 Agent 上报值）。v3 起 batch
	// 层 sample_count 废弃写 0，窗口行 sample_count 仅表示该行信号自身计数。
	windowSignalCounts := make([]map[string]uint64, len(req.Windows))
	for i := range req.Windows {
		windowSignalCounts[i] = continuousWindowSignalCountsFor(req.Windows[i])
	}
	batchSignalCounts := continuousBatchSignalCounts(req.Windows)
	batchSampleCount := uint64(0)
	if !isV3 {
		batchSampleCount = clampContinuousCount(req.SampleCount)
	}
	batch := model.ProfileBatch{
		BID:                 req.BatchID,
		SessionSID:          req.SessionSID,
		TargetIP:            req.TargetIP,
		ObjectKey:           req.ObjectKey,
		StartTime:           req.StartTime,
		EndTime:             req.EndTime,
		WindowCount:         req.WindowCount,
		SampleCount:         batchSampleCount,
		SchemaVersion:       req.SchemaVersion,
		CollectorGeneration: req.CollectorGeneration,
		BatchSequence:       req.BatchSequence,
		ContentSHA256:       req.ContentSHA256,
		SignalCounts:        continuousSignalCountsJSON(batchSignalCounts),
		SignalTypes:         mustJSONBytes(req.SignalTypes),
		Backends:            mustJSONBytes(req.Backends),
		Status:              model.ContinuousBatchStatusReady,
		ProfileFormat:       firstNonEmpty(req.ProfileFormat, "json"),
		BackendStatus:       firstNonEmpty(req.BackendStatus, "ok"),
		BackendReason:       req.BackendReason,
		AttemptedBackends:   mustJSONBytes(req.AttemptedBackends),
		SelectedBackend:     req.SelectedBackend,
		SymbolRefs:          mustJSONBytes(req.SymbolRefs),
		ReceivedAt:          receivedAt,
		AgentClockOffsetMs:  clockOffsetMS,
		CreatedAt:           now,
	}
	duplicate := false
	var payloadStoreErr error
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "bid"}}, DoNothing: true}).Create(&batch)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// 并发竞态：同一 batch_id 已被写入。校验摘要，摘要不同 = 冲突。
			var raced model.ProfileBatch
			if err := tx.Where("bid = ?", req.BatchID).First(&raced).Error; err != nil {
				return err
			}
			if isV3 && raced.ContentSHA256 != "" && req.ContentSHA256 != "" && raced.ContentSHA256 != req.ContentSHA256 {
				return errContinuousConflict
			}
			duplicate = true
			return nil
		}
		if err := s.storeContinuousBatchPayload(c.Request.Context(), req, &batch); err != nil {
			payloadStoreErr = err
			return err
		}
		if batch.PayloadBytes > 0 {
			// Create 时字段尚未算出，补写 payload_bytes（供 compactor 磁盘检查）
			if err := tx.Model(&batch).Update("payload_bytes", batch.PayloadBytes).Error; err != nil {
				return err
			}
		}
		for wi, in := range req.Windows {
			if in.WindowStart.IsZero() || in.WindowEnd.IsZero() || !in.WindowStart.Before(in.WindowEnd) {
				continue
			}
			labels, _ := json.Marshal(in.Labels)
			symbolRefs, _ := json.Marshal(in.SymbolRefs)
			signalRows := continuousWindowSignalRows(in)
			if isV3 {
				// 阶段一 v3：一个逻辑窗口每种信号只建一行（backend 内部信息）。
				signalRows = continuousWindowSignalRowsV3(in)
			}
			for _, signal := range signalRows {
				// 阶段三（协议 v4）：每信号采集状态。v3 批次无 signal_statuses
				// 字段，按旧规则推断（有样本 → collected，否则 unknown）。
				signalStatus := "unknown"
				signalStatusReason := ""
				signalLostEvents := uint64(0)
				if status, ok := in.SignalStatuses[signal.SignalType]; ok && status.Status != "" {
					signalStatus = status.Status
					signalStatusReason = status.Reason
					signalLostEvents = status.LostEvents
				} else if isV3 {
					if continuousWindowSignalHasData(in, signal.SignalType) {
						signalStatus = "collected"
					}
				}
				window := model.ProfileWindow{
					SessionSID:            req.SessionSID,
					BatchBID:              req.BatchID,
					WindowStart:           in.WindowStart,
					WindowEnd:             in.WindowEnd,
					ObjectKey:             firstNonEmpty(in.ObjectKey, req.ObjectKey),
					SampleCount:           clampContinuousCount(continuousWindowSampleCount(in, signal.SignalType)),
					SignalType:            signal.SignalType,
					SchemaVersion:         firstNonZeroUint32(in.SchemaVersion, req.SchemaVersion),
					WindowID:              in.WindowID,
					CollectorGeneration:   firstNonEmpty(in.CollectorGeneration, req.CollectorGeneration),
					TargetFingerprint:     in.TargetFingerprint,
					ContentSHA256:         in.ContentSHA256,
					SignalCounts:          continuousSignalCountsJSON(windowSignalCounts[wi]),
					Backend:               signal.Backend,
					Labels:                labels,
					ProfileFormat:         firstNonEmpty(in.ProfileFormat, req.ProfileFormat, "json"),
					BackendStatus:         firstNonEmpty(in.BackendStatus, req.BackendStatus, "ok"),
					BackendReason:         firstNonEmpty(in.BackendReason, req.BackendReason),
					AttemptedBackends:     mustJSONBytes(firstNonEmptySlice(in.AttemptedBackends, req.AttemptedBackends)),
					SelectedBackend:       firstNonEmpty(in.SelectedBackend, req.SelectedBackend),
					SymbolRefs:            symbolRefs,
					SignalStatus:          signalStatus,
					SignalStatusReason:    signalStatusReason,
					SignalLostEvents:      signalLostEvents,
					PhysicalSampleRateHz:  in.PhysicalSampleRateHz,
					EffectiveSampleRateHz: in.EffectiveSampleRateHz,
					IdentityUnavailable:   in.IdentityUnavailable,
					CreatedAt:             now,
				}
				// 阶段一 v3：窗口级幂等/冲突。同一 (session, window_id, signal_type)
				// 已存在时：内容摘要相同 → 跳过（不重复计数）；不同 → 内容冲突。
				if isV3 && in.WindowID != "" {
					var existingWindow model.ProfileWindow
					ewErr := tx.Where("session_sid = ? AND window_id = ? AND signal_type = ?",
						req.SessionSID, in.WindowID, signal.SignalType).First(&existingWindow).Error
					if ewErr == nil {
						if existingWindow.ContentSHA256 != "" && in.ContentSHA256 != "" &&
							existingWindow.ContentSHA256 != in.ContentSHA256 {
							return errContinuousConflict
						}
						continue
					} else if !errors.Is(ewErr, gorm.ErrRecordNotFound) {
						return ewErr
					}
				}
				if err := tx.Create(&window).Error; err != nil {
					// Postgres 部分唯一索引 (session_sid, window_id, signal_type)
					// 兜底：并发窗口冲突 → 不可重试冲突，整批回滚。
					if continuousIsUniqueViolation(err) {
						return errContinuousConflict
					}
					return err
				}
			}
		}
		return tx.Model(&model.ContinuousSession{}).
			Where("sid = ?", req.SessionSID).
			Updates(map[string]interface{}{
				"last_upload_at":        gorm.Expr("CASE WHEN last_upload_at IS NULL OR last_upload_at < ? THEN ? ELSE last_upload_at END", req.EndTime, req.EndTime),
				"updated_at":            now,
				"agent_clock_offset_ms": clockOffsetMS, "agent_clock_status": clockStatus,
				"agent_clock_observed_at": clockObserved,
			}).Error
	})
	if err != nil {
		if errors.Is(err, errContinuousConflict) {
			s.Logger.Warn("Continuous 批次内容冲突，拒绝入库", zap.String("sid", req.SessionSID), zap.String("bid", req.BatchID))
			s.respondContinuousConflict(c, req, "窗口/批次内容冲突：相同 ID 不同内容，禁止换 ID 重传")
			return
		}
		if payloadStoreErr != nil {
			s.Logger.Error("保存 Continuous ProfileBatch payload 失败", zap.String("sid", req.SessionSID), zap.Error(payloadStoreErr))
			s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "保存 Continuous ProfileBatch payload 失败")
			return
		}
		s.Logger.Error("登记 ProfileBatch 失败", zap.String("sid", req.SessionSID), zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "登记 ProfileBatch 失败")
		return
	}
	if duplicate {
		var raced model.ProfileBatch
		if err := s.DB.Where("bid = ?", req.BatchID).First(&raced).Error; err != nil ||
			raced.SessionSID != req.SessionSID || !raced.StartTime.Equal(req.StartTime) || !raced.EndTime.Equal(req.EndTime) {
			s.RespondHTTPError(c, http.StatusConflict, ErrCodeTaskInvalidArgument, "batch_id 并发冲突")
			return
		}
		s.updateContinuousAgentClock(req.SessionSID, clockOffsetMS, clockStatus, clockObserved)
	}
	s.cleanupContinuousRetention(c.Request.Context(), session)
	s.respondContinuousBatchACK(c, req, duplicate)
}

// validateContinuousV3Batch 收紧 v3 幂等契约。任一身份/摘要字段为空都会让
// 唯一索引或冲突检测失效，因此不能像 v1/v2 一样静默补默认值或跳过窗口。
func validateContinuousV3Batch(req ContinuousBatchIngestReq) string {
	if strings.TrimSpace(req.BatchID) == "" {
		return "v3 batch_id 不能为空"
	}
	if strings.TrimSpace(req.CollectorGeneration) == "" || len(req.CollectorGeneration) > 64 {
		return "v3 collector_generation 不合法"
	}
	if req.BatchSequence == 0 {
		return "v3 batch_sequence 必须大于 0"
	}
	if !continuousValidSHA256(req.ContentSHA256) {
		return "v3 content_sha256 必须是 64 位十六进制 SHA-256"
	}
	if len(req.Windows) == 0 {
		return "v3 batch 必须至少包含一个窗口"
	}
	if req.WindowCount != 0 && int(req.WindowCount) != len(req.Windows) {
		return "v3 window_count 与 windows 数量不一致"
	}
	for i, window := range req.Windows {
		if window.WindowStart.IsZero() || window.WindowEnd.IsZero() || !window.WindowStart.Before(window.WindowEnd) {
			return fmt.Sprintf("v3 windows[%d] 时间范围不合法", i)
		}
		if window.WindowStart.Before(req.StartTime) || window.WindowEnd.After(req.EndTime) {
			return fmt.Sprintf("v3 windows[%d] 超出 batch 时间范围", i)
		}
		if strings.TrimSpace(window.WindowID) == "" || len(window.WindowID) > 128 {
			return fmt.Sprintf("v3 windows[%d].window_id 不合法", i)
		}
		if strings.TrimSpace(window.TargetFingerprint) == "" || len(window.TargetFingerprint) > 256 {
			return fmt.Sprintf("v3 windows[%d].target_fingerprint 不合法", i)
		}
		if window.CollectorGeneration != "" && window.CollectorGeneration != req.CollectorGeneration {
			return fmt.Sprintf("v3 windows[%d].collector_generation 与 batch 不一致", i)
		}
		if !continuousValidSHA256(window.ContentSHA256) {
			return fmt.Sprintf("v3 windows[%d].content_sha256 必须是 64 位十六进制 SHA-256", i)
		}
	}
	return ""
}

func continuousValidSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

// continuousIsUniqueViolation 判断错误是否为唯一约束冲突（Postgres 与 SQLite
// 的诊断串不同）。
func continuousIsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate entry")
}

// respondContinuousConflict 返回 409 不可重试冲突。Agent 侧对 retryable:false
// 的 4xx 会把 spool 文件移入 .rejected 隔离区，绝不换 ID 重传。
func (s *APIServer) respondContinuousConflict(c *gin.Context, req ContinuousBatchIngestReq, message string) {
	incContinuousConflictTotal()
	s.RespondHTTPError(c, http.StatusConflict, ErrCodeTaskInvalidArgument, message)
}

func continuousAgentClock(c *gin.Context, receivedAt time.Time) (int64, string, *time.Time) {
	raw := strings.TrimSpace(c.GetHeader("X-Mini-Drop-Agent-Time-Ms"))
	if raw == "" {
		return 0, "unknown", nil
	}
	agentMS, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || agentMS <= 0 {
		return 0, "unknown", nil
	}
	offset := receivedAt.UnixMilli() - agentMS
	absOffset := offset
	if absOffset < 0 {
		absOffset = -absOffset
	}
	status := "ok"
	if absOffset > 30000 {
		status = "critical"
	} else if absOffset > 5000 {
		status = "warning"
	}
	observed := receivedAt
	return offset, status, &observed
}

func (s *APIServer) updateContinuousAgentClock(sid string, offset int64, status string, observed *time.Time) {
	if observed == nil {
		return
	}
	if err := s.DB.Model(&model.ContinuousSession{}).Where("sid = ?", sid).Updates(map[string]interface{}{
		"agent_clock_offset_ms": offset, "agent_clock_status": status, "agent_clock_observed_at": observed,
	}).Error; err != nil {
		s.Logger.Warn("更新 Agent 时钟偏差失败", zap.String("sid", sid), zap.Error(err))
	}
}

func continuousSessionClockStatus(session model.ContinuousSession) string {
	if session.AgentClockObservedAt == nil {
		return "unknown"
	}
	return firstNonEmpty(session.AgentClockStatus, "unknown")
}

func (s *APIServer) respondContinuousBatchACK(c *gin.Context, req ContinuousBatchIngestReq, duplicate bool) {
	if duplicate {
		incContinuousDuplicateBatchTotal()
	}
	s.RespondOK(c, gin.H{
		"accepted": true, "duplicate": duplicate,
		"batch_id": req.BatchID, "session_sid": req.SessionSID,
	})
}

func (s *APIServer) GetContinuousTimeline(c *gin.Context) {
	session, ok := s.loadReadableContinuousSession(c, c.Param("sid"), s.AuthContext(c))
	if !ok {
		return
	}
	now := time.Now()
	retentionHours := firstNonZeroUint32(session.RetentionHours, 24)
	boundaryFrom := now.Add(-time.Duration(retentionHours) * time.Hour)
	if session.StartedAt.After(boundaryFrom) {
		boundaryFrom = session.StartedAt
	}
	boundaryTo := now
	if session.StoppedAt != nil && session.StoppedAt.Before(boundaryTo) {
		boundaryTo = *session.StoppedAt
	}
	requestedFrom, valid := parseOptionalTime(c, "from")
	if !valid {
		return
	}
	requestedTo, valid := parseOptionalTime(c, "to")
	if !valid {
		return
	}
	if !requestedFrom.IsZero() && requestedFrom.After(boundaryFrom) {
		boundaryFrom = requestedFrom
	}
	if !requestedTo.IsZero() && requestedTo.Before(boundaryTo) {
		boundaryTo = requestedTo
	}
	if !boundaryFrom.Before(boundaryTo) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "timeline 时间范围不合法或超出保留边界")
		return
	}
	query := s.DB.Where("session_sid = ?", session.SID).Order("window_start ASC")
	query = query.Where("window_end >= ? AND window_start <= ?", boundaryFrom, boundaryTo)

	// 阶段一：finalization 状态。running Session 的 finalization grace =
	// upload_batch_sec + 2×aggregation_window_sec + 15s；finalized_to 之前
	// 缺失的数据才计入真实 gap，之后到查询终点属于 pending tail。Session 停止
	// 后以 stopped_at 作为最终边界，不再显示 pending。
	fin := s.continuousFinalizationState(session, now)

	// 阶段六：Timeline 从 coverage segments + 最近热 window 计算，保持
	// windows/gaps/coverage/clock 兼容字段；历史压缩条目 compacted=true +
	// coverage_source=parquet_catalog（避免长范围全量返回明细窗口）。
	windows, gaps, coverage, err := s.continuousTimelineV2(c.Request.Context(), session, boundaryFrom, boundaryTo, fin.FinalizedTo, query)
	if err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 Continuous timeline 失败")
		return
	}
	s.RespondOK(c, gin.H{
		"session": session, "windows": windows, "total": len(windows),
		"gaps": gaps, "coverage": coverage,
		"finalized_to":         fin.FinalizedTo,
		"pending":              fin.Pending,
		"pending_tail_seconds": fin.PendingTailSeconds,
		"ingest_lag_seconds":   fin.IngestLagSeconds,
		"delivery_lag_seconds": fin.DeliveryLagSeconds,
		"collector_stalled":    fin.CollectorStalled,
		"clock": gin.H{
			"offset_ms": session.AgentClockOffsetMs, "status": continuousSessionClockStatus(session),
			"observed_at": session.AgentClockObservedAt,
		},
	})
}

// continuousTimelineFinalization 阶段一：Timeline 正确性所需的上传/交付状态。
type continuousTimelineFinalization struct {
	FinalizedTo        time.Time `json:"finalized_to"`
	Pending            bool      `json:"pending"`
	PendingTailSeconds float64   `json:"pending_tail_seconds"`
	IngestLagSeconds   float64   `json:"ingest_lag_seconds"`
	DeliveryLagSeconds float64   `json:"delivery_lag_seconds"`
	CollectorStalled   bool      `json:"collector_stalled"`
}

// continuousFinalizationState 计算 running/stopped Session 的上传最终化边界。
func (s *APIServer) continuousFinalizationState(session model.ContinuousSession, now time.Time) continuousTimelineFinalization {
	st := continuousTimelineFinalization{}
	// 上一个已确认入库的 batch 结束时间 = 最新 finalized 数据点。
	// 直接按 end_time DESC 取最新 batch，跨 SQLite/Postgres 正确处理 NULL。
	var lastEnd, lastReceivedAt time.Time
	var latestBatch model.ProfileBatch
	if err := s.DB.Model(&model.ProfileBatch{}).
		Where("session_sid = ?", session.SID).
		Order("end_time DESC").Limit(1).First(&latestBatch).Error; err == nil {
		lastEnd = latestBatch.EndTime
		lastReceivedAt = latestBatch.ReceivedAt
	}

	// Session 停止后：stopped_at 是最终边界，不再显示 pending，也不标记
	// collector_stalled（停止后不存在"采集停滞"语义；若数据提前停止，
	// [lastEnd, stopped_at] 的真实缺口由 coverage 在 finalized 域内体现）。
	if session.StoppedAt != nil {
		st.FinalizedTo = *session.StoppedAt
		return st
	}
	// running：grace 之后的数据已经超过正常上传等待期，应进入 finalized 域。
	// finalized_to 取“安全时间地平线”和最新已入库 batch 结束时间的较晚者：
	// 最近已收到的数据可以立即计入覆盖；采集停滞时 [lastEnd,horizon] 会成为
	// 真实 trailing gap，而不是永远被隐藏在 pending tail 中。
	grace := time.Duration(firstNonZeroUint32(session.UploadBatchSec, 60))*time.Second +
		2*time.Duration(firstNonZeroUint32(session.AggregationWindowSec, 10))*time.Second +
		15*time.Second
	horizon := now.Add(-grace)
	if lastEnd.IsZero() {
		startedAt := session.StartedAt
		if startedAt.IsZero() {
			startedAt = session.CreatedAt
		}
		st.FinalizedTo = startedAt
		if horizon.After(st.FinalizedTo) {
			st.FinalizedTo = horizon
			st.CollectorStalled = true
		}
		st.Pending = true
		st.PendingTailSeconds = now.Sub(st.FinalizedTo).Seconds()
		st.IngestLagSeconds = now.Sub(startedAt).Seconds()
		if st.IngestLagSeconds < 0 {
			st.IngestLagSeconds = 0
		}
		if st.PendingTailSeconds < 0 {
			st.PendingTailSeconds = 0
		}
		return st
	}
	st.FinalizedTo = lastEnd
	if horizon.After(st.FinalizedTo) {
		st.FinalizedTo = horizon
	}
	st.IngestLagSeconds = now.Sub(lastEnd).Seconds()
	if st.IngestLagSeconds < 0 {
		st.IngestLagSeconds = 0
	}
	if !lastReceivedAt.IsZero() {
		st.DeliveryLagSeconds = lastReceivedAt.Sub(lastEnd).Seconds()
		if st.DeliveryLagSeconds < 0 {
			st.DeliveryLagSeconds = 0
		}
	}
	st.Pending = true
	st.CollectorStalled = now.Sub(lastEnd) > grace
	st.PendingTailSeconds = now.Sub(st.FinalizedTo).Seconds()
	if st.PendingTailSeconds < 0 {
		st.PendingTailSeconds = 0
	}
	return st
}

// continuousTimelineV2 阶段六 timeline：热 window（< 2h，v1 staging）+
// coverage segments（历史已压缩，compacted=true）合成 windows/gaps/coverage。
// 长范围不再全量返回明细 window，避免响应体积随范围线性膨胀。
func (s *APIServer) continuousTimelineV2(ctx context.Context, session model.ContinuousSession, from, to, finalizedTo time.Time,
	baseQuery *gorm.DB) ([]gin.H, []continuousTimelineGap, gin.H, error) {
	hotRetention := time.Duration(s.Config.ContinuousParquet.HotMetadataRetentionMinutes) * time.Minute
	if hotRetention <= 0 {
		hotRetention = 120 * time.Minute
	}
	hotCutoff := time.Now().Add(-hotRetention)

	var hotWindows []model.ProfileWindow
	if err := baseQuery.WithContext(ctx).
		Where("window_start >= ?", func() time.Time {
			if from.After(hotCutoff) {
				return from
			}
			return hotCutoff
		}()).
		Find(&hotWindows).Error; err != nil {
		return nil, nil, nil, err
	}
	var segments []model.ContinuousCoverageSegment
	catalogTo := to
	if hotCutoff.Before(catalogTo) {
		catalogTo = hotCutoff
	}
	if err := s.DB.WithContext(ctx).
		Where("session_sid = ? AND segment_start < ? AND segment_end > ?", session.SID, catalogTo, from).
		Order("segment_start ASC").Find(&segments).Error; err != nil {
		return nil, nil, nil, err
	}

	items := make([]gin.H, 0, len(hotWindows)+len(segments))
	merged := make([]model.ProfileWindow, 0, len(hotWindows)+len(segments))
	for _, window := range hotWindows {
		items = append(items, gin.H{
			"id": window.ID, "session_sid": window.SessionSID, "batch_bid": window.BatchBID,
			"window_start": window.WindowStart, "window_end": window.WindowEnd,
			"object_key": window.ObjectKey, "sample_count": window.SampleCount,
			"signal_type": window.SignalType, "backend": window.Backend,
			"labels": json.RawMessage(window.Labels), "compacted": false,
			"coverage_source": "v1_staging",
		})
		merged = append(merged, window)
	}
	for _, segment := range segments {
		item := gin.H{
			"session_sid": segment.SessionSID, "signal_type": segment.SignalType,
			"window_start": segment.SegmentStart, "window_end": segment.SegmentEnd,
			"sample_count": segment.SampleCount, "compacted": true,
			"coverage_source": "parquet_catalog", "resolution": segment.Resolution,
			"source_block": segment.SourceBlock, "source_version": segment.SourceVersion,
		}
		if segment.SegmentStart.Before(from) {
			item["window_start"] = from
		}
		if segment.SegmentEnd.After(to) {
			item["window_end"] = to
		}
		if segment.SegmentEnd.After(hotCutoff) {
			item["window_end"] = hotCutoff
		}
		items = append(items, item)
		merged = append(merged, model.ProfileWindow{
			SessionSID: segment.SessionSID, SignalType: segment.SignalType,
			WindowStart: segment.SegmentStart, WindowEnd: segment.SegmentEnd,
			SampleCount: segment.SampleCount,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return windowStartOf(items[i]).Before(windowStartOf(items[j]))
	})
	gaps, coverage := continuousTimelineCoverage(merged, from, to, finalizedTo, 5*time.Second)
	return items, gaps, coverage, nil
}

func windowStartOf(item gin.H) time.Time {
	if value, ok := item["window_start"].(time.Time); ok {
		return value
	}
	return time.Time{}
}

type continuousTimelineGap struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	DurationSeconds float64   `json:"duration_seconds"`
	Type            string    `json:"type"`
}

// continuousTimelineCoverage 计算 [from, to] 内的真实缺口与覆盖率。阶段一：
// finalizedTo 为最终化边界——finalizedTo 之前缺失的数据才计入真实 gap，之后
// 到 to 属于 pending tail（由调用方单独上报，不降低 finalized 覆盖率）。
func continuousTimelineCoverage(windows []model.ProfileWindow, from, to, finalizedTo time.Time, tolerance time.Duration) ([]continuousTimelineGap, gin.H) {
	finalTo := to
	if !finalizedTo.IsZero() && finalizedTo.Before(finalTo) {
		finalTo = finalizedTo
	}
	if finalTo.Before(from) {
		finalTo = from
	}
	type interval struct{ start, end time.Time }
	intervals := make([]interval, 0, len(windows))
	for _, window := range windows {
		start, end := window.WindowStart, window.WindowEnd
		if start.Before(from) {
			start = from
		}
		if end.After(finalTo) {
			end = finalTo
		}
		if start.Before(end) {
			intervals = append(intervals, interval{start: start, end: end})
		}
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start.Before(intervals[j].start) })
	merged := make([]interval, 0, len(intervals))
	for _, item := range intervals {
		if len(merged) == 0 || item.start.Sub(merged[len(merged)-1].end) > tolerance {
			merged = append(merged, item)
			continue
		}
		if item.end.After(merged[len(merged)-1].end) {
			merged[len(merged)-1].end = item.end
		}
	}

	gaps := []continuousTimelineGap{}
	cursor := from
	for i, item := range merged {
		if item.start.Sub(cursor) > tolerance {
			gapType := "internal"
			if i == 0 {
				gapType = "leading"
			}
			gaps = append(gaps, continuousTimelineGap{Start: cursor, End: item.start, DurationSeconds: item.start.Sub(cursor).Seconds(), Type: gapType})
		}
		if item.end.After(cursor) {
			cursor = item.end
		}
	}
	if finalTo.Sub(cursor) > tolerance {
		gaps = append(gaps, continuousTimelineGap{Start: cursor, End: finalTo, DurationSeconds: finalTo.Sub(cursor).Seconds(), Type: "trailing"})
	}
	totalSeconds := finalTo.Sub(from).Seconds()
	gapSeconds := float64(0)
	for _, gap := range gaps {
		gapSeconds += gap.DurationSeconds
	}
	coveredSeconds := totalSeconds - gapSeconds
	if coveredSeconds < 0 {
		coveredSeconds = 0
	}
	ratio := float64(0)
	if totalSeconds > 0 {
		ratio = coveredSeconds / totalSeconds
	}
	return gaps, gin.H{
		"from": from, "to": finalTo, "total_seconds": totalSeconds,
		"covered_seconds": coveredSeconds, "gap_seconds": gapSeconds, "ratio": ratio,
	}
}

func (s *APIServer) QueryContinuousProfile(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	stats := s.pqQueryStatsFor(c.Request.Context(), q)
	fg, found, err := s.queryNativeContinuousFlamegraph(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	topn, _, err := s.queryNativeContinuousTopN(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		s.RespondOK(c, gin.H{
			"query":                 profileLabelSelector(q),
			"nodes":                 []ProfileNode{},
			"items":                 []ProfileTopItem{},
			"total":                 0,
			"unit":                  "samples",
			"empty":                 true,
			"message":               "Native Continuous Profiling 暂无覆盖该时间范围的 10s window",
			"source":                "mini-drop-native",
			"profile_source":        "native",
			"generated_at":          time.Now(),
			"resolution_seconds":    stats.ResolutionSeconds,
			"mixed_resolution":      stats.MixedResolution,
			"storage_source":        stats.StorageSource,
			"earliest_available_at": stats.EarliestAvailable,
		})
		return
	}
	s.RespondOK(c, gin.H{
		"query":                 fg.Query,
		"nodes":                 fg.Nodes,
		"items":                 topn.Items,
		"total":                 fg.Total,
		"unit":                  fg.Unit,
		"empty":                 fg.Empty,
		"message":               fg.Message,
		"source":                fg.Source,
		"profile_source":        fg.ProfileSource,
		"profile_url":           fg.ProfileURL,
		"raw_profile_url":       fg.RawProfileURL,
		"generated_at":          fg.GeneratedAt,
		"resolution_seconds":    stats.ResolutionSeconds,
		"mixed_resolution":      stats.MixedResolution,
		"storage_source":        stats.StorageSource,
		"earliest_available_at": stats.EarliestAvailable,
	})
}

func (s *APIServer) ViewContinuousProfileObject(c *gin.Context) {
	key := strings.TrimSpace(c.Query("key"))
	sid := continuousSessionSIDFromObjectKey(key)
	if sid == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法 Continuous Profile 对象路径")
		return
	}
	if _, ok := s.loadReadableContinuousSession(c, sid, s.AuthContext(c)); !ok {
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
		return
	}
	defer reader.Close()

	c.Header("Content-Type", mimeType(key))
	c.Header("Content-Disposition", contentDisposition("inline", path.Base(key)))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理输出 Continuous Profile 对象失败", zap.String("key", key), zap.Error(err))
	}
}

func (s *APIServer) storeContinuousBatchPayload(ctx context.Context, req ContinuousBatchIngestReq, batch *model.ProfileBatch) error {
	if !s.StorageConnected() {
		return errProfileUnavailable
	}
	payload := continuousStoredBatch{
		SessionSID:          req.SessionSID,
		BatchID:             req.BatchID,
		TargetIP:            req.TargetIP,
		StartTime:           req.StartTime,
		EndTime:             req.EndTime,
		SchemaVersion:       req.SchemaVersion,
		CollectorGeneration: req.CollectorGeneration,
		BatchSequence:       req.BatchSequence,
		ContentSHA256:       req.ContentSHA256,
		SignalCounts:        req.SignalCounts,
		SignalTypes:         req.SignalTypes,
		Backends:            req.Backends,
		ProfileFormat:       req.ProfileFormat,
		BackendStatus:       req.BackendStatus,
		BackendReason:       req.BackendReason,
		AttemptedBackends:   req.AttemptedBackends,
		SelectedBackend:     req.SelectedBackend,
		SymbolRefs:          req.SymbolRefs,
		Windows:             req.Windows,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// 阶段三：记录原始 payload 字节数，供 compactor 磁盘余量检查估算输入大小。
	batch.PayloadBytes = uint64(len(body))
	return s.Storage.PutObject(ctx, s.Config.Storage.Bucket, req.ObjectKey, bytes.NewReader(body), int64(len(body)), "application/json")
}

func continuousBatchObjectKey(sessionSID, batchID string) string {
	return "continuous/" + sessionSID + "/" + batchID + ".json"
}

func (s *APIServer) cleanupContinuousRetention(ctx context.Context, session model.ContinuousSession) {
	retentionHours := session.RetentionHours
	if retentionHours == 0 || retentionHours > 24 {
		// Historical sessions may contain the former 30-day maximum. Phase two
		// applies the fixed 24-hour raw-data ceiling during cleanup as well.
		retentionHours = 24
	}
	cutoff := time.Now().Add(-time.Duration(retentionHours) * time.Hour)

	var expiredBatches []model.ProfileBatch
	if err := s.DB.Where("session_sid = ? AND end_time < ?", session.SID, cutoff).Find(&expiredBatches).Error; err != nil {
		s.Logger.Warn("Native Continuous Profiling retention 查询过期 batch 失败", zap.String("sid", session.SID), zap.Error(err))
		return
	}

	// 冷热分层：硬删原始 window 之前先把它们(cpu_profile 信号)降采样进
	// ContinuousWindowSummary。降采样失败的 window（对应的 batch 对象读取
	// 出错）不能删，留到下一轮重试，避免"摘要没写成功、原始数据却已经
	// 被删了"这种数据丢失。
	var expiredWindows []model.ProfileWindow
	if err := s.DB.Where("session_sid = ? AND window_end < ?", session.SID, cutoff).Find(&expiredWindows).Error; err != nil {
		s.Logger.Warn("Native Continuous Profiling retention 查询过期 window 失败", zap.String("sid", session.SID), zap.Error(err))
		return
	}
	failedObjects, err := s.downsampleContinuousWindows(ctx, session, expiredWindows)
	if err != nil {
		s.Logger.Warn("Native Continuous Profiling 冷层摘要生成失败，本轮跳过硬删",
			zap.String("sid", session.SID), zap.Error(err))
		return
	}
	deletableIDs := make([]uint, 0, len(expiredWindows))
	for _, w := range expiredWindows {
		if w.SignalType == "cpu_profile" && failedObjects[w.ObjectKey] {
			continue
		}
		deletableIDs = append(deletableIDs, w.ID)
	}
	if len(deletableIDs) == 0 {
		return
	}
	if err := s.DB.Where("id IN ?", deletableIDs).Delete(&model.ProfileWindow{}).Error; err != nil {
		s.Logger.Warn("Native Continuous Profiling retention 删除过期 window 失败", zap.String("sid", session.SID), zap.Error(err))
		return
	}

	for _, batch := range expiredBatches {
		// 阶段三：已压缩进块的 batch 不能直接删行/删对象——object_key 已指向
		// 块，必须由 compactor 在重写块时统一移除成员并管理块生命周期。
		if batch.BlockID != "" {
			continue
		}
		var remaining int64
		if err := s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ? AND batch_bid = ?", session.SID, batch.BID).Count(&remaining).Error; err != nil {
			s.Logger.Warn("Native Continuous Profiling retention 检查 batch 引用失败", zap.String("sid", session.SID), zap.String("batch", batch.BID), zap.Error(err))
			continue
		}
		if remaining > 0 {
			continue
		}
		if err := s.DB.Where("bid = ?", batch.BID).Delete(&model.ProfileBatch{}).Error; err != nil {
			s.Logger.Warn("Native Continuous Profiling retention 删除 batch 失败", zap.String("sid", session.SID), zap.String("batch", batch.BID), zap.Error(err))
			continue
		}
		if batch.ObjectKey != "" && s.StorageConnected() {
			if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, batch.ObjectKey); err != nil {
				s.Logger.Warn("Native Continuous Profiling retention 删除对象失败", zap.String("sid", session.SID), zap.String("object_key", batch.ObjectKey), zap.Error(err))
			}
		}
	}
}

func (s *APIServer) queryNativeContinuousFlamegraph(ctx context.Context, q ProfileQuery) (ProfileFlamegraph, bool, error) {
	stats := s.pqQueryStatsFor(ctx, q)
	agg, found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil {
		return ProfileFlamegraph{}, found, err
	}
	if !found {
		// 原始窗口没有了：可能这段时间本来就没采集，也可能已经过期被
		// 降采样进冷层摘要了。冷层摘要没有调用栈，火焰图做不出来，但
		// 至少要把"数据其实还在，只是降级成 TopN 了"这件事告诉调用方，
		// 而不是让它以为压根没采过——两种情况前端展示语义完全不同。
		hasSummary, summaryErr := s.continuousHasSummaryForRange(ctx, q)
		if summaryErr != nil {
			return ProfileFlamegraph{}, false, summaryErr
		}
		if !hasSummary {
			return ProfileFlamegraph{}, false, nil
		}
		return ProfileFlamegraph{
			Empty:         true,
			Degraded:      true,
			Message:       "该时间范围原始数据已过期清理，仅保留降采样 TopN 摘要，无法生成调用树火焰图，请改用 TopN 视图查看",
			Source:        "mini-drop-native-cold",
			ProfileSource: "native",
			Query:         profileLabelSelector(q),
			GeneratedAt:   time.Now(),
		}, true, nil
	}
	maxNodes := q.MaxNodes
	if maxNodes == 0 {
		maxNodes = continuousDefaultMaxNodes
	}
	nodes, truncated := continuousTreeToProfileNodesTruncated(agg.Root, "", maxNodes)
	out := ProfileFlamegraph{
		Nodes:              nodes,
		Total:              agg.Total,
		Unit:               firstNonEmpty(agg.Unit, "samples"),
		Backend:            continuousBackendList(agg.Backends),
		Empty:              len(nodes) == 0 || agg.Total == 0,
		Source:             "mini-drop-native",
		ProfileSource:      "native",
		ProfileURL:         s.continuousProfileURL(ctx, q, agg.ObjectKeys),
		RawProfileURL:      s.continuousRawProfileURL(ctx, agg.ObjectKeys),
		Query:              profileLabelSelector(q),
		SymbolStatus:       agg.SymbolStatus,
		ResolutionSeconds:  stats.ResolutionSeconds,
		MixedResolution:    stats.MixedResolution,
		StorageSource:      stats.StorageSource,
		EarliestAvailable:  stats.EarliestAvailable,
		SymbolDiagnostics:  continuousSymbolDiagnostics(agg),
		RuntimeDiagnostics: continuousRuntimeDiagnostics(agg),
		Truncated:          truncated,
		GeneratedAt:        time.Now(),
	}
	if out.Empty {
		out.Message = "Native Continuous Profiling 暂无匹配样本"
	}
	return out, true, nil
}

// queryNativeContinuousDiffFlamegraph 两次拉 base/compare 期间的调用树，
// 按调用路径逐节点对齐算 delta，输出一棵可以直接喂给差分火焰图渲染的树。
// 和 diffTopN(表格 diff) 是两条独立路径——那个走 queryNativeContinuousTopN
// 拿扁平列表，这个走 queryNativeContinuousAggregate 拿 Root 树，互不影响。
//
// 任一侧原始数据已经过期只剩冷层摘要（没有调用树）时，直接报 Degraded，
// 不强行拼一棵"半棵冷半棵热"的树——冷层摘要本来就没有调用栈结构信息，
// 拼出来的树没有意义。
func (s *APIServer) queryNativeContinuousDiffFlamegraph(ctx context.Context, q ProfileDiffQuery) (ProfileDiffFlamegraph, bool, error) {
	baseAgg, baseFound, err := s.queryNativeContinuousAggregate(ctx, ProfileQuery{
		TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.BaseFrom, To: q.BaseTo,
		ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes,
		OwnerUIDs: q.OwnerUIDs, CanReadAll: q.CanReadAll, StackScope: q.StackScope,
	})
	if err != nil {
		return ProfileDiffFlamegraph{}, false, err
	}
	compareAgg, compareFound, err := s.queryNativeContinuousAggregate(ctx, ProfileQuery{
		TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.CompareFrom, To: q.CompareTo,
		ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes,
		OwnerUIDs: q.OwnerUIDs, CanReadAll: q.CanReadAll, StackScope: q.StackScope,
	})
	if err != nil {
		return ProfileDiffFlamegraph{}, false, err
	}

	baseDegraded, err := s.continuousSideIsDegraded(ctx, q, q.BaseFrom, q.BaseTo, baseFound)
	if err != nil {
		return ProfileDiffFlamegraph{}, false, err
	}
	compareDegraded, err := s.continuousSideIsDegraded(ctx, q, q.CompareFrom, q.CompareTo, compareFound)
	if err != nil {
		return ProfileDiffFlamegraph{}, false, err
	}
	if baseDegraded || compareDegraded {
		return ProfileDiffFlamegraph{
			Empty:       true,
			Degraded:    true,
			Message:     "对比的时间范围里有一段原始数据已过期清理，只剩降采样摘要，无法生成调用树差分火焰图，请改用表格 diff 视图",
			Source:      "mini-drop-native-cold",
			GeneratedAt: time.Now(),
		}, true, nil
	}
	if !baseFound && !compareFound {
		// 两侧都真的什么都没有（不是冷层，是压根没采集过），交回调用方
		// 走原有的"未找到"兜底分支。
		return ProfileDiffFlamegraph{}, false, nil
	}

	var baseRoot, compareRoot *continuousTreeNode
	baseTotal, compareTotal := 0.0, 0.0
	baseUnit, compareUnit := "", ""
	if baseFound {
		baseRoot = baseAgg.Root
		baseTotal = baseAgg.Total
		baseUnit = baseAgg.Unit
	}
	if compareFound {
		compareRoot = compareAgg.Root
		compareTotal = compareAgg.Total
		compareUnit = compareAgg.Unit
	}

	diffRoot := diffContinuousTreeNode("root", baseRoot, compareRoot)
	maxNodes := q.MaxNodes
	if maxNodes == 0 {
		maxNodes = continuousDefaultMaxNodes
	}
	truncatedRoot, truncated := truncateDiffTree(diffRoot, maxNodes)

	out := ProfileDiffFlamegraph{
		Root:         truncatedRoot,
		BaseTotal:    baseTotal,
		CompareTotal: compareTotal,
		Unit:         firstNonEmpty(baseUnit, compareUnit, "samples"),
		Empty:        len(truncatedRoot.Children) == 0,
		Source:       "mini-drop-native",
		Truncated:    truncated,
		GeneratedAt:  time.Now(),
	}
	if out.Empty {
		out.Message = "暂无可对比数据"
	}
	return out, true, nil
}

// continuousSideIsDegraded 判断 base/compare 里的一侧要不要标记成"降级到
// 冷层"——只有"原始查询没找到、但冷层确实有摘要"才算，纯粹没采集过不算。
func (s *APIServer) continuousSideIsDegraded(ctx context.Context, q ProfileDiffQuery, from, to time.Time, found bool) (bool, error) {
	if found {
		return false, nil
	}
	return s.continuousHasSummaryForRange(ctx, ProfileQuery{
		Host: q.Host, From: from, To: to, OwnerUIDs: q.OwnerUIDs, CanReadAll: q.CanReadAll,
	})
}

func (s *APIServer) queryNativeContinuousTopN(ctx context.Context, q ProfileQuery) (ProfileTopN, bool, error) {
	agg, found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil {
		return ProfileTopN{}, found, err
	}
	if !found {
		// 原始窗口不在了——落到冷层摘要兜底；连摘要都没有才是真的"没有
		// 这段数据"，交回调用方走原来的 !found 分支（可能落回旧的
		// profileClient().TopN 兜底路径）。
		return s.queryNativeContinuousSummary(ctx, q)
	}
	items := make([]ProfileTopItem, 0, len(agg.Top))
	for _, item := range agg.Top {
		continuousFinalizeTopItem(item, agg.Total)
		items = append(items, *item)
	}
	// 按栈顶（self）排序，不是按 value（inclusive，函数在调用链任意位置出现都算）。
	// value 会把"调用了很多耗时子函数的胶水代码"排到热点函数前面，self 才是"这行代码本身在烧 CPU"。
	sort.Slice(items, func(i, j int) bool {
		if items[i].Self == items[j].Self {
			return items[i].Name < items[j].Name
		}
		return items[i].Self > items[j].Self
	})
	maxNodes := q.MaxNodes
	if maxNodes == 0 {
		maxNodes = continuousDefaultMaxNodes
	}
	truncated := false
	if len(items) > maxNodes {
		items = items[:maxNodes]
		truncated = true
	}
	out := ProfileTopN{
		Items:              items,
		Total:              agg.Total,
		Unit:               firstNonEmpty(agg.Unit, "samples"),
		Backend:            continuousBackendList(agg.Backends),
		Empty:              len(items) == 0 || agg.Total == 0,
		Source:             "mini-drop-native",
		ProfileSource:      "native",
		ProfileURL:         s.continuousProfileURL(ctx, q, agg.ObjectKeys),
		RawProfileURL:      s.continuousRawProfileURL(ctx, agg.ObjectKeys),
		Query:              profileLabelSelector(q),
		SymbolStatus:       agg.SymbolStatus,
		SymbolDiagnostics:  continuousSymbolDiagnostics(agg),
		RuntimeDiagnostics: continuousRuntimeDiagnostics(agg),
		Truncated:          truncated,
		GeneratedAt:        time.Now(),
	}
	if out.Empty {
		out.Message = "Native Continuous Profiling 暂无匹配样本"
	}
	return out, true, nil
}

// continuousSummarySessionSIDs 返回 q.Host 对应、当前调用方有权限看到的
// session SID 子查询——和 queryNativeContinuousAggregate 里的会话过滤逻辑
// 保持一致，避免冷层摘要绕过权限检查。
func (s *APIServer) continuousSummarySessionSIDs(q ProfileQuery) *gorm.DB {
	sessionQuery := s.DB.Model(&model.ContinuousSession{}).Select("sid").Where("target_ip = ?", q.Host)
	if !q.CanReadAll {
		if len(q.OwnerUIDs) > 0 {
			sessionQuery = sessionQuery.Where("(uid IN ? OR uid = '' OR uid IS NULL)", q.OwnerUIDs)
		} else {
			sessionQuery = sessionQuery.Where("(uid = '' OR uid IS NULL)")
		}
	}
	return sessionQuery
}

// continuousHasSummaryForRange 只判断"这段时间冷层有没有摘要"，不取数据，
// 供火焰图路径决定要不要把"没有数据"改成"数据已降级"的提示。
func (s *APIServer) continuousHasSummaryForRange(ctx context.Context, q ProfileQuery) (bool, error) {
	var count int64
	err := s.DB.WithContext(ctx).Model(&model.ContinuousWindowSummary{}).
		Where("session_sid IN (?)", s.continuousSummarySessionSIDs(q)).
		Where("signal_type = ?", "cpu_profile").
		Where("bucket_end >= ? AND bucket_start <= ?", q.From, q.To).
		Count(&count).Error
	return count > 0, err
}

// queryNativeContinuousSummary 是 queryNativeContinuousTopN 在原始窗口已经
// 过期清理后的冷层兜底：把命中时间范围的若干小时桶摘要按函数名合并、
// 重新排序，返回一份 Degraded=true 的 ProfileTopN。找不到任何摘要时
// found=false，交回调用方走原有的"没有这段数据"分支。
func (s *APIServer) queryNativeContinuousSummary(ctx context.Context, q ProfileQuery) (ProfileTopN, bool, error) {
	var rows []model.ContinuousWindowSummary
	err := s.DB.WithContext(ctx).
		Where("session_sid IN (?)", s.continuousSummarySessionSIDs(q)).
		Where("signal_type = ?", "cpu_profile").
		Where("bucket_end >= ? AND bucket_start <= ?", q.From, q.To).
		Find(&rows).Error
	if err != nil {
		return ProfileTopN{}, false, err
	}
	if len(rows) == 0 {
		return ProfileTopN{}, false, nil
	}

	merged := map[string]*ProfileTopItem{}
	var totalSamples uint64
	for _, row := range rows {
		if len(row.TopSelfJSON) == 0 {
			continue
		}
		var items []ProfileTopItem
		if unmarshalErr := json.Unmarshal(row.TopSelfJSON, &items); unmarshalErr != nil {
			s.Logger.Warn("解析 Native Continuous Profiling 冷层摘要失败，跳过该桶",
				zap.Uint("summary_id", row.ID), zap.Error(unmarshalErr))
			continue
		}
		for _, item := range items {
			key, displayName, unresolved := continuousTopFrameKey(item.Name)
			existing, ok := merged[key]
			if !ok {
				existing = &ProfileTopItem{Name: key, DisplayName: displayName, Unit: firstNonEmpty(item.Unit, "samples"), Unresolved: unresolved}
				merged[key] = existing
			}
			// 冷层不再区分 inclusive/exclusive——Value 退化成等于 Self，
			// 前端排序/占比逻辑复用同一套字段不用特殊分支。
			existing.Self += item.Self
			existing.Value += item.Self
		}
		totalSamples += row.SampleCount
	}

	items := make([]ProfileTopItem, 0, len(merged))
	for _, item := range merged {
		continuousFinalizeTopItem(item, float64(totalSamples))
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Self == items[j].Self {
			return items[i].Name < items[j].Name
		}
		return items[i].Self > items[j].Self
	})
	maxNodes := q.MaxNodes
	if maxNodes == 0 {
		maxNodes = continuousDefaultMaxNodes
	}
	truncated := false
	if len(items) > maxNodes {
		items = items[:maxNodes]
		truncated = true
	}

	out := ProfileTopN{
		Items:         items,
		Total:         float64(totalSamples),
		Unit:          "samples",
		Empty:         len(items) == 0,
		Source:        "mini-drop-native-cold",
		ProfileSource: "native",
		Query:         profileLabelSelector(q),
		Truncated:     truncated,
		Degraded:      true,
		GeneratedAt:   time.Now(),
	}
	if out.Empty {
		out.Message = "该时间范围原始数据已过期清理，且冷层摘要里没有匹配样本"
	} else {
		out.Message = "该时间范围原始数据已过期清理，以下为降采样摘要（仅函数级 self time 汇总，精度低于原始数据，不支持调用树/火焰图）"
	}
	return out, true, nil
}

// downsampleContinuousWindows 把即将被硬删的 cpu_profile 窗口聚合进冷层
// 摘要表，在硬删原始 window/batch 之前调用。非 cpu_profile 信号
// （io_latency/sched_latency）v1 不做冷层摘要，直接跟着走原有的硬删——
// histogram 摘要没有函数名维度，值不值得单独建模留到后续按需再做。
//
// 返回处理失败（MinIO 对象读取/解析出错）涉及到的 batch object key 集合，
// 调用方要把这些 object key 对应的 window 从本轮硬删名单里剔除、留到下一
// 轮重试——绝不能"摘要没写成功、原始数据却已经被删了"。
func (s *APIServer) downsampleContinuousWindows(ctx context.Context, session model.ContinuousSession, windows []model.ProfileWindow) (map[string]bool, error) {
	failedObjects := map[string]bool{}

	// 阶段六：enforce 下 v2 1h 层承担 30 天保留，停止生成新的
	// ContinuousWindowSummary；旧摘要继续按 168h 自然过期。
	if s.pqModeOf() == "enforce" {
		return failedObjects, nil
	}

	byObject := map[string][]model.ProfileWindow{}
	for _, w := range windows {
		if w.SignalType != "cpu_profile" || w.ObjectKey == "" {
			continue
		}
		byObject[w.ObjectKey] = append(byObject[w.ObjectKey], w)
	}
	if len(byObject) == 0 {
		return failedObjects, nil
	}

	type bucketKey struct {
		bucketStart time.Time
	}
	buckets := map[bucketKey]*continuousAggregate{}
	bucketEnds := map[bucketKey]time.Time{}

	for objectKey, rows := range byObject {
		// 阶段三：对象可能是旧分钟 JSON 或 gzip 小时块；块解压一次后按
		// (batch_bid, window start) 精确匹配这轮要降采样的 window。
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			s.Logger.Warn("Native Continuous Profiling 冷层摘要读取 batch 对象失败，本轮跳过这些窗口",
				zap.String("session_sid", session.SID), zap.String("object_key", objectKey), zap.Error(err))
			failedObjects[objectKey] = true
			continue
		}
		// 用 (batch_bid, Unix 秒) 匹配，不直接比较 time.Time：row.WindowStart
		// 是从数据库读回来的（Postgres 时间戳精度可能比 Go 的 time.Time 低），
		// window.WindowStart 是从 MinIO 里的原始 JSON 解析出来的（保留了
		// Agent 上报时的完整精度），两边直接 Equal() 有极小概率因为精度
		// 截断错判成"不匹配"，导致这个 window 被删除却从没进过摘要。
		rowSet := map[string]bool{}
		for _, row := range rows {
			if row.BatchBID != "" {
				rowSet[row.BatchBID+"|"+strconv.FormatInt(row.WindowStart.Unix(), 10)] = true
			} else {
				rowSet["|"+strconv.FormatInt(row.WindowStart.Unix(), 10)] = true
			}
		}
		for _, batch := range batches {
			for _, window := range batch.Windows {
				key := batch.BatchID + "|" + strconv.FormatInt(window.WindowStart.Unix(), 10)
				if !rowSet[key] && !rowSet["|"+strconv.FormatInt(window.WindowStart.Unix(), 10)] {
					continue // 这个 batch 对象里还有别的、这轮还没到期的 window，不动它
				}
				if firstNonEmpty(window.SignalType, "cpu_profile") != "cpu_profile" {
					continue
				}
				bucketStart := window.WindowStart.Truncate(continuousSummaryBucketDuration)
				keyBucket := bucketKey{bucketStart: bucketStart}
				agg, ok := buckets[keyBucket]
				if !ok {
					agg = &continuousAggregate{
						Top:                map[string]*ProfileTopItem{},
						Root:               &continuousTreeNode{Name: "root", Children: map[string]*continuousTreeNode{}},
						LabelValue:         map[string]map[string]bool{"comm": {}, "pid": {}, "process_start_ms": {}, "process_instance": {}, "exe": {}, "runtime": {}},
						Backends:           map[string]bool{},
						SymbolReasons:      map[string]bool{},
						RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{},
						SeenProfileIDs:     map[string]bool{},
						SeenProfileSamples: map[string]int64{},
						Unit:               "samples",
					}
					buckets[keyBucket] = agg
				}
				bucketEnd := bucketStart.Add(continuousSummaryBucketDuration)
				if existing, ok := bucketEnds[keyBucket]; !ok || existing.Before(bucketEnd) {
					bucketEnds[keyBucket] = bucketEnd
				}
				// 复用生产 TopN 路径同一套 continuousAddSample 聚合逻辑，不再
				// 手写一份"取栈顶算 self"——这正是之前 TopN 排序 bug 的教训：
				// 两处独立实现迟早会走偏。
				for _, sample := range continuousProfileSamplesForQuery(window, ProfileQuery{}, agg.SeenProfileIDs) {
					if !continuousSampleMatches(sample, window.Labels, nil) {
						continue
					}
					continuousAddSample(agg, sample, window.Labels)
				}
			}
		}
	}

	for key, agg := range buckets {
		if len(agg.Top) == 0 {
			continue
		}
		if err := s.mergeContinuousWindowSummary(ctx, session.SID, "cpu_profile", key.bucketStart, bucketEnds[key], agg); err != nil {
			return failedObjects, err
		}
	}
	return failedObjects, nil
}

// mergeContinuousWindowSummary 把一个小时桶新算出来的 Top self 值合并进
// 已有的 ContinuousWindowSummary 行（如果存在）——因为 retention 清理是
// 周期性跑的，同一个小时桶里的 window 很可能分几轮才全部过期，需要
// 读出旧摘要、按函数名累加、重新排序截断，而不是覆盖。
//
// 读-改-写三步之间原来没有任何互斥：per-ingest 触发的清理和周期性
// ticker 触发的清理可能并发跑到同一个 (session_sid, signal_type,
// bucket_start) 桶，后写的会用自己读到的旧值覆盖，静默丢样本。这里用
// Postgres 会话级 advisory lock（按桶 key 哈希）把同一个桶的读-改-写
// 序列化：不同桶之间互不阻塞，同一个桶的并发调用变成排队执行，读到的
// 一定是上一个调用已经写完的最新值。锁随事务提交/回滚自动释放
// （pg_advisory_xact_lock），不需要手动 unlock。
func (s *APIServer) mergeContinuousWindowSummary(ctx context.Context, sessionSID, signalType string, bucketStart, bucketEnd time.Time, agg *continuousAggregate) error {
	lockKey := sessionSID + "|" + signalType + "|" + bucketStart.UTC().Format(time.RFC3339)
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// SQLite backs unit tests and does not implement PostgreSQL advisory
		// locks. Production uses PostgreSQL, where this serializes the bucket.
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", lockKey).Error; err != nil {
				return err
			}
		}
		return s.mergeContinuousWindowSummaryLocked(ctx, tx, sessionSID, signalType, bucketStart, bucketEnd, agg)
	})
}

func (s *APIServer) mergeContinuousWindowSummaryLocked(ctx context.Context, tx *gorm.DB, sessionSID, signalType string, bucketStart, bucketEnd time.Time, agg *continuousAggregate) error {
	var existing model.ContinuousWindowSummary
	lookupErr := tx.WithContext(ctx).
		Where("session_sid = ? AND signal_type = ? AND bucket_start = ?", sessionSID, signalType, bucketStart).
		First(&existing).Error

	merged := map[string]float64{}
	var sampleCount uint64
	if lookupErr == nil {
		var oldItems []ProfileTopItem
		if len(existing.TopSelfJSON) > 0 {
			if unmarshalErr := json.Unmarshal(existing.TopSelfJSON, &oldItems); unmarshalErr != nil {
				s.Logger.Warn("解析已有冷层摘要失败，本次将用新数据覆盖",
					zap.String("session_sid", sessionSID), zap.Time("bucket_start", bucketStart), zap.Error(unmarshalErr))
				oldItems = nil
			}
		}
		for _, item := range oldItems {
			merged[item.Name] += item.Self
		}
		sampleCount = existing.SampleCount
	} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		return lookupErr
	}

	for name, item := range agg.Top {
		merged[name] += item.Self
	}
	sampleCount += uint64(agg.Total)

	items := make([]ProfileTopItem, 0, len(merged))
	for name, self := range merged {
		key, displayName, unresolved := continuousTopFrameKey(name)
		item := ProfileTopItem{Name: key, DisplayName: displayName, Self: self, Value: self, Unit: "samples", Unresolved: unresolved}
		continuousFinalizeTopItem(&item, float64(sampleCount))
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Self == items[j].Self {
			return items[i].Name < items[j].Name
		}
		return items[i].Self > items[j].Self
	})
	if len(items) > continuousSummaryTopLimit {
		items = items[:continuousSummaryTopLimit]
	}

	payload, marshalErr := json.Marshal(items)
	if marshalErr != nil {
		return marshalErr
	}

	row := model.ContinuousWindowSummary{}
	result := tx.WithContext(ctx).
		Where(model.ContinuousWindowSummary{SessionSID: sessionSID, SignalType: signalType, BucketStart: bucketStart}).
		Assign(map[string]interface{}{
			"bucket_end":    bucketEnd,
			"sample_count":  sampleCount,
			"top_self_json": payload,
			"unit":          "samples",
		}).
		FirstOrCreate(&row)
	return result.Error
}

func (s *APIServer) queryNativeContinuousLabelValues(ctx context.Context, q ProfileQuery, label string) (ProfileLabelValues, bool, error) {
	if label != "process_instance" && !isAllowedProfileFilterLabel(label) {
		return ProfileLabelValues{
			Label:       label,
			Values:      []string{},
			Available:   false,
			Message:     "Native Continuous Profiling 仅支持 comm/pid/process_start_ms/process_instance/exe/runtime 标签",
			Source:      "mini-drop-native",
			Query:       profileLabelSelector(q),
			GeneratedAt: time.Now(),
		}, true, nil
	}
	agg, found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil || !found {
		return ProfileLabelValues{}, found, err
	}
	values := make([]string, 0, len(agg.LabelValue[label]))
	for value := range agg.LabelValue[label] {
		values = append(values, value)
	}
	sort.Strings(values)
	out := ProfileLabelValues{
		Label:       label,
		Values:      values,
		Available:   len(values) > 0,
		Source:      "mini-drop-native",
		Query:       profileLabelSelector(q),
		GeneratedAt: time.Now(),
	}
	if len(values) == 0 {
		out.Message = "Native Continuous Profiling 暂无可用过滤标签"
	}
	return out, true, nil
}

func (s *APIServer) GetProfileTimeseries(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	metric := strings.ToLower(strings.TrimSpace(c.DefaultQuery("metric", "rss_bytes")))
	if q.ProfileType != "memory" || metric != "rss_bytes" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "timeseries 仅支持 profile_type=memory&metric=rss_bytes")
		return
	}
	maxSeries := 20
	if raw := strings.TrimSpace(c.Query("max_series")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxSeries = parsed
		}
	}
	if maxSeries > 128 {
		maxSeries = 128
	}
	// 阶段六：prefer/enforce 逐小时混合（v2 历史小时 + v1 热小时）
	series, rssTruncated, found, err := s.pqQueryTimeseriesMixed(c.Request.Context(), q, metric, maxSeries)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		series = []ProfileTimeseriesSeries{}
	}
	stats := s.pqQueryStatsFor(c.Request.Context(), q)
	// 阶段七：RSS 空数据诊断。"显示可查询但实际为空"的根因分两类：
	// ① 该范围根本没有 python_rss 窗口（未启用信号/无 Python 进程）；
	// ② 有窗口但被过滤条件排除（comm/pid/实例过滤不匹配）。
	diagnostics := []string{}
	if len(series) == 0 {
		hasRSSWindow, _ := s.continuousHasSignalWindow(c.Request.Context(), q, "python_rss")
		if !hasRSSWindow {
			diagnostics = append(diagnostics, "该时间范围没有 python_rss 采集窗口（Session 未启用 Python RSS 信号，或目标上无 Python 进程）")
		} else if len(q.Filters) > 0 {
			diagnostics = append(diagnostics, "存在 python_rss 窗口，但当前过滤条件（comm/pid/实例/exe）没有匹配到任何进程")
		} else {
			diagnostics = append(diagnostics, "存在 python_rss 窗口，但窗口内没有 rss_bytes 采样点（进程可能已退出或采样被截断）")
		}
	}
	if rssTruncated > 0 {
		diagnostics = append(diagnostics, fmt.Sprintf("Agent 侧进程数超过上限，RSS 采样被截断（最多 %d 个进程）", rssTruncated))
	}
	s.RespondOK(c, gin.H{
		"series": series, "metric": metric, "unit": "bytes", "empty": len(series) == 0, "generated_at": time.Now(),
		"storage_source": stats.StorageSource, "resolution_seconds": stats.ResolutionSeconds,
		"mixed_resolution": stats.MixedResolution, "earliest_available_at": stats.EarliestAvailable,
		"rss_truncated": rssTruncated, "process_count": len(series), "diagnostics": diagnostics,
	})
}

// continuousHasSignalWindow 判断 [from,to] 内是否存在某信号的 profile_windows 行
// （供空数据诊断区分"没采集"与"采集了但被过滤"）。
func (s *APIServer) continuousHasSignalWindow(ctx context.Context, q ProfileQuery, signalType string) (bool, error) {
	var count int64
	err := s.DB.WithContext(ctx).Model(&model.ProfileWindow{}).
		Where("session_sid IN (?)", s.continuousSessionSelection(q)).
		Where("signal_type = ?", signalType).
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Count(&count).Error
	return count > 0, err
}

func (s *APIServer) queryNativeContinuousTimeseries(ctx context.Context, q ProfileQuery, metricName string, maxSeries int) ([]ProfileTimeseriesSeries, int, bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	err := s.DB.Where("session_sid IN (?)", sessionQuery).Where("signal_type = ?", "python_rss").
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).Order("window_start ASC").
		Limit(continuousMaxWindowCount + 1).Find(&windows).Error
	if err != nil {
		return nil, 0, false, err
	}
	if len(windows) > continuousMaxWindowCount {
		return nil, 0, true, errContinuousWindowLimit
	}
	if len(windows) == 0 {
		return []ProfileTimeseriesSeries{}, 0, false, nil
	}
	if !s.StorageConnected() {
		return nil, 0, true, errProfileUnavailable
	}

	byKey := map[string]*ProfileTimeseriesSeries{}
	rssTruncated := 0
	objectOrder, byObject := continuousGroupWindowsByObject(windows)
	for _, objectKey := range objectOrder {
		// 阶段三：块只解压一次，再按 DB 行选中的 batch 关联
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			return nil, 0, true, err
		}
		batchByID := continuousBatchIndex(batches)
		seenBatch := map[string]bool{}
		for _, row := range byObject[objectKey] {
			batch, rowKey, ok := continuousResolveBatch(row, batches, batchByID)
			if !ok || seenBatch[rowKey] {
				continue
			}
			seenBatch[rowKey] = true
			for _, window := range batch.Windows {
				if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
					continue
				}
				// 阶段七：Agent 侧进程数截断诊断（窗口级 RSSTruncated 取最大）。
				if window.RSSTruncated > rssTruncated {
					rssTruncated = window.RSSTruncated
				}
				for _, metric := range window.Metrics {
					if metric.Metric != metricName || metric.Timestamp.Before(q.From) || metric.Timestamp.After(q.To) {
						continue
					}
					sample := ContinuousStackSample{PID: metric.PID, Comm: metric.Comm, Exe: metric.Exe, Runtime: metric.Runtime, Labels: metric.Labels}
					if !continuousSampleMatches(sample, window.Labels, q.Filters) {
						continue
					}
					key := continuousMetricSeriesKey(metric)
					series := byKey[key]
					if series == nil {
						series = &ProfileTimeseriesSeries{PID: metric.PID, ProcessStartMs: metric.ProcessStartMs, Comm: metric.Comm, Exe: metric.Exe, Runtime: firstNonEmpty(metric.Runtime, "python"), Metric: metricName, Unit: firstNonEmpty(metric.Unit, "bytes"), Points: []ProfileTimeseriesPoint{}}
						byKey[key] = series
					}
					series.Points = append(series.Points, ProfileTimeseriesPoint{Timestamp: metric.Timestamp, Value: metric.Value})
					if metric.Value > series.Peak {
						series.Peak = metric.Value
					}
				}
			}
		}
	}
	out := make([]ProfileTimeseriesSeries, 0, len(byKey))
	for _, series := range byKey {
		sort.Slice(series.Points, func(i, j int) bool { return series.Points[i].Timestamp.Before(series.Points[j].Timestamp) })
		series.Points = downsampleRSSPoints(series.Points, 600)
		out = append(out, *series)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Peak == out[j].Peak {
			return out[i].PID < out[j].PID
		}
		return out[i].Peak > out[j].Peak
	})
	if len(out) > maxSeries {
		out = out[:maxSeries]
	}
	return out, rssTruncated, true, nil
}

func continuousMetricSeriesKey(metric ContinuousMetricIngest) string {
	return strconv.Itoa(metric.PID) + "|" + strconv.FormatInt(metric.ProcessStartMs, 10) + "|" + metric.Exe
}

func downsampleRSSPoints(points []ProfileTimeseriesPoint, limit int) []ProfileTimeseriesPoint {
	if limit <= 0 || len(points) <= limit {
		return points
	}
	out := make([]ProfileTimeseriesPoint, 0, limit)
	for bucket := 0; bucket < limit; bucket++ {
		start := bucket * len(points) / limit
		end := (bucket + 1) * len(points) / limit
		peak := points[start]
		for _, point := range points[start+1 : end] {
			if point.Value > peak.Value {
				peak = point
			}
		}
		out = append(out, peak)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

func (s *APIServer) queryNativeContinuousAggregate(ctx context.Context, q ProfileQuery) (continuousAggregate, bool, error) {
	// 阶段五/六：prefer/enforce 模式走逐小时混合规划器（v2 历史小时 +
	// v1 当前小时；某小时 parquet 缺失/失败仅该小时回退 v1）。
	if pqModeQueryV2(s.pqModeOf()) {
		v2Agg, v2Found, err := s.pqQueryAggregateMixed(ctx, q)
		if err != nil {
			return continuousAggregate{}, false, err
		}
		if v2Found {
			return v2Agg, true, nil
		}
		// 无任何 v2/v1 数据 → 走原 v1 空路径（返回 not found）
	}
	agg := continuousAggregate{
		Top: map[string]*ProfileTopItem{},
		Root: &continuousTreeNode{
			Name:     "root",
			Children: map[string]*continuousTreeNode{},
		},
		LabelValue: map[string]map[string]bool{
			"comm":             {},
			"pid":              {},
			"process_start_ms": {},
			"process_instance": {},
			"exe":              {},
			"runtime":          {},
		},
		Backends:           map[string]bool{},
		SymbolStatus:       "not_applicable",
		SymbolReasons:      map[string]bool{},
		Unit:               map[bool]string{true: "bytes", false: "samples"}[q.ProfileType == "memory"],
		RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{},
		SeenProfileIDs:     map[string]bool{},
		SeenProfileSamples: map[string]int64{},
	}
	found, err := s.aggregateV1WindowsInto(ctx, q, q.From, q.To, &agg)
	if err != nil {
		return continuousAggregate{}, true, err
	}
	if !found {
		return continuousAggregate{}, false, nil
	}
	continuousFinalizeSymbolStatus(&agg)
	return agg, true, nil
}

// aggregateV1WindowsInto 把 [from, to) 内的 v1 window 数据聚合进 agg
// （CPU/profile 信号；按 window_start 分区，避免小时边界重复计数）。
// 返回 (是否找到 window, 错误)。供整段 v1 查询与逐小时混合规划器复用。
func (s *APIServer) aggregateV1WindowsInto(ctx context.Context, q ProfileQuery, from, to time.Time, agg *continuousAggregate) (bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	signalType := "cpu_profile"
	if q.ProfileType == "memory" {
		signalType = "python_memory"
	}
	err := s.DB.WithContext(ctx).Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", signalType).
		Where("window_start >= ? AND window_start < ?", from, to).
		Order("window_start ASC").
		Limit(continuousMaxWindowCount + 1).
		Find(&windows).Error
	if err != nil {
		return false, err
	}
	if len(windows) > continuousMaxWindowCount {
		return true, errContinuousWindowLimit
	}
	if len(windows) == 0 {
		return false, nil
	}
	if !s.StorageConnected() {
		return true, errProfileUnavailable
	}
	agg.WindowCount += len(windows)
	byObject := map[string][]model.ProfileWindow{}
	objectOrder := []string{}
	for _, window := range windows {
		if window.ObjectKey == "" {
			continue
		}
		if _, ok := byObject[window.ObjectKey]; !ok {
			objectOrder = append(objectOrder, window.ObjectKey)
		}
		byObject[window.ObjectKey] = append(byObject[window.ObjectKey], window)
	}
	for _, objectKey := range objectOrder {
		// 阶段三：一个对象要么是旧分钟 JSON（单 batch），要么是 gzip 小时块
		// （多 batch）。块只加载解压一次，再按 DB 行选中的 batch_bid 关联。
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			return true, err
		}
		agg.ObjectKeys = append(agg.ObjectKeys, objectKey)
		batchByID := continuousBatchIndex(batches)
		seenBatch := map[string]bool{}
		for _, row := range byObject[objectKey] {
			batch, rowKey, ok := continuousResolveBatch(row, batches, batchByID)
			if !ok || seenBatch[rowKey] {
				continue
			}
			seenBatch[rowKey] = true
			for _, window := range batch.Windows {
				// 阶段六：按 window_start 归属小时（半开区间），避免小时边界
				// window 在相邻小时被重复聚合（旧逻辑 windowOverlaps 会双计）。
				if window.WindowStart.IsZero() || window.WindowStart.Before(from) || !window.WindowStart.Before(to) {
					continue
				}
				matched := make([]ContinuousStackSample, 0)
				relevantDSOs := map[string]bool{}
				for _, sample := range continuousProfileSamplesForQuery(window, q, agg.SeenProfileIDs) {
					if !continuousSampleMatches(sample, window.Labels, q.Filters) {
						continue
					}
					matched = append(matched, sample)
					if exe := strings.TrimSpace(continuousSampleLabel(sample, window.Labels, "exe")); exe != "" {
						relevantDSOs[exe] = true
					}
				}
				continuousAggregateSymbolMetadata(agg, window.SymbolRefs, relevantDSOs)
				continuousAggregateRuntimeMetadata(agg, window.SymbolRefs)
				for _, sample := range matched {
					continuousAddSample(agg, sample, window.Labels)
				}
			}
		}
	}
	return true, nil
}

// continuousAggregateSymbolMetadata only carries extraction state/reasons.
// Resolution status itself is derived from filtered frames below.
func continuousAggregateSymbolMetadata(agg *continuousAggregate, refs map[string]interface{}, relevantDSOs map[string]bool) {
	if len(refs) == 0 {
		return
	}
	native, _ := refs["native_go"].(map[string]interface{})
	collect := func(key string) int {
		values, _ := native[key].([]interface{})
		matched := 0
		for _, value := range values {
			item, _ := value.(map[string]interface{})
			dso, _ := item["dso"].(string)
			if !relevantDSOs[dso] {
				continue
			}
			matched++
			if reason, _ := item["reason"].(string); reason != "" && len(agg.SymbolReasons) < 5 {
				agg.SymbolReasons[reason] = true
			}
		}
		return matched
	}
	agg.GoSymbolReady = agg.GoSymbolReady || collect("ready") > 0
	agg.GoSymbolPending = agg.GoSymbolPending || collect("pending") > 0
	agg.GoSymbolFailed = agg.GoSymbolFailed || collect("failed") > 0
}

func continuousFinalizeSymbolStatus(agg *continuousAggregate) {
	switch {
	case agg.TotalFrameWeight == 0:
		agg.SymbolStatus = "not_applicable"
	case agg.UnresolvedFrameWeight == 0:
		agg.SymbolStatus = "complete"
	case agg.UnresolvedFrameWeight >= agg.TotalFrameWeight:
		agg.SymbolStatus = "missing"
	default:
		agg.SymbolStatus = "partial"
	}
}

func continuousSymbolDiagnostics(agg continuousAggregate) ProfileSymbolDiagnostics {
	state := "not_applicable"
	if agg.GoSymbolPending {
		state = "pending"
	} else if agg.GoSymbolFailed {
		state = "failed"
	} else if agg.GoSymbolReady {
		state = "ready"
	}
	reasons := make([]string, 0, len(agg.SymbolReasons))
	for reason := range agg.SymbolReasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	percent := 0.0
	if agg.TotalFrameWeight > 0 {
		percent = agg.UnresolvedFrameWeight * 100 / agg.TotalFrameWeight
	}
	return ProfileSymbolDiagnostics{
		TotalFrameWeight: agg.TotalFrameWeight, UnresolvedFrameWeight: agg.UnresolvedFrameWeight,
		ModuleUnresolvedFrameWeight: agg.ModuleUnresolvedFrameWeight,
		NoModuleFrameWeight:         agg.NoModuleFrameWeight,
		UnresolvedPercent:           percent, GoSymbolState: state, Reasons: reasons,
	}
}

func continuousRuntimeAccumulator(agg *continuousAggregate, runtimeName string) *runtimeDiagnosticAccumulator {
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	item := agg.RuntimeDiagnostics[runtimeName]
	if item == nil {
		item = &runtimeDiagnosticAccumulator{
			Modes: map[string]bool{}, Detected: map[string]ProfileRuntimeProcessDiagnostic{},
			Ready: map[string]ProfileRuntimeProcessDiagnostic{}, Missing: map[string]ProfileRuntimeProcessDiagnostic{},
			Reasons: map[string]bool{},
		}
		agg.RuntimeDiagnostics[runtimeName] = item
	}
	return item
}

func continuousAggregateRuntimeMetadata(agg *continuousAggregate, refs map[string]interface{}) {
	// 阶段四：优先读取 v2 language_status（diagnostics_version>=2）。
	continuousAggregateLanguageStatusV2(agg, refs)
	runtimeMaps, _ := refs["runtime_maps"].(map[string]interface{})
	for _, runtimeName := range []string{"java", "node", "python"} {
		if existing := agg.RuntimeDiagnostics[runtimeName]; existing != nil && existing.HasV2 {
			continue
		}
		raw, _ := runtimeMaps[runtimeName].(map[string]interface{})
		if len(raw) == 0 {
			continue
		}
		diag := continuousRuntimeAccumulator(agg, runtimeName)
		if ready, _ := raw["ready"].(bool); ready {
			diag.Modes["perf-map"] = true
		}
		reason, _ := raw["reason"].(string)
		if reason != "" {
			diag.Reasons[reason] = true
		}
		missing, _ := raw["missing"].([]interface{})
		for _, value := range missing {
			pid := int(numberAsFloat64(value))
			// 阶段三：runtime map 诊断只有裸 PID（无 start/exe），用
			// "pid|" 前缀键避免与 py-spy/Memray 的完整实例键冲突。
			key := "pid|" + strconv.Itoa(pid)
			process := ProfileRuntimeProcessDiagnostic{PID: pid, Mode: "perf-map", Status: "missing", Reason: reason}
			diag.Detected[key] = process
			diag.Missing[key] = process
		}
		readyPIDs, _ := raw["ready_pids"].([]interface{})
		for _, value := range readyPIDs {
			pid := int(numberAsFloat64(value))
			key := "pid|" + strconv.Itoa(pid)
			process := ProfileRuntimeProcessDiagnostic{PID: pid, Mode: "perf-map", Status: "ready"}
			diag.Detected[key] = process
			diag.Ready[key] = process
		}
	}
	fallback, _ := refs["python_fallback"].(map[string]interface{})
	// 阶段四修复：不再无条件创建 Python 诊断行——只有确实存在 py-spy 结果
	// （或 v2 已标记 detected）时才登记，杜绝"没有检测到 Python 也显示
	// Python 行"的虚假状态。
	fallbackReady, _ := fallback["ready"].([]interface{})
	fallbackFailed, _ := fallback["failed"].([]interface{})
	fallbackLimited := int(numberAsFloat64(fallback["limited_count"]))
	pythonHasSidecar := len(fallbackReady) > 0 || len(fallbackFailed) > 0 || fallbackLimited > 0
	var python *runtimeDiagnosticAccumulator
	getPython := func() *runtimeDiagnosticAccumulator {
		if python == nil {
			python = continuousRuntimeAccumulator(agg, "python")
		}
		return python
	}
	pythonAlreadyV2 := agg.RuntimeDiagnostics["python"] != nil && agg.RuntimeDiagnostics["python"].HasV2
	if pythonHasSidecar && !pythonAlreadyV2 {
		py := getPython()
		py.Limited += fallbackLimited
		for _, field := range []string{"ready", "failed"} {
			items, _ := fallback[field].([]interface{})
			for _, value := range items {
				item, _ := value.(map[string]interface{})
				pid := int(numberAsFloat64(item["pid"]))
				// 阶段三：py-spy 诊断用完整实例键（pid|start|exe）去重，PID
				// 复用显示为两个实例。
				startMs := int64(numberAsFloat64(item["process_start_ms"]))
				exe, _ := item["exe"].(string)
				key := "pid|" + strconv.Itoa(pid) + "|" + strconv.FormatInt(startMs, 10) + "|" + exe
				reason, _ := item["reason"].(string)
				status := "ready"
				if field == "failed" {
					status = "missing"
					if reason != "" {
						py.Reasons[reason] = true
					}
				}
				process := ProfileRuntimeProcessDiagnostic{PID: pid, ProcessStartMs: startMs, Exe: exe, Mode: "py-spy", Status: status, Reason: reason}
				py.Detected[key] = process
				py.Modes["py-spy"] = true
				if status == "ready" {
					py.Ready[key] = process
				} else {
					py.Missing[key] = process
				}
			}
		}
	}
	memory, _ := refs["python_memory"].(map[string]interface{})
	var memoryPython *runtimeDiagnosticAccumulator
	for _, field := range []string{"ready", "failed"} {
		items, _ := memory[field].([]interface{})
		for _, value := range items {
			item, _ := value.(map[string]interface{})
			if memoryPython == nil {
				memoryPython = continuousRuntimeAccumulator(agg, "python")
			}
			pid := int(numberAsFloat64(item["pid"]))
			// 阶段三：Memray 诊断用完整实例键去重。
			startMs := int64(numberAsFloat64(item["process_start_ms"]))
			exe, _ := item["exe"].(string)
			key := "memory|" + strconv.Itoa(pid) + "|" + strconv.FormatInt(startMs, 10) + "|" + exe
			reason, _ := item["reason"].(string)
			status := "ready"
			if field == "failed" {
				status = "missing"
				if reason != "" {
					memoryPython.Reasons[reason] = true
				}
			}
			process := ProfileRuntimeProcessDiagnostic{PID: pid, ProcessStartMs: startMs, Exe: exe, Mode: "memray", Status: status, Reason: reason}
			memoryPython.Detected[key] = process
			memoryPython.Modes["memray"] = true
			if status == "ready" {
				memoryPython.Ready[key] = process
			} else {
				memoryPython.Missing[key] = process
			}
		}
	}
}

// 阶段四：解析 Agent symbol_refs.language_status（v2 语言诊断契约）。
// 任一窗口携带 v2 时该语言的聚合优先采用 v2 口径；进程列表仍与旧字段并集。
func continuousAggregateLanguageStatusV2(agg *continuousAggregate, refs map[string]interface{}) {
	if len(refs) == 0 {
		return
	}
	version := int(numberAsFloat64(refs["diagnostics_version"]))
	if version < 2 {
		return
	}
	statusMap, _ := refs["language_status"].(map[string]interface{})
	for _, name := range []string{"go", "java", "node", "python", "native", "kernel"} {
		raw, _ := statusMap[name].(map[string]interface{})
		if len(raw) == 0 {
			continue
		}
		acc := continuousRuntimeAccumulator(agg, name)
		// 状态/进程/原因描述当前（最新）窗口；覆盖率权重仍在下方跨窗
		// 累加。这样 PID 重启或瞬态缺图恢复后不会被旧身份永久拖成 partial。
		acc.Modes = map[string]bool{}
		acc.Detected = map[string]ProfileRuntimeProcessDiagnostic{}
		acc.Ready = map[string]ProfileRuntimeProcessDiagnostic{}
		acc.Missing = map[string]ProfileRuntimeProcessDiagnostic{}
		acc.Reasons = map[string]bool{}
		detection, _ := raw["runtime_detection"].(string)
		collectorStatus, _ := raw["collector_status"].(string)
		symbolStatus, _ := raw["symbol_status"].(string)
		sampleCount := numberAsFloat64(raw["sample_count"])
		semanticPercent := numberAsFloat64(raw["semantic_frame_percent"])
		semanticSamplePercent, hasSemanticSamplePercent := raw["semantic_sample_percent"]
		semanticSamplePercentValue := numberAsFloat64(semanticSamplePercent)
		unresolvedPercent := numberAsFloat64(raw["unresolved_frame_percent"])
		targetModuleUnresolvedPercent := numberAsFloat64(raw["target_module_unresolved_percent"])

		if detection == "detected" || acc.RuntimeDetection == "" {
			acc.RuntimeDetection = detection
		} else if detection == "not_detected" && acc.RuntimeDetection == "unknown" {
			acc.RuntimeDetection = detection
		}
		if collectorStatus != "" {
			// 窗口按时间顺序聚合；采集状态采用最新窗口，避免一次瞬态
			// attach 失败永久覆盖后来已经恢复的 ready 状态。
			acc.CollectorStatus = collectorStatus
		}
		if symbolStatus != "" {
			// symbol_status 聚合取最差（complete>partial>missing>unknown）。
			rank := map[string]int{"": -1, "complete": 0, "partial": 1, "missing": 2, "unknown": 3, "not_applicable": -1}
			if rank[symbolStatus] > rank[acc.SymbolStatusV2] {
				acc.SymbolStatusV2 = symbolStatus
			}
		}
		if modes, ok := raw["collector_modes"].([]interface{}); ok {
			for _, mode := range modes {
				if text, ok := mode.(string); ok && text != "" {
					acc.Modes[text] = true
				}
			}
		}
		if reasons, ok := raw["reasons"].([]interface{}); ok {
			for _, reason := range reasons {
				if text, ok := reason.(string); ok && text != "" && len(acc.Reasons) < 8 {
					acc.Reasons[text] = true
				}
			}
		}
		if processes, ok := raw["processes"].([]interface{}); ok {
			for _, value := range processes {
				item, _ := value.(map[string]interface{})
				pid := int(numberAsFloat64(item["pid"]))
				startMs := int64(numberAsFloat64(item["process_start_ms"]))
				exe, _ := item["exe"].(string)
				mode, _ := item["mode"].(string)
				processStatus, _ := item["status"].(string)
				reason, _ := item["reason"].(string)
				key := "pid|" + strconv.Itoa(pid) + "|" + strconv.FormatInt(startMs, 10) + "|" + exe
				process := ProfileRuntimeProcessDiagnostic{PID: pid, ProcessStartMs: startMs, Exe: exe, Mode: mode, Status: processStatus, Reason: reason}
				acc.Detected[key] = process
				switch processStatus {
				case "ready":
					acc.Ready[key] = process
				case "missing":
					acc.Missing[key] = process
				case "failed", "pending":
					acc.Missing[key] = process
				}
			}
		}
		// v2 新产物直接累加原始权重，避免用 sample_count 加权帧百分比
		// 在平均栈深不同的窗口之间产生统计偏差。早期 v2 窗口没有原始
		// 权重时才回退到百分比×sample_count。
		frameWeight := numberAsFloat64(raw["frame_weight"])
		semanticFrameWeight := numberAsFloat64(raw["semantic_frame_weight"])
		unresolvedFrameWeight := numberAsFloat64(raw["unresolved_frame_weight"])
		semanticSampleWeight := numberAsFloat64(raw["semantic_sample_weight"])
		targetModuleFrameWeight := numberAsFloat64(raw["target_module_frame_weight"])
		targetModuleUnresolvedWeight := numberAsFloat64(raw["target_module_unresolved_frame_weight"])
		if frameWeight <= 0 && sampleCount > 0 {
			frameWeight = sampleCount
			semanticFrameWeight = semanticPercent * sampleCount / 100
			unresolvedFrameWeight = unresolvedPercent * sampleCount / 100
		}
		if semanticSampleWeight <= 0 && sampleCount > 0 {
			// 兼容阶段四早期 v2：当时只有帧覆盖率，没有样本覆盖率。
			// 仅在字段完全不存在时用旧口径近似；显式的 0 必须保留。
			if !hasSemanticSamplePercent {
				semanticSamplePercentValue = semanticPercent
			}
			semanticSampleWeight = semanticSamplePercentValue * sampleCount / 100
		}
		if targetModuleFrameWeight <= 0 && targetModuleUnresolvedPercent > 0 && sampleCount > 0 {
			targetModuleFrameWeight = sampleCount
			targetModuleUnresolvedWeight = targetModuleUnresolvedPercent * sampleCount / 100
		}
		acc.FrameWeight += frameWeight
		acc.SemanticFrameWeight += semanticFrameWeight
		acc.UnresolvedFrameWeight += unresolvedFrameWeight
		acc.SemanticSampleWeight += semanticSampleWeight
		acc.TargetModuleFrameWeight += targetModuleFrameWeight
		acc.TargetModuleUnresolvedWeight += targetModuleUnresolvedWeight
		acc.V2SampleCount += sampleCount
		acc.HasV2 = true
	}
}

func continuousRuntimeDiagnostics(agg continuousAggregate) map[string]ProfileRuntimeDiagnostic {
	out := map[string]ProfileRuntimeDiagnostic{}
	for _, runtimeName := range []string{"python", "java", "node", "go", "native", "kernel", "unknown"} {
		item := agg.RuntimeDiagnostics[runtimeName]
		if item == nil {
			continue
		}
		modes, reasons := boolMapKeys(item.Modes), boolMapKeys(item.Reasons)
		processes := make([]ProfileRuntimeProcessDiagnostic, 0, len(item.Detected))
		for _, process := range item.Detected {
			processes = append(processes, process)
		}
		sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
		if len(processes) > 20 {
			processes = processes[:20]
		}
		status := "missing"
		if len(item.Ready) > 0 && len(item.Missing) == 0 {
			status = "ready"
		} else if len(item.Ready) > 0 {
			status = "partial"
		}
		diag := ProfileRuntimeDiagnostic{
			Status: status, Modes: modes, DetectedCount: len(item.Detected), ReadyCount: len(item.Ready),
			MissingCount: len(item.Missing), LimitedCount: item.Limited, Reasons: reasons, Processes: processes,
		}
		// 阶段四：优先输出 v2 口径。原始帧/样本权重跨窗精确求和，
		// 避免用 sample_count 平均不同栈深的帧百分比。
		if item.HasV2 {
			if item.RuntimeDetection != "detected" && item.V2SampleCount <= 0 &&
				len(item.Detected) == 0 {
				// Agent 每窗会带全部语言的 not_detected 占位行；查询
				// API 不应把它们显示成虚假语言状态。
				continue
			}
			diag.DiagnosticsVersion = 2
			if item.RuntimeDetection != "" {
				diag.RuntimeDetection = item.RuntimeDetection
			}
			diag.CollectorStatus = item.CollectorStatus
			if item.FrameWeight > 0 {
				diag.SemanticFramePercent = round2(item.SemanticFrameWeight * 100 / item.FrameWeight)
				diag.UnresolvedFramePercent = round2(item.UnresolvedFrameWeight * 100 / item.FrameWeight)
				diag.SymbolStatusV2 = symbolStatusForPercent(diag.UnresolvedFramePercent)
			} else if item.SymbolStatusV2 != "" {
				diag.SymbolStatusV2 = item.SymbolStatusV2
			}
			if item.V2SampleCount > 0 {
				diag.SemanticSamplePercent = round2(item.SemanticSampleWeight * 100 / item.V2SampleCount)
			}
			if item.TargetModuleFrameWeight > 0 {
				diag.TargetModuleFrameWeight = item.TargetModuleFrameWeight
				diag.TargetModuleUnresolvedPercent = round2(
					item.TargetModuleUnresolvedWeight * 100 / item.TargetModuleFrameWeight)
			}
			diag.SampleCount = item.V2SampleCount

			qualityReady := diag.SemanticSamplePercent >= 70 && diag.UnresolvedFramePercent <= 20
			if runtimeName == "native" && item.TargetModuleFrameWeight > 0 {
				qualityReady = diag.SemanticSamplePercent >= 70 &&
					diag.TargetModuleUnresolvedPercent < 5
			}
			if item.V2SampleCount > 0 {
				switch {
				case qualityReady && item.CollectorStatus != "failed" && len(item.Missing) == 0:
					diag.CollectorStatus = "ready"
				case item.CollectorStatus == "failed" && len(item.Ready) == 0:
					diag.CollectorStatus = "failed"
				case item.CollectorStatus == "missing" && len(item.Ready) == 0:
					// 没有任何 ready 采集进程时，偶然解析出的 runtime/native
					// 帧不能把“缺采集能力”抬升为 partial。
					diag.CollectorStatus = "missing"
				default:
					diag.CollectorStatus = "partial"
				}
			}
			diag.Status = v2CollectorToLegacyStatus(diag.CollectorStatus)
		} else if len(item.Detected) == 0 && len(modes) == 0 && len(reasons) == 0 &&
			item.Limited == 0 {
			// 阶段四修复：没有检测到任何进程、没有任何模式与原因时不生成
			// 虚假语言行（历史问题：每个窗口都显示 python/java/node 行）。
			continue
		}
		out[runtimeName] = diag
	}
	return out
}

// v2 collector_status → 旧 status 字段映射（一个兼容周期内保持旧字段可用）。
func v2CollectorToLegacyStatus(collector string) string {
	switch collector {
	case "ready":
		return "ready"
	case "partial", "pending":
		return "partial"
	case "missing", "failed", "not_applicable":
		return "missing"
	default:
		return "missing"
	}
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}

func symbolStatusForPercent(unresolved float64) string {
	switch {
	case unresolved <= 5:
		return "complete"
	case unresolved >= 100:
		return "missing"
	default:
		return "partial"
	}
}

func boolMapKeys(values map[string]bool) []string {
	out := []string{}
	for value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func numberAsFloat64(value interface{}) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case int:
		return float64(number)
	case json.Number:
		parsed, _ := number.Float64()
		return parsed
	}
	return 0
}

func (s *APIServer) QueryContinuousHistogram(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	// 阶段三：histogram 支持实例过滤（pid/process_start_ms/process_instance/
	// exe），与 CPU 查询共用同一过滤语义。strict CO-RE 直方图带完整进程身份
	// 可按实例查询；degraded 无法安全归属的直方图（pid=0）在查询层被排除。
	signalType := strings.ToLower(strings.TrimSpace(c.Query("signal_type")))
	if signalType == "" {
		signalType = strings.ToLower(strings.TrimSpace(c.Query("profile_type")))
	}
	if signalType != "io_latency" && signalType != "io_syscall_latency" && signalType != "sched_latency" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "signal_type 仅支持 io_latency/io_syscall_latency/sched_latency")
		return
	}
	data, found, err := s.pqQueryHistogramMixed(c.Request.Context(), q, signalType)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		stats := s.pqQueryStatsFor(c.Request.Context(), q)
		s.RespondOK(c, gin.H{
			"query":          profileLabelSelector(q),
			"signal_type":    signalType,
			"empty":          true,
			"message":        "Native Continuous eBPF 暂无覆盖该时间范围的 histogram window",
			"source":         "mini-drop-native",
			"generated_at":   time.Now(),
			"buckets":        []ContinuousHistogramBucket{},
			"trend":          []gin.H{},
			"storage_source": stats.StorageSource, "resolution_seconds": stats.ResolutionSeconds,
			"mixed_resolution": stats.MixedResolution, "earliest_available_at": stats.EarliestAvailable,
		})
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) queryNativeContinuousHistogram(ctx context.Context, q ProfileQuery, signalType string) (gin.H, bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	err := s.DB.Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", signalType).
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Order("window_start ASC").
		Limit(continuousMaxWindowCount + 1).
		Find(&windows).Error
	if err != nil {
		return nil, false, err
	}
	if len(windows) > continuousMaxWindowCount {
		return nil, true, errContinuousWindowLimit
	}
	if len(windows) == 0 {
		return nil, false, nil
	}
	if !s.StorageConnected() {
		return nil, true, errProfileUnavailable
	}

	type bucketAgg struct {
		Range string
		Low   float64
		High  float64
		Count uint64
	}
	merged := map[string]*bucketAgg{}
	trend := []gin.H{}
	backends := map[string]bool{}
	var totalEvents uint64
	var unavailableReason string
	objectKeys := []string{}
	seenObject := map[string]bool{}

	objectOrder, byObject := continuousGroupWindowsByObject(windows)
	for _, objectKey := range objectOrder {
		// 阶段三：块只解压一次，再按 DB 行选中的 batch 关联
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			return nil, true, err
		}
		if !seenObject[objectKey] {
			objectKeys = append(objectKeys, objectKey)
			seenObject[objectKey] = true
		}
		batchByID := continuousBatchIndex(batches)
		seenBatch := map[string]bool{}
		for _, row := range byObject[objectKey] {
			batch, rowKey, ok := continuousResolveBatch(row, batches, batchByID)
			if !ok || seenBatch[rowKey] {
				continue
			}
			seenBatch[rowKey] = true
			for _, window := range batch.Windows {
				if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
					continue
				}
				for _, hist := range window.Histograms {
					if strings.ToLower(strings.TrimSpace(hist.SignalType)) != signalType {
						continue
					}
					// 阶段三：实例过滤。strict CO-RE 直方图带完整进程身份
					// 可按实例查询；pid=0 的整机直方图（degraded 无法安全
					// 归属）在实例过滤查询中被排除，不静默混入。
					if !continuousHistogramMatchesFilters(hist, q.Filters) {
						continue
					}
					if hist.Backend != "" {
						backends[hist.Backend] = true
					}
					if hist.Unavailable && unavailableReason == "" {
						unavailableReason = hist.Reason
					}
					totalEvents = addContinuousCount(totalEvents, hist.EventCount)
					for _, bucket := range hist.Buckets {
						key := bucket.Range + "|" + strconv.FormatFloat(bucket.Low, 'f', -1, 64) + "|" + strconv.FormatFloat(bucket.High, 'f', -1, 64)
						item := merged[key]
						if item == nil {
							item = &bucketAgg{Range: bucket.Range, Low: bucket.Low, High: bucket.High}
							merged[key] = item
						}
						item.Count = addContinuousCount(item.Count, bucket.Count)
					}
					trend = append(trend, gin.H{
						"window_start": window.WindowStart,
						"window_end":   window.WindowEnd,
						"event_count":  hist.EventCount,
						"p50":          hist.Summary.P50,
						"p95":          hist.Summary.P95,
						"p99":          hist.Summary.P99,
						"backend":      hist.Backend,
						"unavailable":  hist.Unavailable,
						"reason":       hist.Reason,
					})
				}
			}
		}
	}
	buckets := make([]ContinuousHistogramBucket, 0, len(merged))
	for _, item := range merged {
		buckets = append(buckets, ContinuousHistogramBucket{Range: item.Range, Low: item.Low, High: item.High, Count: item.Count})
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Low == buckets[j].Low {
			return buckets[i].High < buckets[j].High
		}
		return buckets[i].Low < buckets[j].Low
	})
	summary := summarizeContinuousBuckets(buckets)
	backendList := make([]string, 0, len(backends))
	for backend := range backends {
		backendList = append(backendList, backend)
	}
	sort.Strings(backendList)
	empty := len(buckets) == 0 || totalEvents == 0
	message := ""
	if empty {
		message = firstNonEmpty(unavailableReason, "Native Continuous eBPF 暂无 histogram 样本")
	} else if unavailableReason != "" {
		message = "部分窗口不可用: " + unavailableReason
	}
	return gin.H{
		"query":           profileLabelSelector(q),
		"signal_type":     signalType,
		"buckets":         buckets,
		"summary":         summary,
		"trend":           trend,
		"event_count":     totalEvents,
		"unit":            "us",
		"backend":         strings.Join(backendList, ","),
		"backends":        backendList,
		"empty":           empty,
		"message":         message,
		"source":          "mini-drop-native",
		"profile_url":     s.continuousProfileURL(ctx, q, objectKeys),
		"raw_profile_url": s.continuousRawProfileURL(ctx, objectKeys),
		"generated_at":    time.Now(),
	}, true, nil
}

// QueryContinuousDBSnapshot 返回时间范围内的数据库快照：慢查询 digest 排行
// （按窗口累加后按总耗时排序）与锁等待链（逐条保留，因为"谁在等谁"是时点
// 事实，聚合会丢掉关键信息）。
func (s *APIServer) QueryContinuousDBSnapshot(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	data, found, err := s.pqQueryDBMixed(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		stats := s.pqQueryStatsFor(c.Request.Context(), q)
		s.RespondOK(c, gin.H{
			"query":          profileLabelSelector(q),
			"signal_type":    "db_snapshot",
			"empty":          true,
			"message":        "该时间范围暂无数据库快照数据",
			"source":         "mini-drop-native",
			"generated_at":   time.Now(),
			"digests":        []gin.H{},
			"lock_waits":     []gin.H{},
			"storage_source": stats.StorageSource, "resolution_seconds": stats.ResolutionSeconds,
			"mixed_resolution": stats.MixedResolution, "earliest_available_at": stats.EarliestAvailable,
		})
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) queryNativeContinuousDBSnapshot(ctx context.Context, q ProfileQuery) (gin.H, bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	err := s.DB.Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", "db_snapshot").
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Order("window_start ASC").
		Limit(continuousMaxWindowCount + 1).
		Find(&windows).Error
	if err != nil {
		return nil, false, err
	}
	if len(windows) > continuousMaxWindowCount {
		return nil, true, errContinuousWindowLimit
	}
	if len(windows) == 0 {
		return nil, false, nil
	}
	if !s.StorageConnected() {
		return nil, true, errProfileUnavailable
	}

	type digestAgg struct {
		InstanceLabel  string
		SchemaName     string
		DigestText     string
		CallCount      uint64
		TotalLatencyUs uint64
		RowsExamined   uint64
	}
	digests := map[string]*digestAgg{}
	lockWaits := []gin.H{}
	objectKeys := []string{}
	seenObject := map[string]bool{}

	objectOrder, byObject := continuousGroupWindowsByObject(windows)
	for _, objectKey := range objectOrder {
		// 阶段三：块只解压一次，再按 DB 行选中的 batch 关联
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			return nil, true, err
		}
		if !seenObject[objectKey] {
			objectKeys = append(objectKeys, objectKey)
			seenObject[objectKey] = true
		}
		batchByID := continuousBatchIndex(batches)
		seenBatch := map[string]bool{}
		for _, row := range byObject[objectKey] {
			batch, rowKey, ok := continuousResolveBatch(row, batches, batchByID)
			if !ok || seenBatch[rowKey] {
				continue
			}
			seenBatch[rowKey] = true
			for _, window := range batch.Windows {
				if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
					continue
				}
				for _, snap := range window.DBSnapshots {
					switch snap.Kind {
					case "digest":
						key := snap.InstanceLabel + "|" + snap.SchemaName + "|" + snap.DigestText
						item := digests[key]
						if item == nil {
							item = &digestAgg{
								InstanceLabel: snap.InstanceLabel,
								SchemaName:    snap.SchemaName,
								DigestText:    snap.DigestText,
							}
							digests[key] = item
						}
						item.CallCount = addContinuousCount(item.CallCount, snap.CallCount)
						item.TotalLatencyUs = addContinuousCount(item.TotalLatencyUs, snap.TotalLatencyUs)
						item.RowsExamined = addContinuousCount(item.RowsExamined, snap.RowsExaminedTotal)
					case "lock_wait":
						lockWaits = append(lockWaits, gin.H{
							"instance_label": snap.InstanceLabel,
							"timestamp":      snap.Timestamp,
							"waiting_pid":    snap.WaitingPID,
							"waiting_query":  snap.WaitingQuery,
							"blocking_pid":   snap.BlockingPID,
							"blocking_query": snap.BlockingQuery,
							"wait_seconds":   snap.WaitSeconds,
							"locked_table":   snap.LockedTable,
						})
					}
				}
			}
		}
	}

	digestList := make([]gin.H, 0, len(digests))
	for _, item := range digests {
		avgLatencyUs := uint64(0)
		if item.CallCount > 0 {
			avgLatencyUs = item.TotalLatencyUs / item.CallCount
		}
		digestList = append(digestList, gin.H{
			"instance_label":   item.InstanceLabel,
			"schema_name":      item.SchemaName,
			"digest_text":      item.DigestText,
			"call_count":       item.CallCount,
			"total_latency_us": item.TotalLatencyUs,
			"avg_latency_us":   avgLatencyUs,
			"rows_examined":    item.RowsExamined,
		})
	}
	sort.Slice(digestList, func(i, j int) bool {
		return digestList[i]["total_latency_us"].(uint64) > digestList[j]["total_latency_us"].(uint64)
	})
	if len(digestList) > continuousSummaryTopLimit {
		digestList = digestList[:continuousSummaryTopLimit]
	}
	sort.Slice(lockWaits, func(i, j int) bool {
		return lockWaits[i]["wait_seconds"].(uint64) > lockWaits[j]["wait_seconds"].(uint64)
	})

	empty := len(digestList) == 0 && len(lockWaits) == 0
	message := ""
	if empty {
		message = "该时间范围暂无数据库快照数据"
	}
	return gin.H{
		"query":        profileLabelSelector(q),
		"signal_type":  "db_snapshot",
		"digests":      digestList,
		"lock_waits":   lockWaits,
		"empty":        empty,
		"message":      message,
		"source":       "mini-drop-native",
		"profile_url":  s.continuousProfileURL(ctx, q, objectKeys),
		"generated_at": time.Now(),
	}, true, nil
}

func (s *APIServer) loadContinuousStoredBatch(ctx context.Context, objectKey string) (continuousStoredBatch, error) {
	rc, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, objectKey)
	if err != nil {
		return continuousStoredBatch{}, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if err != nil {
		return continuousStoredBatch{}, err
	}
	var batch continuousStoredBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return continuousStoredBatch{}, err
	}
	return batch, nil
}

func continuousSampleMatches(sample ContinuousStackSample, windowLabels map[string]interface{}, filters map[string]interface{}) bool {
	for _, key := range []string{"comm", "pid", "process_start_ms", "exe", "runtime"} {
		want := labelString(filters, key)
		if want == "" {
			continue
		}
		if continuousSampleLabel(sample, windowLabels, key) != want {
			return false
		}
	}
	return true
}

// 阶段三：histogram 实例过滤（与 CPU 查询共用同一过滤语义）。strict CO-RE
// 直方图带完整进程身份可按实例查询；pid=0 的整机直方图（degraded 无法安全
// 归属）在实例过滤查询中被排除，不静默混入。
func continuousHistogramMatchesFilters(hist ContinuousHistogramIngest, filters map[string]interface{}) bool {
	if len(filters) == 0 {
		return true
	}
	// 实例过滤：pid/process_start_ms/process_instance/exe。
	if want := labelString(filters, "pid"); want != "" {
		if hist.PID <= 0 || strconv.Itoa(hist.PID) != want {
			return false
		}
	}
	if want := labelString(filters, "process_start_ms"); want != "" {
		if hist.ProcessStartMs <= 0 || strconv.FormatInt(hist.ProcessStartMs, 10) != want {
			return false
		}
	}
	if want := labelString(filters, "process_instance"); want != "" {
		if hist.PID <= 0 || hist.ProcessStartMs <= 0 ||
			strconv.Itoa(hist.PID)+"|"+strconv.FormatInt(hist.ProcessStartMs, 10) != want {
			return false
		}
	}
	if want := labelString(filters, "exe"); want != "" {
		if hist.Exe == "" || hist.Exe != want {
			return false
		}
	}
	return true
}

func continuousProfileSamplesForQuery(window ContinuousWindowIngest, q ProfileQuery, seenProfileIDs map[string]bool) []ContinuousStackSample {
	out := []ContinuousStackSample{}
	wantedSignal := "cpu_profile"
	if q.ProfileType == "memory" {
		wantedSignal = "python_memory"
	} else {
		out = append(out, window.Samples...)
	}
	scope := strings.ToLower(strings.TrimSpace(q.StackScope))
	if scope == "all" {
		scope = ""
	}
	for _, profile := range window.Profiles {
		if strings.ToLower(strings.TrimSpace(firstNonEmpty(profile.SignalType, "cpu_profile"))) != wantedSignal {
			continue
		}
		if profile.ProfileID != "" && seenProfileIDs != nil {
			// 阶段七：profile 去重键 = profile_id + 完整进程身份。Memray
			// profile_id 是 "memray-<namespacePid>-<startTicks>"，namespace
			// PID 跨容器可能相同（容器内都是 1），仅用 profile_id 会把不同
			// 容器的两个 profile 误判为重复；加上 pid/start/exe 后与
			// process Session 的实例归属语义一致。
			if continuousProfileSeen(seenProfileIDs, profile.ProfileID, profile.Samples) {
				continue
			}
		}
		profileScope := strings.ToLower(strings.TrimSpace(profile.StackScope))
		if scope != "" && profileScope != "" && profileScope != scope {
			continue
		}
		for _, sample := range profile.Samples {
			if sample.StackScope == "" {
				sample.StackScope = profile.StackScope
			}
			if sample.Backend == "" {
				sample.Backend = profile.Backend
			}
			if sample.Labels == nil {
				sample.Labels = profile.Labels
			}
			if sample.Runtime == "" && wantedSignal == "python_memory" {
				sample.Runtime = "python"
			}
			if sample.ProfileID == "" {
				sample.ProfileID = profile.ProfileID
			}
			out = append(out, sample)
		}
	}
	return out
}

// continuousProfileSeen 按 (profile_id + pid + process_start_ms + exe) 判断
// profile 是否已消费并登记。profile 内多个样本共享同一身份时只登记一次；
// 无进程身份的 profile（pid=0）退化为仅按 profile_id 去重。
func continuousProfileSeen(seen map[string]bool, profileID string, samples []ContinuousStackSample) bool {
	key := profileID
	for _, sample := range samples {
		if sample.PID > 0 {
			key = profileID + "|" + strconv.Itoa(sample.PID) + "|" + strconv.FormatInt(sample.ProcessStartMs, 10) + "|" + sample.Exe
			break
		}
	}
	if seen[key] {
		return true
	}
	seen[key] = true
	return false
}

func continuousSampleLabel(sample ContinuousStackSample, windowLabels map[string]interface{}, key string) string {
	if key == "process_instance" {
		if sample.PID > 0 && sample.ProcessStartMs > 0 {
			return strconv.Itoa(sample.PID) + "|" + strconv.FormatInt(sample.ProcessStartMs, 10)
		}
		return ""
	}
	if value := labelString(sample.Labels, key); value != "" {
		return value
	}
	switch key {
	case "comm":
		if sample.Comm != "" {
			return sample.Comm
		}
	case "pid":
		if sample.PID > 0 {
			return strconv.Itoa(sample.PID)
		}
	case "process_start_ms":
		if sample.ProcessStartMs > 0 {
			return strconv.FormatInt(sample.ProcessStartMs, 10)
		}
	case "exe":
		if sample.Exe != "" {
			return sample.Exe
		}
	case "runtime":
		if sample.Runtime != "" {
			return sample.Runtime
		}
	}
	return labelString(windowLabels, key)
}

func continuousAddSample(agg *continuousAggregate, sample ContinuousStackSample, windowLabels map[string]interface{}) {
	if !continuousSampleLooksValid(sample) {
		return
	}
	count := float64(sample.Count)
	if count <= 0 {
		count = 1
	}
	stack := continuousSampleStack(sample)
	if len(stack) == 0 {
		stack = []string{firstNonEmpty(continuousSampleLabel(sample, windowLabels, "comm"), continuousSampleLabel(sample, windowLabels, "exe"), "unknown")}
	}
	agg.Total += count
	runtimeName := firstNonEmpty(continuousSampleLabel(sample, windowLabels, "runtime"), "unknown")
	diag := continuousRuntimeAccumulator(agg, runtimeName)
	// v2 language_status is the authoritative collector-capability contract.
	// A generic perf sample only proves that the process was sampled; it does not
	// prove that the runtime-specific collector (for example Node's JIT map) is
	// ready.  Adding a synthetic perf_rolling/ready row here used to turn an
	// explicit "missing --perf-basic-prof" state into the misleading "partial".
	// Keep the sample-derived fallback only for historical v1 windows.
	if !diag.HasV2 {
		processKey := strconv.Itoa(sample.PID) + "|" + sample.Exe
		process := ProfileRuntimeProcessDiagnostic{PID: sample.PID, Comm: sample.Comm, Exe: sample.Exe, Mode: sample.Backend, Status: "ready"}
		diag.Detected[processKey] = process
		diag.Ready[processKey] = process
		if sample.Backend != "" {
			diag.Modes[sample.Backend] = true
		}
	}
	for _, key := range []string{"comm", "pid", "process_start_ms", "process_instance", "exe", "runtime"} {
		if value := continuousSampleLabel(sample, windowLabels, key); value != "" {
			if agg.LabelValue[key] == nil {
				agg.LabelValue[key] = map[string]bool{}
			}
			agg.LabelValue[key][value] = true
		}
	}
	if backend := strings.TrimSpace(sample.Backend); backend != "" {
		agg.Backends[backend] = true
	}
	for i, frame := range stack {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			frame = "unknown"
		}
		agg.TotalFrameWeight += count
		if continuousFrameLooksUnresolved(frame) {
			agg.UnresolvedFrameWeight += count
			if continuousUnresolvedFrameModule(frame) != "" {
				agg.ModuleUnresolvedFrameWeight += count
			} else {
				agg.NoModuleFrameWeight += count
			}
		}
		topKey, displayName, unresolved := continuousTopFrameKey(frame)
		item := agg.Top[topKey]
		if item == nil {
			item = &ProfileTopItem{Name: topKey, DisplayName: displayName, Unresolved: unresolved, Unit: firstNonEmpty(agg.Unit, "samples")}
			agg.Top[topKey] = item
		}
		item.Value += count
		if i == len(stack)-1 {
			item.Self += count
		}
	}
	node := agg.Root
	node.Value += count
	for i, frame := range stack {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			frame = "unknown"
		}
		if node.Children == nil {
			node.Children = map[string]*continuousTreeNode{}
		}
		child := node.Children[frame]
		if child == nil {
			child = &continuousTreeNode{Name: frame, Children: map[string]*continuousTreeNode{}}
			node.Children[frame] = child
			node.Order = append(node.Order, child)
		}
		child.Value += count
		if i == len(stack)-1 {
			child.Self += count
		}
		node = child
	}
}

func continuousFrameLooksUnresolved(frame string) bool {
	frame = strings.TrimSpace(frame)
	lower := strings.ToLower(frame)
	if lower == "" || lower == "unknown" || lower == "[unknown]" {
		return true
	}
	if strings.HasPrefix(frame, "[") && strings.HasSuffix(frame, "]") {
		return true
	}
	address := lower
	if strings.HasPrefix(address, "0x") {
		address = address[2:]
	}
	hexLen := 0
	for _, c := range address {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			hexLen++
			continue
		}
		break
	}
	return hexLen >= 6 && (hexLen == len(address) || address[hexLen] == ' ' || address[hexLen] == '\t')
}

func continuousTopFrameKey(frame string) (string, string, bool) {
	frame = strings.TrimSpace(frame)
	if frame == "" {
		frame = "unknown"
	}
	if !continuousFrameLooksUnresolved(frame) {
		return frame, frame, false
	}
	// 未解析帧统一保留原始 frame（含地址），不再按模块折叠丢失地址——
	// 模块已知的 "0x<addr> [module]" 与模块未知的裸地址 "0x<addr>" 都各自
	// 成条，方便事后用 addr2line/objdump 核对具体是哪个地址没解析。两类
	// 的成因和修法不同，聚合到一起反而会掩盖差异。
	display := "[未解析] " + frame
	return frame, display, true
}

func continuousUnresolvedFrameModule(frame string) string {
	frame = strings.TrimSpace(frame)
	start := strings.LastIndex(frame, "[")
	end := strings.LastIndex(frame, "]")
	if start >= 0 && end > start {
		module := strings.TrimSpace(frame[start+1 : end])
		if module != "" && strings.ToLower(module) != "unknown" {
			return module
		}
	}
	return ""
}

func continuousFinalizeTopItem(item *ProfileTopItem, total float64) {
	if item == nil {
		return
	}
	if item.DisplayName == "" {
		item.DisplayName = item.Name
	}
	if total > 0 {
		item.Percent = minFloat(item.Value, total) / total * 100
		item.SelfPercent = item.Self / total * 100
	}
}

func continuousBackendList(backends map[string]bool) string {
	if len(backends) == 0 {
		return ""
	}
	out := make([]string, 0, len(backends))
	for backend := range backends {
		if strings.TrimSpace(backend) != "" {
			out = append(out, backend)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func continuousSampleLooksValid(sample ContinuousStackSample) bool {
	if sample.Count > continuousMaxReasonableProfileSampleCount {
		return false
	}
	for _, frame := range continuousSampleStack(sample) {
		if continuousFrameLooksInvalid(frame) {
			return false
		}
	}
	if continuousFrameLooksInvalid(sample.StackString) {
		return false
	}
	return true
}

func continuousFrameLooksInvalid(frame string) bool {
	frame = strings.TrimSpace(frame)
	if frame == "" {
		return false
	}
	lower := strings.ToLower(frame)
	return strings.HasPrefix(frame, "ERROR:") ||
		strings.HasPrefix(lower, "stdin:") ||
		strings.Contains(lower, "failed to look up stack id") ||
		strings.Contains(lower, "unknown error")
}

func continuousSampleStack(sample ContinuousStackSample) []string {
	if len(sample.Stack) > 0 {
		out := make([]string, 0, len(sample.Stack))
		for _, frame := range sample.Stack {
			if strings.TrimSpace(frame) != "" {
				out = append(out, strings.TrimSpace(frame))
			}
		}
		return out
	}
	if sample.StackString == "" {
		return nil
	}
	parts := strings.Split(sample.StackString, ";")
	out := make([]string, 0, len(parts))
	for _, frame := range parts {
		if strings.TrimSpace(frame) != "" {
			out = append(out, strings.TrimSpace(frame))
		}
	}
	return out
}

func continuousTreeToProfileNodes(root *continuousTreeNode, prefix string) []ProfileNode {
	if root == nil {
		return []ProfileNode{}
	}
	children := append([]*continuousTreeNode(nil), root.Order...)
	sort.Slice(children, func(i, j int) bool {
		if children[i].Value == children[j].Value {
			return children[i].Name < children[j].Name
		}
		return children[i].Value > children[j].Value
	})
	out := make([]ProfileNode, 0, len(children))
	for idx, child := range children {
		id := strconv.Itoa(idx)
		if prefix != "" {
			id = prefix + "." + id
		}
		out = append(out, ProfileNode{
			ID:       id,
			Name:     child.Name,
			Value:    child.Value,
			Self:     child.Self,
			Children: continuousTreeToProfileNodes(child, id),
		})
	}
	return out
}

// continuousTreeToProfileNodesTruncated 带节点数上限的火焰图构建。
// 借鉴 Pyroscope maxNodes 查询保护：超过上限时按值排序截断，并返回 truncated=true。
// diffContinuousTreeNode 递归对齐 base/compare 两棵 continuousTreeNode，
// 按 (父节点路径, frame 名) 配对——名字相同的子节点在同一层配成一对递归
// 下去，只在一侧出现的子节点另一侧按 nil 处理（值为 0，纯新增/纯消失）。
// Value 用 inclusive，和普通火焰图口径一致；孩子按 max(base,compare) 值
// 降序排（不是按 delta 排），这样渲染出来的树形状还是符合"宽的分支排
// 前面"的火焰图直觉，delta 只用来着色，不用来决定布局顺序。
func diffContinuousTreeNode(name string, base, compare *continuousTreeNode) ProfileDiffNode {
	var baseValue, compareValue float64
	if base != nil {
		baseValue = base.Value
	}
	if compare != nil {
		compareValue = compare.Value
	}
	node := ProfileDiffNode{
		Name:         name,
		BaseValue:    baseValue,
		CompareValue: compareValue,
		Delta:        compareValue - baseValue,
	}
	switch {
	case baseValue > 0:
		node.DeltaPercent = node.Delta / baseValue * 100
	case compareValue > 0:
		node.DeltaPercent = 100
	}

	seen := map[string]bool{}
	names := make([]string, 0)
	if base != nil {
		for _, c := range base.Order {
			if !seen[c.Name] {
				seen[c.Name] = true
				names = append(names, c.Name)
			}
		}
	}
	if compare != nil {
		for _, c := range compare.Order {
			if !seen[c.Name] {
				seen[c.Name] = true
				names = append(names, c.Name)
			}
		}
	}
	if len(names) == 0 {
		return node
	}

	children := make([]ProfileDiffNode, 0, len(names))
	for _, childName := range names {
		var baseChild, compareChild *continuousTreeNode
		if base != nil {
			baseChild = base.Children[childName]
		}
		if compare != nil {
			compareChild = compare.Children[childName]
		}
		children = append(children, diffContinuousTreeNode(childName, baseChild, compareChild))
	}
	sort.Slice(children, func(i, j int) bool {
		wi := maxFloat(children[i].BaseValue, children[i].CompareValue)
		wj := maxFloat(children[j].BaseValue, children[j].CompareValue)
		if wi == wj {
			return children[i].Name < children[j].Name
		}
		return wi > wj
	})
	node.Children = children
	return node
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// truncateDiffTree 和 continuousTreeToProfileNodesTruncated 用一样的策略：
// 总节点数超过 maxNodes 才截断，DFS 预算用完就停——因为孩子已经按权重
// 降序排过，DFS 优先顺序天然保留权重最大的分支。
func truncateDiffTree(root ProfileDiffNode, maxNodes int) (ProfileDiffNode, bool) {
	if maxNodes <= 0 {
		maxNodes = continuousDefaultMaxNodes
	}
	if countDiffNodes(root) <= maxNodes {
		return root, false
	}
	remaining := maxNodes
	return truncateDiffTreeBudget(root, &remaining), true
}

func countDiffNodes(node ProfileDiffNode) int {
	count := len(node.Children)
	for _, child := range node.Children {
		count += countDiffNodes(child)
	}
	return count
}

func truncateDiffTreeBudget(node ProfileDiffNode, remaining *int) ProfileDiffNode {
	if remaining == nil || *remaining <= 0 || len(node.Children) == 0 {
		node.Children = nil
		return node
	}
	kept := make([]ProfileDiffNode, 0, len(node.Children))
	for _, child := range node.Children {
		if *remaining <= 0 {
			break
		}
		*remaining--
		kept = append(kept, truncateDiffTreeBudget(child, remaining))
	}
	node.Children = kept
	return node
}

func continuousTreeToProfileNodesTruncated(root *continuousTreeNode, prefix string, maxNodes int) ([]ProfileNode, bool) {
	if root == nil {
		return []ProfileNode{}, false
	}
	if maxNodes <= 0 {
		maxNodes = continuousDefaultMaxNodes
	}
	count := countTreeNodes(root)
	truncated := count > maxNodes
	if !truncated {
		return continuousTreeToProfileNodes(root, prefix), false
	}
	remaining := maxNodes
	nodes := continuousTreeToProfileNodesBudget(root, prefix, &remaining)
	return nodes, true
}

func continuousTreeToProfileNodesBudget(root *continuousTreeNode, prefix string, remaining *int) []ProfileNode {
	if root == nil || remaining == nil || *remaining <= 0 {
		return []ProfileNode{}
	}
	children := append([]*continuousTreeNode(nil), root.Order...)
	sort.Slice(children, func(i, j int) bool {
		if children[i].Value == children[j].Value {
			return children[i].Name < children[j].Name
		}
		return children[i].Value > children[j].Value
	})
	out := make([]ProfileNode, 0, len(children))
	for idx, child := range children {
		if *remaining <= 0 {
			break
		}
		id := strconv.Itoa(idx)
		if prefix != "" {
			id = prefix + "." + id
		}
		*remaining = *remaining - 1
		node := ProfileNode{
			ID:    id,
			Name:  child.Name,
			Value: child.Value,
			Self:  child.Self,
		}
		node.Children = continuousTreeToProfileNodesBudget(child, id, remaining)
		out = append(out, node)
	}
	return out
}

func countTreeNodes(node *continuousTreeNode) int {
	if node == nil {
		return 0
	}
	count := len(node.Order)
	for _, child := range node.Order {
		count += countTreeNodes(child)
	}
	return count
}

func (s *APIServer) continuousProfileURL(ctx context.Context, q ProfileQuery, objectKeys []string) string {
	base := "/profiles"
	if q.SessionSID != "" {
		base = "/continuous/sessions/" + url.PathEscape(q.SessionSID)
	} else if sid := continuousSessionSIDFromObjectKeys(objectKeys); sid != "" && q.TargetID == "" {
		// Legacy callers may omit target_id. A single-session object is still a
		// valid detail-page fallback; multi-session aggregate links stay on the
		// generic visual profile page below.
		base = "/continuous/sessions/" + url.PathEscape(sid)
	}
	values := url.Values{}
	if q.SessionSID == "" && q.TargetID != "" {
		values.Set("target_id", q.TargetID)
	}
	if !q.From.IsZero() {
		values.Set("from", q.From.Format(time.RFC3339Nano))
	}
	if !q.To.IsZero() {
		values.Set("to", q.To.Format(time.RFC3339Nano))
	}
	if q.ProfileType != "" && q.ProfileType != "cpu" {
		values.Set("profile_type", q.ProfileType)
	}
	if q.StackScope != "" {
		values.Set("stack_scope", q.StackScope)
	}
	if len(q.Filters) > 0 {
		if encoded, err := json.Marshal(q.Filters); err == nil {
			values.Set("filters", string(encoded))
		}
	}
	if encoded := values.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

func (s *APIServer) continuousRawProfileURL(ctx context.Context, objectKeys []string) string {
	if len(objectKeys) == 0 || !s.StorageConnected() {
		return ""
	}
	key := strings.TrimSpace(objectKeys[0])
	if continuousSessionSIDFromObjectKey(key) == "" {
		return ""
	}
	return "/api/v1/continuous/raw?key=" + url.QueryEscape(key)
}

func continuousSessionSIDFromObjectKeys(objectKeys []string) string {
	for _, objectKey := range objectKeys {
		if sid := continuousSessionSIDFromObjectKey(objectKey); sid != "" {
			return sid
		}
	}
	return ""
}

func continuousSessionSIDFromObjectKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		return ""
	}
	parts := strings.Split(key, "/")
	// 旧格式：continuous/{session}/{batch}.json
	if len(parts) == 3 && parts[0] == "continuous" && parts[1] != "" && path.Ext(parts[2]) == ".json" {
		return parts[1]
	}
	// 阶段三块格式：continuous-blocks/{session}/{YYYY}/{MM}/{DD}/{HH}/{block}.json.gz
	if len(parts) >= 7 && parts[0] == "continuous-blocks" && parts[1] != "" {
		return parts[1]
	}
	return ""
}

func windowOverlaps(start, end, from, to time.Time) bool {
	return !end.Before(from) && !start.After(to)
}

func applyContinuousDefaults(req *CreateContinuousSessionReq) error {
	req.Name = strings.TrimSpace(req.Name)
	req.TargetIP = strings.TrimSpace(req.TargetIP)
	req.Hostname = strings.TrimSpace(req.Hostname)
	req.ServiceName = strings.TrimSpace(req.ServiceName)
	req.Scope = strings.ToLower(strings.TrimSpace(req.Scope))
	if req.Scope == "" {
		req.Scope = "host"
	}
	req.SelectorExe = strings.TrimSpace(req.SelectorExe)
	const deletedSuffix = " (deleted)"
	if strings.HasSuffix(req.SelectorExe, deletedSuffix) {
		req.SelectorExe = strings.TrimSuffix(req.SelectorExe, deletedSuffix)
	}
	req.SelectorMode = strings.ToLower(strings.TrimSpace(req.SelectorMode))
	if req.SelectorMode == "" {
		req.SelectorMode = "all_instances"
	}
	// 阶段六：all_instances 是 exe_all_instances 的历史别名，统一归一化。
	if req.SelectorMode == "all_instances" {
		req.SelectorMode = "exe_all_instances"
	}
	// 阶段六：selector_params 与 selector_exe 兼容同步。新客户端传结构化
	// selector_params；旧客户端只传 selector_exe 时从 exe 推导 params。
	if req.SelectorParams != nil {
		req.SelectorParams.Exe = strings.TrimSpace(req.SelectorParams.Exe)
		req.SelectorParams.Cgroup = strings.TrimSpace(req.SelectorParams.Cgroup)
		req.SelectorParams.ContainerID = strings.TrimSpace(req.SelectorParams.ContainerID)
		if req.SelectorParams.Exe != "" {
			req.SelectorExe = req.SelectorParams.Exe
		}
		if req.SelectorMode == "container_id" {
			req.SelectorParams.ContainerID = strings.ToLower(req.SelectorParams.ContainerID)
		}
		if req.SelectorMode == "container_id" {
			req.SelectorParams.ContainerID = strings.ToLower(req.SelectorParams.ContainerID)
		}
	} else if req.SelectorExe != "" {
		req.SelectorParams = &ContinuousSelectorParams{Exe: req.SelectorExe}
	}
	req.ContinuityMode = strings.ToLower(strings.TrimSpace(req.ContinuityMode))
	if req.ContinuityMode == "" {
		req.ContinuityMode = "strict"
	}
	// 阶段一：信号控制面严格校验（未知值 400，空值四类默认，按序去重）。
	signals, err := normalizeContinuousRequestedSignals(req.Signals)
	if err != nil {
		return err
	}
	req.Signals = signals
	if req.SampleRateHz == 0 {
		req.SampleRateHz = 19
	}
	if req.AggregationWindowSec == 0 {
		req.AggregationWindowSec = 10
	}
	if req.UploadBatchSec == 0 {
		req.UploadBatchSec = 60
	}
	if req.RetentionHours == 0 {
		req.RetentionHours = 24
	}
	if req.Labels == nil {
		req.Labels = map[string]interface{}{}
	}
	if req.Capabilities == nil {
		req.Capabilities = map[string]interface{}{}
	}
	return nil
}

type continuousSignalRow struct {
	SignalType string
	Backend    string
}

func continuousWindowSignalRows(window ContinuousWindowIngest) []continuousSignalRow {
	seen := map[string]bool{}
	rows := []continuousSignalRow{}
	add := func(signalType, backend string) {
		signalType = strings.ToLower(strings.TrimSpace(firstNonEmpty(signalType, "cpu_profile")))
		if signalType == "" {
			signalType = "cpu_profile"
		}
		key := signalType + "|" + backend
		if seen[key] {
			return
		}
		seen[key] = true
		rows = append(rows, continuousSignalRow{SignalType: signalType, Backend: backend})
	}
	if len(window.Samples) > 0 {
		add(firstNonEmpty(window.SignalType, "cpu_profile"), continuousLegacyProfileBackend(window))
	}
	for _, profile := range window.Profiles {
		add(firstNonEmpty(profile.SignalType, "cpu_profile"), firstNonEmpty(profile.Backend, window.Backend))
	}
	for _, hist := range window.Histograms {
		add(hist.SignalType, firstNonEmpty(hist.Backend, window.Backend))
	}
	if len(window.Metrics) > 0 {
		add("python_rss", "procfs")
	}
	if len(window.DBSnapshots) > 0 {
		add("db_snapshot", "db_system_views")
	}
	if len(rows) == 0 {
		add(firstNonEmpty(window.SignalType, "cpu_profile"), window.Backend)
	}
	return rows
}

// continuousBatchSignalSet 返回 batch 内所有窗口实际携带的信号类型集合。
func continuousBatchSignalSet(windows []ContinuousWindowIngest) map[string]bool {
	set := map[string]bool{}
	for _, window := range windows {
		for _, row := range continuousWindowSignalRowsV3(window) {
			set[row.SignalType] = true
		}
	}
	return set
}

func continuousLegacyProfileBackend(window ContinuousWindowIngest) string {
	if window.Backend != "" {
		return window.Backend
	}
	for _, sample := range window.Samples {
		if sample.Backend != "" {
			return sample.Backend
		}
	}
	return ""
}

// continuousWindowSignalRowsV3 阶段一 v3：一个逻辑窗口每种信号只建立一条
// profile_windows 元数据行（backend 取首个出现者，多个 backend 作为该信号
// payload 的内部信息，不再按 backend 重复建行）。窗口级幂等键是
// (session_sid, window_id, signal_type)，与之一致。
func continuousWindowSignalRowsV3(window ContinuousWindowIngest) []continuousSignalRow {
	seen := map[string]bool{}
	rows := []continuousSignalRow{}
	add := func(signalType, backend string) {
		signalType = strings.ToLower(strings.TrimSpace(firstNonEmpty(signalType, "cpu_profile")))
		if signalType == "" {
			signalType = "cpu_profile"
		}
		if seen[signalType] {
			return
		}
		seen[signalType] = true
		rows = append(rows, continuousSignalRow{SignalType: signalType, Backend: backend})
	}
	if len(window.Samples) > 0 {
		add(firstNonEmpty(window.SignalType, "cpu_profile"), continuousLegacyProfileBackend(window))
	}
	for _, profile := range window.Profiles {
		add(firstNonEmpty(profile.SignalType, "cpu_profile"), firstNonEmpty(profile.Backend, window.Backend))
	}
	for _, hist := range window.Histograms {
		add(hist.SignalType, firstNonEmpty(hist.Backend, window.Backend))
	}
	if len(window.Metrics) > 0 {
		add("python_rss", "procfs")
	}
	if len(window.DBSnapshots) > 0 {
		add("db_snapshot", "db_system_views")
	}
	for signal := range window.SignalStatuses {
		add(signal, "")
	}
	if len(rows) == 0 {
		add(firstNonEmpty(window.SignalType, "cpu_profile"), window.Backend)
	}
	return rows
}

func validateContinuousV4Windows(windows []ContinuousWindowIngest) string {
	allowedStatus := map[string]bool{
		"collected": true, "target_idle": true, "no_events": true,
		"unavailable": true, "failed": true, "unknown": true,
	}
	allowedSignal := map[string]bool{}
	for _, signal := range continuousAllSignals {
		allowedSignal[signal] = true
	}
	for _, window := range windows {
		if window.PhysicalSampleRateHz < 0 || window.EffectiveSampleRateHz < 0 {
			return "v4 窗口采样率不能为负数"
		}
		if window.IdentityUnavailable > continuousMaxDBCount {
			return "v4 窗口 identity_unavailable_count 超出范围"
		}
		for signal, status := range window.SignalStatuses {
			normalized := strings.ToLower(strings.TrimSpace(signal))
			if normalized != signal || !allowedSignal[normalized] {
				return "v4 窗口包含未知 signal_statuses 信号: " + signal
			}
			if !allowedStatus[status.Status] {
				return "v4 窗口包含未知采集状态: " + status.Status
			}
			if len(status.Reason) > 512 {
				return "v4 窗口采集状态原因超过 512 字节"
			}
			if status.LostEvents > continuousMaxDBCount {
				return "v4 窗口 lost_events 超出范围"
			}
			hasData := continuousWindowSignalHasData(window, normalized)
			if status.Status == "collected" && !hasData {
				return "v4 窗口将零数据信号标记为 collected: " + normalized
			}
			if (status.Status == "target_idle" || status.Status == "no_events") && hasData {
				return "v4 窗口的空闲状态与实际数据冲突: " + normalized
			}
		}
		for _, signal := range continuousAllSignals {
			if continuousWindowSignalHasData(window, signal) {
				if _, ok := window.SignalStatuses[signal]; !ok {
					return "v4 窗口数据缺少 signal_statuses: " + signal
				}
			}
		}
	}
	return ""
}

// 阶段三：窗口是否包含某信号的真实数据（v3 批次无 signal_statuses 时按
// 内容推断状态：有样本 → collected，否则 unknown）。
func continuousWindowSignalHasData(window ContinuousWindowIngest, signalType string) bool {
	signalType = strings.ToLower(strings.TrimSpace(signalType))
	switch signalType {
	case "cpu_profile":
		if len(window.Samples) > 0 {
			return true
		}
		for _, profile := range window.Profiles {
			if strings.ToLower(strings.TrimSpace(firstNonEmpty(profile.SignalType, "cpu_profile"))) == "cpu_profile" &&
				len(profile.Samples) > 0 {
				return true
			}
		}
	case "python_memory":
		for _, profile := range window.Profiles {
			if strings.ToLower(strings.TrimSpace(firstNonEmpty(profile.SignalType, "cpu_profile"))) == "python_memory" &&
				len(profile.Samples) > 0 {
				return true
			}
		}
	case "python_rss":
		return len(window.Metrics) > 0
	case "db_snapshot":
		return len(window.DBSnapshots) > 0
	default:
		for _, hist := range window.Histograms {
			if strings.ToLower(strings.TrimSpace(hist.SignalType)) == signalType && !hist.Unavailable {
				return true
			}
		}
	}
	return false
}

func continuousWindowSampleCount(window ContinuousWindowIngest, signalType string) uint64 {
	signalType = strings.ToLower(strings.TrimSpace(signalType))
	var count uint64
	if signalType == "cpu_profile" {
		for _, sample := range window.Samples {
			count = addContinuousCount(count, firstNonZeroUint64(sample.Count, 1))
		}
		for _, profile := range window.Profiles {
			if strings.ToLower(strings.TrimSpace(firstNonEmpty(profile.SignalType, "cpu_profile"))) != "cpu_profile" {
				continue
			}
			for _, sample := range profile.Samples {
				count = addContinuousCount(count, firstNonZeroUint64(sample.Count, 1))
			}
		}
	}
	if signalType == "python_rss" {
		for _, metric := range window.Metrics {
			if metric.Metric == "rss_bytes" {
				count = addContinuousCount(count, 1)
			}
		}
	}
	if signalType == "db_snapshot" {
		count = addContinuousCount(count, uint64(len(window.DBSnapshots)))
	}
	for _, hist := range window.Histograms {
		if strings.ToLower(strings.TrimSpace(hist.SignalType)) == signalType {
			count = addContinuousCount(count, hist.EventCount)
		}
	}
	if count == 0 && signalType == "cpu_profile" {
		return clampContinuousCount(window.SampleCount)
	}
	return count
}

func addContinuousCount(total uint64, value uint64) uint64 {
	value = clampContinuousCount(value)
	if total >= continuousMaxDBCount || value >= continuousMaxDBCount {
		return continuousMaxDBCount
	}
	if total > continuousMaxDBCount-value {
		return continuousMaxDBCount
	}
	return total + value
}

func clampContinuousCount(value uint64) uint64 {
	if value > continuousMaxDBCount {
		return continuousMaxDBCount
	}
	return value
}

// continuousWindowSignalCountsFor 返回某 window 的分信号计数（服务端权威重算，
// 不信任 Agent 上报的 signal_counts/sample_count）。按信号类型去重（每个信号
// 只算一次），与 v3 的"一个逻辑窗口每种信号一行"语义一致。
func continuousWindowSignalCountsFor(window ContinuousWindowIngest) map[string]uint64 {
	counts := map[string]uint64{}
	for _, signal := range continuousWindowSignalRowsV3(window) {
		counts[signal.SignalType] = continuousWindowSampleCount(window, signal.SignalType)
	}
	return counts
}

// continuousBatchSignalCounts 汇总 batch 内所有 window 的分信号计数。
func continuousBatchSignalCounts(windows []ContinuousWindowIngest) map[string]uint64 {
	counts := map[string]uint64{}
	for _, window := range windows {
		for signal, count := range continuousWindowSignalCountsFor(window) {
			counts[signal] = addContinuousCount(counts[signal], count)
		}
	}
	return counts
}

// continuousSignalCountsJSON 将分信号计数 map 序列化为 jsonb 字节。
func continuousSignalCountsJSON(counts map[string]uint64) []byte {
	if len(counts) == 0 {
		return nil
	}
	data, err := json.Marshal(counts)
	if err != nil {
		return nil
	}
	return data
}

func normalizeContinuousSignalTypes(req ContinuousBatchIngestReq) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(signal string) {
		signal = strings.ToLower(strings.TrimSpace(signal))
		if signal == "" {
			return
		}
		if seen[signal] {
			return
		}
		seen[signal] = true
		out = append(out, signal)
	}
	for _, signal := range req.SignalTypes {
		add(signal)
	}
	for _, window := range req.Windows {
		if len(window.Samples) > 0 {
			add(firstNonEmpty(window.SignalType, "cpu_profile"))
		}
		for _, profile := range window.Profiles {
			add(firstNonEmpty(profile.SignalType, "cpu_profile"))
		}
		for _, hist := range window.Histograms {
			add(hist.SignalType)
		}
		if len(window.DBSnapshots) > 0 {
			add("db_snapshot")
		}
	}
	if len(out) == 0 {
		out = []string{"cpu_profile"}
	}
	return out
}

func summarizeContinuousBuckets(buckets []ContinuousHistogramBucket) ContinuousHistogramSummary {
	if len(buckets) == 0 {
		return ContinuousHistogramSummary{}
	}
	var total uint64
	min := buckets[0].Low
	max := buckets[0].High
	for _, bucket := range buckets {
		total += bucket.Count
		if bucket.Low < min {
			min = bucket.Low
		}
		if bucket.High > max {
			max = bucket.High
		}
	}
	valueAt := func(target float64) float64 {
		if total == 0 {
			return 0
		}
		threshold := uint64(float64(total)*target + 0.999999)
		if threshold == 0 {
			threshold = 1
		}
		var seen uint64
		for _, bucket := range buckets {
			seen += bucket.Count
			if seen >= threshold {
				return (bucket.Low + bucket.High) / 2
			}
		}
		last := buckets[len(buckets)-1]
		return (last.Low + last.High) / 2
	}
	return ContinuousHistogramSummary{
		Min: min,
		Max: max,
		P50: valueAt(0.50),
		P95: valueAt(0.95),
		P99: valueAt(0.99),
	}
}

func mustJSONBytes(value interface{}) []byte {
	if value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return body
}

func firstNonZeroUint64(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func firstNonEmptySlice(slices ...[]string) []string {
	for _, s := range slices {
		if len(s) > 0 {
			return s
		}
	}
	return nil
}

func (s *APIServer) loadReadableContinuousSession(c *gin.Context, sid string, auth AuthContext) (model.ContinuousSession, bool) {
	var session model.ContinuousSession
	if err := s.DB.Where("sid = ?", sid).First(&session).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "ContinuousSession 不存在")
		return session, false
	}
	if !s.canReadOwner(session.UID, auth) {
		s.forbid(c)
		return session, false
	}
	return session, true
}

func (s *APIServer) loadManageableContinuousSession(c *gin.Context, sid string, auth AuthContext) (model.ContinuousSession, bool) {
	session, ok := s.loadReadableContinuousSession(c, sid, auth)
	if !ok {
		return session, false
	}
	if !s.canManageOwner(session.UID, auth) {
		s.forbid(c)
		return session, false
	}
	return session, true
}

func parseOptionalTime(c *gin.Context, name string) (time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return time.Time{}, true
	}
	return parseProfileTime(c, name, time.Time{})
}
