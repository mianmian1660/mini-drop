package server

import (
	"context"
	"encoding/json"
	"errors"
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
	api.POST("/internal/continuous/sessions", s.CreateInternalContinuousSession)
	api.POST("/internal/continuous/batches", s.IngestContinuousBatch)
	api.GET("/continuous/raw", s.ViewContinuousProfileObject)
	api.GET("/continuous/histogram", s.QueryContinuousHistogram)
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

func TestListProfileTargetsSharesAllTargets(t *testing.T) {
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
	for _, target := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"} {
		if !strings.Contains(w.Body.String(), target) {
			t.Fatalf("response missing shared target %s: %s", target, w.Body.String())
		}
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

func TestProfileTypeValidationAndContinuousMemoryEmptyState(t *testing.T) {
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
	if !strings.Contains(w.Body.String(), "Memray") || !strings.Contains(w.Body.String(), "\"unit\":\"bytes\"") {
		t.Fatalf("expected Memray bytes empty state, got %s", w.Body.String())
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
				{"stack":["runtime.main","main.busy"],"count":19,"comm":"python3","pid":123,"process_start_ms":1724160000123,"exe":"/usr/bin/python3"},
				{"stack":["runtime.main","main.idle"],"count":3,"comm":"bash","pid":234,"process_start_ms":1724160000234,"exe":"/usr/bin/bash"}
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

	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/label-values?session_sid=cps-test&target_id=10.0.0.1:hotmethod&from="+url.QueryEscape(now.Add(-30*time.Second).Format(time.RFC3339))+"&to="+url.QueryEscape(now.Format(time.RFC3339))+"&label=process_instance", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"123|1724160000123"`) || !strings.Contains(w.Body.String(), `"234|1724160000234"`) {
		t.Fatalf("expected SID-isolated process instances, status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLegacyInternalContinuousCreateIsDisabledAndHistoricalSystemSessionIsReadable(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC()
	router := profileRouter(s)
	_ = s.DB.Create(&model.AgentInfo{
		Hostname: "node-system",
		IPAddr:   "10.0.0.10",
		AgentID:  "agent-001",
		Online:   true,
		LastSeen: now,
	}).Error
	_ = s.DB.Create(&model.ContinuousAgentState{
		TargetIP: "10.0.0.10", AgentID: "agent-001", StrictCapable: true,
		ObservedAt: now, UpdatedAt: now,
	}).Error

	createBody := `{
		"name":"Native Continuous Profiling",
		"target_ip":"10.0.0.10",
		"hostname":"node-system",
		"service_name":"hotmethod",
		"capabilities":{"sampler":"perf_event"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/sessions", strings.NewReader(createBody))
	req.Header.Set("Drop-User-Uid", "agent-001")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Fatalf("legacy Agent create status=%d body=%s", w.Code, w.Body.String())
	}
	session := model.ContinuousSession{
		SID: "cps-system-history", Name: "Native Continuous Profiling", TargetIP: "10.0.0.10",
		Hostname: "node-system", ServiceName: "hotmethod", SampleRateHz: 19, AggregationWindowSec: 10,
		UploadBatchSec: 60, RetentionHours: 24, Capabilities: []byte(`{"sampler":"perf_event"}`),
		Status: model.ContinuousSessionStatusRunning, Scope: "host", DesiredState: "running", ObservedState: "running",
		AgentID: "agent-001", StartedAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	if err := s.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}

	start := now.Add(-20 * time.Second)
	end := now.Add(-10 * time.Second)
	batchBody := fmt.Sprintf(`{
		"session_sid":%q,
		"batch_id":"cpb-system",
		"start_time":%q,
		"end_time":%q,
		"windows":[{
			"window_start":%q,
			"window_end":%q,
			"sample_count":7,
			"samples":[{"stack":["runtime.main","main.hot"],"count":7,"comm":"hotmethod","pid":345,"exe":"/usr/bin/hotmethod"}]
		}]
	}`, session.SID, start.Format(time.RFC3339), end.Format(time.RFC3339), start.Format(time.RFC3339), end.Format(time.RFC3339))
	req = httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(batchBody))
	req.Header.Set("Drop-User-Uid", "agent-001")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest system batch status=%d body=%s", w.Code, w.Body.String())
	}

	queryRange := "&from=" + url.QueryEscape(now.Add(-30*time.Second).Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Format(time.RFC3339))
	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/targets", nil)
	req.Header.Set("Drop-User-Uid", "user-demo")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("targets status=%d body=%s", w.Code, w.Body.String())
	}
	var targetsResp struct {
		Data struct {
			Targets []ProfileTarget `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &targetsResp); err != nil {
		t.Fatalf("targets json: %v", err)
	}
	found := false
	for _, target := range targetsResp.Data.Targets {
		if target.ID == "10.0.0.10:hotmethod" {
			found = true
			if target.ProfileStatus != model.ContinuousSessionStatusRunning || !target.ContinuousActive {
				t.Fatalf("target should expose running system profile: %+v", target)
			}
			if target.ContinuousSession == nil {
				t.Fatalf("target should expose continuous session metadata: %+v", target)
			}
			meta := target.ContinuousSession
			if meta.SID != session.SID || meta.Status != model.ContinuousSessionStatusRunning || meta.Sampler != "perf_event" {
				t.Fatalf("unexpected continuous session metadata: %+v", meta)
			}
			if meta.SampleRateHz != 19 || meta.AggregationWindowSec != 10 || meta.UploadBatchSec != 60 || meta.RetentionHours != 24 {
				t.Fatalf("unexpected continuous session runtime params: %+v", meta)
			}
			if meta.Capabilities["sampler"] != "perf_event" {
				t.Fatalf("continuous session capabilities should include sampler, got %+v", meta.Capabilities)
			}
		}
	}
	if !found {
		t.Fatalf("system profile target missing: %+v", targetsResp.Data.Targets)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/topn?target_id=10.0.0.10:hotmethod"+queryRange, nil)
	req.Header.Set("Drop-User-Uid", "user-demo")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("topn status=%d body=%s", w.Code, w.Body.String())
	}
	var topResp struct {
		Data ProfileTopN `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &topResp); err != nil {
		t.Fatalf("topn json: %v", err)
	}
	if topResp.Data.Empty || topResp.Data.Total != 7 || len(topResp.Data.Items) == 0 {
		t.Fatalf("ordinary user should read system topn: %+v", topResp.Data)
	}
	if !strings.HasPrefix(topResp.Data.ProfileURL, "/profiles?") || !strings.Contains(topResp.Data.ProfileURL, "target_id=10.0.0.10%3Ahotmethod") || !strings.Contains(topResp.Data.ProfileURL, "from=") || !strings.Contains(topResp.Data.ProfileURL, "to=") {
		t.Fatalf("profile_url should preserve the visual query context, got %q", topResp.Data.ProfileURL)
	}
	if !strings.HasPrefix(topResp.Data.RawProfileURL, "/api/v1/continuous/raw?key=") || strings.Contains(topResp.Data.RawProfileURL, "localhost") {
		t.Fatalf("raw_profile_url should be same-origin raw endpoint, got %q", topResp.Data.RawProfileURL)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/flamegraph?target_id=10.0.0.10:hotmethod"+queryRange, nil)
	req.Header.Set("Drop-User-Uid", "user-demo")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("flamegraph status=%d body=%s", w.Code, w.Body.String())
	}
	var flameResp struct {
		Data ProfileFlamegraph `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &flameResp); err != nil {
		t.Fatalf("flamegraph json: %v", err)
	}
	if flameResp.Data.Empty || flameResp.Data.Total != 7 || len(flameResp.Data.Nodes) == 0 {
		t.Fatalf("ordinary user should read system flamegraph: %+v", flameResp.Data)
	}

	req = httptest.NewRequest(http.MethodGet, topResp.Data.RawProfileURL, nil)
	req.Header.Set("Drop-User-Uid", "user-demo")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"batch_id":"cpb-system"`) {
		t.Fatalf("raw continuous object status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestContinuousHistogramIngestAndQuery(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC()
	router := profileRouter(s)
	_ = s.DB.Create(&model.ContinuousSession{
		SID:                  "cps-hist",
		Name:                 "dual track",
		TargetIP:             "10.0.0.30",
		Hostname:             "node-hist",
		ServiceName:          "hotmethod",
		SampleRateHz:         19,
		AggregationWindowSec: 10,
		UploadBatchSec:       60,
		RetentionHours:       24,
		Capabilities:         []byte(`{"sampler":"dual_track","io_backend":"bpftrace","sched_backend":"bpftrace"}`),
		Status:               model.ContinuousSessionStatusRunning,
		UID:                  "owner",
		StartedAt:            now.Add(-time.Minute),
		CreatedAt:            now.Add(-time.Minute),
		UpdatedAt:            now.Add(-time.Minute),
	}).Error
	start := now.Add(-20 * time.Second)
	end := now.Add(-10 * time.Second)
	body := fmt.Sprintf(`{
		"session_sid":"cps-hist",
		"batch_id":"cpb-hist",
		"schema_version":2,
		"signal_types":["cpu_profile","io_latency"],
		"backends":{"cpu_user":"bpftrace","io_latency":"bpftrace"},
		"start_time":%q,
		"end_time":%q,
		"windows":[{
			"window_start":%q,
			"window_end":%q,
			"profiles":[{
				"signal_type":"cpu_profile",
				"backend":"bpftrace",
				"stack_scope":"user",
				"samples":[{"stack":["main","hot"],"count":5,"stack_scope":"user","backend":"bpftrace"}]
			}],
			"histograms":[{
				"signal_type":"io_latency",
				"backend":"bpftrace",
				"unit":"us",
				"event_count":9,
				"summary":{"min":1,"max":8,"p50":2,"p95":6,"p99":6},
				"buckets":[
					{"range":"[1, 2)","low":1,"high":2,"count":4},
					{"range":"[4, 8)","low":4,"high":8,"count":5}
				]
			}]
		}]
	}`, start.Format(time.RFC3339), end.Format(time.RFC3339), start.Format(time.RFC3339), end.Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(body))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest histogram status=%d body=%s", w.Code, w.Body.String())
	}

	queryRange := "&from=" + url.QueryEscape(now.Add(-30*time.Second).Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Format(time.RFC3339))
	req = httptest.NewRequest(http.MethodGet, "/api/v1/continuous/histogram?target_id=10.0.0.30:hotmethod&host=10.0.0.30&signal_type=io_latency"+queryRange, nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("histogram query status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"event_count":9`) || !strings.Contains(w.Body.String(), `"p95":6`) || !strings.Contains(w.Body.String(), `"backend":"bpftrace"`) {
		t.Fatalf("unexpected histogram response: %s", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/profile/topn?target_id=10.0.0.30:hotmethod&host=10.0.0.30&stack_scope=user"+queryRange, nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"hot"`) {
		t.Fatalf("stack_scope topn failed status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestUserContinuousSessionIsSharedReadOnly(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC()
	_ = s.DB.Create(&model.AgentInfo{
		Hostname: "node-private",
		IPAddr:   "10.0.0.20",
		Online:   true,
		LastSeen: now,
	}).Error
	_ = s.DB.Create(&model.ContinuousSession{
		SID:                  "cps-private",
		Name:                 "private session",
		TargetIP:             "10.0.0.20",
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

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/targets", nil)
	req.Header.Set("Drop-User-Uid", "user-demo")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("targets status=%d body=%s", w.Code, w.Body.String())
	}
	var targetsResp struct {
		Data struct {
			Targets []ProfileTarget `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &targetsResp); err != nil {
		t.Fatalf("targets json: %v", err)
	}
	found := false
	for _, target := range targetsResp.Data.Targets {
		if target.ID == "10.0.0.20:hotmethod" && target.ProfileStatus == model.ContinuousSessionStatusRunning {
			found = true
		}
	}
	if !found {
		t.Fatalf("ordinary user should see shared continuous session: %+v", targetsResp.Data.Targets)
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
	objects  map[string]string
	modified map[string]time.Time
}

func newContinuousMemoryStorage() *continuousMemoryStorage {
	return &continuousMemoryStorage{objects: map[string]string{}, modified: map[string]time.Time{}}
}

func (m *continuousMemoryStorage) EnsureBucket(context.Context, string) error { return nil }

func (m *continuousMemoryStorage) PutObject(_ context.Context, _, key string, reader io.Reader, _ int64, _ string) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.objects[key] = string(body)
	m.modified[key] = time.Now()
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

func (m *continuousMemoryStorage) ListObjects(_ context.Context, _, prefix string) ([]storage.FileInfo, error) {
	files := make([]storage.FileInfo, 0)
	for key, body := range m.objects {
		if strings.HasPrefix(key, prefix) {
			files = append(files, storage.FileInfo{Name: key, Size: int64(len(body)), LastModified: m.modified[key]})
		}
	}
	return files, nil
}

func (m *continuousMemoryStorage) DeleteObject(_ context.Context, _, key string) error {
	delete(m.objects, key)
	delete(m.modified, key)
	return nil
}

func (m *continuousMemoryStorage) ObjectExists(_ context.Context, _, key string) (bool, error) {
	_, ok := m.objects[key]
	return ok, nil
}

func (m *continuousMemoryStorage) StatObject(_ context.Context, _, key string) (int64, error) {
	body, ok := m.objects[key]
	if !ok {
		return 0, fmt.Errorf("对象不存在: %s", key)
	}
	return int64(len(body)), nil
}

func TestProfileQueryUsesSessionRetentionWindow(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now().UTC()
	_ = s.DB.Create(&model.AgentInfo{Hostname: "node-a", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.ContinuousSession{
		SID: "cps-retention-query", Name: "test", TargetIP: "10.0.0.1", ServiceName: "hotmethod",
		SampleRateHz: 19, AggregationWindowSec: 10, UploadBatchSec: 60, RetentionHours: 24,
		Status: model.ContinuousSessionStatusRunning, UID: "owner",
		StartedAt: now.Add(-24 * time.Hour), CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now,
	}).Error
	router := profileRouter(s)

	from := now.Add(-7 * time.Hour)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod&from="+
			url.QueryEscape(from.Format(time.RFC3339))+"&to="+url.QueryEscape(now.Format(time.RFC3339)), nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 7h query inside retention to pass, got %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod&from="+
			url.QueryEscape(now.Add(-25*time.Hour).Format(time.RFC3339))+"&to="+url.QueryEscape(now.Format(time.RFC3339)), nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 beyond retention, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "24 小时") {
		t.Fatalf("expected retention limit message, got %s", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod&from="+
			url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339))+"&to="+url.QueryEscape(now.Add(2*time.Minute).Format(time.RFC3339)), nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不能晚于当前时间") {
		t.Fatalf("expected future time rejection, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestContinuousAggregateWindowLimitIsNotPartial(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC()
	_ = s.DB.Create(&model.ContinuousSession{
		SID: "cps-window-limit", Name: "test", TargetIP: "10.0.0.1", ServiceName: "hotmethod",
		RetentionHours: 24, Status: model.ContinuousSessionStatusRunning, StartedAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}).Error
	windows := make([]model.ProfileWindow, 0, continuousMaxWindowCount)
	for i := 0; i < continuousMaxWindowCount; i++ {
		windows = append(windows, model.ProfileWindow{
			SessionSID: "cps-window-limit", BatchBID: fmt.Sprintf("batch-%d", i),
			WindowStart: now.Add(-time.Minute), WindowEnd: now, SignalType: "cpu_profile",
		})
	}
	if err := s.DB.CreateInBatches(windows, 500).Error; err != nil {
		t.Fatalf("create windows: %v", err)
	}
	q := ProfileQuery{Host: "10.0.0.1", From: now.Add(-2 * time.Minute), To: now, CanReadAll: true}
	if _, found, err := s.queryNativeContinuousAggregate(context.Background(), q); err != nil || !found {
		t.Fatalf("20000 windows should be accepted, found=%v err=%v", found, err)
	}
	if err := s.DB.Create(&model.ProfileWindow{
		SessionSID: "cps-window-limit", BatchBID: "batch-over-limit",
		WindowStart: now.Add(-time.Minute), WindowEnd: now, SignalType: "cpu_profile",
	}).Error; err != nil {
		t.Fatalf("create extra window: %v", err)
	}
	if _, _, err := s.queryNativeContinuousAggregate(context.Background(), q); !errors.Is(err, errContinuousWindowLimit) {
		t.Fatalf("20001 windows should fail without partial data, err=%v", err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/profile/flamegraph?host=10.0.0.1&from="+url.QueryEscape(q.From.Format(time.RFC3339))+"&to="+url.QueryEscape(q.To.Format(time.RFC3339)), nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "20000") {
		t.Fatalf("window limit should return actionable 400, status=%d body=%s", w.Code, w.Body.String())
	}
}

type recordingProfileClient struct {
	diffQuery ProfileDiffQuery
}

func (r *recordingProfileClient) Flamegraph(context.Context, ProfileQuery) (ProfileFlamegraph, error) {
	return emptyFlamegraph("empty"), nil
}
func (r *recordingProfileClient) TopN(context.Context, ProfileQuery) (ProfileTopN, error) {
	return emptyTopN("empty"), nil
}
func (r *recordingProfileClient) Diff(_ context.Context, q ProfileDiffQuery) (ProfileDiff, error) {
	r.diffQuery = q
	return ProfileDiff{Items: []ProfileDiffItem{}, Empty: true}, nil
}
func (r *recordingProfileClient) LabelValues(context.Context, ProfileQuery, string) (ProfileLabelValues, error) {
	return ProfileLabelValues{}, nil
}

func TestProfileDiffPassesStackScopeAndFilters(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now().UTC()
	_ = s.DB.Create(&model.AgentInfo{Hostname: "node-a", IPAddr: "10.0.0.2", UID: "owner", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.ContinuousSession{
		SID: "cps-diff-query", Name: "test", TargetIP: "10.0.0.2", ServiceName: "hotmethod",
		RetentionHours: 24, Status: model.ContinuousSessionStatusRunning, UID: "owner",
		StartedAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}).Error
	recorder := &recordingProfileClient{}
	s.ProfileCli = recorder
	query := fmt.Sprintf("/api/v1/profile/diff?target_id=10.0.0.2:hotmethod&stack_scope=user&filters=%s&base_from=%s&base_to=%s&compare_from=%s&compare_to=%s",
		url.QueryEscape(`{"comm":"worker"}`),
		url.QueryEscape(now.Add(-30*time.Minute).Format(time.RFC3339)),
		url.QueryEscape(now.Add(-15*time.Minute).Format(time.RFC3339)),
		url.QueryEscape(now.Add(-15*time.Minute).Format(time.RFC3339)),
		url.QueryEscape(now.Format(time.RFC3339)))
	req := httptest.NewRequest(http.MethodGet, query, nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("diff status=%d body=%s", w.Code, w.Body.String())
	}
	if recorder.diffQuery.StackScope != "user" || recorder.diffQuery.Filters["comm"] != "worker" {
		t.Fatalf("diff query did not preserve scope/filter: %+v", recorder.diffQuery)
	}

	unequal := fmt.Sprintf("/api/v1/profile/diff?target_id=10.0.0.2:hotmethod&base_from=%s&base_to=%s&compare_from=%s&compare_to=%s",
		url.QueryEscape(now.Add(-45*time.Minute).Format(time.RFC3339)),
		url.QueryEscape(now.Add(-15*time.Minute).Format(time.RFC3339)),
		url.QueryEscape(now.Add(-15*time.Minute).Format(time.RFC3339)),
		url.QueryEscape(now.Format(time.RFC3339)))
	req = httptest.NewRequest(http.MethodGet, unequal, nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "等长") {
		t.Fatalf("unequal diff windows should be rejected, status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestProfileFlamegraphMaxNodesTruncatesAndReportsSymbolStatus(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC()
	_ = s.DB.Create(&model.AgentInfo{Hostname: "node-a", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.ContinuousSession{
		SID: "cps-maxnodes", Name: "test", TargetIP: "10.0.0.1", ServiceName: "hotmethod",
		SampleRateHz: 19, AggregationWindowSec: 10, UploadBatchSec: 60, RetentionHours: 24,
		Status: model.ContinuousSessionStatusRunning, UID: "owner",
		StartedAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}).Error

	// build a batch with symbol_refs containing build_id and many distinct stacks
	samples := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		samples = append(samples, fmt.Sprintf(`{"stack":["runtime.main","main.fn%d"],"count":5,"comm":"app","pid":%d,"exe":"/app/bin"}`, i, 100+i))
	}
	body := fmt.Sprintf(`{
		"session_sid":"cps-maxnodes",
		"start_time":%q,
		"end_time":%q,
		"profile_format":"pprof",
		"backend_status":"ok",
		"selected_backend":"bpftrace",
		"attempted_backends":["core","bpftrace"],
		"symbol_refs":{"build_id":"abc123","kallsyms_sha256":"sha-kernel"},
		"windows":[{
			"window_start":%q,
			"window_end":%q,
			"sample_count":50,
			"profile_format":"pprof",
			"backend_status":"ok",
			"selected_backend":"bpftrace",
			"symbol_refs":{"build_id":"abc123","kallsyms_sha256":"sha-kernel"},
			"samples":[%s]
		}]
	}`, now.Add(-20*time.Second).Format(time.RFC3339), now.Add(-10*time.Second).Format(time.RFC3339),
		now.Add(-20*time.Second).Format(time.RFC3339), now.Add(-10*time.Second).Format(time.RFC3339),
		strings.Join(samples, ","))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(body))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router := profileRouter(s)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest status=%d body=%s", w.Code, w.Body.String())
	}

	// max_nodes=1 should truncate
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod&from="+
			url.QueryEscape(now.Add(-30*time.Second).Format(time.RFC3339))+
			"&to="+url.QueryEscape(now.Format(time.RFC3339))+"&max_nodes=1", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("flamegraph status=%d body=%s", w.Code, w.Body.String())
	}
	var fg struct {
		Data ProfileFlamegraph `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fg); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !fg.Data.Truncated {
		t.Fatalf("expected truncated=true with max_nodes=1")
	}
	if fg.Data.SymbolStatus != "complete" {
		t.Fatalf("expected symbol_status=complete, got %q", fg.Data.SymbolStatus)
	}
}

func TestGoPprofHeapTaskKind(t *testing.T) {
	defs := taskKindDefinitions()
	found := false
	for _, d := range defs {
		if d.ID == TaskKindGoPprofHeap {
			found = true
			if d.Runner != "pprof" {
				t.Fatalf("expected runner=pprof, got %s", d.Runner)
			}
			if d.AnalysisPipeline != "pprof_heap" {
				t.Fatalf("expected pipeline=pprof_heap, got %s", d.AnalysisPipeline)
			}
			if d.TaskType != TaskTypePprof {
				t.Fatalf("expected task_type=%d, got %d", TaskTypePprof, d.TaskType)
			}
		}
	}
	if !found {
		t.Fatalf("TaskKindGoPprofHeap not found in definitions")
	}

	// inferTaskKind should detect heap URL
	kind := inferTaskKind(CreateTaskReq{
		ProfilerType: ProfilerPprof,
		PprofURL:     "http://127.0.0.1:6060/debug/pprof/heap",
	})
	if kind != TaskKindGoPprofHeap {
		t.Fatalf("expected go_pprof_heap, got %s", kind)
	}

	// CPU URL should still infer go_pprof
	kind = inferTaskKind(CreateTaskReq{
		ProfilerType: ProfilerPprof,
		PprofURL:     "http://127.0.0.1:6060/debug/pprof/profile",
	})
	if kind != TaskKindGoPprof {
		t.Fatalf("expected go_pprof, got %s", kind)
	}
}

func TestContinuousSymbolCheckReturnsStatus(t *testing.T) {
	s := newTestAPIServer(t)
	st := newContinuousMemoryStorage()
	s.Storage = st
	// pre-populate one build-id symbol
	_ = st.PutObject(context.Background(), "", "symbols/abc123", strings.NewReader("binary"), 6, "application/octet-stream")

	router := gin.New()
	api := router.Group("/api/v1")
	api.POST("/internal/continuous/symbol-check", s.ContinuousSymbolCheck)

	body := `{"build_ids":["abc123","def456"],"kallsyms_sha256":"sha-kernel"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/symbol-check", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			SymbolCheck struct {
				BuildIDs     map[string]bool `json:"build_ids"`
				Kallsyms     bool            `json:"kallsyms"`
				Missing      []string        `json:"missing"`
				SymbolStatus string          `json:"symbol_status"`
			} `json:"symbol_check"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !resp.Data.SymbolCheck.BuildIDs["abc123"] {
		t.Fatalf("expected abc123 to exist")
	}
	if resp.Data.SymbolCheck.BuildIDs["def456"] {
		t.Fatalf("expected def456 to be missing")
	}
	if resp.Data.SymbolCheck.SymbolStatus != "partial" {
		t.Fatalf("expected partial, got %s", resp.Data.SymbolCheck.SymbolStatus)
	}
}
