package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
)

type ProfileClient interface {
	Flamegraph(context.Context, ProfileQuery) (ProfileFlamegraph, error)
	TopN(context.Context, ProfileQuery) (ProfileTopN, error)
	Diff(context.Context, ProfileDiffQuery) (ProfileDiff, error)
	LabelValues(context.Context, ProfileQuery, string) (ProfileLabelValues, error)
}

type ProfileTarget struct {
	ID                string                 `json:"id"`
	Hostname          string                 `json:"hostname"`
	IP                string                 `json:"ip"`
	ServiceName       string                 `json:"service_name"`
	Environment       string                 `json:"environment"`
	Labels            map[string]interface{} `json:"labels"`
	ProfileStatus     string                 `json:"profile_status"`
	ProfileSource     string                 `json:"profile_source"`
	ProfileURL        string                 `json:"profile_url,omitempty"`
	DropAgentStatus   string                 `json:"drop_agent_status"`
	LastProfileAt     *time.Time             `json:"last_profile_at"`
	LastSeen          *time.Time             `json:"last_seen"`
	DropAgentOnline   bool                   `json:"drop_agent_online"`
	ContinuousActive  bool                   `json:"continuous_active"`
	ContinuousSession *ContinuousSessionMeta `json:"continuous_session,omitempty"`
}

type ContinuousSessionMeta struct {
	SID                  string                 `json:"sid"`
	Name                 string                 `json:"name"`
	Status               string                 `json:"status"`
	Sampler              string                 `json:"sampler"`
	SampleRateHz         uint32                 `json:"sample_rate_hz"`
	AggregationWindowSec uint32                 `json:"aggregation_window_sec"`
	UploadBatchSec       uint32                 `json:"upload_batch_sec"`
	RetentionHours       uint32                 `json:"retention_hours"`
	LastUploadAt         *time.Time             `json:"last_upload_at"`
	AgentClockOffsetMs   int64                  `json:"agent_clock_offset_ms"`
	AgentClockStatus     string                 `json:"agent_clock_status"`
	AgentClockObservedAt *time.Time             `json:"agent_clock_observed_at"`
	StartedAt            time.Time              `json:"started_at"`
	StoppedAt            *time.Time             `json:"stopped_at"`
	Capabilities         map[string]interface{} `json:"capabilities"`
}

type ProfileQuery struct {
	SessionSID  string                 `json:"session_sid"`
	TargetID    string                 `json:"target_id"`
	Host        string                 `json:"host"`
	Service     string                 `json:"service"`
	From        time.Time              `json:"from"`
	To          time.Time              `json:"to"`
	ProfileType string                 `json:"profile_type"`
	StackScope  string                 `json:"stack_scope"`
	Labels      map[string]interface{} `json:"labels"`
	Filters     map[string]interface{} `json:"filters"`
	MaxNodes    int                    `json:"max_nodes"`
	OwnerUIDs   []string               `json:"-"`
	CanReadAll  bool                   `json:"-"`
}

type ProfileDiffQuery struct {
	SessionSID  string                 `json:"session_sid"`
	TargetID    string                 `json:"target_id"`
	Host        string                 `json:"host"`
	Service     string                 `json:"service"`
	BaseFrom    time.Time              `json:"base_from"`
	BaseTo      time.Time              `json:"base_to"`
	CompareFrom time.Time              `json:"compare_from"`
	CompareTo   time.Time              `json:"compare_to"`
	ProfileType string                 `json:"profile_type"`
	StackScope  string                 `json:"stack_scope"`
	Labels      map[string]interface{} `json:"labels"`
	Filters     map[string]interface{} `json:"filters"`
	MaxNodes    int                    `json:"max_nodes"`
	OwnerUIDs   []string               `json:"-"`
	CanReadAll  bool                   `json:"-"`
}

type ProfileNode struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Value    float64       `json:"value"`
	Self     float64       `json:"self"`
	Children []ProfileNode `json:"children,omitempty"`
}

type ProfileSymbolDiagnostics struct {
	TotalFrameWeight      float64  `json:"total_frame_weight"`
	UnresolvedFrameWeight float64  `json:"unresolved_frame_weight"`
	UnresolvedPercent     float64  `json:"unresolved_percent"`
	GoSymbolState         string   `json:"go_symbol_state"`
	Reasons               []string `json:"reasons"`
}

type ProfileRuntimeProcessDiagnostic struct {
	PID    int    `json:"pid"`
	Comm   string `json:"comm,omitempty"`
	Exe    string `json:"exe,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type ProfileRuntimeDiagnostic struct {
	Status        string                            `json:"status"`
	Modes         []string                          `json:"modes"`
	DetectedCount int                               `json:"detected_count"`
	ReadyCount    int                               `json:"ready_count"`
	MissingCount  int                               `json:"missing_count"`
	LimitedCount  int                               `json:"limited_count"`
	Reasons       []string                          `json:"reasons"`
	Processes     []ProfileRuntimeProcessDiagnostic `json:"processes"`
}

type ProfileFlamegraph struct {
	Nodes              []ProfileNode                       `json:"nodes"`
	Total              float64                             `json:"total"`
	Unit               string                              `json:"unit"`
	Backend            string                              `json:"backend,omitempty"`
	Empty              bool                                `json:"empty"`
	Message            string                              `json:"message"`
	Source             string                              `json:"source"`
	ProfileSource      string                              `json:"profile_source"`
	ProfileURL         string                              `json:"profile_url,omitempty"`
	Query              string                              `json:"query,omitempty"`
	SymbolStatus       string                              `json:"symbol_status,omitempty"`
	SymbolDiagnostics  ProfileSymbolDiagnostics            `json:"symbol_diagnostics"`
	RuntimeDiagnostics map[string]ProfileRuntimeDiagnostic `json:"runtime_diagnostics"`
	Truncated          bool                                `json:"truncated"`
	GeneratedAt        time.Time                           `json:"generated_at"`
	// Degraded 为 true 表示这段时间原始数据已经过期清理，展示的是冷层降
	// 采样摘要（火焰图场景下摘要没有调用树，实际不会有 Nodes，只用来
	// 承载 Message 提示前端引导去看 TopN）。
	Degraded bool `json:"degraded,omitempty"`
}

type ProfileTopItem struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Self  float64 `json:"self"`
	Unit  string  `json:"unit"`
}

type ProfileTopN struct {
	Items              []ProfileTopItem                    `json:"items"`
	Total              float64                             `json:"total"`
	Unit               string                              `json:"unit"`
	Backend            string                              `json:"backend,omitempty"`
	Empty              bool                                `json:"empty"`
	Message            string                              `json:"message"`
	Source             string                              `json:"source"`
	ProfileSource      string                              `json:"profile_source"`
	ProfileURL         string                              `json:"profile_url,omitempty"`
	Query              string                              `json:"query,omitempty"`
	SymbolStatus       string                              `json:"symbol_status,omitempty"`
	SymbolDiagnostics  ProfileSymbolDiagnostics            `json:"symbol_diagnostics"`
	RuntimeDiagnostics map[string]ProfileRuntimeDiagnostic `json:"runtime_diagnostics"`
	Truncated          bool                                `json:"truncated"`
	GeneratedAt        time.Time                           `json:"generated_at"`
	// Degraded 为 true 表示原始数据已过期清理，Items 来自冷层降采样摘要
	// （ContinuousWindowSummary）——只有函数级 self time 汇总，精度和口径
	// 上不等价于原始数据的 TopN（跨多个小时桶合并、可能截断过 Top 50）。
	Degraded bool `json:"degraded,omitempty"`
}

type ProfileLabelValues struct {
	Label       string    `json:"label"`
	Values      []string  `json:"values"`
	Available   bool      `json:"available"`
	Message     string    `json:"message,omitempty"`
	Source      string    `json:"source"`
	Query       string    `json:"query,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
}

type ProfileDiffItem struct {
	Name         string  `json:"name"`
	BaseValue    float64 `json:"base_value"`
	CompareValue float64 `json:"compare_value"`
	Delta        float64 `json:"delta"`
	Unit         string  `json:"unit"`
}

type ProfileDiff struct {
	Items       []ProfileDiffItem `json:"items"`
	Empty       bool              `json:"empty"`
	Message     string            `json:"message"`
	Source      string            `json:"source"`
	GeneratedAt time.Time         `json:"generated_at"`
}

// ProfileDiffNode 是差分火焰图的一个调用栈节点：base/compare 两棵树按
// (父节点路径, frame 名) 对齐后逐节点算出来的。只在某一侧出现的函数，
// 另一侧的值就是 0（纯新增/纯消失）。Value 用 inclusive（和普通火焰图
// 口径一致，树形展示天然是 inclusive，这点和 TopN 用 self 排序不同）。
type ProfileDiffNode struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	BaseValue    float64 `json:"base_value"`
	CompareValue float64 `json:"compare_value"`
	Delta        float64 `json:"delta"`
	// DeltaPercent 相对 BaseValue 的变化百分比；BaseValue 为 0（纯新增函数）
	// 时约定为 100，前端可以用它和"BaseValue==0"一起识别出"全新出现"的节点，
	// 单独着色，而不是套用普通的深浅渐变。
	DeltaPercent float64           `json:"delta_percent"`
	Children     []ProfileDiffNode `json:"children,omitempty"`
}

type ProfileDiffFlamegraph struct {
	Root         ProfileDiffNode `json:"root"`
	BaseTotal    float64         `json:"base_total"`
	CompareTotal float64         `json:"compare_total"`
	Unit         string          `json:"unit"`
	Empty        bool            `json:"empty"`
	Message      string          `json:"message"`
	Source       string          `json:"source"`
	Truncated    bool            `json:"truncated"`
	GeneratedAt  time.Time       `json:"generated_at"`
	// Degraded 为 true 表示 base/compare 至少有一侧的原始数据已经过期清理、
	// 只剩冷层 TopN 摘要（没有调用树），没法做树形 diff——这种情况 Root
	// 是空节点，前端应该引导用户退回表格 diff（现有 diffTopN 路径本来就
	// 兼容冷层摘要）。
	Degraded bool `json:"degraded,omitempty"`
}

var errProfileUnavailable = errors.New("native continuous profiling unavailable")
var errContinuousWindowLimit = errors.New("匹配的持续采集窗口超过 20000 个，请缩小时间范围")

type nativeProfileClient struct{}

func NewNativeProfileClient() ProfileClient {
	return nativeProfileClient{}
}

func (nativeProfileClient) Flamegraph(context.Context, ProfileQuery) (ProfileFlamegraph, error) {
	return emptyFlamegraph("Native Continuous Profiling 未启用或暂无 session 数据"), nil
}

func (nativeProfileClient) TopN(context.Context, ProfileQuery) (ProfileTopN, error) {
	return emptyTopN("Native Continuous Profiling 未启用或暂无 session 数据"), nil
}

func (nativeProfileClient) Diff(context.Context, ProfileDiffQuery) (ProfileDiff, error) {
	return ProfileDiff{
		Items:       []ProfileDiffItem{},
		Empty:       true,
		Message:     "Native Continuous Profiling 未启用或暂无可对比数据",
		Source:      "mini-drop-native",
		GeneratedAt: time.Now(),
	}, nil
}

func (nativeProfileClient) LabelValues(_ context.Context, _ ProfileQuery, label string) (ProfileLabelValues, error) {
	return ProfileLabelValues{
		Label:       label,
		Values:      []string{},
		Available:   false,
		Message:     "Native Continuous Profiling 暂无可用过滤标签",
		Source:      "mini-drop-native",
		GeneratedAt: time.Now(),
	}, nil
}

func (s *APIServer) ListProfileTargets(c *gin.Context) {
	targets, err := s.profileTargets(s.AuthContext(c))
	if err != nil {
		s.Logger.Error("查询 ProfileTarget 失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询可观测对象失败")
		return
	}
	s.RespondOK(c, gin.H{"targets": targets, "total": len(targets)})
}

func (s *APIServer) GetProfileFlamegraph(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	if reserved := s.respondReservedProfileType(c, q.ProfileType); reserved {
		return
	}
	if data, found, err := s.queryNativeContinuousFlamegraph(c.Request.Context(), q); err != nil {
		s.respondProfileDependencyError(c, err)
		return
	} else if found {
		s.RespondOK(c, data)
		return
	}
	if q.ProfileType == "memory" {
		data := emptyFlamegraph("尚无 Memray allocation profile；Python RSS 仍可通过 timeseries 查询")
		data.Unit = "bytes"
		data.RuntimeDiagnostics = map[string]ProfileRuntimeDiagnostic{"python": {Status: "missing", Modes: []string{"memray"}, Reasons: []string{"Mini-Drop/Memray SDK 未启用或最近没有完整 profile"}, Processes: []ProfileRuntimeProcessDiagnostic{}}}
		s.RespondOK(c, data)
		return
	}
	data, err := s.profileClient().Flamegraph(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) GetProfileTopN(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	if reserved := s.respondReservedProfileType(c, q.ProfileType); reserved {
		return
	}
	if data, found, err := s.queryNativeContinuousTopN(c.Request.Context(), q); err != nil {
		s.respondProfileDependencyError(c, err)
		return
	} else if found {
		s.RespondOK(c, data)
		return
	}
	if q.ProfileType == "memory" {
		data := emptyTopN("尚无 Memray allocation profile；Python RSS 仍可通过 timeseries 查询")
		data.Unit = "bytes"
		data.RuntimeDiagnostics = map[string]ProfileRuntimeDiagnostic{"python": {Status: "missing", Modes: []string{"memray"}, Reasons: []string{"Mini-Drop/Memray SDK 未启用或最近没有完整 profile"}, Processes: []ProfileRuntimeProcessDiagnostic{}}}
		s.RespondOK(c, data)
		return
	}
	data, err := s.profileClient().TopN(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) GetProfileDiff(c *gin.Context) {
	q, ok := s.profileDiffQueryFromRequest(c)
	if !ok {
		return
	}
	if reserved := s.respondReservedProfileType(c, q.ProfileType); reserved {
		return
	}
	if strings.EqualFold(strings.TrimSpace(c.Query("format")), "flamegraph") {
		if data, found, err := s.queryNativeContinuousDiffFlamegraph(c.Request.Context(), q); err != nil {
			s.respondProfileDependencyError(c, err)
			return
		} else if found {
			s.RespondOK(c, data)
			return
		}
		s.RespondOK(c, ProfileDiffFlamegraph{
			Empty:       true,
			Message:     "Native Continuous Profiling 未启用或暂无可对比数据",
			Source:      "mini-drop-native",
			GeneratedAt: time.Now(),
		})
		return
	}
	if baseTop, baseFound, err := s.queryNativeContinuousTopN(c.Request.Context(), ProfileQuery{
		TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.BaseFrom, To: q.BaseTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes, OwnerUIDs: q.OwnerUIDs, CanReadAll: q.CanReadAll,
		SessionSID: q.SessionSID,
		StackScope: q.StackScope,
	}); err != nil {
		s.respondProfileDependencyError(c, err)
		return
	} else if compareTop, compareFound, err := s.queryNativeContinuousTopN(c.Request.Context(), ProfileQuery{
		TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.CompareFrom, To: q.CompareTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes, OwnerUIDs: q.OwnerUIDs, CanReadAll: q.CanReadAll,
		SessionSID: q.SessionSID,
		StackScope: q.StackScope,
	}); err != nil {
		s.respondProfileDependencyError(c, err)
		return
	} else if baseFound || compareFound {
		s.RespondOK(c, diffTopN(baseTop, compareTop))
		return
	}
	data, err := s.profileClient().Diff(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) GetProfileLabelValues(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	label := strings.TrimSpace(c.Query("label"))
	if label == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "label 不能为空")
		return
	}
	if data, found, err := s.queryNativeContinuousLabelValues(c.Request.Context(), q, label); err != nil {
		s.respondProfileDependencyError(c, err)
		return
	} else if found {
		s.RespondOK(c, data)
		return
	}
	data, err := s.profileClient().LabelValues(c.Request.Context(), q, label)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) profileClient() ProfileClient {
	if s.ProfileCli != nil {
		return s.ProfileCli
	}
	return NewNativeProfileClient()
}

func (s *APIServer) profileQueryFromRequest(c *gin.Context) (ProfileQuery, bool) {
	now := time.Now()
	from, ok := parseProfileTime(c, "from", now.Add(-30*time.Minute))
	if !ok {
		return ProfileQuery{}, false
	}
	to, ok := parseProfileTime(c, "to", now)
	if !ok {
		return ProfileQuery{}, false
	}
	q := ProfileQuery{
		SessionSID:  strings.TrimSpace(c.Query("session_sid")),
		TargetID:    strings.TrimSpace(c.Query("target_id")),
		Host:        strings.TrimSpace(c.Query("host")),
		Service:     strings.TrimSpace(c.DefaultQuery("service", "hotmethod")),
		From:        from,
		To:          to,
		ProfileType: strings.ToLower(strings.TrimSpace(c.DefaultQuery("profile_type", "cpu"))),
		StackScope:  strings.ToLower(strings.TrimSpace(c.DefaultQuery("stack_scope", ""))),
		Labels:      parseProfileLabels(c.Query("labels")),
		Filters:     parseProfileFilters(c.Query("filters")),
		MaxNodes:    parseMaxNodes(c, "max_nodes"),
	}
	if _, ok := s.validateProfileQuery(c, &q); !ok {
		return ProfileQuery{}, false
	}
	return q, true
}

func (s *APIServer) profileDiffQueryFromRequest(c *gin.Context) (ProfileDiffQuery, bool) {
	now := time.Now()
	baseFrom, ok := parseProfileTime(c, "base_from", now.Add(-60*time.Minute))
	if !ok {
		return ProfileDiffQuery{}, false
	}
	baseTo, ok := parseProfileTime(c, "base_to", now.Add(-30*time.Minute))
	if !ok {
		return ProfileDiffQuery{}, false
	}
	compareFrom, ok := parseProfileTime(c, "compare_from", now.Add(-30*time.Minute))
	if !ok {
		return ProfileDiffQuery{}, false
	}
	compareTo, ok := parseProfileTime(c, "compare_to", now)
	if !ok {
		return ProfileDiffQuery{}, false
	}
	q := ProfileDiffQuery{
		SessionSID:  strings.TrimSpace(c.Query("session_sid")),
		TargetID:    strings.TrimSpace(c.Query("target_id")),
		Host:        strings.TrimSpace(c.Query("host")),
		Service:     strings.TrimSpace(c.DefaultQuery("service", "hotmethod")),
		BaseFrom:    baseFrom,
		BaseTo:      baseTo,
		CompareFrom: compareFrom,
		CompareTo:   compareTo,
		ProfileType: strings.ToLower(strings.TrimSpace(c.DefaultQuery("profile_type", "cpu"))),
		StackScope:  strings.ToLower(strings.TrimSpace(c.DefaultQuery("stack_scope", ""))),
		Labels:      parseProfileLabels(c.Query("labels")),
		Filters:     parseProfileFilters(c.Query("filters")),
		MaxNodes:    parseMaxNodes(c, "max_nodes"),
	}
	pq := ProfileQuery{SessionSID: q.SessionSID, TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: baseFrom, To: baseTo, ProfileType: q.ProfileType, StackScope: q.StackScope, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes}
	target, ok := s.validateProfileQuery(c, &pq)
	if !ok {
		return ProfileDiffQuery{}, false
	}
	if !s.validateProfileTimeRange(c, compareFrom, compareTo, profileRetentionDuration(target)) {
		return ProfileDiffQuery{}, false
	}
	if baseTo.Sub(baseFrom) != compareTo.Sub(compareFrom) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "Baseline 与 Compare 必须使用等长时间窗")
		return ProfileDiffQuery{}, false
	}
	q.Host, q.TargetID, q.Service, q.Labels, q.Filters = pq.Host, pq.TargetID, pq.Service, pq.Labels, pq.Filters
	q.StackScope = pq.StackScope
	q.SessionSID = pq.SessionSID
	q.OwnerUIDs, q.CanReadAll = pq.OwnerUIDs, pq.CanReadAll
	return q, true
}

func (s *APIServer) validateProfileQuery(c *gin.Context, q *ProfileQuery) (ProfileTarget, bool) {
	if q.ProfileType == "" {
		q.ProfileType = "cpu"
	}
	if q.ProfileType != "cpu" && q.ProfileType != "memory" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "不支持的 profile_type: "+q.ProfileType)
		return ProfileTarget{}, false
	}
	if q.TargetID == "" && q.Host == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "target_id 或 host 至少提供一个")
		return ProfileTarget{}, false
	}
	target, err := s.resolveProfileTarget(s.AuthContext(c), q.TargetID, q.Host)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "可观测对象不存在或无权限访问")
		return ProfileTarget{}, false
	}
	retention := profileRetentionDuration(target)
	if q.SessionSID != "" {
		var session model.ContinuousSession
		if err := s.DB.Where("sid = ?", q.SessionSID).First(&session).Error; err != nil || !s.canReadOwner(session.UID, s.AuthContext(c)) {
			s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "持续采集任务不存在或无权限访问")
			return ProfileTarget{}, false
		}
		if session.TargetIP != target.IP {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "session_sid 不属于当前主机")
			return ProfileTarget{}, false
		}
		retention = time.Duration(firstNonZeroUint32(session.RetentionHours, 24)) * time.Hour
		target.ContinuousSession = continuousSessionMeta(session)
	}
	if !s.validateProfileTimeRange(c, q.From, q.To, retention) {
		return ProfileTarget{}, false
	}
	q.TargetID = target.ID
	q.Host = target.IP
	if q.Service == "" {
		q.Service = target.ServiceName
	}
	q.Labels = mergeProfileLabels(target, q.Labels)
	q.Filters = sanitizeProfileFilters(q.Filters)
	q.CanReadAll = true
	q.OwnerUIDs = nil
	return target, true
}

func profileRetentionDuration(target ProfileTarget) time.Duration {
	hours := uint32(24)
	if target.ContinuousSession != nil && target.ContinuousSession.RetentionHours > 0 {
		hours = target.ContinuousSession.RetentionHours
	}
	return time.Duration(hours) * time.Hour
}

func (s *APIServer) validateProfileTimeRange(c *gin.Context, from, to time.Time, retention time.Duration) bool {
	if !from.Before(to) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "时间范围不合法")
		return false
	}
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	if to.Sub(from) > retention {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument,
			fmt.Sprintf("查询时间窗口过大，当前 Session 最多支持 %s，请缩小时间范围", formatProfileDuration(retention)))
		return false
	}
	now := time.Now()
	if to.After(now.Add(time.Minute)) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "结束时间不能晚于当前时间")
		return false
	}
	if from.Before(now.Add(-retention).Add(-time.Minute)) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument,
			fmt.Sprintf("开始时间已超出当前 Session 的 %s 数据保留期", formatProfileDuration(retention)))
		return false
	}
	return true
}

func formatProfileDuration(value time.Duration) string {
	if value%time.Hour == 0 {
		return fmt.Sprintf("%d 小时", int(value/time.Hour))
	}
	return value.String()
}

func parseMaxNodes(c *gin.Context, name string) int {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	if n > continuousMaxNodesCap {
		return continuousMaxNodesCap
	}
	return n
}

func (s *APIServer) respondReservedProfileType(c *gin.Context, profileType string) bool {
	return false
}

func (s *APIServer) respondProfileDependencyError(c *gin.Context, err error) {
	if errors.Is(err, errContinuousWindowLimit) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, err.Error())
		return
	}
	s.Logger.Warn("Native Continuous Profiling 查询失败", zap.Error(err))
	s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, err.Error())
}

func parseProfileTime(c *gin.Context, name string, fallback time.Time) (time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if unix > 1_000_000_000_000 {
			return time.UnixMilli(unix), true
		}
		return time.Unix(unix, 0), true
	}
	c.JSON(http.StatusBadRequest, gin.H{
		"request_id": requestIDFromGin(c),
		"data":       nil,
		"error":      APIError{Code: ErrCodeTaskInvalidArgument, Message: name + " 必须是 RFC3339 或 Unix 时间戳", Retryable: false, Stage: "dispatch"},
		"code":       http.StatusBadRequest,
		"message":    name + " 必须是 RFC3339 或 Unix 时间戳",
	})
	return time.Time{}, false
}

func parseProfileLabels(raw string) map[string]interface{} {
	labels := map[string]interface{}{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return labels
	}
	_ = json.Unmarshal([]byte(raw), &labels)
	return labels
}

func parseProfileFilters(raw string) map[string]interface{} {
	filters := map[string]interface{}{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return filters
	}
	_ = json.Unmarshal([]byte(raw), &filters)
	return filters
}

func (s *APIServer) resolveProfileTarget(auth AuthContext, targetID, host string) (ProfileTarget, error) {
	targets, err := s.profileTargets(auth)
	if err != nil {
		return ProfileTarget{}, err
	}
	for _, target := range targets {
		if targetID != "" && target.ID == targetID {
			return target, nil
		}
		if host != "" && (target.IP == host || target.Hostname == host) {
			return target, nil
		}
	}
	return ProfileTarget{}, gorm.ErrRecordNotFound
}

func (s *APIServer) profileTargets(auth AuthContext) ([]ProfileTarget, error) {
	byKey := map[string]*ProfileTarget{}
	var agents []model.AgentInfo
	query := s.DB.Order("last_seen DESC")
	if err := query.Find(&agents).Error; err != nil {
		return nil, err
	}
	for _, agent := range agents {
		if !s.canReadAgent(agent, auth) {
			continue
		}
		lastSeen := agent.LastSeen
		labels := profileLabelsFromAgent(agent.Labels)
		target := &ProfileTarget{
			ID:              profileTargetID(agent.IPAddr, "hotmethod"),
			Hostname:        firstNonEmpty(agent.Hostname, agent.IPAddr),
			IP:              agent.IPAddr,
			ServiceName:     "hotmethod",
			Environment:     firstNonEmpty(labelString(labels, "env"), s.defaultProfileEnvironment()),
			Labels:          labels,
			ProfileStatus:   "no_session",
			ProfileSource:   "native",
			DropAgentStatus: map[bool]string{true: "online", false: "offline"}[agent.Online],
			LastSeen:        &lastSeen,
			DropAgentOnline: agent.Online,
		}
		target.Labels = profileTargetLabels(*target, labels)
		byKey[profileTargetKey(agent)] = target
	}

	var recentTasks []model.HotmethodTask
	taskQuery := s.DB.Model(&model.HotmethodTask{}).
		Select("target_ip, create_time").
		Where("target_ip != ''")
	if err := taskQuery.Order("create_time DESC").Limit(500).Find(&recentTasks).Error; err != nil {
		return nil, err
	}
	seenTaskTargets := map[string]bool{}
	for _, task := range recentTasks {
		if task.TargetIP == "" || seenTaskTargets[task.TargetIP] {
			continue
		}
		seenTaskTargets[task.TargetIP] = true
		if target, ok := findProfileTargetByIP(byKey, task.TargetIP); ok {
			lastAt := task.CreateTime
			target.LastProfileAt = &lastAt
			continue
		}
		lastAt := task.CreateTime
		byKey["task:"+task.TargetIP] = &ProfileTarget{
			ID:              profileTargetID(task.TargetIP, "hotmethod"),
			Hostname:        task.TargetIP,
			IP:              task.TargetIP,
			ServiceName:     "hotmethod",
			Environment:     s.defaultProfileEnvironment(),
			Labels:          map[string]interface{}{},
			ProfileStatus:   "no_session",
			ProfileSource:   "native",
			DropAgentStatus: "unknown",
			LastProfileAt:   &lastAt,
		}
		byKey["task:"+task.TargetIP].Labels = profileTargetLabels(*byKey["task:"+task.TargetIP], byKey["task:"+task.TargetIP].Labels)
	}

	s.attachContinuousSessionStatus(byKey, auth)

	out := make([]ProfileTarget, 0, len(byKey))
	for _, target := range byKey {
		out = append(out, *target)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContinuousActive != out[j].ContinuousActive {
			return out[i].ContinuousActive
		}
		if out[i].DropAgentOnline != out[j].DropAgentOnline {
			return out[i].DropAgentOnline
		}
		if out[i].LastSeen != nil && out[j].LastSeen != nil {
			return out[i].LastSeen.After(*out[j].LastSeen)
		}
		return out[i].IP < out[j].IP
	})
	return out, nil
}

func (s *APIServer) attachContinuousSessionStatus(targets map[string]*ProfileTarget, auth AuthContext) {
	var sessions []model.ContinuousSession
	query := s.DB.Where("deleted_at IS NULL")
	if err := query.Order("updated_at DESC").Find(&sessions).Error; err != nil {
		return
	}
	attached := map[string]bool{}
	for _, session := range sessions {
		target, ok := findProfileTargetByIP(targets, session.TargetIP)
		if !ok && session.Status != model.ContinuousSessionStatusRunning {
			continue
		}
		if !ok {
			target = &ProfileTarget{
				ID:              profileTargetID(session.TargetIP, firstNonEmpty(session.ServiceName, "hotmethod")),
				Hostname:        firstNonEmpty(session.Hostname, session.TargetIP),
				IP:              session.TargetIP,
				ServiceName:     firstNonEmpty(session.ServiceName, "hotmethod"),
				Environment:     s.defaultProfileEnvironment(),
				Labels:          map[string]interface{}{},
				ProfileSource:   "native",
				DropAgentStatus: "unknown",
			}
			targets["session:"+session.SID] = target
		}
		if attached[target.ID] {
			continue
		}
		target.ProfileSource = "native"
		target.ProfileStatus = session.Status
		target.ContinuousActive = session.Status == model.ContinuousSessionStatusRunning
		target.ProfileURL = "/continuous/sessions/" + session.SID
		target.ContinuousSession = continuousSessionMeta(session)
		if session.LastUploadAt != nil {
			target.LastProfileAt = session.LastUploadAt
		}
		if len(session.Labels) > 0 {
			target.Labels = mergeProfileLabels(*target, profileLabelsFromAgent(session.Labels))
		}
		attached[target.ID] = true
	}
}

func continuousSessionMeta(session model.ContinuousSession) *ContinuousSessionMeta {
	capabilities := map[string]interface{}{}
	if len(session.Capabilities) > 0 {
		_ = json.Unmarshal(session.Capabilities, &capabilities)
	}
	return &ContinuousSessionMeta{
		SID:                  session.SID,
		Name:                 session.Name,
		Status:               session.Status,
		Sampler:              firstNonEmpty(labelString(capabilities, "sampler"), "perf_event"),
		SampleRateHz:         firstNonZeroUint32(session.SampleRateHz, 19),
		AggregationWindowSec: firstNonZeroUint32(session.AggregationWindowSec, 10),
		UploadBatchSec:       firstNonZeroUint32(session.UploadBatchSec, 60),
		RetentionHours:       firstNonZeroUint32(session.RetentionHours, 24),
		LastUploadAt:         session.LastUploadAt,
		AgentClockOffsetMs:   session.AgentClockOffsetMs,
		AgentClockStatus:     continuousSessionClockStatus(session),
		AgentClockObservedAt: session.AgentClockObservedAt,
		StartedAt:            session.StartedAt,
		StoppedAt:            session.StoppedAt,
		Capabilities:         capabilities,
	}
}

func profileTargetKey(agent model.AgentInfo) string {
	if strings.TrimSpace(agent.AgentID) != "" {
		return "agent:" + strings.TrimSpace(agent.AgentID)
	}
	return "ip:" + strings.TrimSpace(agent.IPAddr)
}

func findProfileTargetByIP(targets map[string]*ProfileTarget, ip string) (*ProfileTarget, bool) {
	for _, target := range targets {
		if target.IP == ip {
			return target, true
		}
	}
	return nil, false
}

func profileTargetID(ip, service string) string {
	return strings.ReplaceAll(strings.TrimSpace(ip), ":", "_") + ":" + firstNonEmpty(service, "hotmethod")
}

func profileLabelSelector(q ProfileQuery) string {
	labels := mergeProfileLabels(ProfileTarget{IP: q.Host, ServiceName: q.Service, Labels: q.Labels}, q.Labels)
	parts := make([]string, 0, len(labels)+len(q.Filters))
	for _, key := range []string{"comm", "pid", "process_start_ms", "exe", "runtime"} {
		if v := labelString(q.Filters, key); v != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", key, v))
		}
	}
	for _, key := range []string{"instance", "job", "env", "node"} {
		if v := labelString(labels, key); v != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", key, v))
		}
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func mergeProfileLabels(target ProfileTarget, labels map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range target.Labels {
		out[k] = v
	}
	for k, v := range labels {
		out[k] = v
	}
	if target.ServiceName != "" {
		out["job"] = target.ServiceName
	}
	if target.IP != "" {
		out["instance"] = target.IP
	}
	return out
}

func profileTargetLabels(target ProfileTarget, labels map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range labels {
		out[k] = v
	}
	if target.ServiceName != "" {
		if _, ok := out["job"]; !ok {
			out["job"] = target.ServiceName
		}
	}
	if target.IP != "" {
		if _, ok := out["instance"]; !ok {
			out["instance"] = target.IP
		}
	}
	if target.Hostname != "" {
		if _, ok := out["node"]; !ok {
			out["node"] = target.Hostname
		}
	}
	if target.Environment != "" {
		if _, ok := out["env"]; !ok {
			out["env"] = target.Environment
		}
	}
	return out
}

func sanitizeProfileFilters(filters map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, key := range []string{"comm", "pid", "process_start_ms", "exe", "runtime"} {
		if v := labelString(filters, key); v != "" {
			out[key] = v
		}
	}
	return out
}

func isAllowedProfileFilterLabel(label string) bool {
	switch label {
	case "comm", "pid", "process_start_ms", "exe", "runtime":
		return true
	default:
		return false
	}
}

func profileLabelsFromAgent(raw []byte) map[string]interface{} {
	labels := map[string]interface{}{}
	if len(raw) == 0 || string(raw) == "null" {
		return labels
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err == nil {
		for k, v := range obj {
			if strings.TrimSpace(k) != "" {
				labels[k] = v
			}
		}
		return labels
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, item := range arr {
			parts := strings.SplitN(item, "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" {
				labels[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}
	return labels
}

func labelString(labels map[string]interface{}, key string) string {
	if labels == nil {
		return ""
	}
	value, ok := labels[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func emptyFlamegraph(message string) ProfileFlamegraph {
	return ProfileFlamegraph{
		Nodes:         []ProfileNode{},
		Total:         0,
		Unit:          "samples",
		Empty:         true,
		Message:       message,
		Source:        "mini-drop-native",
		ProfileSource: "native",
		GeneratedAt:   time.Now(),
	}
}

func emptyTopN(message string) ProfileTopN {
	return ProfileTopN{
		Items:         []ProfileTopItem{},
		Total:         0,
		Unit:          "samples",
		Empty:         true,
		Message:       message,
		Source:        "mini-drop-native",
		ProfileSource: "native",
		GeneratedAt:   time.Now(),
	}
}

func diffTopN(base, compare ProfileTopN) ProfileDiff {
	baseMap := map[string]ProfileTopItem{}
	for _, item := range base.Items {
		baseMap[item.Name] = item
	}
	items := []ProfileDiffItem{}
	seen := map[string]bool{}
	unit := firstNonEmpty(base.Unit, compare.Unit, "samples")
	// 按栈顶（self）对比，不是 value（inclusive）——道理同 queryNativeContinuousTopN 的排序：
	// diff 要回答"哪个函数自己变热/变冷了"，用 inclusive 值会被调用链形状变化干扰。
	for _, item := range compare.Items {
		baseValue := baseMap[item.Name].Self
		items = append(items, ProfileDiffItem{Name: item.Name, BaseValue: baseValue, CompareValue: item.Self, Delta: item.Self - baseValue, Unit: unit})
		seen[item.Name] = true
	}
	for _, item := range base.Items {
		if seen[item.Name] {
			continue
		}
		items = append(items, ProfileDiffItem{Name: item.Name, BaseValue: item.Self, CompareValue: 0, Delta: -item.Self, Unit: unit})
	}
	sort.Slice(items, func(i, j int) bool {
		return absFloat(items[i].Delta) > absFloat(items[j].Delta)
	})
	return ProfileDiff{
		Items:       items,
		Empty:       len(items) == 0,
		Message:     map[bool]string{true: "暂无可对比数据", false: ""}[len(items) == 0],
		Source:      "mini-drop-native",
		GeneratedAt: time.Now(),
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonZeroUint32(value uint32, fallback uint32) uint32 {
	if value != 0 {
		return value
	}
	return fallback
}

func (s *APIServer) defaultProfileEnvironment() string {
	if s == nil || s.Config == nil || strings.TrimSpace(s.Config.Security.Environment) == "" {
		return "development"
	}
	return s.Config.Security.Environment
}

func profileTimeoutSec(s *APIServer) int {
	if s == nil || s.Config == nil || s.Config.Profile.TimeoutSec <= 0 {
		return 5
	}
	return s.Config.Profile.TimeoutSec
}
