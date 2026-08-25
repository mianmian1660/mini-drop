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
	"github.com/mini-drop/apiserver/util"
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

func sampleHostMetadata() *commonpb.HostMetadata {
	return &commonpb.HostMetadata{
		OsName:            "Ubuntu",
		OsVersion:         "24.04",
		KernelVersion:     "6.8.0-31-generic",
		Architecture:      "x86_64",
		CpuModel:          "AMD EPYC 7B12",
		CpuCores:          8,
		UptimeSeconds:     86400,
		CollectedAtUnixMs: time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC).UnixMilli(),
	}
}

func sampleStatResponseWithMeta(host *commonpb.HostStats, meta *commonpb.HostMetadata) *pb_control.StatAgentResponse {
	resp := sampleStatResponse(host)
	if meta != nil {
		resp.HostMetadata = meta
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

// ============================================================
// 主机身份与系统信息（host_metadata）测试
// ============================================================

// HostMetadata 完整字段转换
func TestHostMetadataFromPB(t *testing.T) {
	full := hostMetadataFromPB(sampleHostMetadata())
	if full == nil {
		t.Fatal("full host metadata should not be nil")
	}
	if full["os_name"] != "Ubuntu" || full["os_version"] != "24.04" {
		t.Fatalf("os fields wrong: %#v", full)
	}
	if full["kernel_version"] != "6.8.0-31-generic" || full["architecture"] != "x86_64" {
		t.Fatalf("kernel/arch fields wrong: %#v", full)
	}
	if full["cpu_model"] != "AMD EPYC 7B12" || full["cpu_cores"] != int32(8) {
		t.Fatalf("cpu fields wrong: %#v", full)
	}
	if full["uptime_seconds"] != int64(86400) {
		t.Fatalf("uptime wrong: %#v", full)
	}
	if full["collected_at"] != "2026-08-25T10:30:00Z" {
		t.Fatalf("collected_at=%v, want RFC3339 UTC", full["collected_at"])
	}

	// nil（旧 Agent 未上报）→ 返回 nil
	if got := hostMetadataFromPB(nil); got != nil {
		t.Fatalf("nil host metadata should map to nil, got %#v", got)
	}

	// 单字段采集失败不影响其他字段：缺失字段为空/0，不伪造默认值
	partial := hostMetadataFromPB(&commonpb.HostMetadata{
		OsName:        "Ubuntu",
		KernelVersion: "6.8.0-31-generic",
		// os_version/architecture/cpu_model/cpu_cores/uptime 全缺省
	})
	if partial == nil {
		t.Fatal("partial host metadata should not be nil")
	}
	if partial["os_name"] != "Ubuntu" || partial["kernel_version"] != "6.8.0-31-generic" {
		t.Fatalf("present fields lost: %#v", partial)
	}
	if partial["os_version"] != "" || partial["architecture"] != "" || partial["cpu_model"] != "" {
		t.Fatalf("missing fields should be empty string: %#v", partial)
	}
	if partial["cpu_cores"] != int32(0) || partial["uptime_seconds"] != int64(0) {
		t.Fatalf("missing numeric fields should be 0: %#v", partial)
	}
	if partial["collected_at"] != "" {
		t.Fatalf("zero timestamp should map to empty string, got %v", partial["collected_at"])
	}
}

// 新 Agent 返回实时元数据：/api/v1/agent/detail 的 gRPC 路径
func TestAgentDetailIncludesHostMetadata(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: true,
		Version: "1.0.0", Environment: "test", LastSeen: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}, resp: sampleStatResponseWithMeta(sampleHostStats(), sampleHostMetadata())}

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
	if stat["host_metadata_source"] != "grpc" {
		t.Fatalf("source should be grpc, got %#v", stat["host_metadata_source"])
	}
	meta, ok := stat["host_metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("host_metadata object missing: %#v", stat["host_metadata"])
	}
	if meta["os_name"] != "Ubuntu" || meta["os_version"] != "24.04" || meta["cpu_cores"] != float64(8) {
		t.Fatalf("host_metadata wrong: %#v", meta)
	}
	if meta["collected_at"] != "2026-08-25T10:30:00Z" {
		t.Fatalf("collected_at wrong: %#v", meta)
	}
}

// 旧 Agent（StatAgent 响应无 HostMetadata 字段）→ host_metadata 为 null 且不报错
func TestAgentDetailHostMetadataNullForOldAgent(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: true,
		Version: "0.9.0", Environment: "test", LastSeen: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}, resp: sampleStatResponseWithMeta(nil, nil)}

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
	if stat["host_metadata"] != nil {
		t.Fatalf("old agent should return host_metadata=null, got %#v", stat["host_metadata"])
	}
	if stat["host_metadata_source"] != "none" {
		t.Fatalf("old agent source should be none, got %#v", stat["host_metadata_source"])
	}
}

// gRPC 失败回退 DB：返回数据库最后已知元数据，来源为 db
func TestAgentDetailHostMetadataDBFallback(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	metaJSON, err := util.MarshalJSONB(hostMetadataFromPB(sampleHostMetadata()))
	if err != nil {
		t.Fatal(err)
	}
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: false,
		Version: "1.0.0", Environment: "test", LastSeen: now, CreatedAt: now, UpdatedAt: now,
		HostMetadata: metaJSON,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	// ControlCli 返回 404 → 回退 DB
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}}

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
	if stat["host_metadata_source"] != "db" {
		t.Fatalf("source should be db, got %#v", stat["host_metadata_source"])
	}
	meta, ok := stat["host_metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("db host_metadata missing: %#v", stat["host_metadata"])
	}
	if meta["os_name"] != "Ubuntu" || meta["cpu_cores"] != float64(8) {
		t.Fatalf("db host_metadata wrong: %#v", meta)
	}
}

// /api/v1/agent/stat 的 gRPC 路径返回 host_metadata（来源 grpc）
func TestAgentStatIncludesHostMetadata(t *testing.T) {
	s := newTestAPIServer(t)
	s.ControlCli = &hostStatsControlClient{fakeControlClient: &fakeControlClient{}, resp: sampleStatResponseWithMeta(sampleHostStats(), sampleHostMetadata())}

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
	if data["host_metadata_source"] != "grpc" {
		t.Fatalf("source should be grpc, got %#v", data["host_metadata_source"])
	}
	meta, ok := data["host_metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("host_metadata missing in stat: %#v", data["host_metadata"])
	}
	if meta["kernel_version"] != "6.8.0-31-generic" || meta["architecture"] != "x86_64" {
		t.Fatalf("host_metadata wrong: %#v", meta)
	}
}

// 旧 Agent 未上报元数据时，upsert 不覆盖数据库已有值（Agent 离线/旧版本不覆盖）
func TestUpsertAgentKeepsExistingHostMetadata(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	metaJSON, err := util.MarshalJSONB(hostMetadataFromPB(sampleHostMetadata()))
	if err != nil {
		t.Fatal(err)
	}
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: true,
		AgentID: "agent-stable-1", Version: "1.0.0", Environment: "test",
		LastSeen: now, CreatedAt: now, UpdatedAt: now,
		HostMetadata: metaJSON,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}

	// 旧 Agent 响应：无 HostMetadata 字段
	oldResp := sampleStatResponseWithMeta(nil, nil)
	oldResp.AgentId = "agent-stable-1"
	updated, _, err := s.upsertAgentFromStat("10.0.0.2", oldResp)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.HostMetadata) == 0 {
		t.Fatal("existing host_metadata should be preserved when old agent reports none")
	}
	var meta map[string]interface{}
	if err := util.UnmarshalJSONB(updated.HostMetadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["os_name"] != "Ubuntu" {
		t.Fatalf("preserved host_metadata wrong: %#v", meta)
	}

	// 新 Agent 响应：上报元数据 → 覆盖为最新值
	newResp := sampleStatResponseWithMeta(nil, &commonpb.HostMetadata{
		OsName: "Debian", OsVersion: "12", KernelVersion: "6.1.0-18-amd64",
		Architecture: "x86_64", CpuCores: 4, UptimeSeconds: 100,
	})
	newResp.AgentId = "agent-stable-1"
	updated2, _, err := s.upsertAgentFromStat("10.0.0.2", newResp)
	if err != nil {
		t.Fatal(err)
	}
	var meta2 map[string]interface{}
	if err := util.UnmarshalJSONB(updated2.HostMetadata, &meta2); err != nil {
		t.Fatal(err)
	}
	if meta2["os_name"] != "Debian" || meta2["os_version"] != "12" {
		t.Fatalf("new host_metadata should overwrite: %#v", meta2)
	}
}
