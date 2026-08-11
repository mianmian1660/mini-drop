package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
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

func TestProfileFlamegraphUnconfiguredReturnsEmptyState(t *testing.T) {
	s := newTestAPIServer(t)
	s.Config.Profile = config.ProfileConfig{Enabled: false}
	_ = s.DB.Create(&model.AgentInfo{Hostname: "node-a", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: time.Now()}).Error

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profile/flamegraph?target_id=10.0.0.1:hotmethod&profile_type=cpu", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"empty":true`) || !strings.Contains(w.Body.String(), "Parca") {
		t.Fatalf("expected empty Parca state, got %s", w.Body.String())
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
