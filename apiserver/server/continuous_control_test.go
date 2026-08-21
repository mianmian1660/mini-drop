package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

func continuousControlRouter(s *APIServer) *gin.Engine {
	router := gin.New()
	api := router.Group("/api/v1")
	api.Use(s.CheckLogin)
	api.POST("/continuous/sessions", s.CreateContinuousSession)
	api.POST("/continuous/sessions/:sid/stop", s.StopContinuousSession)
	api.POST("/internal/continuous/reconcile", s.ReconcileContinuousSessions)
	return router
}

func TestContinuousActiveSetRules(t *testing.T) {
	if err := validateContinuousActiveSet([]model.ContinuousSession{{Scope: "process"}}, "host", ""); err != errContinuousModeConflict {
		t.Fatalf("host/process conflict=%v", err)
	}
	if err := validateContinuousActiveSet([]model.ContinuousSession{{Scope: "host"}}, "host", ""); err != errContinuousHostLimitReached {
		t.Fatalf("host limit=%v", err)
	}
	if err := validateContinuousActiveSet([]model.ContinuousSession{{Scope: "host"}}, "process", "/opt/api"); err != errContinuousModeConflict {
		t.Fatalf("process/host conflict=%v", err)
	}
	if err := validateContinuousActiveSet([]model.ContinuousSession{{Scope: "process", SelectorExe: "/opt/api"}}, "process", "/opt/api"); err != errContinuousDuplicateSelector {
		t.Fatalf("duplicate selector=%v", err)
	}
	active := make([]model.ContinuousSession, 16)
	for index := range active {
		active[index] = model.ContinuousSession{Scope: "process", SelectorExe: "/opt/worker-" + string(rune('a'+index))}
	}
	if err := validateContinuousActiveSet(active, "process", "/opt/new"); err != errContinuousLimitReached {
		t.Fatalf("process limit=%v", err)
	}
}

func TestContinuousPendingSessionBecomesOfflineWithoutFirstAgentReport(t *testing.T) {
	now := time.Now()
	session := model.ContinuousSession{
		DesiredState:  model.ContinuousDesiredStateRunning,
		ObservedState: model.ContinuousObservedStatePending,
		StartedAt:     now.Add(-16 * time.Second),
	}
	markContinuousSessionOffline(&session, now)
	if session.ObservedState != model.ContinuousObservedStateOffline {
		t.Fatalf("pending Session without first report stayed %q", session.ObservedState)
	}
	session.ObservedState = model.ContinuousObservedStatePending
	session.StartedAt = now.Add(-14 * time.Second)
	markContinuousSessionOffline(&session, now)
	if session.ObservedState != model.ContinuousObservedStatePending {
		t.Fatalf("fresh pending Session became %q", session.ObservedState)
	}
}

func TestCreateContinuousSessionRequiresDegradedConfirmation(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.8", Hostname: "node", Online: true, LastSeen: now}).Error
	_ = s.DB.Create(&model.ContinuousAgentState{TargetIP: "10.0.0.8", AgentID: "agent", StrictCapable: false, ObservedAt: now, UpdatedAt: now}).Error
	body := []byte(`{"name":"api","target_ip":"10.0.0.8","scope":"process","selector_exe":"/opt/api","continuity_mode":"strict"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/continuous/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("without confirmation status=%d body=%s", w.Code, w.Body.String())
	}

	body = []byte(`{"name":"api","target_ip":"10.0.0.8","scope":"process","selector_exe":"/opt/api","continuity_mode":"strict","allow_degraded":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/continuous/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"continuity_mode":"degraded"`)) {
		t.Fatalf("confirmed create status=%d body=%s", w.Code, w.Body.String())
	}
	var first model.ContinuousSession
	if err := s.DB.Where("target_ip = ?", "10.0.0.8").First(&first).Error; err != nil {
		t.Fatal(err)
	}
	var state model.ContinuousAgentState
	if err := s.DB.Where("target_ip = ?", "10.0.0.8").First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if first.ObservedState != model.ContinuousObservedStatePending || first.Revision != 1 || state.Revision != 1 {
		t.Fatalf("unexpected initial state session=%+v agent=%+v", first, state)
	}
	if !first.AllowDegraded {
		t.Fatalf("confirmed fallback policy was not persisted: %+v", first)
	}
}

func TestCreateContinuousSessionRequiresFreshAgentControlPlane(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	if err := s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.18", Hostname: "node", Online: true, LastSeen: now}).Error; err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"name":"host","target_ip":"10.0.0.18","scope":"host","allow_degraded":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/continuous/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte("尚未连接持续采集控制面")) {
		t.Fatalf("unready Agent status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestContinuousStopWaitsForAgentAcknowledgement(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	if err := s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.9", Hostname: "node", AgentID: "agent", Online: true, LastSeen: now}).Error; err != nil {
		t.Fatal(err)
	}
	session := model.ContinuousSession{SID: "cps-stop", TargetIP: "10.0.0.9", UID: "owner", Status: "running", Scope: "process", DesiredState: "running", ObservedState: "running", Revision: 1, StartedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := s.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&model.ContinuousAgentState{TargetIP: session.TargetIP, AgentID: "agent", Revision: 7, ObservedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/continuous/sessions/cps-stop/stop", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stop status=%d body=%s", w.Code, w.Body.String())
	}
	_ = s.DB.Where("sid = ?", session.SID).First(&session).Error
	if session.DesiredState != "stopped" || session.ObservedState != "stopping" || session.StoppedAt != nil || session.Revision != 8 {
		t.Fatalf("unexpected stopping state: %+v", session)
	}

	reconcile := map[string]interface{}{"target_ip": session.TargetIP, "agent_id": "agent", "processes": []interface{}{}, "sessions": []interface{}{map[string]interface{}{"sid": session.SID, "observed_state": "stopped"}}}
	payload, _ := json.Marshal(reconcile)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/reconcile", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "agent")
	w = httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reconcile status=%d body=%s", w.Code, w.Body.String())
	}
	_ = s.DB.Where("sid = ?", session.SID).First(&session).Error
	if session.Status != "stopped" || session.ObservedState != "stopped" || session.StoppedAt == nil {
		t.Fatalf("unexpected stopped state: %+v", session)
	}
	var state model.ContinuousAgentState
	if err := s.DB.Where("target_ip = ?", session.TargetIP).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Revision != 8 {
		t.Fatalf("authoritative revision=%d, want 8", state.Revision)
	}
}

func TestFirstContinuousReconcileCreatesFreshAgentState(t *testing.T) {
	s := newTestAPIServer(t)
	if err := s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.19", Hostname: "node", AgentID: "agent-19", Online: true, LastSeen: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"target_ip":"10.0.0.19","agent_id":"agent-19","strict_capable":false,"capabilities":["perf"],"processes":[],"sessions":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/reconcile", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "agent-19")
	w := httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reconcile status=%d body=%s", w.Code, w.Body.String())
	}
	var state model.ContinuousAgentState
	if err := s.DB.Where("target_ip = ?", "10.0.0.19").First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.AgentID != "agent-19" || state.ObservedAt.IsZero() || time.Since(state.ObservedAt) > time.Second {
		t.Fatalf("first reconcile did not create fresh state: %+v", state)
	}
}

func TestContinuousReconcileCannotUndoStopWithStaleRunningReport(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	if err := s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.20", Hostname: "node", AgentID: "agent-20", Online: true, LastSeen: now}).Error; err != nil {
		t.Fatal(err)
	}
	session := model.ContinuousSession{
		SID: "cps-stale-stop", TargetIP: "10.0.0.20", AgentID: "agent-20", Status: "running",
		Scope: "process", SelectorExe: "/opt/api", DesiredState: "stopped", ObservedState: "stopping",
		StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"target_ip":"10.0.0.20","agent_id":"agent-20","processes":[],"sessions":[{"sid":"cps-stale-stop","observed_state":"running","active_processes":[{"pid":42,"process_start_ms":1000,"exe":"/opt/api"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/reconcile", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "agent-20")
	w := httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"desired_state":"stopped"`)) {
		t.Fatalf("reconcile status=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.DB.Where("sid = ?", session.SID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.ObservedState != model.ContinuousObservedStateStopping || session.StoppedAt != nil {
		t.Fatalf("stale report undid stop: %+v", session)
	}
}

func TestContinuousReconcileRejectsMismatchedAgentIdentity(t *testing.T) {
	s := newTestAPIServer(t)
	if err := s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.21", Hostname: "node", AgentID: "agent-21", Online: true, LastSeen: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"target_ip":"10.0.0.21","agent_id":"other-agent","processes":[],"sessions":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/reconcile", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "other-agent")
	w := httptest.NewRecorder()
	continuousControlRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("mismatched Agent status=%d body=%s", w.Code, w.Body.String())
	}
	var count int64
	if err := s.DB.Model(&model.ContinuousAgentState{}).Where("target_ip = ?", "10.0.0.21").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unauthorized reconcile persisted state count=%d err=%v", count, err)
	}
}

func TestContinuousSessionSelectionIsSIDIsolated(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	for _, session := range []model.ContinuousSession{
		{SID: "cps-a", TargetIP: "10.0.0.10", UID: "owner", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{SID: "cps-b", TargetIP: "10.0.0.10", UID: "owner", StartedAt: now, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)},
	} {
		if err := s.DB.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}
	var sids []string
	if err := s.continuousSessionSelection(ProfileQuery{SessionSID: "cps-a", Host: "10.0.0.10", OwnerUIDs: []string{"owner"}}).Pluck("sid", &sids).Error; err != nil {
		t.Fatal(err)
	}
	if len(sids) != 1 || sids[0] != "cps-a" {
		t.Fatalf("SID query leaked sessions: %v", sids)
	}
	sids = nil
	if err := s.continuousSessionSelection(ProfileQuery{Host: "10.0.0.10", OwnerUIDs: []string{"owner"}}).Pluck("sid", &sids).Error; err != nil {
		t.Fatal(err)
	}
	if len(sids) != 1 || sids[0] != "cps-b" {
		t.Fatalf("legacy query should choose one latest session: %v", sids)
	}
}
