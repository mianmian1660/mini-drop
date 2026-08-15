package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/pkg/storage"
	pb_control "github.com/mini-drop/apiserver/proto/control"
)

func profileRouter(s *APIServer) *gin.Engine {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Next()
	})
	api := router.Group("/api/v1")
	api.Use(s.CheckLogin)
	api.GET("/profile/targets", s.ListProfileTargets)
	api.GET("/profile/flamegraph", s.GetProfileFlamegraph)
	api.GET("/profile/topn", s.GetProfileTopN)
	api.GET("/profile/diff", s.GetProfileDiff)
	api.GET("/profile/label-values", s.GetProfileLabelValues)
	api.POST("/internal/continuous/batches", s.IngestContinuousBatch)
	return router
}

func TestListProfileTargetsAggregatesAgentsAndTasks(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	if err := s.DB.Create(&model.AgentInfo{
		Hostname:    "node-a",
		IPAddr:      "10.0.0.1",
		Online:      true,
		UID:         "owner",
		Environment: "staging",
		Labels:      []byte(`{"job":"api"}`),
		LastSeen:    now,
	}).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.DB.Create(&model.HotmethodTask{
		TID:        "tid-profile-1",
		Name:       "profile",
		TargetIP:   "10.0.0.1",
		UID:        "owner",
		CreateTime: now.Add(-time.Minute),
	}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/targets", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Code int `json:"code"`
		Data struct {
			Targets []ProfileTarget `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(body.Data.Targets) != 1 {
		t.Fatalf("targets=%d want 1", len(body.Data.Targets))
	}
	target := body.Data.Targets[0]
	if target.ID != "10.0.0.1:hotmethod" || target.Hostname != "node-a" || target.DropAgentStatus != "online" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if target.LastProfileAt == nil {
		t.Fatalf("last_profile_at should be populated")
	}
	if target.Labels["job"] != "api" {
		t.Fatalf("labels=%v", target.Labels)
	}
}

func TestListProfileTargetsHonorsVisibility(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.AgentInfo{Hostname: "owned", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.AgentInfo{Hostname: "other", IPAddr: "10.0.0.2", UID: "other", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.HotmethodTask{TID: "tid-owned", TargetIP: "10.0.0.3", UID: "owner", CreateTime: now}).Error
	_ = s.DB.Create(&model.HotmethodTask{TID: "tid-other", TargetIP: "10.0.0.4", UID: "other", CreateTime: now}).Error

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/targets", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "10.0.0.2") || strings.Contains(w.Body.String(), "10.0.0.4") {
		t.Fatalf("response leaked inaccessible targets: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "10.0.0.1") || !strings.Contains(w.Body.String(), "10.0.0.3") {
		t.Fatalf("response missing accessible targets: %s", w.Body.String())
	}
}

func TestListProfileTargetsKeepsDistinctAgentIDsWithSameIP(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.AgentInfo{Hostname: "local-host", IPAddr: "127.0.0.1", AgentID: "agent-local", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.AgentInfo{Hostname: "cloud-server-111-230-29-115", IPAddr: "127.0.0.1", AgentID: "cloud-agent-111-230-29-115", Online: true, LastSeen: now}).Error

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/targets", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Code int `json:"code"`
		Data struct {
			Targets []ProfileTarget `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(body.Data.Targets) != 2 {
		t.Fatalf("targets=%d want 2: %+v", len(body.Data.Targets), body.Data.Targets)
	}
	if !strings.Contains(w.Body.String(), "local-host") || !strings.Contains(w.Body.String(), "cloud-server-111-230-29-115") {
		t.Fatalf("response should contain both hosts: %s", w.Body.String())
	}
}

func TestProfileTargetsUseRemoteAgentLabelsForSelector(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.AgentInfo{
		Hostname: "cloud-node",
		IPAddr:   "111.230.29.115",
		AgentID:  "cloud-agent",
		Online:   true,
		Labels:   []byte(`["job=hotmethod","env=development","instance=111.230.29.115","node=cloud-node"]`),
		LastSeen: now,
	}).Error

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/targets", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Targets []ProfileTarget `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(body.Data.Targets) != 1 {
		t.Fatalf("targets=%d want 1", len(body.Data.Targets))
	}
	labels := body.Data.Targets[0].Labels
	if labels["instance"] != "111.230.29.115" || labels["node"] != "cloud-node" || labels["job"] != "hotmethod" {
		t.Fatalf("remote labels not preserved: %#v", labels)
	}
}

func TestUpsertAgentFromStatPrefersAgentIDOverSameIP(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.AgentInfo{Hostname: "cloud-old", IPAddr: "127.0.0.1", AgentID: "cloud-agent-111-230-29-115", Online: true, LastSeen: now}).Error

	_, _, err := s.upsertAgentFromStat("127.0.0.1", &pb_control.StatAgentResponse{
		Code:     0,
		AgentId:  "agent-local",
		Hostname: "local-host",
		Online:   true,
	})
	if err != nil {
		t.Fatalf("upsert local: %v", err)
	}
	_, _, err = s.upsertAgentFromStat("111.230.29.115", &pb_control.StatAgentResponse{
		Code:     0,
		AgentId:  "cloud-agent-111-230-29-115",
		Hostname: "cloud-server-111-230-29-115",
		Online:   true,
	})
	if err != nil {
		t.Fatalf("upsert cloud: %v", err)
	}

	var agents []model.AgentInfo
	if err := s.DB.Order("agent_id ASC").Find(&agents).Error; err != nil {
		t.Fatalf("load agents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents=%d want 2: %+v", len(agents), agents)
	}
	got := map[string]string{}
	for _, agent := range agents {
		got[agent.AgentID] = agent.IPAddr
	}
	if got["agent-local"] != "127.0.0.1" || got["cloud-agent-111-230-29-115"] != "111.230.29.115" {
		t.Fatalf("unexpected agent IPs: %#v", got)
	}
}

func TestConfiguredAgentDiscoveryIPsFiltersInvalidEntries(t *testing.T) {
	s := newTestAPIServer(t)
	s.Config.AgentDiscovery.ExtraIPs = "111.230.29.115, not-an-ip, 111.230.29.115,10.0.0.9"

	got := s.configuredAgentDiscoveryIPs()
	want := []string{"111.230.29.115", "10.0.0.9"}
	if len(got) != len(want) {
		t.Fatalf("ips=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ips=%v want %v", got, want)
		}
	}
}

func TestEnsureAgentDiscoveryPlaceholderCanBeUpdatedByLaterAgentStat(t *testing.T) {
	s := newTestAPIServer(t)

	placeholder, created := s.ensureAgentDiscoveryPlaceholder("111.230.29.115")
	if !created {
		t.Fatalf("placeholder should be created: %+v", placeholder)
	}
	if placeholder.IPAddr != "111.230.29.115" || placeholder.Online {
		t.Fatalf("unexpected placeholder: %+v", placeholder)
	}

	updated, _, err := s.upsertAgentFromStat("111.230.29.115", &pb_control.StatAgentResponse{
		Code:     0,
		AgentId:  "cloud-agent-111-230-29-115",
		Hostname: "cloud-server-111-230-29-115",
		Online:   true,
		Version:  "1.0.0",
	})
	if err != nil {
		t.Fatalf("upsert cloud after placeholder: %v", err)
	}
	if updated.ID != placeholder.ID || updated.AgentID != "cloud-agent-111-230-29-115" || !updated.Online {
		t.Fatalf("placeholder should be updated in place: %+v", updated)
	}
}

func TestUpsertAgentFromStatCleansDuplicateAgentIDRows(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.AgentInfo{Hostname: "cloud-current", IPAddr: "111.230.29.115", AgentID: "cloud-agent-111-230-29-115", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.AgentInfo{Hostname: "cloud-duplicate", IPAddr: "111.230.29.115", AgentID: "cloud-agent-111-230-29-115", Online: false, LastSeen: now}).Error

	_, _, err := s.upsertAgentFromStat("111.230.29.115", &pb_control.StatAgentResponse{
		Code:     0,
		AgentId:  "cloud-agent-111-230-29-115",
		Hostname: "cloud-server-111-230-29-115",
		Online:   true,
	})
	if err != nil {
		t.Fatalf("upsert cloud duplicate: %v", err)
	}

	var count int64
	if err := s.DB.Model(&model.AgentInfo{}).Where("agent_id = ?", "cloud-agent-111-230-29-115").Count(&count).Error; err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if count != 1 {
		t.Fatalf("duplicate agent rows should be cleaned, count=%d", count)
	}
}

func TestProfileFlamegraphUnconfiguredReturnsEmptyState(t *testing.T) {
	s := newTestAPIServer(t)
	_ = s.DB.Create(&model.AgentInfo{Hostname: "node-a", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: time.Now()}).Error

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod&profile_type=cpu", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"empty":true`) || !strings.Contains(w.Body.String(), "Native Continuous Profiling") {
		t.Fatalf("expected native empty state, got %s", w.Body.String())
	}
}

func TestProfileTypeValidationAndReservedMemory(t *testing.T) {
	s := newTestAPIServer(t)
	_ = s.DB.Create(&model.AgentInfo{Hostname: "node-a", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: time.Now()}).Error
	router := profileRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod&profile_type=wall", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/topn?target_id=10.0.0.1:hotmethod&profile_type=memory", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "memory profiling") {
		t.Fatalf("expected reserved memory message, got %s", w.Body.String())
	}
}

func TestProfileDependencyUnavailable(t *testing.T) {
	s := newTestAPIServer(t)
	s.ProfileCli = failingProfileClient{}
	_ = s.DB.Create(&model.AgentInfo{Hostname: "node-a", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: time.Now()}).Error
	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestNativeProfileSelectorIncludesAllowedFilters(t *testing.T) {
	query := profileLabelSelector(ProfileQuery{
		Host:    "111.230.29.115",
		Service: "hotmethod",
		Labels: map[string]interface{}{
			"job":      "hotmethod",
			"instance": "111.230.29.115",
		},
		Filters: map[string]interface{}{
			"comm":     "python3",
			"pid":      "1234",
			"exe":      "/usr/bin/python3",
			"job":      "evil",
			"instance": "other-host",
			"env":      "prod",
		},
	})
	if query != `{comm="python3",pid="1234",exe="/usr/bin/python3",instance="111.230.29.115",job="hotmethod"}` {
		t.Fatalf("query=%s", query)
	}
}

func TestProfileQueryLabelsCannotOverrideTargetIdentity(t *testing.T) {
	target := ProfileTarget{
		IP:          "111.230.29.115",
		ServiceName: "hotmethod",
		Labels:      map[string]interface{}{"job": "hotmethod", "instance": "111.230.29.115", "env": "development"},
	}
	labels := mergeProfileLabels(target, map[string]interface{}{
		"job":      "other-job",
		"instance": "other-host",
		"env":      "prod",
	})
	if labels["job"] != "hotmethod" || labels["instance"] != "111.230.29.115" || labels["env"] != "prod" {
		t.Fatalf("labels=%#v", labels)
	}
}

func TestNativeContinuousBatchIngestQueryAndFilters(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC()
	_ = s.DB.Create(&model.AgentInfo{
		Hostname: "node-a",
		IPAddr:   "10.0.0.1",
		UID:      "owner",
		Online:   true,
		LastSeen: now,
	}).Error
	_ = s.DB.Create(&model.ContinuousSession{
		SID:                  "cps-test",
		Name:                 "test session",
		TargetIP:             "10.0.0.1",
		ServiceName:          "hotmethod",
		SampleRateHz:         19,
		AggregationWindowSec: 10,
		UploadBatchSec:       60,
		RetentionHours:       24,
		Status:               model.ContinuousSessionStatusRunning,
		UID:                  "owner",
		StartedAt:            now.Add(-time.Minute),
		CreatedAt:            now.Add(-time.Minute),
		UpdatedAt:            now.Add(-time.Minute),
	}).Error

	body := fmt.Sprintf(`{
		"session_sid":"cps-test",
		"start_time":%q,
		"end_time":%q,
		"windows":[{
			"window_start":%q,
			"window_end":%q,
			"sample_count":22,
			"samples":[
				{"stack":["runtime.main","main.busy"],"count":19,"comm":"python3","pid":123,"exe":"/usr/bin/python3"},
				{"stack":["runtime.main","main.idle"],"count":3,"comm":"bash","pid":234,"exe":"/usr/bin/bash"}
			]
		}]
	}`, now.Add(-20*time.Second).Format(time.RFC3339), now.Add(-10*time.Second).Format(time.RFC3339), now.Add(-20*time.Second).Format(time.RFC3339), now.Add(-10*time.Second).Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(body))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router := profileRouter(s)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest status=%d body=%s", w.Code, w.Body.String())
	}

	filter := `{"comm":"python3","pid":"123","exe":"/usr/bin/python3"}`
	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/topn?target_id=10.0.0.1:hotmethod&from="+url.QueryEscape(now.Add(-30*time.Second).Format(time.RFC3339))+"&to="+url.QueryEscape(now.Format(time.RFC3339))+"&filters="+url.QueryEscape(filter), nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("topn status=%d body=%s", w.Code, w.Body.String())
	}
	var topBody struct {
		Data ProfileTopN `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &topBody); err != nil {
		t.Fatalf("topn json: %v", err)
	}
	if topBody.Data.Empty || topBody.Data.Total != 19 || len(topBody.Data.Items) == 0 || topBody.Data.Items[0].Name != "main.busy" {
		t.Fatalf("unexpected topn: %+v", topBody.Data)
	}
	if !strings.Contains(topBody.Data.Query, `comm="python3"`) || !strings.Contains(topBody.Data.Query, `pid="123"`) || topBody.Data.ProfileURL == "" {
		t.Fatalf("query/url not populated: %+v", topBody.Data)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/label-values?target_id=10.0.0.1:hotmethod&from="+url.QueryEscape(now.Add(-30*time.Second).Format(time.RFC3339))+"&to="+url.QueryEscape(now.Format(time.RFC3339))+"&label=comm", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("labels status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"bash"`) || !strings.Contains(w.Body.String(), `"python3"`) {
		t.Fatalf("expected comm label values, got %s", w.Body.String())
	}
}

func TestNativeContinuousRetentionDeletesExpiredWindowsBatchesAndObjects(t *testing.T) {
	s := newTestAPIServer(t)
	mem := newContinuousMemoryStorage()
	s.Storage = mem
	now := time.Now().UTC()
	_ = s.DB.Create(&model.AgentInfo{
		Hostname: "node-a",
		IPAddr:   "10.0.0.1",
		UID:      "owner",
		Online:   true,
		LastSeen: now,
	}).Error
	_ = s.DB.Create(&model.ContinuousSession{
		SID:                  "cps-retention",
		Name:                 "retention session",
		TargetIP:             "10.0.0.1",
		ServiceName:          "hotmethod",
		SampleRateHz:         19,
		AggregationWindowSec: 10,
		UploadBatchSec:       60,
		RetentionHours:       1,
		Status:               model.ContinuousSessionStatusRunning,
		UID:                  "owner",
		StartedAt:            now.Add(-3 * time.Hour),
		CreatedAt:            now.Add(-3 * time.Hour),
		UpdatedAt:            now.Add(-3 * time.Hour),
	}).Error

	start := now.Add(-2 * time.Hour)
	end := start.Add(10 * time.Second)
	body := fmt.Sprintf(`{
		"session_sid":"cps-retention",
		"batch_id":"cpb-old",
		"start_time":%q,
		"end_time":%q,
		"windows":[{
			"window_start":%q,
			"window_end":%q,
			"sample_count":1,
			"samples":[{"stack":["main.old"],"count":1,"comm":"old","pid":123,"exe":"/bin/old"}]
		}]
	}`, start.Format(time.RFC3339), end.Format(time.RFC3339), start.Format(time.RFC3339), end.Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(body))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest status=%d body=%s", w.Code, w.Body.String())
	}

	var windowCount int64
	s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ?", "cps-retention").Count(&windowCount)
	if windowCount != 0 {
		t.Fatalf("expired profile windows should be deleted, count=%d", windowCount)
	}
	var batchCount int64
	s.DB.Model(&model.ProfileBatch{}).Where("session_sid = ?", "cps-retention").Count(&batchCount)
	if batchCount != 0 {
		t.Fatalf("expired profile batches should be deleted, count=%d", batchCount)
	}
	if _, ok := mem.objects["continuous/cps-retention/cpb-old.json"]; ok {
		t.Fatalf("expired profile batch object should be deleted")
	}
}

type failingProfileClient struct{}

func (failingProfileClient) Flamegraph(context.Context, ProfileQuery) (ProfileFlamegraph, error) {
	return ProfileFlamegraph{}, errProfileUnavailable
}

func (failingProfileClient) TopN(context.Context, ProfileQuery) (ProfileTopN, error) {
	return ProfileTopN{}, errProfileUnavailable
}

func (failingProfileClient) Diff(context.Context, ProfileDiffQuery) (ProfileDiff, error) {
	return ProfileDiff{}, errProfileUnavailable
}

func (failingProfileClient) LabelValues(context.Context, ProfileQuery, string) (ProfileLabelValues, error) {
	return ProfileLabelValues{}, errProfileUnavailable
}

type continuousMemoryStorage struct {
	objects map[string]string
}

func newContinuousMemoryStorage() *continuousMemoryStorage {
	return &continuousMemoryStorage{objects: map[string]string{}}
}

func (m *continuousMemoryStorage) EnsureBucket(context.Context, string) error { return nil }

func (m *continuousMemoryStorage) PutObject(_ context.Context, _, key string, reader io.Reader, _ int64, _ string) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.objects[key] = string(body)
	return nil
}

func (m *continuousMemoryStorage) GetObject(_ context.Context, _, key string) (io.ReadCloser, error) {
	body, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("missing object %s", key)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (m *continuousMemoryStorage) PresignedGetURL(context.Context, string, string, time.Duration) (string, error) {
	return "http://example.test/continuous.json", nil
}

func (m *continuousMemoryStorage) ListObjects(context.Context, string, string) ([]storage.FileInfo, error) {
	return []storage.FileInfo{}, nil
}

func (m *continuousMemoryStorage) DeleteObject(_ context.Context, _, key string) error {
	delete(m.objects, key)
	return nil
}

func (m *continuousMemoryStorage) ObjectExists(_ context.Context, _, key string) (bool, error) {
	_, ok := m.objects[key]
	return ok, nil
}
