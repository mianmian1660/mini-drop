package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
	pb_control "github.com/mini-drop/apiserver/proto/control"
	parcapb "github.com/mini-drop/apiserver/proto/parca/query/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
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

func TestParcaGRPCTopNMapsNativeResponse(t *testing.T) {
	addr, dialer, stop := startTestParcaQueryService(t, &testParcaQueryService{
		top: &parcapb.QueryResponse{
			Report: &parcapb.QueryResponse_Top{Top: &parcapb.Top{
				Unit: "nanoseconds",
				List: []*parcapb.TopNode{
					{Meta: &parcapb.TopNodeMeta{Function: &parcapb.Function{Name: "main.busy"}}, Cumulative: 42, Flat: 30},
					{Meta: &parcapb.TopNodeMeta{Mapping: &parcapb.Mapping{File: "libc.so"}}, Cumulative: 12, Flat: 5},
				},
			}},
			Total: 54,
		},
	})
	defer stop()
	client := NewHTTPProfileClient(config.ProfileConfig{Enabled: true, ParcaGRPCAddr: addr, ParcaUIURL: "http://parca.local"}).(*parcaProfileClient)
	client.dialOptions = append(client.dialOptions, grpc.WithContextDialer(dialer))

	out, err := client.TopN(context.Background(), ProfileQuery{
		Host:        "111.230.29.115",
		Service:     "hotmethod",
		From:        time.Now().Add(-time.Minute),
		To:          time.Now(),
		ProfileType: "cpu",
		Labels:      map[string]interface{}{"job": "hotmethod", "instance": "111.230.29.115"},
	})
	if err != nil {
		t.Fatalf("TopN: %v", err)
	}
	if out.Empty || out.Total != 54 || len(out.Items) != 2 {
		t.Fatalf("unexpected TopN: %+v", out)
	}
	if out.Items[0].Name != "main.busy" || out.Items[0].Value != 42 || out.Items[0].Self != 30 || out.Unit != "nanoseconds" {
		t.Fatalf("unexpected item mapping: %+v", out)
	}
	if !strings.Contains(out.Query, `instance="111.230.29.115"`) || out.ParcaURL == "" {
		t.Fatalf("query/url not populated: %+v", out)
	}
}

func TestParcaQuerySelectorIncludesAllowedFilters(t *testing.T) {
	query := parcaQueryLabelSelector(ProfileQuery{
		Host:    "wrong-host",
		Service: "wrong-service",
		Labels: map[string]interface{}{
			"job":      "hotmethod",
			"instance": "111.230.29.115",
		},
		Filters: map[string]interface{}{
			"comm":     "python3",
			"job":      "evil",
			"instance": "other-host",
			"env":      "prod",
		},
	})
	if query != `{comm="python3",instance="111.230.29.115",job="hotmethod"}` {
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

func TestParcaLabelValuesUsesTargetScopedComm(t *testing.T) {
	svc := &testParcaQueryService{
		labels: []string{"job", "instance", "comm"},
		valuesByLabel: map[string][]string{
			"comm": {"python3", "test_c_profilin", "python3"},
		},
	}
	addr, dialer, stop := startTestParcaQueryService(t, svc)
	defer stop()
	client := NewHTTPProfileClient(config.ProfileConfig{Enabled: true, ParcaGRPCAddr: addr}).(*parcaProfileClient)
	client.dialOptions = append(client.dialOptions, grpc.WithContextDialer(dialer))

	out, err := client.LabelValues(context.Background(), ProfileQuery{
		Host:        "111.230.29.115",
		Service:     "hotmethod",
		From:        time.Now().Add(-time.Minute),
		To:          time.Now(),
		ProfileType: "cpu",
		Labels:      map[string]interface{}{"job": "hotmethod", "instance": "111.230.29.115"},
	}, "comm")
	if err != nil {
		t.Fatalf("LabelValues: %v", err)
	}
	if !out.Available || strings.Join(out.Values, ",") != "python3,test_c_profilin" {
		t.Fatalf("unexpected label values: %+v", out)
	}
	if svc.lastValuesRequest == nil || len(svc.lastValuesRequest.GetMatch()) != 1 {
		t.Fatalf("Values request not captured")
	}
	match := svc.lastValuesRequest.GetMatch()[0]
	if !strings.Contains(match, `job="hotmethod"`) || !strings.Contains(match, `instance="111.230.29.115"`) || strings.Contains(match, "comm=") {
		t.Fatalf("unexpected label-values match: %s", match)
	}
}

func TestParcaGRPCFlamegraphMapsNativeResponse(t *testing.T) {
	addr, dialer, stop := startTestParcaQueryService(t, &testParcaQueryService{
		flamegraph: &parcapb.QueryResponse{
			Report: &parcapb.QueryResponse_Flamegraph{Flamegraph: &parcapb.Flamegraph{
				Unit: "nanoseconds",
				Root: &parcapb.FlamegraphRootNode{
					Cumulative: 100,
					Children: []*parcapb.FlamegraphNode{
						{
							Meta:       &parcapb.TopNodeMeta{Function: &parcapb.Function{Name: "root.fn"}},
							Cumulative: 100,
							Children: []*parcapb.FlamegraphNode{
								{Meta: &parcapb.TopNodeMeta{Function: &parcapb.Function{Name: "leaf.fn"}}, Cumulative: 40},
							},
						},
					},
				},
			}},
			Total: 100,
		},
	})
	defer stop()
	client := NewHTTPProfileClient(config.ProfileConfig{Enabled: true, ParcaGRPCAddr: addr, ParcaUIURL: "http://parca.local"}).(*parcaProfileClient)
	client.dialOptions = append(client.dialOptions, grpc.WithContextDialer(dialer))

	out, err := client.Flamegraph(context.Background(), ProfileQuery{
		Host:        "111.230.29.115",
		Service:     "hotmethod",
		From:        time.Now().Add(-time.Minute),
		To:          time.Now(),
		ProfileType: "cpu",
		Labels:      map[string]interface{}{"job": "hotmethod", "instance": "111.230.29.115"},
	})
	if err != nil {
		t.Fatalf("Flamegraph: %v", err)
	}
	if out.Empty || out.Total != 100 || out.Unit != "nanoseconds" || len(out.Nodes) != 1 {
		t.Fatalf("unexpected flamegraph: %+v", out)
	}
	if out.Nodes[0].Name != "root.fn" || out.Nodes[0].Self != 60 || len(out.Nodes[0].Children) != 1 {
		t.Fatalf("unexpected node mapping: %+v", out.Nodes[0])
	}
	if !strings.Contains(out.Query, `instance="111.230.29.115"`) || out.ParcaURL == "" {
		t.Fatalf("query/url not populated: %+v", out)
	}
}

func TestParcaGRPCFlamegraphResolvesTableIndexesAsOneBased(t *testing.T) {
	out := parcaFlamegraphResponseToMiniDrop(&parcapb.QueryResponse{
		Report: &parcapb.QueryResponse_Flamegraph{Flamegraph: &parcapb.Flamegraph{
			Unit: "nanoseconds",
			Root: &parcapb.FlamegraphRootNode{
				Cumulative: 100,
				Children: []*parcapb.FlamegraphNode{
					{
						Meta:       &parcapb.TopNodeMeta{LocationIndex: 1},
						Cumulative: 100,
						Children: []*parcapb.FlamegraphNode{
							{Meta: &parcapb.TopNodeMeta{LocationIndex: 2}, Cumulative: 40},
						},
					},
				},
			},
			Locations: []*parcapb.Location{
				{Lines: []*parcapb.Line{{FunctionIndex: 1}}},
				{MappingIndex: 1},
			},
			Function: []*parcapb.Function{
				{Name: "first.fn"},
				{Name: "wrong.second.fn"},
			},
			Mapping: []*parcapb.Mapping{
				{File: "first.mapping"},
				{File: "wrong.second.mapping"},
			},
		}},
		Total: 100,
	})

	if out.Empty || len(out.Nodes) != 1 {
		t.Fatalf("unexpected flamegraph: %+v", out)
	}
	if out.Nodes[0].Name != "first.fn" {
		t.Fatalf("root name=%q, want first.fn", out.Nodes[0].Name)
	}
	if len(out.Nodes[0].Children) != 1 || out.Nodes[0].Children[0].Name != "first.mapping" {
		t.Fatalf("child mapping name mismatch: %+v", out.Nodes[0].Children)
	}
}

func TestParcaTargetStatusUsesRemoteStoreLabels(t *testing.T) {
	addr, dialer, stop := startTestParcaQueryService(t, &testParcaQueryService{
		valuesByLabel: map[string][]string{
			"job":      {"hotmethod"},
			"instance": {"111.230.29.115"},
		},
	})
	defer stop()
	client := NewHTTPProfileClient(config.ProfileConfig{Enabled: true, ParcaGRPCAddr: addr}).(*parcaProfileClient)
	client.dialOptions = append(client.dialOptions, grpc.WithContextDialer(dialer))

	status := client.TargetStatus(context.Background(), ProfileTarget{
		IP:          "111.230.29.115",
		ServiceName: "hotmethod",
		Labels:      map[string]interface{}{"job": "hotmethod", "instance": "111.230.29.115"},
	})
	if status.Status != "online_with_samples" || status.Error != "" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestParcaTargetStatusDistinguishesNoSamples(t *testing.T) {
	addr, dialer, stop := startTestParcaQueryService(t, &testParcaQueryService{labels: []string{"job", "instance"}})
	defer stop()
	client := NewHTTPProfileClient(config.ProfileConfig{Enabled: true, ParcaGRPCAddr: addr}).(*parcaProfileClient)
	client.dialOptions = append(client.dialOptions, grpc.WithContextDialer(dialer))

	status := client.TargetStatus(context.Background(), ProfileTarget{
		IP:          "111.230.29.115",
		ServiceName: "hotmethod",
		Labels:      map[string]interface{}{"job": "hotmethod", "instance": "111.230.29.115"},
	})
	if status.Status != "online_no_samples" || status.Error != "" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

type testParcaQueryService struct {
	parcapb.UnimplementedQueryServiceServer
	top               *parcapb.QueryResponse
	flamegraph        *parcapb.QueryResponse
	valuesByLabel     map[string][]string
	values            []string
	labels            []string
	lastValuesRequest *parcapb.ValuesRequest
	lastLabelsRequest *parcapb.LabelsRequest
	lastQueryRequest  *parcapb.QueryRequest
}

func (s *testParcaQueryService) ProfileTypes(context.Context, *parcapb.ProfileTypesRequest) (*parcapb.ProfileTypesResponse, error) {
	return &parcapb.ProfileTypesResponse{Types: []*parcapb.ProfileType{{Name: "process_cpu", SampleType: "samples", SampleUnit: "count", PeriodType: "cpu", PeriodUnit: "nanoseconds", Delta: true}}}, nil
}

func (s *testParcaQueryService) Query(_ context.Context, req *parcapb.QueryRequest) (*parcapb.QueryResponse, error) {
	s.lastQueryRequest = req
	if req.GetReportType() == parcapb.QueryRequest_REPORT_TYPE_FLAMEGRAPH_TABLE && s.flamegraph != nil {
		return s.flamegraph, nil
	}
	if s.top != nil {
		return s.top, nil
	}
	return &parcapb.QueryResponse{Report: &parcapb.QueryResponse_Top{Top: &parcapb.Top{}}}, nil
}

func (s *testParcaQueryService) Values(_ context.Context, req *parcapb.ValuesRequest) (*parcapb.ValuesResponse, error) {
	s.lastValuesRequest = req
	if s.valuesByLabel != nil {
		return &parcapb.ValuesResponse{LabelValues: s.valuesByLabel[req.GetLabelName()]}, nil
	}
	return &parcapb.ValuesResponse{LabelValues: s.values}, nil
}

func (s *testParcaQueryService) Labels(_ context.Context, req *parcapb.LabelsRequest) (*parcapb.LabelsResponse, error) {
	s.lastLabelsRequest = req
	return &parcapb.LabelsResponse{LabelNames: s.labels}, nil
}

func startTestParcaQueryService(t *testing.T, svc parcapb.QueryServiceServer) (string, func(context.Context, string) (net.Conn, error), func()) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	parcapb.RegisterQueryServiceServer(server, svc)
	go func() {
		_ = server.Serve(lis)
	}()
	return "bufnet", func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}, func() {
			server.Stop()
			_ = lis.Close()
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
