package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

var (
	errContinuousModeConflict      = errors.New("同一主机的整机持续任务与进程持续任务互斥")
	errContinuousHostLimitReached  = errors.New("同一主机最多同时运行 1 个整机持续任务")
	errContinuousLimitReached      = errors.New("同一主机最多同时运行 16 个进程持续任务")
	errContinuousDuplicateSelector = errors.New("同一主机已存在相同 exe 的活动持续任务")
	errContinuousAgentUnavailable  = errors.New("目标 Agent 尚未连接持续采集控制面")
)

var continuousDefaultSignals = []string{"cpu_profile", "io_latency", "io_syscall_latency", "sched_latency"}

// normalizeContinuousRequestedSignals 阶段一信号控制面：
//   - 空 signals → 四类默认值（按固定顺序）；
//   - 非空 signals → 校验每个值，未知值返回错误（API 侧 400，不再静默过滤后
//     回退为全开）；已知值按出现顺序去重保存。
//
// 返回的切片是去重后的副本，不持有入参引用。
func normalizeContinuousRequestedSignals(signals []string) ([]string, error) {
	allowed := map[string]bool{}
	for _, signal := range continuousDefaultSignals {
		allowed[signal] = true
	}
	if len(signals) == 0 {
		return append([]string(nil), continuousDefaultSignals...), nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(signals))
	for _, signal := range signals {
		signal = strings.ToLower(strings.TrimSpace(signal))
		if signal == "" {
			continue
		}
		if !allowed[signal] {
			return nil, fmt.Errorf("未知信号类型: %s（支持 %s）", signal, strings.Join(continuousDefaultSignals, ", "))
		}
		if seen[signal] {
			continue
		}
		seen[signal] = true
		out = append(out, signal)
	}
	if len(out) == 0 {
		return append([]string(nil), continuousDefaultSignals...), nil
	}
	return out, nil
}

// continuousSessionSignals 从 session 的 signals jsonb 解析字符串数组；为空或
// 解析失败时回退四类默认值（用于 Reconcile assignment DTO 显式下发）。
func continuousSessionSignals(session model.ContinuousSession) []string {
	if len(session.Signals) > 0 {
		var signals []string
		if err := json.Unmarshal(session.Signals, &signals); err == nil && len(signals) > 0 {
			normalized, err := normalizeContinuousRequestedSignals(signals)
			if err == nil {
				return normalized
			}
		}
	}
	return append([]string(nil), continuousDefaultSignals...)
}

// continuousSessionSignalSet 返回 session 请求信号的集合（供 v3 ingest 校验
// "Session 只保存其请求的信号"）。
func continuousSessionSignalSet(session model.ContinuousSession) map[string]bool {
	set := map[string]bool{}
	for _, signal := range continuousSessionSignals(session) {
		set[signal] = true
	}
	return set
}

// continuousCoreSignal 判断是否为四类核心选择器信号（cpu_profile/io_latency/
// io_syscall_latency/sched_latency）。python_rss/db_snapshot 是独立数据通道，
// 不受 signals 选择器约束。
func continuousCoreSignal(signal string) bool {
	for _, core := range continuousDefaultSignals {
		if signal == core {
			return true
		}
	}
	return false
}

type continuousProcessReport struct {
	PID            int    `json:"pid"`
	ProcessStartMs int64  `json:"process_start_ms"`
	Comm           string `json:"comm"`
	Exe            string `json:"exe"`
	RSSBytes       uint64 `json:"rss_bytes"`
}

type continuousObservedReport struct {
	SID               string                    `json:"sid"`
	ObservedState     string                    `json:"observed_state"`
	ActiveProcesses   []continuousProcessReport `json:"active_processes"`
	ContinuityMode    string                    `json:"continuity_mode"`
	DegradationReason string                    `json:"degradation_reason"`
	LastError         string                    `json:"last_error"`
}

type reconcileContinuousReq struct {
	TargetIP      string                     `json:"target_ip" binding:"required"`
	Hostname      string                     `json:"hostname"`
	AgentID       string                     `json:"agent_id"`
	StrictCapable bool                       `json:"strict_capable"`
	Capabilities  []string                   `json:"capabilities"`
	Revision      uint64                     `json:"revision"`
	Processes     []continuousProcessReport  `json:"processes"`
	Sessions      []continuousObservedReport `json:"sessions"`
}

// continuousAssignmentDTO 阶段一：Reconcile 下发的显式 assignment DTO。
// signals / requested_signals 直接返回字符串数组（不依赖 GORM 对 jsonb
// []byte 的 base64 自动编码），Agent 侧无需再猜解码方式；labels 保持现有
// base64 JSON（避免扩大改动范围）。collector_generation / target_fingerprint /
// batch_sequence 是 Agent 侧采集器实例概念，DTO 仅透传服务端已知值（通常为空
// 字符串/0），由 Agent 在生成 batch/window 时填充。
type continuousAssignmentDTO struct {
	SID                  string     `json:"sid"`
	Name                 string     `json:"name"`
	TargetIP             string     `json:"target_ip"`
	Hostname             string     `json:"hostname"`
	ServiceName          string     `json:"service_name"`
	SampleRateHz         uint32     `json:"sample_rate_hz"`
	AggregationWindowSec uint32     `json:"aggregation_window_sec"`
	UploadBatchSec       uint32     `json:"upload_batch_sec"`
	RetentionHours       uint32     `json:"retention_hours"`
	Labels               []byte     `json:"labels"`
	Capabilities         []byte     `json:"capabilities"`
	Status               string     `json:"status"`
	Scope                string     `json:"scope"`
	SelectorExe          string     `json:"selector_exe"`
	SelectorMode         string     `json:"selector_mode"`
	Signals              []string   `json:"signals"`
	RequestedSignals     []string   `json:"requested_signals"`
	DesiredState         string     `json:"desired_state"`
	ObservedState        string     `json:"observed_state"`
	ContinuityMode       string     `json:"continuity_mode"`
	AllowDegraded        bool       `json:"allow_degraded"`
	DegradationReason    string     `json:"degradation_reason"`
	LastError            string     `json:"last_error"`
	Revision             uint64     `json:"revision"`
	AgentID              string     `json:"agent_id"`
	UID                  string     `json:"uid"`
	UserName             string     `json:"user_name"`
	StartedAt            time.Time  `json:"started_at"`
	StoppedAt            *time.Time `json:"stopped_at"`
	CollectorGeneration  string     `json:"collector_generation,omitempty"`
	TargetFingerprint    string     `json:"target_fingerprint,omitempty"`
	BatchSequence        uint64     `json:"batch_sequence,omitempty"`
}

// continuousAssignmentDTOs 把 GORM Session 行转换为显式 assignment DTO。
func continuousAssignmentDTOs(sessions []model.ContinuousSession) []continuousAssignmentDTO {
	out := make([]continuousAssignmentDTO, 0, len(sessions))
	for _, session := range sessions {
		signals := continuousSessionSignals(session)
		out = append(out, continuousAssignmentDTO{
			SID:                  session.SID,
			Name:                 session.Name,
			TargetIP:             session.TargetIP,
			Hostname:             session.Hostname,
			ServiceName:          session.ServiceName,
			SampleRateHz:         session.SampleRateHz,
			AggregationWindowSec: session.AggregationWindowSec,
			UploadBatchSec:       session.UploadBatchSec,
			RetentionHours:       session.RetentionHours,
			Labels:               session.Labels,
			Capabilities:         session.Capabilities,
			Status:               session.Status,
			Scope:                session.Scope,
			SelectorExe:          session.SelectorExe,
			SelectorMode:         session.SelectorMode,
			Signals:              signals,
			RequestedSignals:     signals,
			DesiredState:         session.DesiredState,
			ObservedState:        session.ObservedState,
			ContinuityMode:       session.ContinuityMode,
			AllowDegraded:        session.AllowDegraded,
			DegradationReason:    session.DegradationReason,
			LastError:            session.LastError,
			Revision:             session.Revision,
			AgentID:              session.AgentID,
			UID:                  session.UID,
			UserName:             session.UserName,
			StartedAt:            session.StartedAt,
			StoppedAt:            session.StoppedAt,
		})
	}
	return out
}

func validateContinuousCreateRequest(req CreateContinuousSessionReq) string {
	if req.Name == "" {
		return "name 不能为空"
	}
	if len(req.Name) > 256 {
		return "name 不能超过 256 个字符"
	}
	if req.Scope != "host" && req.Scope != "process" {
		return "scope 仅支持 host/process"
	}
	if req.Scope == "process" {
		if req.SelectorExe == "" || !filepath.IsAbs(req.SelectorExe) {
			return "进程持续任务必须提供绝对路径 selector_exe"
		}
		if req.SelectorMode != "all_instances" {
			return "selector_mode 仅支持 all_instances"
		}
	}
	if req.SampleRateHz < 1 || req.SampleRateHz > 999 {
		return "sample_rate_hz 必须在 1 到 999 之间"
	}
	if req.AggregationWindowSec < 5 || req.AggregationWindowSec > 300 {
		return "aggregation_window_sec 必须在 5 到 300 之间"
	}
	if req.UploadBatchSec < req.AggregationWindowSec || req.UploadBatchSec > 3600 {
		return "upload_batch_sec 必须不小于聚合窗口且不超过 3600"
	}
	if req.RetentionHours < 1 || req.RetentionHours > 24 {
		return "retention_hours 必须在 1 到 24 之间（持续采集原始数据最长保留 24 小时）"
	}
	if req.ContinuityMode != "strict" && req.ContinuityMode != "degraded" {
		return "continuity_mode 仅支持 strict/degraded"
	}
	return ""
}

func validateContinuousActiveSet(active []model.ContinuousSession, scope, selectorExe string) error {
	if scope == "host" {
		for _, session := range active {
			if session.Scope == "host" || session.Scope == "" {
				return errContinuousHostLimitReached
			}
		}
		if len(active) > 0 {
			return errContinuousModeConflict
		}
		return nil
	}
	processCount := 0
	for _, session := range active {
		if session.Scope == "host" || session.Scope == "" {
			return errContinuousModeConflict
		}
		processCount++
		if session.SelectorExe == selectorExe {
			return errContinuousDuplicateSelector
		}
	}
	if processCount >= 16 {
		return errContinuousLimitReached
	}
	return nil
}

func continuousPagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	return page, pageSize
}

func continuousSessionOrderSQL() string {
	return "CASE observed_state WHEN 'running' THEN 0 WHEN 'waiting' THEN 1 WHEN 'degraded' THEN 2 WHEN 'pending' THEN 3 WHEN 'stopping' THEN 4 WHEN 'error' THEN 5 WHEN 'offline' THEN 6 ELSE 7 END, created_at DESC"
}

func markContinuousSessionOffline(session *model.ContinuousSession, now time.Time) {
	if session == nil || session.DesiredState != model.ContinuousDesiredStateRunning {
		return
	}
	anchor := session.StartedAt
	if anchor.IsZero() {
		anchor = session.CreatedAt
	}
	if session.ObservedAt != nil {
		anchor = *session.ObservedAt
	}
	if !anchor.IsZero() && now.Sub(anchor) > 15*time.Second {
		session.ObservedState = model.ContinuousObservedStateOffline
	}
}

func validContinuousObservedState(value string) bool {
	switch value {
	case model.ContinuousObservedStatePending,
		model.ContinuousObservedStateRunning,
		model.ContinuousObservedStateWaiting,
		model.ContinuousObservedStateDegraded,
		model.ContinuousObservedStateStopping,
		model.ContinuousObservedStateStopped,
		model.ContinuousObservedStateError,
		model.ContinuousObservedStateOffline:
		return true
	default:
		return false
	}
}

// continuousSessionSelection is the only query selector used by profile readers.
// New clients always pass session_sid. Legacy clients are restricted to the most
// recently updated single Session so profiles from separate tasks are never merged.
func (s *APIServer) continuousSessionSelection(q ProfileQuery) *gorm.DB {
	query := s.DB.Model(&model.ContinuousSession{}).Select("sid").Where("target_ip = ?", q.Host)
	if q.SessionSID != "" {
		query = query.Where("sid = ?", q.SessionSID)
	} else {
		query = query.Order("updated_at DESC").Limit(1)
	}
	if !q.CanReadAll {
		if len(q.OwnerUIDs) > 0 {
			query = query.Where("(uid IN ? OR uid = '' OR uid IS NULL)", q.OwnerUIDs)
		} else {
			query = query.Where("(uid = '' OR uid IS NULL)")
		}
	}
	return query
}

func (s *APIServer) GetContinuousSession(c *gin.Context) {
	auth := s.AuthContext(c)
	session, ok := s.loadReadableContinuousSession(c, strings.TrimSpace(c.Param("sid")), auth)
	if !ok {
		return
	}
	markContinuousSessionOffline(&session, time.Now())
	session.CanManage = s.canManageOwner(session.UID, auth)
	s.RespondOK(c, gin.H{"session": session})
}

func (s *APIServer) ListContinuousProcesses(c *gin.Context) {
	targetIP := strings.TrimSpace(c.Query("target_ip"))
	if targetIP == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "target_ip 不能为空")
		return
	}
	var agent model.AgentInfo
	if err := s.DB.Where("ip_addr = ?", targetIP).Order("last_seen DESC").First(&agent).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "目标 Agent 不存在")
		return
	}
	if !s.canReadAgent(agent, s.AuthContext(c)) {
		s.forbid(c)
		return
	}
	query := s.DB.Where("target_ip = ?", targetIP)
	if keyword := strings.TrimSpace(c.Query("keyword")); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where("comm LIKE ? OR exe LIKE ?", like, like)
	}
	var processes []model.ContinuousProcessSnapshot
	if err := query.Order("rss_bytes DESC, pid ASC").Limit(2000).Find(&processes).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询进程快照失败")
		return
	}
	if processes == nil {
		processes = []model.ContinuousProcessSnapshot{}
	}
	var state model.ContinuousAgentState
	_ = s.DB.Where("target_ip = ?", targetIP).First(&state).Error
	fresh := !state.ObservedAt.IsZero() && time.Since(state.ObservedAt) <= 15*time.Second
	s.RespondOK(c, gin.H{"processes": processes, "total": len(processes), "agent_state": state, "fresh": fresh})
}

func (s *APIServer) ReconcileContinuousSessions(c *gin.Context) {
	var req reconcileContinuousReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	req.TargetIP = strings.TrimSpace(req.TargetIP)
	req.AgentID = strings.TrimSpace(req.AgentID)
	var reportingAgent model.AgentInfo
	if err := s.DB.Where("ip_addr = ?", req.TargetIP).Order("last_seen DESC").First(&reportingAgent).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "目标 Agent 不存在")
		return
	}
	if req.AgentID == "" || reportingAgent.AgentID == "" || req.AgentID != reportingAgent.AgentID || getRequestUID(c) != req.AgentID {
		s.forbid(c)
		return
	}
	now := time.Now()
	capabilities, _ := util.MarshalJSONB(req.Capabilities)
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		state := model.ContinuousAgentState{
			TargetIP: req.TargetIP, AgentID: req.AgentID, StrictCapable: req.StrictCapable,
			Capabilities: capabilities, Revision: 0, ObservedAt: now, UpdatedAt: now,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "target_ip"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"agent_id": req.AgentID, "strict_capable": req.StrictCapable,
				"capabilities": capabilities,
				"observed_at":  now, "updated_at": now,
			}),
		}).Create(&state).Error; err != nil {
			return err
		}
		if err := tx.Where("target_ip = ?", req.TargetIP).Delete(&model.ContinuousProcessSnapshot{}).Error; err != nil {
			return err
		}
		for _, process := range req.Processes {
			if process.PID <= 0 || process.ProcessStartMs <= 0 || strings.TrimSpace(process.Exe) == "" {
				continue
			}
			row := model.ContinuousProcessSnapshot{
				TargetIP: req.TargetIP, AgentID: req.AgentID, PID: process.PID,
				ProcessStartMs: process.ProcessStartMs, Comm: process.Comm,
				Exe: process.Exe, RSSBytes: process.RSSBytes, ObservedAt: now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		for _, observed := range req.Sessions {
			if strings.TrimSpace(observed.SID) == "" || !validContinuousObservedState(observed.ObservedState) {
				continue
			}
			var current model.ContinuousSession
			if err := tx.Where("sid = ? AND target_ip = ?", observed.SID, req.TargetIP).First(&current).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			// desired_state is authoritative. A delayed running/waiting report must
			// never undo a user's stop request, and an Agent cannot autonomously
			// terminate a task whose desired state is still running.
			if current.DesiredState == model.ContinuousDesiredStateStopped && observed.ObservedState != model.ContinuousObservedStateStopped {
				observed.ObservedState = model.ContinuousObservedStateStopping
				observed.ActiveProcesses = nil
			}
			if current.DesiredState == model.ContinuousDesiredStateRunning && observed.ObservedState == model.ContinuousObservedStateStopped {
				observed.ObservedState = model.ContinuousObservedStateError
				observed.LastError = firstNonEmpty(observed.LastError, "Agent stopped collection without a user stop request")
			}
			active, _ := json.Marshal(observed.ActiveProcesses)
			updates := map[string]interface{}{
				"observed_state": observed.ObservedState, "observed_at": now,
				"active_processes": active, "last_error": observed.LastError,
				"agent_id": req.AgentID, "updated_at": now,
			}
			if observed.ContinuityMode != "" {
				updates["continuity_mode"] = observed.ContinuityMode
			}
			if observed.DegradationReason != "" {
				updates["degradation_reason"] = observed.DegradationReason
			}
			if current.DesiredState == model.ContinuousDesiredStateStopped && observed.ObservedState == model.ContinuousObservedStateStopped {
				updates["status"] = model.ContinuousSessionStatusStopped
				updates["stopped_at"] = now
			}
			if err := tx.Model(&model.ContinuousSession{}).
				Where("sid = ? AND target_ip = ?", observed.SID, req.TargetIP).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "ContinuousSession 对账失败")
		return
	}
	var assignments []model.ContinuousSession
	if err := s.DB.Where("target_ip = ? AND (desired_state = ? OR (desired_state = ? AND stopped_at IS NULL))",
		req.TargetIP, model.ContinuousDesiredStateRunning, model.ContinuousDesiredStateStopped).
		Order("created_at ASC").Find(&assignments).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "读取持续采集任务失败")
		return
	}
	var authoritativeState model.ContinuousAgentState
	_ = s.DB.Where("target_ip = ?", req.TargetIP).First(&authoritativeState).Error
	revision := authoritativeState.Revision
	// 阶段一：Reconcile 下发显式 assignment DTO，signals 直接返回字符串数组。
	assignmentDTOs := continuousAssignmentDTOs(assignments)
	// 阶段五：容量暂停时通过心跳把 server_storage_pressure 推给 Agent，
	// Agent 停止产生新窗口（已产生窗口继续上报/ACK）。
	halted := s.capacityHalted()
	pressure := gin.H{"halted": halted}
	if halted {
		snap := s.currentStorageSnapshot()
		pressure["required_free_bytes"] = s.diskV2().lastRequiredFree
		pressure["available_bytes"] = snap.AvailableBytes
		pressure["reason"] = "server_storage_pressure"
	}
	s.RespondOK(c, gin.H{
		"assignments": assignmentDTOs, "revision": revision, "server_time": now,
		"server_pressure": pressure,
	})
}
