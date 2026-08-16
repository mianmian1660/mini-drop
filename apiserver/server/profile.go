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
	StartedAt            time.Time              `json:"started_at"`
	StoppedAt            *time.Time             `json:"stopped_at"`
	Capabilities         map[string]interface{} `json:"capabilities"`
}

type ProfileQuery struct {
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
	TargetID    string                 `json:"target_id"`
	Host        string                 `json:"host"`
	Service     string                 `json:"service"`
	BaseFrom    time.Time              `json:"base_from"`
	BaseTo      time.Time              `json:"base_to"`
	CompareFrom time.Time              `json:"compare_from"`
	CompareTo   time.Time              `json:"compare_to"`
	ProfileType string                 `json:"profile_type"`
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

type ProfileFlamegraph struct {
	Nodes         []ProfileNode `json:"nodes"`
	Total         float64       `json:"total"`
	Unit          string        `json:"unit"`
	Backend       string        `json:"backend,omitempty"`
	Empty         bool          `json:"empty"`
	Message       string        `json:"message"`
	Source        string        `json:"source"`
	ProfileSource string        `json:"profile_source"`
	ProfileURL    string        `json:"profile_url,omitempty"`
	Query         string        `json:"query,omitempty"`
	SymbolStatus  string        `json:"symbol_status,omitempty"`
	Truncated     bool          `json:"truncated"`
	GeneratedAt   time.Time     `json:"generated_at"`
}

type ProfileTopItem struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Self  float64 `json:"self"`
	Unit  string  `json:"unit"`
}

type ProfileTopN struct {
	Items         []ProfileTopItem `json:"items"`
	Total         float64          `json:"total"`
	Unit          string           `json:"unit"`
	Backend       string           `json:"backend,omitempty"`
	Empty         bool             `json:"empty"`
	Message       string           `json:"message"`
	Source        string           `json:"source"`
	ProfileSource string           `json:"profile_source"`
	ProfileURL    string           `json:"profile_url,omitempty"`
	Query         string           `json:"query,omitempty"`
	SymbolStatus  string           `json:"symbol_status,omitempty"`
	Truncated     bool             `json:"truncated"`
	GeneratedAt   time.Time        `json:"generated_at"`
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

var errProfileUnavailable = errors.New("native continuous profiling unavailable")

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
	if baseTop, baseFound, err := s.queryNativeContinuousTopN(c.Request.Context(), ProfileQuery{
		TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.BaseFrom, To: q.BaseTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes, OwnerUIDs: q.OwnerUIDs, CanReadAll: q.CanReadAll,
	}); err != nil {
		s.respondProfileDependencyError(c, err)
		return
	} else if compareTop, compareFound, err := s.queryNativeContinuousTopN(c.Request.Context(), ProfileQuery{
		TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.CompareFrom, To: q.CompareTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes, OwnerUIDs: q.OwnerUIDs, CanReadAll: q.CanReadAll,
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
	if !s.validateProfileQuery(c, &q) {
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
		TargetID:    strings.TrimSpace(c.Query("target_id")),
		Host:        strings.TrimSpace(c.Query("host")),
		Service:     strings.TrimSpace(c.DefaultQuery("service", "hotmethod")),
		BaseFrom:    baseFrom,
		BaseTo:      baseTo,
		CompareFrom: compareFrom,
		CompareTo:   compareTo,
		ProfileType: strings.ToLower(strings.TrimSpace(c.DefaultQuery("profile_type", "cpu"))),
		Labels:      parseProfileLabels(c.Query("labels")),
		Filters:     parseProfileFilters(c.Query("filters")),
		MaxNodes:    parseMaxNodes(c, "max_nodes"),
	}
	if !baseFrom.Before(baseTo) || !compareFrom.Before(compareTo) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "对比时间范围不合法")
		return ProfileDiffQuery{}, false
	}
	if compareTo.Sub(compareFrom) > continuousMaxQueryWindow {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument,
			"查询时间窗口过大，最大支持 6 小时，请缩小时间范围")
		return ProfileDiffQuery{}, false
	}
	pq := ProfileQuery{TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: baseFrom, To: baseTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters, MaxNodes: q.MaxNodes}
	if !s.validateProfileQuery(c, &pq) {
		return ProfileDiffQuery{}, false
	}
	q.Host, q.TargetID, q.Service, q.Labels, q.Filters = pq.Host, pq.TargetID, pq.Service, pq.Labels, pq.Filters
	q.OwnerUIDs, q.CanReadAll = pq.OwnerUIDs, pq.CanReadAll
	return q, true
}

func (s *APIServer) validateProfileQuery(c *gin.Context, q *ProfileQuery) bool {
	if q.ProfileType == "" {
		q.ProfileType = "cpu"
	}
	if q.ProfileType != "cpu" && q.ProfileType != "memory" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "不支持的 profile_type: "+q.ProfileType)
		return false
	}
	if !q.From.Before(q.To) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "时间范围不合法")
		return false
	}
	if q.To.Sub(q.From) > continuousMaxQueryWindow {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument,
			"查询时间窗口过大，最大支持 6 小时，请缩小时间范围")
		return false
	}
	if q.TargetID == "" && q.Host == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "target_id 或 host 至少提供一个")
		return false
	}
	target, err := s.resolveProfileTarget(s.AuthContext(c), q.TargetID, q.Host)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "可观测对象不存在或无权限访问")
		return false
	}
	q.TargetID = target.ID
	q.Host = target.IP
	if q.Service == "" {
		q.Service = target.ServiceName
	}
	q.Labels = mergeProfileLabels(target, q.Labels)
	q.Filters = sanitizeProfileFilters(q.Filters)
	auth := s.AuthContext(c)
	q.CanReadAll = auth.IsPlatformAdmin()
	if !q.CanReadAll {
		q.OwnerUIDs = s.visibleOwnerUIDs(auth)
		if len(q.OwnerUIDs) == 0 && auth.UID != "" {
			q.OwnerUIDs = []string{auth.UID}
		}
	}
	return true
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
	if profileType != "memory" {
		return false
	}
	s.RespondOK(c, gin.H{
		"nodes":          []ProfileNode{},
		"items":          []ProfileTopItem{},
		"total":          0,
		"unit":           "bytes",
		"empty":          true,
		"message":        "memory profiling 暂无 continuous 数据，请使用 Go pprof Heap 按需任务采集堆 profile",
		"source":         "mini-drop-native",
		"profile_source": "native",
		"memory_mode":    "go_pprof_heap_task",
		"create_url":     "/task/create?task_kind=go_pprof_heap",
		"generated_at":   time.Now(),
	})
	return true
}

func (s *APIServer) respondProfileDependencyError(c *gin.Context, err error) {
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
	if !auth.IsPlatformAdmin() && auth.UID != "" {
		if len(auth.Groups) > 0 {
			query = query.Where("(uid = ? OR uid = '' OR uid IS NULL OR gid IN ? OR gid = '' OR gid IS NULL)", auth.UID, auth.Groups)
		} else {
			query = query.Where("(uid = ? OR uid = '' OR uid IS NULL) AND (gid = '' OR gid IS NULL)", auth.UID)
		}
	}
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
	if !auth.IsPlatformAdmin() {
		owners := s.visibleOwnerUIDs(auth)
		if len(owners) > 0 {
			taskQuery = taskQuery.Where("(uid IN ? OR uid = '' OR uid IS NULL)", owners)
		} else {
			taskQuery = taskQuery.Where("(uid = '' OR uid IS NULL)")
		}
	}
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
	if !auth.IsPlatformAdmin() {
		owners := s.visibleOwnerUIDs(auth)
		if len(owners) > 0 {
			query = query.Where("(uid IN ? OR uid = '' OR uid IS NULL)", owners)
		} else {
			query = query.Where("(uid = '' OR uid IS NULL)")
		}
	}
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
	for _, key := range []string{"comm", "pid", "exe"} {
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
	for _, key := range []string{"comm", "pid", "exe"} {
		if v := labelString(filters, key); v != "" {
			out[key] = v
		}
	}
	return out
}

func isAllowedProfileFilterLabel(label string) bool {
	switch label {
	case "comm", "pid", "exe":
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
	for _, item := range compare.Items {
		baseValue := baseMap[item.Name].Value
		items = append(items, ProfileDiffItem{Name: item.Name, BaseValue: baseValue, CompareValue: item.Value, Delta: item.Value - baseValue, Unit: unit})
		seen[item.Name] = true
	}
	for _, item := range base.Items {
		if seen[item.Name] {
			continue
		}
		items = append(items, ProfileDiffItem{Name: item.Name, BaseValue: item.Value, CompareValue: 0, Delta: -item.Value, Unit: unit})
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
