// ============================================================
// server/storage_status_test.go — 阶段 0 磁盘止血测试
// ============================================================
// 覆盖：
//   - 磁盘等级边界（恰好 8GiB / 4GiB / 1GiB 及阈值上下 1 byte）
//   - statfs 失败时 fail-closed（unknown + 拒绝采集）
//   - warning/critical 不拒收，emergency 才拒绝（HTTP 507）
//   - /api/v1/storage/status 鉴权与响应结构
//   - Prometheus 指标值及按来源的拒收计数
// ============================================================

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/config"
)

const (
	giB = uint64(1 << 30)
)

// setDiskFree 替换 statfs 读取函数并返回清理函数。
func setDiskFree(t *testing.T, total, avail, used uint64, err error) {
	t.Helper()
	original := readStorageDiskSnapshot
	readStorageDiskSnapshot = func(string) (uint64, uint64, uint64, error) {
		return total, avail, used, err
	}
	t.Cleanup(func() { readStorageDiskSnapshot = original })
}

func newDiskGuardServer(t *testing.T) *APIServer {
	t.Helper()
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
	}}
	return s
}

func storageStatusRouter(s *APIServer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api/v1")
	api.Use(s.CheckLogin)
	api.GET("/storage/status", s.StorageStatus)
	api.POST("/tasks", s.CreateTask)
	return router
}

func authRequest(method, path string, body []byte) *http.Request {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "user-1")
	req.Header.Set("Drop-User-Name", "tester")
	return req
}

// ---------------------------------------------------------------------------
// 等级边界：恰好阈值与阈值上下 1 byte
// ---------------------------------------------------------------------------

func TestStorageLevelBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		avail       uint64
		wantLevel   StoragePressureLevel
		wantAllowed bool
	}{
		{"exactly 8GiB -> normal", 8 * giB, StoragePressureNormal, true},
		{"8GiB minus 1 -> warning", 8*giB - 1, StoragePressureWarning, true},
		{"exactly 4GiB -> warning", 4 * giB, StoragePressureWarning, true},
		{"4GiB minus 1 -> critical", 4*giB - 1, StoragePressureCritical, true},
		{"exactly 1GiB -> critical", 1 * giB, StoragePressureCritical, true},
		{"1GiB minus 1 -> emergency", 1*giB - 1, StoragePressureEmergency, false},
		{"zero -> emergency", 0, StoragePressureEmergency, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDiskFree(t, 40*giB, tc.avail, 40*giB-tc.avail, nil)
			s := newTestAPIServer(t)
			s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
				Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
			}}
			snap := s.currentStorageSnapshot()
			if snap.Level != tc.wantLevel {
				t.Fatalf("level = %s, want %s (avail=%d)", snap.Level, tc.wantLevel, tc.avail)
			}
			if snap.CollectionAllowed != tc.wantAllowed {
				t.Fatalf("collection_allowed = %v, want %v", snap.CollectionAllowed, tc.wantAllowed)
			}
			if snap.AvailableBytes != tc.avail {
				t.Fatalf("available_bytes = %d, want %d", snap.AvailableBytes, tc.avail)
			}
			if snap.CheckedAt.IsZero() {
				t.Fatal("checked_at must be set")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// statfs 失败 → fail-closed
// ---------------------------------------------------------------------------

func TestStorageStatfsFailureFailsClosed(t *testing.T) {
	setDiskFree(t, 0, 0, 0, http.ErrBodyNotAllowed) // 任意错误即可
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
	}}
	snap := s.currentStorageSnapshot()
	if snap.Level != StoragePressureUnknown {
		t.Fatalf("level = %s, want unknown", snap.Level)
	}
	if snap.CollectionAllowed {
		t.Fatal("statfs failure must not allow collection")
	}
	ok, message, _ := s.canStartCollection(CollectionSourceOneShot)
	if ok || !strings.Contains(message, "采集被拒绝") {
		t.Fatalf("canStartCollection = ok:%v message:%q", ok, message)
	}
	// 不伪造剩余空间：unknown 快照保持 0
	if snap.AvailableBytes != 0 || snap.TotalBytes != 0 {
		t.Fatalf("unknown snapshot must not fabricate space: total=%d avail=%d", snap.TotalBytes, snap.AvailableBytes)
	}
}

// ---------------------------------------------------------------------------
// warning/critical 不拒收，emergency/unknown 才拒收
// ---------------------------------------------------------------------------

func TestCollectionOnlyRejectedOnEmergencyOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		avail   uint64
		wantRej bool
	}{
		{"warning", 5 * giB, false},
		{"critical", 2 * giB, false},
		{"emergency", 512 * 1024 * 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetMetricsForTest()
			setDiskFree(t, 40*giB, tc.avail, 40*giB-tc.avail, nil)
			s := newTestAPIServer(t)
			s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
				Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
			}}
			ok, _, _ := s.canStartCollection(CollectionSourceOneShot)
			if ok == tc.wantRej {
				t.Fatalf("rejected = %v, want %v (avail=%d)", !ok, tc.wantRej, tc.avail)
			}
			counts := snapshotRejectedLowDiskCounts()
			wantCount := 0
			if tc.wantRej {
				wantCount = 1
			}
			if got := counts[CollectionSourceOneShot]; got != int64(wantCount) {
				t.Fatalf("rejected counter[one_shot] = %d, want %d", got, wantCount)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// /api/v1/storage/status：鉴权与响应结构
// ---------------------------------------------------------------------------

func TestStorageStatusEndpointRequiresAuth(t *testing.T) {
	setDiskFree(t, 40*giB, 20*giB, 20*giB, nil)
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
	}}
	router := storageStatusRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/storage/status", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without identity status = %d, want 401", w.Code)
	}
}

func TestStorageStatusEndpointResponseStructure(t *testing.T) {
	setDiskFree(t, 40*giB, 6*giB, 34*giB, nil) // 6GiB -> warning
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
	}}
	router := storageStatusRouter(s)

	req := authRequest(http.MethodGet, "/api/v1/storage/status", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var envelope struct {
		Data struct {
			Path              string `json:"path"`
			TotalBytes        uint64 `json:"total_bytes"`
			AvailableBytes    uint64 `json:"available_bytes"`
			UsedBytes         uint64 `json:"used_bytes"`
			Level             string `json:"level"`
			CollectionAllowed bool   `json:"collection_allowed"`
			CheckedAt         string `json:"checked_at"`
		} `json:"data"`
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope.Code != 0 || envelope.Data.Path != "/tmp" ||
		envelope.Data.TotalBytes != 40*giB || envelope.Data.AvailableBytes != 6*giB ||
		envelope.Data.UsedBytes != 34*giB || envelope.Data.Level != "warning" ||
		!envelope.Data.CollectionAllowed {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, envelope.Data.CheckedAt); err != nil {
		t.Fatalf("checked_at not RFC3339: %q", envelope.Data.CheckedAt)
	}
}

func TestStorageStatusEndpointUnknownWhenStatfsFails(t *testing.T) {
	setDiskFree(t, 0, 0, 0, http.ErrBodyNotAllowed)
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
	}}
	router := storageStatusRouter(s)
	req := authRequest(http.MethodGet, "/api/v1/storage/status", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"level":"unknown"`) ||
		!strings.Contains(w.Body.String(), `"collection_allowed":false`) {
		t.Fatalf("unknown response: %d %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// emergency → 采集入口 HTTP 507
// ---------------------------------------------------------------------------

func TestCreateTaskRejectedWith507OnEmergency(t *testing.T) {
	resetMetricsForTest()
	setDiskFree(t, 40*giB, 1*giB-1, 40*giB-(1*giB-1), nil) // emergency
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
	}}
	router := storageStatusRouter(s)

	body := []byte(`{"name":"t","target_ip":"10.0.0.99","task_kind":"perf_cpu","duration":5}`)
	req := authRequest(http.MethodPost, "/api/v1/tasks", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d body=%s, want 507", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "STORAGE_LOW_DISK") {
		t.Fatalf("missing error code: %s", w.Body.String())
	}
	counts := snapshotRejectedLowDiskCounts()
	if counts[CollectionSourceOneShot] != 1 {
		t.Fatalf("one_shot rejection counter = %d, want 1", counts[CollectionSourceOneShot])
	}
}

// ---------------------------------------------------------------------------
// Prometheus 指标
// ---------------------------------------------------------------------------

func TestStorageMetricsExposed(t *testing.T) {
	resetMetricsForTest()
	setDiskFree(t, 40*giB, 6*giB, 34*giB, nil) // warning
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 * giB, CriticalFreeBytes: 4 * giB, MinFreeBytes: 1 * giB,
	}}

	s.updateStorageMetricsForTest()
	incCollectionRejectedLowDisk(CollectionSourceScheduled)
	incCollectionRejectedLowDisk(CollectionSourceContinuous)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/metrics", s.Metrics)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", w.Code)
	}
	out := w.Body.String()
	for _, want := range []string{
		"mini_drop_storage_total_bytes 42949672960",
		"mini_drop_storage_available_bytes 6442450944",
		"mini_drop_storage_pressure_level 1",
		`mini_drop_collection_rejected_low_disk_total{source="scheduled"} 1`,
		`mini_drop_collection_rejected_low_disk_total{source="continuous"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, out)
		}
	}
}

// updateStorageMetricsForTest 用当前磁盘快照刷新一次指标（供测试直接调用）。
func (s *APIServer) updateStorageMetricsForTest() {
	updateStorageMetrics(s.currentStorageSnapshot())
}
