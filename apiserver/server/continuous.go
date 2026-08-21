package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	Signals              []string               `json:"signals"`
	ContinuityMode       string                 `json:"continuity_mode"`
	AllowDegraded        bool                   `json:"allow_degraded"`
}

type ContinuousBatchIngestReq struct {
	SessionSID        string                   `json:"session_sid" binding:"required"`
	BatchID           string                   `json:"batch_id"`
	TargetIP          string                   `json:"target_ip"`
	ObjectKey         string                   `json:"object_key"`
	StartTime         time.Time                `json:"start_time" binding:"required"`
	EndTime           time.Time                `json:"end_time" binding:"required"`
	WindowCount       uint32                   `json:"window_count"`
	SampleCount       uint64                   `json:"sample_count"`
	SchemaVersion     uint32                   `json:"schema_version"`
	SignalTypes       []string                 `json:"signal_types"`
	Backends          map[string]string        `json:"backends"`
	ProfileFormat     string                   `json:"profile_format"`
	BackendStatus     string                   `json:"backend_status"`
	BackendReason     string                   `json:"backend_reason"`
	AttemptedBackends []string                 `json:"attempted_backends"`
	SelectedBackend   string                   `json:"selected_backend"`
	SymbolRefs        map[string]interface{}   `json:"symbol_refs"`
	Windows           []ContinuousWindowIngest `json:"windows"`
}

type ContinuousWindowIngest struct {
	WindowStart       time.Time                   `json:"window_start"`
	WindowEnd         time.Time                   `json:"window_end"`
	ObjectKey         string                      `json:"object_key"`
	SampleCount       uint64                      `json:"sample_count"`
	SignalType        string                      `json:"signal_type"`
	SchemaVersion     uint32                      `json:"schema_version"`
	Backend           string                      `json:"backend"`
	Labels            map[string]interface{}      `json:"labels"`
	ProfileFormat     string                      `json:"profile_format"`
	BackendStatus     string                      `json:"backend_status"`
	BackendReason     string                      `json:"backend_reason"`
	AttemptedBackends []string                    `json:"attempted_backends"`
	SelectedBackend   string                      `json:"selected_backend"`
	SymbolRefs        map[string]interface{}      `json:"symbol_refs"`
	Samples           []ContinuousStackSample     `json:"samples"`
	Profiles          []ContinuousProfileIngest   `json:"profiles"`
	Histograms        []ContinuousHistogramIngest `json:"histograms"`
	Metrics           []ContinuousMetricIngest    `json:"metrics"`
	RSSTruncated      int                         `json:"rss_truncated"`
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
	SessionSID        string                   `json:"session_sid"`
	BatchID           string                   `json:"batch_id"`
	TargetIP          string                   `json:"target_ip"`
	StartTime         time.Time                `json:"start_time"`
	EndTime           time.Time                `json:"end_time"`
	SchemaVersion     uint32                   `json:"schema_version"`
	SignalTypes       []string                 `json:"signal_types,omitempty"`
	Backends          map[string]string        `json:"backends,omitempty"`
	ProfileFormat     string                   `json:"profile_format,omitempty"`
	BackendStatus     string                   `json:"backend_status,omitempty"`
	BackendReason     string                   `json:"backend_reason,omitempty"`
	AttemptedBackends []string                 `json:"attempted_backends,omitempty"`
	SelectedBackend   string                   `json:"selected_backend,omitempty"`
	SymbolRefs        map[string]interface{}   `json:"symbol_refs,omitempty"`
	Windows           []ContinuousWindowIngest `json:"windows"`
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
	GoSymbolReady         bool
	GoSymbolPending       bool
	GoSymbolFailed        bool
	SymbolReasons         map[string]bool
	WindowCount           int
	Unit                  string
	RuntimeDiagnostics    map[string]*runtimeDiagnosticAccumulator
	SeenProfileIDs        map[string]bool
}

type runtimeDiagnosticAccumulator struct {
	Modes    map[string]bool
	Detected map[string]ProfileRuntimeProcessDiagnostic
	Ready    map[string]ProfileRuntimeProcessDiagnostic
	Missing  map[string]ProfileRuntimeProcessDiagnostic
	Limited  int
	Reasons  map[string]bool
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
	applyContinuousDefaults(&req)
	if message := validateContinuousCreateRequest(req); message != "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, message)
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
		Signals:              signals,
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
		if err := validateContinuousActiveSet(active, req.Scope, req.SelectorExe); err != nil {
			conflictSession = findContinuousConflict(active, req.Scope, req.SelectorExe)
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

func findContinuousConflict(active []model.ContinuousSession, scope, selectorExe string) *model.ContinuousSession {
	for index := range active {
		session := &active[index]
		if scope == "host" || session.Scope == "host" || session.Scope == "" || session.SelectorExe == selectorExe {
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
	for index := range sessions {
		markContinuousSessionOffline(&sessions[index], time.Now())
		sessions[index].CanManage = s.canManageOwner(sessions[index].UID, auth)
	}
	s.RespondOK(c, gin.H{"sessions": sessions, "total": total, "page": page, "page_size": pageSize})
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

func (s *APIServer) IngestContinuousBatch(c *gin.Context) {
	var req ContinuousBatchIngestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	if !req.StartTime.Before(req.EndTime) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "batch 时间范围不合法")
		return
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
	if req.SchemaVersion == 0 {
		req.SchemaVersion = 1
	}
	receivedAt := time.Now()
	clockOffsetMS, clockStatus, clockObserved := continuousAgentClock(c, receivedAt)

	var existing model.ProfileBatch
	if err := s.DB.Where("bid = ?", req.BatchID).First(&existing).Error; err == nil {
		if existing.SessionSID != req.SessionSID || !existing.StartTime.Equal(req.StartTime) || !existing.EndTime.Equal(req.EndTime) {
			s.RespondHTTPError(c, http.StatusConflict, ErrCodeTaskInvalidArgument, "batch_id 已被不同采集批次使用")
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
	batch := model.ProfileBatch{
		BID:                req.BatchID,
		SessionSID:         req.SessionSID,
		TargetIP:           req.TargetIP,
		ObjectKey:          req.ObjectKey,
		StartTime:          req.StartTime,
		EndTime:            req.EndTime,
		WindowCount:        req.WindowCount,
		SampleCount:        clampContinuousCount(req.SampleCount),
		SchemaVersion:      req.SchemaVersion,
		SignalTypes:        mustJSONBytes(req.SignalTypes),
		Backends:           mustJSONBytes(req.Backends),
		Status:             model.ContinuousBatchStatusReady,
		ProfileFormat:      firstNonEmpty(req.ProfileFormat, "json"),
		BackendStatus:      firstNonEmpty(req.BackendStatus, "ok"),
		BackendReason:      req.BackendReason,
		AttemptedBackends:  mustJSONBytes(req.AttemptedBackends),
		SelectedBackend:    req.SelectedBackend,
		SymbolRefs:         mustJSONBytes(req.SymbolRefs),
		ReceivedAt:         receivedAt,
		AgentClockOffsetMs: clockOffsetMS,
		CreatedAt:          now,
	}
	duplicate := false
	var payloadStoreErr error
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "bid"}}, DoNothing: true}).Create(&batch)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			duplicate = true
			return nil
		}
		if err := s.storeContinuousBatchPayload(c.Request.Context(), req); err != nil {
			payloadStoreErr = err
			return err
		}
		for _, in := range req.Windows {
			if in.WindowStart.IsZero() || in.WindowEnd.IsZero() || !in.WindowStart.Before(in.WindowEnd) {
				continue
			}
			labels, _ := json.Marshal(in.Labels)
			symbolRefs, _ := json.Marshal(in.SymbolRefs)
			for _, signal := range continuousWindowSignalRows(in) {
				window := model.ProfileWindow{
					SessionSID:        req.SessionSID,
					BatchBID:          req.BatchID,
					WindowStart:       in.WindowStart,
					WindowEnd:         in.WindowEnd,
					ObjectKey:         firstNonEmpty(in.ObjectKey, req.ObjectKey),
					SampleCount:       clampContinuousCount(continuousWindowSampleCount(in, signal.SignalType)),
					SignalType:        signal.SignalType,
					SchemaVersion:     firstNonZeroUint32(in.SchemaVersion, req.SchemaVersion),
					Backend:           signal.Backend,
					Labels:            labels,
					ProfileFormat:     firstNonEmpty(in.ProfileFormat, req.ProfileFormat, "json"),
					BackendStatus:     firstNonEmpty(in.BackendStatus, req.BackendStatus, "ok"),
					BackendReason:     firstNonEmpty(in.BackendReason, req.BackendReason),
					AttemptedBackends: mustJSONBytes(firstNonEmptySlice(in.AttemptedBackends, req.AttemptedBackends)),
					SelectedBackend:   firstNonEmpty(in.SelectedBackend, req.SelectedBackend),
					SymbolRefs:        symbolRefs,
					CreatedAt:         now,
				}
				if err := tx.Create(&window).Error; err != nil {
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
	var windows []model.ProfileWindow
	if err := query.Find(&windows).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 Continuous timeline 失败")
		return
	}
	if windows == nil {
		windows = []model.ProfileWindow{}
	}
	gaps, coverage := continuousTimelineCoverage(windows, boundaryFrom, boundaryTo, 5*time.Second)
	s.RespondOK(c, gin.H{
		"session": session, "windows": windows, "total": len(windows),
		"gaps": gaps, "coverage": coverage,
		"clock": gin.H{
			"offset_ms": session.AgentClockOffsetMs, "status": continuousSessionClockStatus(session),
			"observed_at": session.AgentClockObservedAt,
		},
	})
}

type continuousTimelineGap struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	DurationSeconds float64   `json:"duration_seconds"`
	Type            string    `json:"type"`
}

func continuousTimelineCoverage(windows []model.ProfileWindow, from, to time.Time, tolerance time.Duration) ([]continuousTimelineGap, gin.H) {
	type interval struct{ start, end time.Time }
	intervals := make([]interval, 0, len(windows))
	for _, window := range windows {
		start, end := window.WindowStart, window.WindowEnd
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
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
	if to.Sub(cursor) > tolerance {
		gaps = append(gaps, continuousTimelineGap{Start: cursor, End: to, DurationSeconds: to.Sub(cursor).Seconds(), Type: "trailing"})
	}
	totalSeconds := to.Sub(from).Seconds()
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
		"from": from, "to": to, "total_seconds": totalSeconds,
		"covered_seconds": coveredSeconds, "gap_seconds": gapSeconds, "ratio": ratio,
	}
}

func (s *APIServer) QueryContinuousProfile(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
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
			"query":          profileLabelSelector(q),
			"nodes":          []ProfileNode{},
			"items":          []ProfileTopItem{},
			"total":          0,
			"unit":           "samples",
			"empty":          true,
			"message":        "Native Continuous Profiling 暂无覆盖该时间范围的 10s window",
			"source":         "mini-drop-native",
			"profile_source": "native",
			"generated_at":   time.Now(),
		})
		return
	}
	s.RespondOK(c, gin.H{
		"query":          fg.Query,
		"nodes":          fg.Nodes,
		"items":          topn.Items,
		"total":          fg.Total,
		"unit":           fg.Unit,
		"empty":          fg.Empty,
		"message":        fg.Message,
		"source":         fg.Source,
		"profile_source": fg.ProfileSource,
		"profile_url":    fg.ProfileURL,
		"generated_at":   fg.GeneratedAt,
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

func (s *APIServer) storeContinuousBatchPayload(ctx context.Context, req ContinuousBatchIngestReq) error {
	if !s.StorageConnected() {
		return errProfileUnavailable
	}
	payload := continuousStoredBatch{
		SessionSID:        req.SessionSID,
		BatchID:           req.BatchID,
		TargetIP:          req.TargetIP,
		StartTime:         req.StartTime,
		EndTime:           req.EndTime,
		SchemaVersion:     req.SchemaVersion,
		SignalTypes:       req.SignalTypes,
		Backends:          req.Backends,
		ProfileFormat:     req.ProfileFormat,
		BackendStatus:     req.BackendStatus,
		BackendReason:     req.BackendReason,
		AttemptedBackends: req.AttemptedBackends,
		SelectedBackend:   req.SelectedBackend,
		SymbolRefs:        req.SymbolRefs,
		Windows:           req.Windows,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.Storage.PutObject(ctx, s.Config.Storage.Bucket, req.ObjectKey, bytes.NewReader(body), int64(len(body)), "application/json")
}

func continuousBatchObjectKey(sessionSID, batchID string) string {
	return "continuous/" + sessionSID + "/" + batchID + ".json"
}

func (s *APIServer) cleanupContinuousRetention(ctx context.Context, session model.ContinuousSession) {
	retentionHours := session.RetentionHours
	if retentionHours == 0 {
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
		ProfileURL:         s.continuousProfileURL(ctx, agg.ObjectKeys),
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
		ProfileURL:         s.continuousProfileURL(ctx, agg.ObjectKeys),
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
			existing, ok := merged[item.Name]
			if !ok {
				existing = &ProfileTopItem{Name: item.Name, Unit: firstNonEmpty(item.Unit, "samples")}
				merged[item.Name] = existing
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
		batch, err := s.loadContinuousStoredBatch(ctx, objectKey)
		if err != nil {
			s.Logger.Warn("Native Continuous Profiling 冷层摘要读取 batch 对象失败，本轮跳过这些窗口",
				zap.String("session_sid", session.SID), zap.String("object_key", objectKey), zap.Error(err))
			failedObjects[objectKey] = true
			continue
		}
		// 用 Unix 秒比较，不直接比较 time.Time：row.WindowStart 是从数据库
		// 读回来的（Postgres 时间戳精度可能比 Go 的 time.Time 低），
		// window.WindowStart 是从 MinIO 里的原始 JSON 解析出来的（保留了
		// Agent 上报时的完整精度），两边直接 Equal() 有极小概率因为精度
		// 截断错判成"不匹配"，导致这个 window 被删除却从没进过摘要。
		rowSet := map[int64]bool{}
		for _, row := range rows {
			rowSet[row.WindowStart.Unix()] = true
		}
		for _, window := range batch.Windows {
			if firstNonEmpty(window.SignalType, "cpu_profile") != "cpu_profile" {
				continue
			}
			if !rowSet[window.WindowStart.Unix()] {
				continue // 这个 batch 对象里还有别的、这轮还没到期的 window，不动它
			}
			bucketStart := window.WindowStart.Truncate(continuousSummaryBucketDuration)
			key := bucketKey{bucketStart: bucketStart}
			agg, ok := buckets[key]
			if !ok {
				agg = &continuousAggregate{
					Top:                map[string]*ProfileTopItem{},
					Root:               &continuousTreeNode{Name: "root", Children: map[string]*continuousTreeNode{}},
					LabelValue:         map[string]map[string]bool{"comm": {}, "pid": {}, "exe": {}, "runtime": {}},
					Backends:           map[string]bool{},
					SymbolReasons:      map[string]bool{},
					RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{},
					SeenProfileIDs:     map[string]bool{},
					Unit:               "samples",
				}
				buckets[key] = agg
			}
			bucketEnd := bucketStart.Add(continuousSummaryBucketDuration)
			if existing, ok := bucketEnds[key]; !ok || existing.Before(bucketEnd) {
				bucketEnds[key] = bucketEnd
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
func (s *APIServer) mergeContinuousWindowSummary(ctx context.Context, sessionSID, signalType string, bucketStart, bucketEnd time.Time, agg *continuousAggregate) error {
	var existing model.ContinuousWindowSummary
	lookupErr := s.DB.WithContext(ctx).
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
		items = append(items, ProfileTopItem{Name: name, Self: self, Value: self, Unit: "samples"})
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
	result := s.DB.WithContext(ctx).
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
	series, found, err := s.queryNativeContinuousTimeseries(c.Request.Context(), q, metric, maxSeries)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		series = []ProfileTimeseriesSeries{}
	}
	s.RespondOK(c, gin.H{"series": series, "metric": metric, "unit": "bytes", "empty": len(series) == 0, "generated_at": time.Now()})
}

func (s *APIServer) queryNativeContinuousTimeseries(ctx context.Context, q ProfileQuery, metricName string, maxSeries int) ([]ProfileTimeseriesSeries, bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	err := s.DB.Where("session_sid IN (?)", sessionQuery).Where("signal_type = ?", "python_rss").
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).Order("window_start ASC").
		Limit(continuousMaxWindowCount + 1).Find(&windows).Error
	if err != nil {
		return nil, false, err
	}
	if len(windows) > continuousMaxWindowCount {
		return nil, true, errContinuousWindowLimit
	}
	if len(windows) == 0 {
		return []ProfileTimeseriesSeries{}, false, nil
	}
	if !s.StorageConnected() {
		return nil, true, errProfileUnavailable
	}

	byKey := map[string]*ProfileTimeseriesSeries{}
	loaded := map[string]bool{}
	for _, row := range windows {
		if row.ObjectKey == "" || loaded[row.ObjectKey] {
			continue
		}
		loaded[row.ObjectKey] = true
		batch, err := s.loadContinuousStoredBatch(ctx, row.ObjectKey)
		if err != nil {
			return nil, true, err
		}
		for _, window := range batch.Windows {
			if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
				continue
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
	return out, true, nil
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
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	signalType := "cpu_profile"
	if q.ProfileType == "memory" {
		signalType = "python_memory"
	}
	err := s.DB.Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", signalType).
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Order("window_start ASC").
		Limit(continuousMaxWindowCount + 1).
		Find(&windows).Error
	if err != nil {
		return continuousAggregate{}, false, err
	}
	if len(windows) > continuousMaxWindowCount {
		return continuousAggregate{}, true, errContinuousWindowLimit
	}
	if len(windows) == 0 {
		return continuousAggregate{}, false, nil
	}
	if !s.StorageConnected() {
		return continuousAggregate{}, true, errProfileUnavailable
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
		WindowCount:        len(windows),
		Unit:               map[bool]string{true: "bytes", false: "samples"}[q.ProfileType == "memory"],
		RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{},
		SeenProfileIDs:     map[string]bool{},
	}
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
		batch, err := s.loadContinuousStoredBatch(ctx, objectKey)
		if err != nil {
			return continuousAggregate{}, true, err
		}
		agg.ObjectKeys = append(agg.ObjectKeys, objectKey)
		for _, window := range batch.Windows {
			if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
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
			continuousAggregateSymbolMetadata(&agg, window.SymbolRefs, relevantDSOs)
			continuousAggregateRuntimeMetadata(&agg, window.SymbolRefs)
			for _, sample := range matched {
				continuousAddSample(&agg, sample, window.Labels)
			}
		}
	}
	continuousFinalizeSymbolStatus(&agg)
	return agg, true, nil
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
		UnresolvedPercent: percent, GoSymbolState: state, Reasons: reasons,
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
	runtimeMaps, _ := refs["runtime_maps"].(map[string]interface{})
	for _, runtimeName := range []string{"java", "node", "python"} {
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
			key := strconv.Itoa(pid)
			process := ProfileRuntimeProcessDiagnostic{PID: pid, Mode: "perf-map", Status: "missing", Reason: reason}
			diag.Detected[key] = process
			diag.Missing[key] = process
		}
		readyPIDs, _ := raw["ready_pids"].([]interface{})
		for _, value := range readyPIDs {
			pid := int(numberAsFloat64(value))
			key := strconv.Itoa(pid)
			process := ProfileRuntimeProcessDiagnostic{PID: pid, Mode: "perf-map", Status: "ready"}
			diag.Detected[key] = process
			diag.Ready[key] = process
		}
	}
	fallback, _ := refs["python_fallback"].(map[string]interface{})
	python := continuousRuntimeAccumulator(agg, "python")
	python.Limited += int(numberAsFloat64(fallback["limited_count"]))
	for _, field := range []string{"ready", "failed"} {
		items, _ := fallback[field].([]interface{})
		for _, value := range items {
			item, _ := value.(map[string]interface{})
			pid := int(numberAsFloat64(item["pid"]))
			key := strconv.Itoa(pid)
			reason, _ := item["reason"].(string)
			status := "ready"
			if field == "failed" {
				status = "missing"
				if reason != "" {
					python.Reasons[reason] = true
				}
			}
			process := ProfileRuntimeProcessDiagnostic{PID: pid, Mode: "py-spy", Status: status, Reason: reason}
			python.Detected[key] = process
			python.Modes["py-spy"] = true
			if status == "ready" {
				python.Ready[key] = process
			} else {
				python.Missing[key] = process
			}
		}
	}
	memory, _ := refs["python_memory"].(map[string]interface{})
	for _, field := range []string{"ready", "failed"} {
		items, _ := memory[field].([]interface{})
		for _, value := range items {
			item, _ := value.(map[string]interface{})
			pid := int(numberAsFloat64(item["pid"]))
			key := "memory|" + strconv.Itoa(pid)
			reason, _ := item["reason"].(string)
			status := "ready"
			if field == "failed" {
				status = "missing"
				if reason != "" {
					python.Reasons[reason] = true
				}
			}
			process := ProfileRuntimeProcessDiagnostic{PID: pid, Mode: "memray", Status: status, Reason: reason}
			python.Detected[key] = process
			python.Modes["memray"] = true
			if status == "ready" {
				python.Ready[key] = process
			} else {
				python.Missing[key] = process
			}
		}
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
		out[runtimeName] = ProfileRuntimeDiagnostic{
			Status: status, Modes: modes, DetectedCount: len(item.Detected), ReadyCount: len(item.Ready),
			MissingCount: len(item.Missing), LimitedCount: item.Limited, Reasons: reasons, Processes: processes,
		}
	}
	return out
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
	if len(q.Filters) > 0 {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument,
			"当前延迟 histogram 不支持 PID/进程标签过滤，请在 CPU 视图使用实例筛选")
		return
	}
	signalType := strings.ToLower(strings.TrimSpace(c.Query("signal_type")))
	if signalType == "" {
		signalType = strings.ToLower(strings.TrimSpace(c.Query("profile_type")))
	}
	if signalType != "io_latency" && signalType != "io_syscall_latency" && signalType != "sched_latency" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "signal_type 仅支持 io_latency/io_syscall_latency/sched_latency")
		return
	}
	data, found, err := s.queryNativeContinuousHistogram(c.Request.Context(), q, signalType)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		s.RespondOK(c, gin.H{
			"query":        profileLabelSelector(q),
			"signal_type":  signalType,
			"empty":        true,
			"message":      "Native Continuous eBPF 暂无覆盖该时间范围的 histogram window",
			"source":       "mini-drop-native",
			"generated_at": time.Now(),
			"buckets":      []ContinuousHistogramBucket{},
			"trend":        []gin.H{},
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

	for _, objectKey := range orderedContinuousObjectKeys(windows) {
		batch, err := s.loadContinuousStoredBatch(ctx, objectKey)
		if err != nil {
			return nil, true, err
		}
		if !seenObject[objectKey] {
			objectKeys = append(objectKeys, objectKey)
			seenObject[objectKey] = true
		}
		for _, window := range batch.Windows {
			if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
				continue
			}
			for _, hist := range window.Histograms {
				if strings.ToLower(strings.TrimSpace(hist.SignalType)) != signalType {
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
		"query":        profileLabelSelector(q),
		"signal_type":  signalType,
		"buckets":      buckets,
		"summary":      summary,
		"trend":        trend,
		"event_count":  totalEvents,
		"unit":         "us",
		"backend":      strings.Join(backendList, ","),
		"backends":     backendList,
		"empty":        empty,
		"message":      message,
		"source":       "mini-drop-native",
		"profile_url":  s.continuousProfileURL(ctx, objectKeys),
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
			if seenProfileIDs[profile.ProfileID] {
				continue
			}
			seenProfileIDs[profile.ProfileID] = true
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
			out = append(out, sample)
		}
	}
	return out
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
	processKey := strconv.Itoa(sample.PID) + "|" + sample.Exe
	process := ProfileRuntimeProcessDiagnostic{PID: sample.PID, Comm: sample.Comm, Exe: sample.Exe, Mode: sample.Backend, Status: "ready"}
	diag.Detected[processKey] = process
	diag.Ready[processKey] = process
	if sample.Backend != "" {
		diag.Modes[sample.Backend] = true
	}
	for _, key := range []string{"comm", "pid", "process_start_ms", "process_instance", "exe", "runtime"} {
		if value := continuousSampleLabel(sample, windowLabels, key); value != "" {
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
		}
		item := agg.Top[frame]
		if item == nil {
			item = &ProfileTopItem{Name: frame, Unit: firstNonEmpty(agg.Unit, "samples")}
			agg.Top[frame] = item
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

func (s *APIServer) continuousProfileURL(ctx context.Context, objectKeys []string) string {
	if len(objectKeys) == 0 || !s.StorageConnected() {
		return ""
	}
	key := strings.TrimSpace(objectKeys[0])
	if continuousSessionSIDFromObjectKey(key) == "" {
		return ""
	}
	return "/api/v1/continuous/raw?key=" + url.QueryEscape(key)
}

func continuousSessionSIDFromObjectKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		return ""
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "continuous" || parts[1] == "" || path.Ext(parts[2]) != ".json" {
		return ""
	}
	return parts[1]
}

func windowOverlaps(start, end, from, to time.Time) bool {
	return !end.Before(from) && !start.After(to)
}

func applyContinuousDefaults(req *CreateContinuousSessionReq) {
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
	req.ContinuityMode = strings.ToLower(strings.TrimSpace(req.ContinuityMode))
	if req.ContinuityMode == "" {
		req.ContinuityMode = "strict"
	}
	if len(req.Signals) == 0 {
		req.Signals = append([]string(nil), continuousDefaultSignals...)
	} else {
		// The first user-driven version exposes one complete, coherent signal set.
		req.Signals = append([]string(nil), continuousDefaultSignals...)
	}
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
	if len(rows) == 0 {
		add(firstNonEmpty(window.SignalType, "cpu_profile"), window.Backend)
	}
	return rows
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
	}
	if len(out) == 0 {
		out = []string{"cpu_profile"}
	}
	return out
}

func orderedContinuousObjectKeys(windows []model.ProfileWindow) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, window := range windows {
		if window.ObjectKey == "" || seen[window.ObjectKey] {
			continue
		}
		seen[window.ObjectKey] = true
		out = append(out, window.ObjectKey)
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
