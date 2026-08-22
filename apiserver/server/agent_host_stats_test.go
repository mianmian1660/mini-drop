// ============================================================
// server/agent_host_stats_test.go — 整机资源（stat.host）测试
// 覆盖：host 对象完整返回、部分维度不可用、旧 Agent 无字段返回 null、
// 数据库回退场景返回 null，并确认旧的 CPU/内存字段保持兼容。
// ============================================================

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"

	"github.com/mini-drop/apiserver/model"
	commonpb "github.com/mini-drop/apiserver/proto/common"
	pb_control "github.com/mini-drop/apiserver/proto/control"
)

// hostStatsControlClient 复用一个可配置的 StatAgent 响应的假 ControlClient，
// 通过内嵌 *fakeControlClient 补齐其余方法，满足 ControlClient 接口。
type hostStatsControlClient struct {
	*fakeControlClient
	resp *pb_control.StatAgentResponse
}

func (f *hostStatsControlClient) StatAgent(context.Context, *pb_control.StatAgentRequest, ...grpc.CallOption) (*pb_control.StatAgentResponse, error) {
	if f.resp != nil {
		return f.resp, nil
	}
	return &pb_control.StatAgentResponse{Code: 404, Msg: "not found"}, nil
}

func sampleHostStats() *commonpb.HostStats {
	return &commonpb.HostStats{
		CpuPercent:        32.5,
		CpuAvailable:      true,
		MemoryUsedBytes:   1400000000,
		MemoryTotalBytes:  3900000000,
		MemoryPercent:     35.9,
		MemoryAvailable:   true,
		DiskUsedBytes:     30700000000,
		DiskTotalBytes:    42100000000,
		DiskPercent:       72.8,
		DiskAvailable:     true,
		DiskMount:         "/",
		CollectedAtUnixMs: time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC).UnixMilli(),
	}
}

func sampleStatResponse(host *commonpb.HostStats) *pb_control.StatAgentResponse {
	resp := &pb_control.StatAgentResponse{
		Code:         0,
		Msg:          "ok",
		CpuPercent:   12.3,
		MemoryKb:     4567,
		ReadKbPerS:   10,
		WriteKbPerS:  20,
		Hostname:     "host-a",
		AgentId:      "agent-stable-1",
		Version:      "1.0.0",
		Platform:     "linux/amd64",
		Capabilities: []string{"perf_cpu"},
		Labels:       []string{"local"},
		Online:       true,
	}
	if host != nil {
		resp.HostStats = host
	}
	return resp
}

func TestHostStatsFromPB(t *testing.T) {
	// 完整对象
	full := hostStatsFromPB(sampleHostStats())
	if full == nil {
		t.Fatal("full host stats should not be nil")
	}
	if full["cpu_percent"] != 32.5 || full["cpu_available"] != true {
		t.Fatalf("cpu fields wrong: %#v", full)
	}
	if full["memory_used_bytes"] != uint64(1400000000) || full["memory_total_bytes"] != uint64(3900000000) {
		t.Fatalf("memory fields wrong: %#v", full)
	}
	if full["disk_mount"] != "/" || full["disk_percent"] != 72.8 {
		t.Fatalf("disk fields wrong: %#v", full)
	}
	if full["collected_at"] != "2026-08-22T01:02:03Z" {
		t.Fatalf("collected_at=%v, want RFC3339 UTC", full["collected_at"])
	}

	// nil（旧 Agent 未上报）→ 返回 nil
	if got := hostStatsFromPB(nil); got != nil {
		t.Fatalf("nil host stats should map to nil, got %#v", got)
	}

	// 部分维度不可用：缺失字段仍能返回，不 panic，也不伪造 0%
	partial := hostStatsFromPB(&commonpb.HostStats{
		CpuPercent:   10.0,
		CpuAvailable: true,
		// memory/disk 全缺省
	})
	if partial == nil {
		t.Fatal("partial host stats should not be nil")
	}
	if partial["memory_available"] != false || partial["disk_available"] != false {
		t.Fatalf("unavailable dimensions should report false: %#v", partial)
	}
	if partial["collected_at"] != "" {
		t.Fatalf("zero timestamp should map to empty string, got %v", partial["collected_at"])
	}
}

// 通过 /api/v1/agent/detail 返回 host 对象
func TestAgentDetailIncludesHostStats(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: true,
		Version: "1.0.0", Environment: "test", LastSeen: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}, resp: sampleStatResponse(sampleHostStats())}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/agent/detail", s.GetAgentDetail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/detail?ip=10.0.0.2", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	stat := response["data"].(map[string]interface{})["stat"].(map[string]interface{})
	// 旧的进程维度字段保持兼容
	if stat["cpu_percent"] != 12.3 || stat["memory_kb"] != float64(4567) || stat["source"] != "grpc" {
		t.Fatalf("legacy stat fields regressed: %#v", stat)
	}
	host, ok := stat["host"].(map[string]interface{})
	if !ok {
		t.Fatalf("host object missing: %#v", stat["host"])
	}
	if host["cpu_percent"] != 32.5 || host["disk_mount"] != "/" || host["memory_available"] != true {
		t.Fatalf("host object wrong: %#v", host)
	}
}

// 旧 Agent（StatAgent 响应无 HostStats 字段）→ host 为 null
func TestAgentDetailHostNullForOldAgent(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: true,
		Version: "0.9.0", Environment: "test", LastSeen: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}, resp: sampleStatResponse(nil)}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/agent/detail", s.GetAgentDetail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/detail?ip=10.0.0.2", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	stat := response["data"].(map[string]interface{})["stat"].(map[string]interface{})
	if stat["host"] != nil {
		t.Fatalf("old agent should return host=null, got %#v", stat["host"])
	}
}

// /api/v1/agent/stat 的 gRPC 路径返回 host 对象
func TestAgentStatIncludesHostStats(t *testing.T) {
	s := newTestAPIServer(t)
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}, resp: sampleStatResponse(sampleHostStats())}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/agent/stat", s.StatAgent)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/stat?ip=10.0.0.2", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stat status=%d body=%s", w.Code, w.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data := response["data"].(map[string]interface{})
	if data["cpu_percent"] != 12.3 {
		t.Fatalf("legacy cpu_percent regressed: %#v", data["cpu_percent"])
	}
	host, ok := data["host"].(map[string]interface{})
	if !ok {
		t.Fatalf("host object missing in stat: %#v", data["host"])
	}
	if host["disk_percent"] != 72.8 || host["collected_at"] != "2026-08-22T01:02:03Z" {
		t.Fatalf("host object wrong: %#v", host)
	}
}

// gRPC 不可达回退 DB → host 为 null
func TestAgentStatDBFallbackHostNull(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: false,
		Version: "1.0.0", Environment: "test", LastSeen: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	// ControlCli 返回 404 → 回退 DB；host 应为 null
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/agent/stat", s.StatAgent)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/stat?ip=10.0.0.2", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stat status=%d body=%s", w.Code, w.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data := response["data"].(map[string]interface{})
	if data["host"] != nil {
		t.Fatalf("db fallback should return host=null, got %#v", data["host"])
	}
}
