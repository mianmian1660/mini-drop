// ============================================================
// server/continuous_test.go — continuous symbol diagnostics 单测
// ============================================================
// 覆盖（修复计划 Step 4 要求的单测）：
//   - 最终状态只取决于过滤后实际帧，不受 runtime 元数据干扰
//   - native Go ready/pending/failed 仅作为结构化诊断
// ============================================================

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

func TestContinuousTimelineCoverageUsesFiveSecondTolerance(t *testing.T) {
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	windows := []model.ProfileWindow{
		{WindowStart: base, WindowEnd: base.Add(10 * time.Second)},
		{WindowStart: base.Add(15 * time.Second), WindowEnd: base.Add(25 * time.Second)},
		{WindowStart: base.Add(31 * time.Second), WindowEnd: base.Add(40 * time.Second)},
	}
	gaps, coverage := continuousTimelineCoverage(windows, base, base.Add(40*time.Second), 5*time.Second)
	if len(gaps) != 1 || gaps[0].Start != base.Add(25*time.Second) || gaps[0].DurationSeconds != 6 {
		t.Fatalf("unexpected gaps: %#v", gaps)
	}
	if coverage["gap_seconds"].(float64) != 6 {
		t.Fatalf("unexpected coverage: %#v", coverage)
	}
}

func TestContinuousAgentClockClassification(t *testing.T) {
	received := time.UnixMilli(100_000)
	for _, tc := range []struct {
		offset int64
		want   string
	}{{5000, "ok"}, {5001, "warning"}, {30001, "critical"}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Request.Header.Set("X-Mini-Drop-Agent-Time-Ms", fmt.Sprint(received.UnixMilli()-tc.offset))
		offset, status, observed := continuousAgentClock(c, received)
		if offset != tc.offset || status != tc.want || observed == nil {
			t.Fatalf("offset=%d status=%s observed=%v, want %d/%s", offset, status, observed, tc.offset, tc.want)
		}
	}
}

func TestContinuousSessionClockStatusRequiresObservation(t *testing.T) {
	session := model.ContinuousSession{AgentClockStatus: "ok"}
	if got := continuousSessionClockStatus(session); got != "unknown" {
		t.Fatalf("unobserved clock status=%q, want unknown", got)
	}
	now := time.Now()
	session.AgentClockObservedAt = &now
	if got := continuousSessionClockStatus(session); got != "ok" {
		t.Fatalf("observed clock status=%q, want ok", got)
	}
}

func TestContinuousBatchDuplicateReturnsAckWithoutDuplicateWindows(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.DB.Create(&model.ContinuousSession{
		SID: "cps-idempotent", TargetIP: "10.0.0.8", RetentionHours: 24,
		Status: model.ContinuousSessionStatusRunning, StartedAt: now.Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"session_sid":"cps-idempotent","batch_id":"cpb-idempotent","start_time":%q,"end_time":%q,"windows":[{"window_start":%q,"window_end":%q,"samples":[{"stack":["main"],"count":1}]}]}`,
		now.Add(-20*time.Second).Format(time.RFC3339Nano), now.Add(-10*time.Second).Format(time.RFC3339Nano),
		now.Add(-20*time.Second).Format(time.RFC3339Nano), now.Add(-10*time.Second).Format(time.RFC3339Nano))
	router := profileRouter(s)
	for attempt := 0; attempt < 2; attempt++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Drop-User-Uid", "owner")
		req.Header.Set("X-Mini-Drop-Agent-Time-Ms", fmt.Sprint(now.UnixMilli()))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) ||
			!strings.Contains(w.Body.String(), fmt.Sprintf(`"duplicate":%t`, attempt == 1)) {
			t.Fatalf("attempt %d status=%d body=%s", attempt, w.Code, w.Body.String())
		}
	}
	var count int64
	if err := s.DB.Model(&model.ProfileWindow{}).Where("batch_bid = ?", "cpb-idempotent").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("window count=%d err=%v", count, err)
	}

	conflictBody := strings.Replace(body, now.Add(-10*time.Second).Format(time.RFC3339Nano), now.Add(-9*time.Second).Format(time.RFC3339Nano), 1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(conflictBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("conflicting batch status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestContinuousBatchStorageFailureDoesNotAckOrPersist(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.DB.Create(&model.ContinuousSession{
		SID: "cps-no-storage", TargetIP: "10.0.0.9", RetentionHours: 24,
		Status: model.ContinuousSessionStatusRunning, StartedAt: now.Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"session_sid":"cps-no-storage","batch_id":"cpb-no-storage","start_time":%q,"end_time":%q,"windows":[]}`,
		now.Add(-20*time.Second).Format(time.RFC3339Nano), now.Add(-10*time.Second).Format(time.RFC3339Nano))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatalf("storage failure status=%d body=%s", w.Code, w.Body.String())
	}
	var count int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "cpb-no-storage").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed batch persisted count=%d err=%v", count, err)
	}
}

func TestContinuousSymbolMetadataCollectsGoState(t *testing.T) {
	agg := &continuousAggregate{SymbolStatus: "not_applicable", SymbolReasons: map[string]bool{}}
	refs := map[string]interface{}{
		"symbol_status": "missing",
		"native_go": map[string]interface{}{
			"pending": []interface{}{map[string]interface{}{"build_id": "abc", "dso": "/usr/bin/containerd", "reason": "background extraction"}},
			"ready":   []interface{}{},
			"failed":  []interface{}{},
		},
	}
	continuousAggregateSymbolMetadata(agg, refs, map[string]bool{"/usr/bin/containerd": true})
	if !agg.GoSymbolPending || !agg.SymbolReasons["background extraction"] {
		t.Fatalf("expected pending Go metadata, got %#v", agg)
	}
	// Explicit legacy status is intentionally ignored.
	agg.TotalFrameWeight = 10
	agg.UnresolvedFrameWeight = 2
	continuousFinalizeSymbolStatus(agg)
	if agg.SymbolStatus != "partial" {
		t.Fatalf("expected frame-derived partial, got %q", agg.SymbolStatus)
	}
}

func TestContinuousRuntimeFilterAndMemoryProfile(t *testing.T) {
	window := ContinuousWindowIngest{
		Samples: []ContinuousStackSample{{PID: 1, Runtime: "java", Count: 2, Stack: []string{"javaBurnWorker"}}},
		Profiles: []ContinuousProfileIngest{
			{SignalType: "python_memory", ProfileID: "mem-1", Unit: "bytes", Backend: "memray", Samples: []ContinuousStackSample{{PID: 2, Runtime: "python", Count: 4096, Stack: []string{"pyMemoryPeakWorker"}}}},
			{SignalType: "python_memory", ProfileID: "mem-1", Unit: "bytes", Backend: "memray", Samples: []ContinuousStackSample{{PID: 2, Runtime: "python", Count: 4096, Stack: []string{"duplicate"}}}},
		},
	}
	cpu := continuousProfileSamplesForQuery(window, ProfileQuery{ProfileType: "cpu"}, map[string]bool{})
	if len(cpu) != 1 || cpu[0].Runtime != "java" {
		t.Fatalf("unexpected CPU samples: %#v", cpu)
	}
	memory := continuousProfileSamplesForQuery(window, ProfileQuery{ProfileType: "memory"}, map[string]bool{})
	if len(memory) != 1 || memory[0].Stack[0] != "pyMemoryPeakWorker" {
		t.Fatalf("memory profile dedupe failed: %#v", memory)
	}
	if !continuousSampleMatches(memory[0], nil, map[string]interface{}{"runtime": "python", "pid": "2"}) {
		t.Fatal("expected intersecting runtime/pid filters to match")
	}
	if continuousSampleMatches(memory[0], nil, map[string]interface{}{"runtime": "java"}) {
		t.Fatal("unexpected runtime filter match")
	}
}

func TestDownsampleRSSPointsKeepsBucketPeaks(t *testing.T) {
	points := make([]ProfileTimeseriesPoint, 1201)
	start := time.Unix(0, 0)
	for i := range points {
		points[i] = ProfileTimeseriesPoint{Timestamp: start.Add(time.Duration(i) * time.Second), Value: uint64(i % 17)}
	}
	points[700].Value = 99999
	out := downsampleRSSPoints(points, 600)
	if len(out) != 600 {
		t.Fatalf("got %d points", len(out))
	}
	foundPeak := false
	for i, point := range out {
		if point.Value == 99999 {
			foundPeak = true
		}
		if i > 0 && point.Timestamp.Before(out[i-1].Timestamp) {
			t.Fatal("points not time ordered")
		}
	}
	if !foundPeak {
		t.Fatal("peak was lost during downsampling")
	}
}

func TestRSSSeriesKeySeparatesReusedPID(t *testing.T) {
	first := ContinuousMetricIngest{PID: 42, ProcessStartMs: 1000, Exe: "/usr/bin/python3"}
	second := ContinuousMetricIngest{PID: 42, ProcessStartMs: 2000, Exe: "/usr/bin/python3"}
	legacy := ContinuousMetricIngest{PID: 42, Exe: "/usr/bin/python3"}
	if continuousMetricSeriesKey(first) == continuousMetricSeriesKey(second) {
		t.Fatal("PID reuse must produce separate RSS series")
	}
	if continuousMetricSeriesKey(first) == continuousMetricSeriesKey(legacy) {
		t.Fatal("new and legacy process identities must not be merged")
	}
}

func TestFailedMemoryProfileCreatesQueryableSignalRow(t *testing.T) {
	window := ContinuousWindowIngest{Profiles: []ContinuousProfileIngest{{
		SignalType: "python_memory", Backend: "memray", ProfileID: "memray-7-12345-acde",
	}}}
	rows := continuousWindowSignalRows(window)
	if len(rows) != 1 || rows[0].SignalType != "python_memory" || rows[0].Backend != "memray" {
		t.Fatalf("unexpected signal rows: %#v", rows)
	}
	if count := continuousWindowSampleCount(window, "python_memory"); count != 0 {
		t.Fatalf("diagnostic-only memory window count=%d, want 0", count)
	}
}

func TestContinuousSymbolMetadataExcludesUnrelatedDSO(t *testing.T) {
	agg := &continuousAggregate{SymbolStatus: "not_applicable", SymbolReasons: map[string]bool{}}
	refs := map[string]interface{}{
		"native_go": map[string]interface{}{
			"pending": []interface{}{map[string]interface{}{"build_id": "dockerd-id", "dso": "/usr/bin/dockerd", "reason": "background extraction"}},
			"ready":   []interface{}{map[string]interface{}{"build_id": "containerd-id", "dso": "/usr/bin/containerd"}},
			"failed":  []interface{}{},
		},
	}
	continuousAggregateSymbolMetadata(agg, refs, map[string]bool{"/usr/bin/containerd": true})
	if agg.GoSymbolPending || !agg.GoSymbolReady {
		t.Fatalf("expected only containerd ready metadata, got %#v", agg)
	}
	if len(agg.SymbolReasons) != 0 {
		t.Fatalf("unexpected unrelated reasons: %#v", agg.SymbolReasons)
	}
}

func TestContinuousFinalizeSymbolStatusFromFrames(t *testing.T) {
	tests := []struct {
		total, unresolved float64
		want              string
	}{
		{0, 0, "not_applicable"},
		{10, 0, "complete"},
		{10, 2, "partial"},
		{10, 10, "missing"},
	}
	for _, tt := range tests {
		agg := &continuousAggregate{TotalFrameWeight: tt.total, UnresolvedFrameWeight: tt.unresolved}
		continuousFinalizeSymbolStatus(agg)
		if agg.SymbolStatus != tt.want {
			t.Fatalf("total=%v unresolved=%v got=%q want=%q", tt.total, tt.unresolved, agg.SymbolStatus, tt.want)
		}
	}
}

func TestContinuousFrameLooksUnresolved(t *testing.T) {
	for _, frame := range []string{"0x5c0d180b8f25 [containerd]", "5c0d180b8f25", "[kernel.kallsyms]", "unknown"} {
		if !continuousFrameLooksUnresolved(frame) {
			t.Fatalf("expected unresolved frame: %q", frame)
		}
	}
	for _, frame := range []string{"runtime.main", "github.com/acme/pkg.Func", "net/http.(*Server).Serve"} {
		if continuousFrameLooksUnresolved(frame) {
			t.Fatalf("expected resolved frame: %q", frame)
		}
	}
}
