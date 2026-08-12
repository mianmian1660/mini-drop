package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	parcapb "github.com/mini-drop/apiserver/proto/parca/query/v1alpha1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

type ProfileClient interface {
	Flamegraph(context.Context, ProfileQuery) (ProfileFlamegraph, error)
	TopN(context.Context, ProfileQuery) (ProfileTopN, error)
	Diff(context.Context, ProfileDiffQuery) (ProfileDiff, error)
	LabelValues(context.Context, ProfileQuery, string) (ProfileLabelValues, error)
}

type ProfileStatusClient interface {
	TargetStatus(context.Context, ProfileTarget) ProfileTargetStatus
}

type ProfileTargetStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type ProfileTarget struct {
	ID                 string                 `json:"id"`
	Hostname           string                 `json:"hostname"`
	IP                 string                 `json:"ip"`
	ServiceName        string                 `json:"service_name"`
	Environment        string                 `json:"environment"`
	Labels             map[string]interface{} `json:"labels"`
	ParcaUIURL         string                 `json:"parca_ui_url"`
	ParcaQueryURL      string                 `json:"parca_query_url"`
	ParcaError         string                 `json:"parca_error,omitempty"`
	PprofScrapeStatus  string                 `json:"pprof_scrape_status"`
	PprofScrapeMessage string                 `json:"pprof_scrape_message"`
	PprofScrapeTargets []string               `json:"pprof_scrape_targets"`
	ParcaAgentStatus   string                 `json:"parca_agent_status"`
	DropAgentStatus    string                 `json:"drop_agent_status"`
	LastProfileAt      *time.Time             `json:"last_profile_at"`
	LastSeen           *time.Time             `json:"last_seen"`
	DropAgentOnline    bool                   `json:"drop_agent_online"`
}

type ProfileQuery struct {
	TargetID    string                 `json:"target_id"`
	Host        string                 `json:"host"`
	Service     string                 `json:"service"`
	From        time.Time              `json:"from"`
	To          time.Time              `json:"to"`
	ProfileType string                 `json:"profile_type"`
	Labels      map[string]interface{} `json:"labels"`
	Filters     map[string]interface{} `json:"filters"`
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
}

type ProfileNode struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Value    float64       `json:"value"`
	Self     float64       `json:"self"`
	Children []ProfileNode `json:"children,omitempty"`
}

type ProfileFlamegraph struct {
	Nodes       []ProfileNode `json:"nodes"`
	Total       float64       `json:"total"`
	Unit        string        `json:"unit"`
	Empty       bool          `json:"empty"`
	Message     string        `json:"message"`
	Source      string        `json:"source"`
	ParcaURL    string        `json:"parca_url,omitempty"`
	Query       string        `json:"query,omitempty"`
	GeneratedAt time.Time     `json:"generated_at"`
}

type ProfileTopItem struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Self  float64 `json:"self"`
	Unit  string  `json:"unit"`
}

type ProfileTopN struct {
	Items       []ProfileTopItem `json:"items"`
	Total       float64          `json:"total"`
	Unit        string           `json:"unit"`
	Empty       bool             `json:"empty"`
	Message     string           `json:"message"`
	Source      string           `json:"source"`
	ParcaURL    string           `json:"parca_url,omitempty"`
	Query       string           `json:"query,omitempty"`
	GeneratedAt time.Time        `json:"generated_at"`
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

type parcaProfileClient struct {
	baseURL      string
	uiURL        string
	grpcAddr     string
	timeout      time.Duration
	enabled      bool
	client       *http.Client
	cpuProfileID string
	dialOptions  []grpc.DialOption
}

var errProfileUnavailable = errors.New("profile dependency unavailable")

const defaultCPUProfileType = "parca_agent:samples:count:cpu:nanoseconds:delta"
const processCPUProfileType = "process_cpu:samples:count:cpu:nanoseconds:delta"

func NewHTTPProfileClient(cfg config.ProfileConfig) ProfileClient {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	baseURL := strings.TrimRight(cfg.ParcaURL, "/")
	if baseURL == "" && strings.TrimSpace(cfg.ParcaGRPCAddr) != "" {
		baseURL = "http://" + strings.TrimSpace(cfg.ParcaGRPCAddr)
	}
	uiURL := strings.TrimRight(strings.TrimSpace(cfg.ParcaUIURL), "/")
	if uiURL == "" {
		uiURL = baseURL
	}
	return &parcaProfileClient{
		baseURL:      baseURL,
		uiURL:        uiURL,
		grpcAddr:     strings.TrimSpace(cfg.ParcaGRPCAddr),
		timeout:      timeout,
		enabled:      cfg.Enabled && (baseURL != "" || strings.TrimSpace(cfg.ParcaGRPCAddr) != ""),
		client:       &http.Client{Timeout: timeout},
		cpuProfileID: defaultCPUProfileType,
	}
}

func (c *parcaProfileClient) Flamegraph(ctx context.Context, q ProfileQuery) (ProfileFlamegraph, error) {
	if c == nil || !c.enabled {
		return emptyFlamegraph("Parca 未配置，持续画像暂无数据"), nil
	}
	query, err := c.queryString(ctx, q)
	if err != nil {
		return ProfileFlamegraph{}, err
	}
	response, err := c.queryFlamegraph(ctx, q, query)
	if err != nil {
		return ProfileFlamegraph{}, err
	}
	out := parcaFlamegraphResponseToMiniDrop(response)
	out.Query = query
	out.ParcaURL = c.queryURL(q, query)
	return out, nil
}

func (c *parcaProfileClient) TopN(ctx context.Context, q ProfileQuery) (ProfileTopN, error) {
	if c == nil || !c.enabled {
		return emptyTopN("Parca 未配置，持续画像暂无数据"), nil
	}
	query, err := c.queryString(ctx, q)
	if err != nil {
		return ProfileTopN{}, err
	}
	response, err := c.queryTop(ctx, q, query)
	if err != nil {
		return ProfileTopN{}, err
	}
	out := parcaTopResponseToMiniDrop(response)
	out.Query = query
	out.ParcaURL = c.queryURL(q, query)
	return out, nil
}

func (c *parcaProfileClient) Diff(ctx context.Context, q ProfileDiffQuery) (ProfileDiff, error) {
	if c == nil || !c.enabled {
		return ProfileDiff{Empty: true, Message: "Parca 未配置，持续画像暂无数据", Source: "mini-drop", GeneratedAt: time.Now()}, nil
	}
	base, err := c.TopN(ctx, ProfileQuery{TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.BaseFrom, To: q.BaseTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters})
	if err != nil {
		return ProfileDiff{}, err
	}
	compare, err := c.TopN(ctx, ProfileQuery{TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: q.CompareFrom, To: q.CompareTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters})
	if err != nil {
		return ProfileDiff{}, err
	}
	return diffTopN(base, compare), nil
}

func (c *parcaProfileClient) LabelValues(ctx context.Context, q ProfileQuery, label string) (ProfileLabelValues, error) {
	label = strings.TrimSpace(label)
	out := ProfileLabelValues{
		Label:       label,
		Values:      []string{},
		Source:      "parca",
		Query:       parcaQueryLabelSelector(ProfileQuery{Labels: q.Labels}),
		GeneratedAt: time.Now(),
	}
	if c == nil || !c.enabled {
		out.Message = "Parca 未配置，暂无可用过滤标签"
		return out, nil
	}
	if !isAllowedProfileFilterLabel(label) {
		out.Message = "不支持的 profile 过滤标签: " + label
		return out, nil
	}
	profileType, err := c.parcaProfileType(ctx, q.ProfileType, q.From, q.To)
	if err != nil {
		return ProfileLabelValues{}, err
	}
	client, closeConn, err := c.queryService(ctx)
	if err != nil {
		return ProfileLabelValues{}, err
	}
	defer closeConn()
	baseSelector := parcaQueryLabelSelector(ProfileQuery{Labels: q.Labels})
	labelsOut, err := client.Labels(ctx, &parcapb.LabelsRequest{
		Start:       timestampOrNil(q.From),
		End:         timestampOrNil(q.To),
		ProfileType: profileType,
	})
	if err != nil {
		return ProfileLabelValues{}, fmt.Errorf("%w: Parca Labels: %v", errProfileUnavailable, err)
	}
	if !stringSliceContains(labelsOut.GetLabelNames(), label) {
		out.Message = "当前数据源没有 " + label + " 标签，只能查看主机级 profile"
		return out, nil
	}
	values, err := client.Values(ctx, &parcapb.ValuesRequest{
		LabelName:   label,
		Match:       []string{baseSelector},
		Start:       timestampOrNil(q.From),
		End:         timestampOrNil(q.To),
		ProfileType: profileType,
	})
	if err != nil {
		return ProfileLabelValues{}, fmt.Errorf("%w: Parca Values: %v", errProfileUnavailable, err)
	}
	out.Values = uniqueSortedStrings(values.GetLabelValues())
	out.Available = true
	return out, nil
}

func (c *parcaProfileClient) TargetStatus(ctx context.Context, target ProfileTarget) ProfileTargetStatus {
	if c == nil || !c.enabled {
		return ProfileTargetStatus{Status: "unconfigured"}
	}
	status, err := c.targetSampleStatus(ctx, target)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, errProfileUnavailable) {
			return ProfileTargetStatus{Status: "parca_unreachable", Error: msg}
		}
		return ProfileTargetStatus{Status: "query_unsupported", Error: msg}
	}
	return ProfileTargetStatus{Status: status}
}

func (c *parcaProfileClient) queryURL(q ProfileQuery, query string) string {
	if c == nil || c.baseURL == "" {
		return ""
	}
	if query == "" {
		query = defaultCPUProfileType + parcaQueryLabelSelector(q)
	}
	values := url.Values{}
	values.Set("expression_a", query)
	values.Set("from_a", q.From.Format(time.RFC3339))
	values.Set("to_a", q.To.Format(time.RFC3339))
	values.Set("time_selection_a", "custom")
	return c.uiURL + "/?" + values.Encode()
}

func (c *parcaProfileClient) queryReport(ctx context.Context, q ProfileQuery, reportType string, dst *parcaQueryResponse) error {
	query, err := c.queryString(ctx, q)
	if err != nil {
		return err
	}
	values := url.Values{}
	values.Set("mode", "MODE_MERGE")
	values.Set("merge.query", query)
	values.Set("merge.start", q.From.Format(time.RFC3339))
	values.Set("merge.end", q.To.Format(time.RFC3339))
	values.Set("report_type", reportType)
	values.Set("reportType", reportType)
	values.Set("node_trim_threshold", "0")
	values.Set("nodeTrimThreshold", "0")
	return c.getJSON(ctx, "/profiles/query", values, dst)
}

func (c *parcaProfileClient) queryString(ctx context.Context, q ProfileQuery) (string, error) {
	profileType, err := c.parcaProfileType(ctx, q.ProfileType, q.From, q.To)
	if err != nil {
		return "", err
	}
	return profileType + parcaQueryLabelSelector(q), nil
}

func (c *parcaProfileClient) parcaProfileType(ctx context.Context, profileType string, from, to time.Time) (string, error) {
	if strings.EqualFold(profileType, "cpu") || strings.TrimSpace(profileType) == "" {
		id, err := c.discoverCPUProfileType(ctx, from, to)
		if err != nil {
			return "", err
		}
		return id, nil
	}
	return profileType, nil
}

func (c *parcaProfileClient) discoverCPUProfileType(ctx context.Context, from, to time.Time) (string, error) {
	client, closeConn, err := c.queryService(ctx)
	if err != nil {
		return c.cpuProfileID, nil
	}
	defer closeConn()
	out, err := client.ProfileTypes(ctx, &parcapb.ProfileTypesRequest{
		Start: timestampOrNil(from),
		End:   timestampOrNil(to),
	})
	if err != nil {
		return c.cpuProfileID, nil
	}
	available := map[string]bool{}
	for _, typ := range out.GetTypes() {
		id := parcaProfileTypeID(typ)
		available[id] = true
	}
	if available[c.cpuProfileID] {
		return c.cpuProfileID, nil
	}
	if available[processCPUProfileType] {
		return processCPUProfileType, nil
	}
	for id := range available {
		blob := strings.ToLower(id)
		if strings.Contains(blob, "cpu") && strings.TrimSpace(id) != "" {
			return id, nil
		}
	}
	return c.cpuProfileID, nil
}

func parcaProfileTypeID(typ *parcapb.ProfileType) string {
	if typ == nil {
		return ""
	}
	parts := []string{typ.GetName(), typ.GetSampleType(), typ.GetSampleUnit(), typ.GetPeriodType(), typ.GetPeriodUnit()}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return strings.TrimSpace(typ.GetName())
		}
	}
	id := strings.Join(parts, ":")
	if typ.GetDelta() {
		id += ":delta"
	}
	return id
}

func (c *parcaProfileClient) hasProfileData(ctx context.Context) (bool, error) {
	client, closeConn, err := c.queryService(ctx)
	if err != nil {
		return false, err
	}
	defer closeConn()
	out, err := client.ProfileTypes(ctx, &parcapb.ProfileTypesRequest{})
	if err != nil {
		return false, fmt.Errorf("%w: Parca ProfileTypes: %v", errProfileUnavailable, err)
	}
	return len(out.GetTypes()) > 0, nil
}

func (c *parcaProfileClient) queryTop(ctx context.Context, q ProfileQuery, query string) (*parcapb.QueryResponse, error) {
	client, closeConn, err := c.queryService(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	out, err := client.Query(ctx, &parcapb.QueryRequest{
		Mode: parcapb.QueryRequest_MODE_MERGE,
		Options: &parcapb.QueryRequest_Merge{Merge: &parcapb.MergeProfile{
			Query: query,
			Start: timestampOrNil(q.From),
			End:   timestampOrNil(q.To),
		}},
		ReportType:        parcapb.QueryRequest_REPORT_TYPE_TOP,
		NodeTrimThreshold: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: Parca Query: %v", errProfileUnavailable, err)
	}
	return out, nil
}

func (c *parcaProfileClient) queryFlamegraph(ctx context.Context, q ProfileQuery, query string) (*parcapb.QueryResponse, error) {
	client, closeConn, err := c.queryService(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	out, err := client.Query(ctx, &parcapb.QueryRequest{
		Mode: parcapb.QueryRequest_MODE_MERGE,
		Options: &parcapb.QueryRequest_Merge{Merge: &parcapb.MergeProfile{
			Query: query,
			Start: timestampOrNil(q.From),
			End:   timestampOrNil(q.To),
		}},
		ReportType:        parcapb.QueryRequest_REPORT_TYPE_FLAMEGRAPH_TABLE,
		NodeTrimThreshold: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: Parca Flamegraph Query: %v", errProfileUnavailable, err)
	}
	return out, nil
}

func (c *parcaProfileClient) targetSampleStatus(ctx context.Context, target ProfileTarget) (string, error) {
	labels := mergeProfileLabels(target, target.Labels)
	instance := labelString(labels, "instance")
	if instance == "" {
		instance = target.IP
	}
	profileType, err := c.parcaProfileType(ctx, "cpu", time.Now().Add(-30*time.Minute), time.Now())
	if err != nil {
		return "", err
	}
	client, closeConn, err := c.queryService(ctx)
	if err != nil {
		return "", err
	}
	defer closeConn()
	job := firstNonEmpty(labelString(labels, "job"), target.ServiceName, "hotmethod")
	jobs, err := client.Values(ctx, &parcapb.ValuesRequest{
		LabelName:   "job",
		Start:       timestamppb.New(time.Now().Add(-30 * time.Minute)),
		End:         timestamppb.Now(),
		ProfileType: profileType,
	})
	if err != nil {
		return "", fmt.Errorf("%w: Parca Values: %v", errProfileUnavailable, err)
	}
	if !stringSliceContains(jobs.GetLabelValues(), job) {
		return "online_no_samples", nil
	}
	instances, err := client.Values(ctx, &parcapb.ValuesRequest{
		LabelName:   "instance",
		Start:       timestamppb.New(time.Now().Add(-30 * time.Minute)),
		End:         timestamppb.Now(),
		ProfileType: profileType,
	})
	if err != nil {
		return "", fmt.Errorf("%w: Parca Values: %v", errProfileUnavailable, err)
	}
	if stringSliceContains(instances.GetLabelValues(), instance) {
		return "online_with_samples", nil
	}
	labelsOut, err := client.Labels(ctx, &parcapb.LabelsRequest{
		Start:       timestamppb.New(time.Now().Add(-30 * time.Minute)),
		End:         timestamppb.Now(),
		ProfileType: profileType,
	})
	if err != nil {
		return "", fmt.Errorf("%w: Parca Labels: %v", errProfileUnavailable, err)
	}
	if len(labelsOut.GetLabelNames()) == 0 {
		return "online_no_samples", nil
	}
	return "online_no_samples", nil
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (c *parcaProfileClient) queryService(ctx context.Context) (parcapb.QueryServiceClient, func(), error) {
	addr := strings.TrimSpace(c.grpcAddr)
	if addr == "" {
		return nil, func() {}, fmt.Errorf("%w: Parca gRPC address empty", errProfileUnavailable)
	}
	dialCtx, cancel := context.WithTimeout(ctx, c.timeout)
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock()}
	opts = append(opts, c.dialOptions...)
	conn, err := grpc.DialContext(dialCtx, addr, opts...)
	cancel()
	if err != nil {
		return nil, func() {}, fmt.Errorf("%w: Parca gRPC %s: %v", errProfileUnavailable, addr, err)
	}
	return parcapb.NewQueryServiceClient(conn), func() { _ = conn.Close() }, nil
}

func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

type parcaTargetsResponse struct {
	Targets map[string]struct {
		Targets []parcaTarget `json:"targets"`
	} `json:"targets"`
}

type parcaTarget struct {
	Labels struct {
		Labels []parcaLabel `json:"labels"`
	} `json:"labels"`
	DiscoveredLabels struct {
		Labels []parcaLabel `json:"labels"`
	} `json:"discoveredLabels"`
	Health    string `json:"health"`
	LastError string `json:"lastError"`
}

type parcaLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (c *parcaProfileClient) targets(ctx context.Context) (parcaTargetsResponse, error) {
	var out parcaTargetsResponse
	err := c.getJSON(ctx, "/api/targets", nil, &out)
	return out, err
}

func parcaTargetsContain(resp parcaTargetsResponse, labels map[string]interface{}) bool {
	if len(resp.Targets) == 0 {
		return false
	}
	required := map[string]string{}
	for _, key := range []string{"job", "instance"} {
		if value := strings.TrimSpace(fmt.Sprint(labels[key])); value != "" && value != "<nil>" {
			required[key] = value
		}
	}
	if len(required) == 0 {
		return false
	}
	for _, group := range resp.Targets {
		for _, target := range group.Targets {
			if target.Health != "" && target.Health != "HEALTH_GOOD" {
				continue
			}
			actual := map[string]string{}
			for _, label := range target.Labels.Labels {
				actual[label.Name] = label.Value
			}
			matches := true
			for key, want := range required {
				if actual[key] != want {
					matches = false
					break
				}
			}
			if matches {
				return true
			}
		}
	}
	return false
}

func (c *parcaProfileClient) getJSON(ctx context.Context, path string, values url.Values, dst interface{}) error {
	u := c.baseURL + path
	if encoded := values.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errProfileUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: Parca HTTP %d", errProfileUnavailable, resp.StatusCode)
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: Parca 返回空响应", errProfileUnavailable)
		}
		return fmt.Errorf("%w: Parca JSON 不兼容: %v", errProfileUnavailable, err)
	}
	return nil
}

func (c *parcaProfileClient) isGatewayFallback(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Parca JSON 不兼容") ||
		strings.Contains(msg, "invalid character '<'") ||
		strings.Contains(msg, "Parca HTTP 404") ||
		strings.Contains(msg, "Parca HTTP 405")
}

func (s *APIServer) ListProfileTargets(c *gin.Context) {
	targets, err := s.profileTargets(s.AuthContext(c))
	if err != nil {
		s.Logger.Error("查询 ProfileTarget 失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询可观测对象失败")
		return
	}
	s.RespondOK(c, gin.H{"targets": targets, "total": len(targets), "parca_ui_url": s.parcaUIURL()})
}

func (s *APIServer) GetProfileFlamegraph(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	if reserved := s.respondReservedProfileType(c, q.ProfileType); reserved {
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
	if s.Config == nil {
		return NewHTTPProfileClient(config.ProfileConfig{})
	}
	return NewHTTPProfileClient(s.Config.Profile)
}

func (s *APIServer) profileQueryFromRequest(c *gin.Context) (ProfileQuery, bool) {
	now := time.Now()
	fromDefault := now.Add(-30 * time.Minute)
	from, ok := parseProfileTime(c, "from", fromDefault)
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
		Labels:      parseProfileLabels(c.Query("labels")),
		Filters:     parseProfileFilters(c.Query("filters")),
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
	}
	if !baseFrom.Before(baseTo) || !compareFrom.Before(compareTo) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "对比时间范围不合法")
		return ProfileDiffQuery{}, false
	}
	pq := ProfileQuery{TargetID: q.TargetID, Host: q.Host, Service: q.Service, From: baseFrom, To: baseTo, ProfileType: q.ProfileType, Labels: q.Labels, Filters: q.Filters}
	if !s.validateProfileQuery(c, &pq) {
		return ProfileDiffQuery{}, false
	}
	q.Host, q.TargetID, q.Service, q.Labels, q.Filters = pq.Host, pq.TargetID, pq.Service, pq.Labels, pq.Filters
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
	return true
}

func (s *APIServer) respondReservedProfileType(c *gin.Context, profileType string) bool {
	if profileType != "memory" {
		return false
	}
	s.RespondOK(c, gin.H{
		"nodes":        []ProfileNode{},
		"items":        []ProfileTopItem{},
		"total":        0,
		"unit":         "bytes",
		"empty":        true,
		"message":      "memory profiling 已预留，v1 暂未启用",
		"source":       "mini-drop",
		"generated_at": time.Now(),
	})
	return true
}

func (s *APIServer) respondProfileDependencyError(c *gin.Context, err error) {
	s.Logger.Warn("持续画像依赖查询失败", zap.Error(err))
	msg := err.Error()
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "i/o timeout") {
		msg = "Parca Server 不可达，持续 profiling 暂无数据: " + msg
	}
	s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, msg)
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
	byIP := map[string]*ProfileTarget{}
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
			ID:               profileTargetID(agent.IPAddr, "hotmethod"),
			Hostname:         firstNonEmpty(agent.Hostname, agent.IPAddr),
			IP:               agent.IPAddr,
			ServiceName:      "hotmethod",
			Environment:      firstNonEmpty(labelString(labels, "env"), s.defaultProfileEnvironment()),
			Labels:           labels,
			ParcaUIURL:       s.parcaUIURL(),
			ParcaAgentStatus: s.defaultParcaStatus(),
			DropAgentStatus:  map[bool]string{true: "online", false: "offline"}[agent.Online],
			LastSeen:         &lastSeen,
			DropAgentOnline:  agent.Online,
		}
		target.Labels = mergeProfileLabels(*target, labels)
		target.ParcaQueryURL = s.parcaQueryURL(*target)
		byIP[profileTargetKey(agent)] = target
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
		if target, ok := findProfileTargetByIP(byIP, task.TargetIP); ok {
			lastAt := task.CreateTime
			target.LastProfileAt = &lastAt
			continue
		}
		lastAt := task.CreateTime
		byIP[task.TargetIP] = &ProfileTarget{
			ID:               profileTargetID(task.TargetIP, "hotmethod"),
			Hostname:         task.TargetIP,
			IP:               task.TargetIP,
			ServiceName:      "hotmethod",
			Environment:      s.defaultProfileEnvironment(),
			Labels:           map[string]interface{}{},
			ParcaUIURL:       s.parcaUIURL(),
			ParcaAgentStatus: s.defaultParcaStatus(),
			DropAgentStatus:  "unknown",
			LastProfileAt:    &lastAt,
		}
		byIP[task.TargetIP].Labels = mergeProfileLabels(*byIP[task.TargetIP], byIP[task.TargetIP].Labels)
		byIP[task.TargetIP].ParcaQueryURL = s.parcaQueryURL(*byIP[task.TargetIP])
	}

	if statusClient, ok := s.profileClient().(ProfileStatusClient); ok {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(profileTimeoutSec(s))*time.Second)
		defer cancel()
		for _, target := range byIP {
			status := statusClient.TargetStatus(ctx, *target)
			target.ParcaAgentStatus = status.Status
			target.ParcaError = status.Error
		}
	}
	s.attachLocalPprofScrapeStatus(byIP)

	out := make([]ProfileTarget, 0, len(byIP))
	for _, target := range byIP {
		out = append(out, *target)
	}
	sort.Slice(out, func(i, j int) bool {
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

func (s *APIServer) defaultParcaStatus() string {
	if s == nil || s.Config == nil || !s.Config.Profile.Enabled {
		return "unconfigured"
	}
	if strings.TrimSpace(s.Config.Profile.ParcaURL) == "" && strings.TrimSpace(s.Config.Profile.ParcaGRPCAddr) == "" {
		return "unconfigured"
	}
	return "unknown"
}

func (s *APIServer) parcaUIURL() string {
	if s == nil || s.Config == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(s.Config.Profile.ParcaUIURL), "/")
}

func (s *APIServer) parcaQueryURL(target ProfileTarget) string {
	base := s.parcaUIURL()
	if base == "" {
		return ""
	}
	q := ProfileQuery{
		Host:        target.IP,
		Service:     target.ServiceName,
		From:        time.Now().Add(-30 * time.Minute),
		To:          time.Now(),
		ProfileType: "cpu",
		Labels:      target.Labels,
	}
	query := defaultCPUProfileType + parcaQueryLabelSelector(q)
	values := url.Values{}
	values.Set("expression_a", query)
	values.Set("from_a", q.From.Format(time.RFC3339))
	values.Set("to_a", q.To.Format(time.RFC3339))
	values.Set("time_selection_a", "custom")
	return base + "/?" + values.Encode()
}

func profileTargetID(ip, service string) string {
	return strings.ReplaceAll(strings.TrimSpace(ip), ":", "_") + ":" + firstNonEmpty(service, "hotmethod")
}

func profileQueryValues(q ProfileQuery) url.Values {
	values := url.Values{}
	values.Set("target_id", q.TargetID)
	values.Set("host", q.Host)
	values.Set("service", q.Service)
	values.Set("from", q.From.Format(time.RFC3339))
	values.Set("to", q.To.Format(time.RFC3339))
	values.Set("profile_type", q.ProfileType)
	if len(q.Labels) > 0 {
		if b, err := json.Marshal(q.Labels); err == nil {
			values.Set("labels", string(b))
		}
	}
	if len(q.Filters) > 0 {
		if b, err := json.Marshal(q.Filters); err == nil {
			values.Set("filters", string(b))
		}
	}
	return values
}

type parcaQueryRangeResponse struct {
	Series []struct {
		Samples []struct {
			Value          json.RawMessage `json:"value"`
			ValuePerSecond float64         `json:"valuePerSecond"`
		} `json:"samples"`
	} `json:"series"`
}

func (r parcaQueryRangeResponse) hasSamples() bool {
	for _, series := range r.Series {
		for _, sample := range series.Samples {
			if rawJSONNumber(sample.Value) > 0 || sample.ValuePerSecond > 0 {
				return true
			}
		}
	}
	return false
}

type parcaQueryResponse struct {
	Flamegraph *parcaFlamegraph `json:"flamegraph"`
	Top        *parcaTop        `json:"top"`
	Total      json.RawMessage  `json:"total"`
}

type parcaFlamegraph struct {
	Root        parcaFlamegraphRoot `json:"root"`
	Unit        string              `json:"unit"`
	StringTable []string            `json:"stringTable"`
	Total       json.RawMessage     `json:"total"`
}

type parcaFlamegraphRoot struct {
	Cumulative json.RawMessage       `json:"cumulative"`
	Children   []parcaFlamegraphNode `json:"children"`
}

type parcaFlamegraphNode struct {
	Meta       parcaNodeMeta         `json:"meta"`
	Cumulative json.RawMessage       `json:"cumulative"`
	Children   []parcaFlamegraphNode `json:"children"`
}

type parcaTop struct {
	List []parcaTopNode `json:"list"`
	Unit string         `json:"unit"`
}

type parcaTopNode struct {
	Meta       parcaNodeMeta   `json:"meta"`
	Cumulative json.RawMessage `json:"cumulative"`
	Flat       json.RawMessage `json:"flat"`
	Diff       json.RawMessage `json:"diff"`
}

type parcaNodeMeta struct {
	Function parcaFunction `json:"function"`
	Mapping  parcaMapping  `json:"mapping"`
}

type parcaFunction struct {
	Name                  string `json:"name"`
	SystemName            string `json:"systemName"`
	Filename              string `json:"filename"`
	NameStringIndex       uint32 `json:"nameStringIndex"`
	SystemNameStringIndex uint32 `json:"systemNameStringIndex"`
}

type parcaMapping struct {
	File            string `json:"file"`
	FileStringIndex uint32 `json:"fileStringIndex"`
}

func (r parcaQueryResponse) toMiniDropFlamegraph() ProfileFlamegraph {
	out := ProfileFlamegraph{
		Nodes:       []ProfileNode{},
		Total:       rawJSONNumber(r.Total),
		Unit:        "samples",
		Source:      "parca",
		GeneratedAt: time.Now(),
	}
	if r.Flamegraph == nil {
		out.Empty = true
		out.Message = "Parca 未返回火焰图数据；WSL/eBPF 权限不足或当前时间段无样本时会出现这种情况"
		return out
	}
	if r.Flamegraph.Unit != "" {
		out.Unit = r.Flamegraph.Unit
	}
	if out.Total == 0 {
		out.Total = rawJSONNumber(r.Flamegraph.Total)
	}
	if out.Total == 0 {
		out.Total = rawJSONNumber(r.Flamegraph.Root.Cumulative)
	}
	for i, child := range r.Flamegraph.Root.Children {
		out.Nodes = append(out.Nodes, parcaFlamegraphNodeToProfileNode(child, r.Flamegraph.StringTable, fmt.Sprintf("fg-%d", i)))
	}
	normalizeFlamegraph(&out)
	return out
}

func (r parcaQueryResponse) toMiniDropTopN() ProfileTopN {
	out := ProfileTopN{
		Items:       []ProfileTopItem{},
		Total:       rawJSONNumber(r.Total),
		Unit:        "samples",
		Source:      "parca",
		GeneratedAt: time.Now(),
	}
	if r.Top == nil {
		out.Empty = true
		out.Message = "Parca 未返回 TopN 数据；WSL/eBPF 权限不足或当前时间段无样本时会出现这种情况"
		return out
	}
	if r.Top.Unit != "" {
		out.Unit = r.Top.Unit
	}
	for _, node := range r.Top.List {
		out.Items = append(out.Items, ProfileTopItem{
			Name:  parcaNodeName(node.Meta, nil),
			Value: rawJSONNumber(node.Cumulative),
			Self:  rawJSONNumber(node.Flat),
			Unit:  out.Unit,
		})
	}
	sort.Slice(out.Items, func(i, j int) bool {
		return out.Items[i].Value > out.Items[j].Value
	})
	normalizeTopN(&out)
	return out
}

func parcaTopResponseToMiniDrop(r *parcapb.QueryResponse) ProfileTopN {
	out := ProfileTopN{
		Items:       []ProfileTopItem{},
		Total:       float64(r.GetTotal()),
		Unit:        "samples",
		Source:      "parca",
		GeneratedAt: time.Now(),
	}
	top := r.GetTop()
	if top == nil {
		out.Empty = true
		out.Message = "Parca gRPC 未返回 TopN 数据；当前时间段可能没有样本"
		return out
	}
	if top.GetUnit() != "" {
		out.Unit = top.GetUnit()
	}
	for _, node := range top.GetList() {
		out.Items = append(out.Items, ProfileTopItem{
			Name:  parcaPBNodeName(node.GetMeta()),
			Value: float64(node.GetCumulative()),
			Self:  float64(node.GetFlat()),
			Unit:  out.Unit,
		})
	}
	sort.Slice(out.Items, func(i, j int) bool {
		return out.Items[i].Value > out.Items[j].Value
	})
	normalizeTopN(&out)
	return out
}

func parcaFlamegraphResponseToMiniDrop(r *parcapb.QueryResponse) ProfileFlamegraph {
	out := ProfileFlamegraph{
		Nodes:       []ProfileNode{},
		Total:       float64(r.GetTotal()),
		Unit:        "samples",
		Source:      "parca",
		GeneratedAt: time.Now(),
	}
	fg := r.GetFlamegraph()
	if fg == nil || fg.GetRoot() == nil {
		out.Empty = true
		out.Message = "Parca gRPC 未返回火焰图数据；当前时间段可能没有样本"
		return out
	}
	if fg.GetUnit() != "" {
		out.Unit = fg.GetUnit()
	}
	if out.Total == 0 {
		out.Total = float64(fg.GetUntrimmedTotal())
	}
	if out.Total == 0 {
		out.Total = float64(fg.GetTotal())
	}
	if out.Total == 0 {
		out.Total = float64(fg.GetRoot().GetCumulative())
	}
	for i, child := range fg.GetRoot().GetChildren() {
		out.Nodes = append(out.Nodes, parcaPBFlamegraphNodeToProfileNode(child, fg, fmt.Sprintf("fg-%d", i)))
	}
	normalizeFlamegraph(&out)
	return out
}

func parcaPBFlamegraphNodeToProfileNode(node *parcapb.FlamegraphNode, fg *parcapb.Flamegraph, id string) ProfileNode {
	children := make([]ProfileNode, 0, len(node.GetChildren()))
	var childTotal float64
	for i, child := range node.GetChildren() {
		out := parcaPBFlamegraphNodeToProfileNode(child, fg, fmt.Sprintf("%s-%d", id, i))
		childTotal += out.Value
		children = append(children, out)
	}
	value := float64(node.GetCumulative())
	self := value - childTotal
	if self < 0 {
		self = 0
	}
	return ProfileNode{
		ID:       id,
		Name:     parcaPBFlamegraphNodeName(node.GetMeta(), fg),
		Value:    value,
		Self:     self,
		Children: children,
	}
}

func parcaPBFlamegraphNodeName(meta *parcapb.TopNodeMeta, fg *parcapb.Flamegraph) string {
	if meta == nil {
		return "unknown"
	}
	if fg == nil {
		return parcaPBNodeNameWithStringTable(meta, nil)
	}
	if name := parcaPBNodeNameWithStringTable(meta, fg.GetStringTable()); name != "unknown" {
		return name
	}
	location := parcaPBLocationByIndex(fg.GetLocations(), meta.GetLocationIndex())
	if location == nil {
		return "unknown"
	}
	for _, line := range location.GetLines() {
		fn := parcaPBFunctionByIndex(fg.GetFunction(), line.GetFunctionIndex())
		if fn == nil {
			continue
		}
		if name := parcaPBFunctionName(fn, fg.GetStringTable()); name != "" {
			return name
		}
	}
	if mapping := parcaPBMappingByIndex(fg.GetMapping(), location.GetMappingIndex()); mapping != nil {
		if name := firstNonEmpty(mapping.GetFile(), stringTableValue(fg.GetStringTable(), mapping.GetFileStringIndex())); name != "" {
			return name
		}
	}
	return "unknown"
}

func parcaPBNodeName(meta *parcapb.TopNodeMeta) string {
	return parcaPBNodeNameWithStringTable(meta, nil)
}

func parcaPBNodeNameWithStringTable(meta *parcapb.TopNodeMeta, stringTable []string) string {
	if meta == nil {
		return "unknown"
	}
	if fn := meta.GetFunction(); fn != nil {
		if name := parcaPBFunctionName(fn, stringTable); name != "" {
			return name
		}
	}
	if mapping := meta.GetMapping(); mapping != nil && mapping.GetFile() != "" {
		return mapping.GetFile()
	} else if mapping := meta.GetMapping(); mapping != nil {
		if name := stringTableValue(stringTable, mapping.GetFileStringIndex()); name != "" {
			return name
		}
	}
	return "unknown"
}

func parcaPBFunctionName(fn *parcapb.Function, stringTable []string) string {
	if fn == nil {
		return ""
	}
	return firstNonEmpty(
		fn.GetName(),
		fn.GetSystemName(),
		fn.GetFilename(),
		stringTableValue(stringTable, fn.GetNameStringIndex()),
		stringTableValue(stringTable, fn.GetSystemNameStringIndex()),
		stringTableValue(stringTable, fn.GetFilenameStringIndex()),
	)
}

func parcaPBLocationByIndex(values []*parcapb.Location, index uint32) *parcapb.Location {
	if index == 0 {
		return nil
	}
	i := int(index - 1)
	if i >= 0 && i < len(values) {
		return values[i]
	}
	return nil
}

func parcaPBFunctionByIndex(values []*parcapb.Function, index uint32) *parcapb.Function {
	if index == 0 {
		return nil
	}
	i := int(index - 1)
	if i >= 0 && i < len(values) {
		return values[i]
	}
	return nil
}

func parcaPBMappingByIndex(values []*parcapb.Mapping, index uint32) *parcapb.Mapping {
	if index == 0 {
		return nil
	}
	i := int(index - 1)
	if i >= 0 && i < len(values) {
		return values[i]
	}
	return nil
}

func parcaFlamegraphNodeToProfileNode(node parcaFlamegraphNode, stringTable []string, id string) ProfileNode {
	children := make([]ProfileNode, 0, len(node.Children))
	var childTotal float64
	for i, child := range node.Children {
		out := parcaFlamegraphNodeToProfileNode(child, stringTable, fmt.Sprintf("%s-%d", id, i))
		childTotal += out.Value
		children = append(children, out)
	}
	value := rawJSONNumber(node.Cumulative)
	self := value - childTotal
	if self < 0 {
		self = 0
	}
	return ProfileNode{
		ID:       id,
		Name:     parcaNodeName(node.Meta, stringTable),
		Value:    value,
		Self:     self,
		Children: children,
	}
}

func parcaNodeName(meta parcaNodeMeta, stringTable []string) string {
	if meta.Function.Name != "" {
		return meta.Function.Name
	}
	if name := stringTableValue(stringTable, meta.Function.NameStringIndex); name != "" {
		return name
	}
	if meta.Function.SystemName != "" {
		return meta.Function.SystemName
	}
	if name := stringTableValue(stringTable, meta.Function.SystemNameStringIndex); name != "" {
		return name
	}
	if meta.Mapping.File != "" {
		return meta.Mapping.File
	}
	if name := stringTableValue(stringTable, meta.Mapping.FileStringIndex); name != "" {
		return name
	}
	return "unknown"
}

func stringTableValue(values []string, index uint32) string {
	if index == 0 || int(index) >= len(values) {
		return ""
	}
	return values[index]
}

func rawJSONNumber(raw json.RawMessage) float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	return 0
}

func profileLabelSelector(q ProfileQuery) string {
	labels := map[string]interface{}{}
	for k, v := range q.Labels {
		if strings.TrimSpace(k) != "" && fmt.Sprint(v) != "" {
			labels[k] = v
		}
	}
	if _, ok := labels["job"]; !ok && q.Service != "" {
		labels["job"] = q.Service
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, strings.ReplaceAll(fmt.Sprint(labels[k]), `"`, `\"`)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func parcaQueryLabelSelector(q ProfileQuery) string {
	labels := map[string]interface{}{}
	if value := labelString(q.Labels, "job"); value != "" {
		labels["job"] = value
	} else if strings.TrimSpace(q.Service) != "" {
		labels["job"] = strings.TrimSpace(q.Service)
	}
	if value := labelString(q.Labels, "instance"); value != "" {
		labels["instance"] = value
	} else if strings.TrimSpace(q.Host) != "" {
		labels["instance"] = strings.TrimSpace(q.Host)
	}
	for key, value := range sanitizeProfileFilters(q.Filters) {
		labels[key] = value
	}
	if len(labels) == 0 {
		return profileLabelSelector(q)
	}
	return profileLabelSelector(ProfileQuery{Labels: labels})
}

func sanitizeProfileFilters(filters map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for key, value := range filters {
		key = strings.TrimSpace(key)
		valueString := strings.TrimSpace(fmt.Sprint(value))
		if !isAllowedProfileFilterLabel(key) || valueString == "" || valueString == "<nil>" {
			continue
		}
		out[key] = valueString
	}
	return out
}

func isAllowedProfileFilterLabel(label string) bool {
	switch strings.TrimSpace(label) {
	case "comm", "thread_name", "thread_id", "cpu":
		return true
	default:
		return false
	}
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func labelString(labels map[string]interface{}, key string) string {
	value := strings.TrimSpace(fmt.Sprint(labels[key]))
	if value == "" || value == "<nil>" {
		return ""
	}
	return value
}

func profileLabelsFromAgent(raw []byte) map[string]interface{} {
	labels := map[string]interface{}{}
	if len(raw) == 0 {
		return labels
	}
	if err := util.UnmarshalJSONB(raw, &labels); err == nil {
		return labels
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return labels
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if key, val, ok := strings.Cut(value, "="); ok {
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(val)
			if key != "" && val != "" {
				labels[key] = val
			}
		}
	}
	return labels
}

func mergeProfileLabels(target ProfileTarget, explicit map[string]interface{}) map[string]interface{} {
	labels := map[string]interface{}{}
	for k, v := range target.Labels {
		if strings.TrimSpace(k) != "" && fmt.Sprint(v) != "" {
			labels[k] = v
		}
	}
	for k, v := range explicit {
		key := strings.TrimSpace(k)
		if key == "job" || key == "instance" {
			continue
		}
		if key != "" && fmt.Sprint(v) != "" {
			labels[k] = v
		}
	}
	if _, ok := labels["job"]; !ok {
		labels["job"] = firstNonEmpty(target.ServiceName, "hotmethod")
	}
	if _, ok := labels["env"]; !ok {
		labels["env"] = firstNonEmpty(target.Environment, "development")
	}
	if _, ok := labels["node"]; !ok {
		labels["node"] = firstNonEmpty(target.Hostname, "mini-drop-local")
	}
	if _, ok := labels["instance"]; !ok {
		labels["instance"] = firstNonEmpty(target.IP, "127.0.0.1")
	}
	return labels
}

func profileTimeoutSec(s *APIServer) int {
	if s != nil && s.Config != nil && s.Config.Profile.TimeoutSec > 0 {
		return s.Config.Profile.TimeoutSec
	}
	return 5
}

func (s *APIServer) defaultProfileEnvironment() string {
	if s != nil && s.Config != nil {
		env := strings.TrimSpace(s.Config.Security.Environment)
		if env != "" {
			return env
		}
	}
	return "development"
}

func (s *APIServer) attachLocalPprofScrapeStatus(targets map[string]*ProfileTarget) {
	status, message, scrapeTargets := s.localPprofScrapeState()
	for _, target := range targets {
		target.PprofScrapeStatus = status
		target.PprofScrapeMessage = message
		target.PprofScrapeTargets = scrapeTargets
	}
}

func (s *APIServer) localPprofScrapeState() (string, string, []string) {
	scrapeTargets := []string{"parca:7070", "pprof_demo:6060"}
	if s == nil || s.Config == nil || !s.Config.Profile.Enabled {
		return "unconfigured", "持续 profiling 未启用", scrapeTargets
	}
	if strings.TrimSpace(s.Config.Profile.ParcaURL) == "" {
		return "unconfigured", "Parca Server 未配置", scrapeTargets
	}
	timeout := 1200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	parcaOK := probeHTTP(ctx, http.DefaultClient, strings.TrimRight(s.Config.Profile.ParcaURL, "/"))
	pprofOK := probeHTTP(ctx, http.DefaultClient, "http://pprof_demo:6060/healthz")
	switch {
	case parcaOK && pprofOK:
		return "available", "Parca UI 与 pprof_demo 端点可达；Parca 正在按本地配置 scrape 标准 Go pprof。直接访问 CPU profile 若返回 500，通常表示 profile 正被 scrape 占用。", scrapeTargets
	case parcaOK:
		return "partial", "Parca UI 可达，但 pprof_demo 端点当前不可达；可先查看 Parca 自身 profile 或重启 pprof_demo。", scrapeTargets
	case pprofOK:
		return "partial", "pprof_demo 端点可达，但 Parca Server 当前不可达；Mini-Drop 只能显示本地降级状态。", scrapeTargets
	default:
		return "unreachable", "Parca UI 与 pprof_demo 端点当前都不可达，请检查 docker compose 服务状态。", scrapeTargets
	}
}

func probeHTTP(ctx context.Context, client *http.Client, rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

func diffTopN(base, compare ProfileTopN) ProfileDiff {
	itemsByName := map[string]*ProfileDiffItem{}
	for _, item := range base.Items {
		entry := itemsByName[item.Name]
		if entry == nil {
			entry = &ProfileDiffItem{Name: item.Name, Unit: firstNonEmpty(item.Unit, base.Unit)}
			itemsByName[item.Name] = entry
		}
		entry.BaseValue = item.Value
	}
	for _, item := range compare.Items {
		entry := itemsByName[item.Name]
		if entry == nil {
			entry = &ProfileDiffItem{Name: item.Name, Unit: firstNonEmpty(item.Unit, compare.Unit)}
			itemsByName[item.Name] = entry
		}
		entry.CompareValue = item.Value
		entry.Delta = entry.CompareValue - entry.BaseValue
	}
	items := make([]ProfileDiffItem, 0, len(itemsByName))
	for _, item := range itemsByName {
		item.Delta = item.CompareValue - item.BaseValue
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		return absFloat(items[i].Delta) > absFloat(items[j].Delta)
	})
	out := ProfileDiff{Items: items, Source: "parca", GeneratedAt: time.Now()}
	if len(out.Items) == 0 {
		out.Empty = true
		out.Message = "所选时间段没有可对比的持续画像数据"
	}
	return out
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func normalizeFlamegraph(out *ProfileFlamegraph) {
	if out.GeneratedAt.IsZero() {
		out.GeneratedAt = time.Now()
	}
	if out.Source == "" {
		out.Source = "parca"
	}
	if out.Unit == "" {
		out.Unit = "samples"
	}
	if out.Total == 0 {
		out.Total = sumProfileNodes(out.Nodes)
	}
	if len(out.Nodes) == 0 {
		out.Empty = true
		if out.Message == "" {
			out.Message = "所选时间范围没有持续画像数据"
		}
	}
}

func normalizeTopN(out *ProfileTopN) {
	if out.GeneratedAt.IsZero() {
		out.GeneratedAt = time.Now()
	}
	if out.Source == "" {
		out.Source = "parca"
	}
	if out.Unit == "" {
		out.Unit = "samples"
	}
	if out.Total == 0 {
		for _, item := range out.Items {
			out.Total += item.Value
		}
	}
	if len(out.Items) == 0 {
		out.Empty = true
		if out.Message == "" {
			out.Message = "所选时间范围没有持续画像数据"
		}
	}
}

func emptyFlamegraph(message string) ProfileFlamegraph {
	return ProfileFlamegraph{Nodes: []ProfileNode{}, Unit: "samples", Empty: true, Message: message, Source: "mini-drop", GeneratedAt: time.Now()}
}

func emptyTopN(message string) ProfileTopN {
	return ProfileTopN{Items: []ProfileTopItem{}, Unit: "samples", Empty: true, Message: message, Source: "mini-drop", GeneratedAt: time.Now()}
}

func sumProfileNodes(nodes []ProfileNode) float64 {
	var total float64
	for _, node := range nodes {
		total += node.Value
	}
	return total
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
