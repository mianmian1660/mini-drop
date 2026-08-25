// ============================================================
// server/continuous_correctness_test.go — 阶段一：持续采集数据正确性
// ============================================================
// 覆盖（计划「测试、迁移与交付门禁」要求）：
//   - 精确重传（同 batch_id + 同摘要）→ 200 duplicate，窗口不重复计数
//   - 同 batch_id 不同内容 → 409 不可重试冲突（禁止换 ID）
//   - 不同 batch 携带相同 window_id → 窗口级去重（最多入库一次）
//   - 相同 window_id 不同内容 → 409 冲突整批回滚
//   - v3 batch.sample_count=0；分信号 signal_counts 独立计数
//   - CPU-only Session：非 CPU 信号拒绝；入库仅 CPU 窗口
//   - Timeline finalization（running pending tail / stopped 边界 / stalled）
//   - 历史重复自动修复（dry-run / apply + 审计）
// ============================================================

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

// postContinuousBatch 发送一个 v3 continuous batch，返回响应 recorder。
func postContinuousBatch(t *testing.T, s *APIServer, body string, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", sid)
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	return w
}

// makeV3Batch 构造 v3 batch JSON。
func makeV3Batch(sid, batchID string, windows []map[string]interface{}) string {
	start := time.Now().UTC().Add(-30 * time.Second)
	end := time.Now().UTC()
	windowsJSON, _ := json.Marshal(windows)
	body := fmt.Sprintf(`{"session_sid":%q,"batch_id":%q,"target_ip":"10.0.0.8","schema_version":3,
		"collector_generation":"gen-test-1","batch_sequence":1,"content_sha256":%q,
		"start_time":%q,"end_time":%q,"windows":%s}`,
		sid, batchID, strings.Repeat("a", 64), start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), string(windowsJSON))
	return body
}

func makeV4Batch(sid, batchID string, windows []map[string]interface{}) string {
	return strings.Replace(makeV3Batch(sid, batchID, windows), `"schema_version":3`, `"schema_version":4`, 1)
}

func createCorrectnessSession(t *testing.T, s *APIServer, sid, signals string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	session := model.ContinuousSession{
		SID: sid, TargetIP: "10.0.0.8", RetentionHours: 24,
		Status: model.ContinuousSessionStatusRunning, Scope: "host",
		DesiredState: model.ContinuousDesiredStateRunning, StartedAt: now.Add(-time.Minute),
		CreatedAt: now, UpdatedAt: now,
	}
	if signals != "" {
		session.Signals = []byte(`["` + strings.Join(strings.Split(signals, ","), `","`) + `"]`)
		session.RequestedSignals = session.Signals
	}
	if err := s.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
}

func TestV3BatchExactRetransmitIsDuplicate(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-dup", "")
	window := map[string]interface{}{
		"window_id": "cpw-test-1", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("1", 64), "signal_counts": map[string]interface{}{"cpu_profile": 2},
		"window_start": time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":   time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":      []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 2, "stack": []string{"main"}}},
	}
	body := makeV3Batch("cps-v3-dup", "cpb-v3-dup", []map[string]interface{}{window})
	w1 := postContinuousBatch(t, s, body, "cps-v3-dup")
	if w1.Code != http.StatusOK || !strings.Contains(w1.Body.String(), `"duplicate":false`) {
		t.Fatalf("first ingest status=%d body=%s", w1.Code, w1.Body.String())
	}
	w2 := postContinuousBatch(t, s, body, "cps-v3-dup")
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"duplicate":true`) {
		t.Fatalf("retransmit status=%d body=%s", w2.Code, w2.Body.String())
	}
	var count int64
	if err := s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ?", "cps-v3-dup").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("window count=%d err=%v, want 1", count, err)
	}
}

func TestV3BatchRejectsMissingCorrectnessIdentity(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-invalid", "")
	window := map[string]interface{}{
		"window_id": "cpw-invalid", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": "",
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"count": 1, "stack": []string{"main"}}},
	}
	w := postContinuousBatch(t, s, makeV3Batch("cps-v3-invalid", "cpb-v3-invalid", []map[string]interface{}{window}), "cps-v3-invalid")
	if w.Code != http.StatusConflict {
		t.Fatalf("missing v3 digest status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	var batches int64
	_ = s.DB.Model(&model.ProfileBatch{}).Where("session_sid = ?", "cps-v3-invalid").Count(&batches).Error
	if batches != 0 {
		t.Fatalf("invalid v3 batch must not be stored, got %d", batches)
	}
}

func TestV3BatchSameIDDifferentContentConflict(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-conflict", "")
	window := map[string]interface{}{
		"window_id": "cpw-test-conflict", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("2", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 2, "stack": []string{"main"}}},
	}
	body := makeV3Batch("cps-v3-conflict", "cpb-v3-conflict", []map[string]interface{}{window})
	if w := postContinuousBatch(t, s, body, "cps-v3-conflict"); w.Code != http.StatusOK {
		t.Fatalf("first ingest status=%d body=%s", w.Code, w.Body.String())
	}
	// 同 batch_id，batch 级 content_sha256 不同 → 409 不可重试冲突。
	// （窗口级 content_sha256 不变；此处校验的是 batch 层幂等摘要。）
	conflictBody := strings.Replace(body, strings.Repeat("a", 64), strings.Repeat("b", 64), 1)
	w := postContinuousBatch(t, s, conflictBody, "cps-v3-conflict")
	if w.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	var count int64
	if err := s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ?", "cps-v3-conflict").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("window count=%d err=%v, want 1 (conflict must not add windows)", count, err)
	}
}

func TestV3WindowDedupAcrossBatches(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-window-dup", "")
	window := map[string]interface{}{
		"window_id": "cpw-shared", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("3", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 1, "stack": []string{"main"}}},
	}
	bodyA := makeV3Batch("cps-v3-window-dup", "cpb-batch-a", []map[string]interface{}{window})
	bodyB := makeV3Batch("cps-v3-window-dup", "cpb-batch-b", []map[string]interface{}{window})
	if w := postContinuousBatch(t, s, bodyA, "cps-v3-window-dup"); w.Code != http.StatusOK {
		t.Fatalf("batch A status=%d body=%s", w.Code, w.Body.String())
	}
	if w := postContinuousBatch(t, s, bodyB, "cps-v3-window-dup"); w.Code != http.StatusOK {
		t.Fatalf("batch B status=%d body=%s", w.Code, w.Body.String())
	}
	var count int64
	if err := s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ? AND window_id = ?", "cps-v3-window-dup", "cpw-shared").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("window count=%d err=%v, want 1 (dedup across batches)", count, err)
	}
	// 两个 batch 都入账，但窗口只一份。
	var batches int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("session_sid = ?", "cps-v3-window-dup").Count(&batches).Error; err != nil || batches != 2 {
		t.Fatalf("batch count=%d err=%v, want 2", batches, err)
	}
}

func TestV3WindowConflictRollsBackBatch(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-window-conflict", "")
	windowA := map[string]interface{}{
		"window_id": "cpw-conflict", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("4", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 1, "stack": []string{"main"}}},
	}
	bodyA := makeV3Batch("cps-v3-window-conflict", "cpb-wc-a", []map[string]interface{}{windowA})
	if w := postContinuousBatch(t, s, bodyA, "cps-v3-window-conflict"); w.Code != http.StatusOK {
		t.Fatalf("batch A status=%d body=%s", w.Code, w.Body.String())
	}
	// 同一 window_id 但内容不同（sha-B）→ 409 冲突，batch B 整批回滚不落库。
	windowB := map[string]interface{}{
		"window_id": "cpw-conflict", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("5", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 9, "stack": []string{"other"}}},
	}
	bodyB := makeV3Batch("cps-v3-window-conflict", "cpb-wc-b", []map[string]interface{}{windowB})
	w := postContinuousBatch(t, s, bodyB, "cps-v3-window-conflict")
	if w.Code != http.StatusConflict {
		t.Fatalf("window conflict status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	var batches int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("session_sid = ?", "cps-v3-window-conflict").Count(&batches).Error; err != nil || batches != 1 {
		t.Fatalf("batch count=%d err=%v, want 1 (batch B rolled back)", batches, err)
	}
}

func TestV3BatchSampleCountZeroAndSignalCounts(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-counts", "")
	window := map[string]interface{}{
		"window_id": "cpw-counts", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("6", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 5, "stack": []string{"main"}}},
		"histograms": []map[string]interface{}{
			{"signal_type": "io_latency", "backend": "perf", "event_count": 7,
				"summary": map[string]interface{}{"min": 1, "max": 2, "p50": 1, "p95": 2, "p99": 2},
				"buckets": []map[string]interface{}{{"range": "[1, 2)", "low": 1, "high": 2, "count": 7}}},
		},
	}
	body := makeV3Batch("cps-v3-counts", "cpb-v3-counts", []map[string]interface{}{window})
	if w := postContinuousBatch(t, s, body, "cps-v3-counts"); w.Code != http.StatusOK {
		t.Fatalf("ingest status=%d body=%s", w.Code, w.Body.String())
	}
	var batch model.ProfileBatch
	if err := s.DB.Where("bid = ?", "cpb-v3-counts").First(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if batch.SampleCount != 0 {
		t.Fatalf("v3 batch sample_count=%d, want 0", batch.SampleCount)
	}
	if batch.SchemaVersion != 3 || batch.CollectorGeneration != "gen-test-1" || batch.BatchSequence != 1 {
		t.Fatalf("v3 batch metadata=%+v", batch)
	}
	var counts map[string]uint64
	_ = json.Unmarshal(batch.SignalCounts, &counts)
	if counts["cpu_profile"] != 5 || counts["io_latency"] != 7 {
		t.Fatalf("batch signal_counts=%v, want cpu_profile=5 io_latency=7", counts)
	}
	// 窗口行独立计数：cpu 行 5，io 行 7。
	var cpuWindow model.ProfileWindow
	if err := s.DB.Where("session_sid = ? AND signal_type = ?", "cps-v3-counts", "cpu_profile").First(&cpuWindow).Error; err != nil {
		t.Fatal(err)
	}
	if cpuWindow.SampleCount != 5 {
		t.Fatalf("cpu window sample_count=%d, want 5", cpuWindow.SampleCount)
	}
	var ioWindow model.ProfileWindow
	if err := s.DB.Where("session_sid = ? AND signal_type = ?", "cps-v3-counts", "io_latency").First(&ioWindow).Error; err != nil {
		t.Fatal(err)
	}
	if ioWindow.SampleCount != 7 {
		t.Fatalf("io window sample_count=%d, want 7", ioWindow.SampleCount)
	}
}

func TestV3CpuOnlySessionRejectsUnrequestedSignal(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-cpuonly", "cpu_profile")
	window := map[string]interface{}{
		"window_id": "cpw-cpuonly", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("7", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 3, "stack": []string{"main"}}},
		"histograms": []map[string]interface{}{
			{"signal_type": "io_latency", "backend": "perf", "event_count": 7,
				"summary": map[string]interface{}{"min": 1, "max": 2, "p50": 1, "p95": 2, "p99": 2},
				"buckets": []map[string]interface{}{{"range": "[1, 2)", "low": 1, "high": 2, "count": 7}}},
		},
	}
	body := makeV3Batch("cps-v3-cpuonly", "cpb-cpuonly", []map[string]interface{}{window})
	w := postContinuousBatch(t, s, body, "cps-v3-cpuonly")
	if w.Code != http.StatusConflict {
		t.Fatalf("cpu-only session with io signal status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	var ioCount int64
	_ = s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ? AND signal_type = ?", "cps-v3-cpuonly", "io_latency").Count(&ioCount).Error
	if ioCount != 0 {
		t.Fatalf("cpu-only session should have zero io windows, got %d", ioCount)
	}
}

func TestV3CpuOnlySessionStoresOnlyCPU(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v3-cpuonly2", "cpu_profile")
	window := map[string]interface{}{
		"window_id": "cpw-cpuonly2", "collector_generation": "gen-test-1", "target_fingerprint": "fp-1",
		"content_sha256": strings.Repeat("8", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"samples":        []map[string]interface{}{{"comm": "test", "pid": 1, "exe": "/bin/test", "count": 3, "stack": []string{"main"}}},
	}
	body := makeV3Batch("cps-v3-cpuonly2", "cpb-cpuonly2", []map[string]interface{}{window})
	if w := postContinuousBatch(t, s, body, "cps-v3-cpuonly2"); w.Code != http.StatusOK {
		t.Fatalf("ingest status=%d body=%s", w.Code, w.Body.String())
	}
	var total int64
	_ = s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ?", "cps-v3-cpuonly2").Count(&total).Error
	if total != 1 {
		t.Fatalf("cpu-only session window total=%d, want 1", total)
	}
	var ioCount, schedCount int64
	_ = s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ? AND signal_type IN ?", "cps-v3-cpuonly2", []string{"io_latency", "sched_latency"}).Count(&ioCount).Error
	if ioCount != 0 {
		t.Fatalf("cpu-only session has non-cpu windows: %d", ioCount)
	}
	_ = schedCount // keep var referenced
}

func TestV4StatusOnlyWindowCreatesEverySignalRow(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v4-status", "cpu_profile,io_latency")
	window := map[string]interface{}{
		"window_id": "cpw-v4-status", "collector_generation": "gen-test-1", "target_fingerprint": "fp-v4",
		"content_sha256": strings.Repeat("9", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"signal_statuses": map[string]interface{}{
			"cpu_profile": map[string]interface{}{"status": "target_idle"},
			"io_latency":  map[string]interface{}{"status": "no_events"},
		},
	}
	body := makeV4Batch("cps-v4-status", "cpb-v4-status", []map[string]interface{}{window})
	if w := postContinuousBatch(t, s, body, "cps-v4-status"); w.Code != http.StatusOK {
		t.Fatalf("v4 status ingest=%d body=%s", w.Code, w.Body.String())
	}
	var rows []model.ProfileWindow
	if err := s.DB.Where("session_sid = ?", "cps-v4-status").Order("signal_type").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SignalType != "cpu_profile" || rows[1].SignalType != "io_latency" {
		t.Fatalf("status rows=%+v", rows)
	}
	if rows[0].SignalStatus != "target_idle" || rows[1].SignalStatus != "no_events" {
		t.Fatalf("status values=%+v", rows)
	}
}

func TestV4RejectsUnrequestedSideChannelSignal(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-v4-cpu", "cpu_profile")
	window := map[string]interface{}{
		"window_id": "cpw-v4-side", "collector_generation": "gen-test-1", "target_fingerprint": "fp-v4",
		"content_sha256": strings.Repeat("b", 64),
		"window_start":   time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano),
		"window_end":     time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano),
		"signal_statuses": map[string]interface{}{
			"python_rss": map[string]interface{}{"status": "no_events"},
		},
	}
	w := postContinuousBatch(t, s, makeV4Batch("cps-v4-cpu", "cpb-v4-side", []map[string]interface{}{window}), "cps-v4-cpu")
	if w.Code != http.StatusConflict {
		t.Fatalf("unrequested v4 side signal status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestContinuousFinalizationStateRunningPendingAndStalled(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	session := model.ContinuousSession{
		SID: "cps-finalize", TargetIP: "10.0.0.8", UploadBatchSec: 60, AggregationWindowSec: 10,
		DesiredState: "running", StartedAt: now.Add(-5 * time.Second), CreatedAt: now.Add(-5 * time.Second),
	}
	if err := s.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	// 刚启动且无 batch → pending，finalized_to = started_at。
	fin := s.continuousFinalizationState(session, now)
	if !fin.Pending || fin.CollectorStalled {
		t.Fatalf("no-batch state=%+v, want pending", fin)
	}
	// 最新 batch end = now-5s（grace 内）→ pending，非 stalled。
	if err := s.DB.Create(&model.ProfileBatch{
		BID: "cpb-finalize-1", SessionSID: session.SID, StartTime: now.Add(-70 * time.Second),
		EndTime: now.Add(-5 * time.Second), ReceivedAt: now.Add(-4 * time.Second), CreatedAt: now.Add(-4 * time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}
	fin = s.continuousFinalizationState(session, now)
	if !fin.Pending || fin.CollectorStalled {
		t.Fatalf("grace state=%+v", fin)
	}
	if fin.DeliveryLagSeconds < 0.9 || fin.DeliveryLagSeconds > 1.1 {
		t.Fatalf("delivery_lag_seconds=%v, want ~1s", fin.DeliveryLagSeconds)
	}
	wantEnd := now.Add(-5 * time.Second)
	if fin.FinalizedTo.Sub(wantEnd).Abs() > time.Millisecond {
		t.Fatalf("finalized_to=%v, want ~%v", fin.FinalizedTo, wantEnd)
	}
	// 最新 batch end 超过 grace（now-3min）→ stalled；grace 尾部仍是
	// pending，而 lastEnd 到 finalized horizon 已进入真实 gap 计算域。
	s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "cpb-finalize-1").
		Updates(map[string]interface{}{"end_time": now.Add(-3 * time.Minute)})
	fin = s.continuousFinalizationState(session, now)
	if !fin.Pending || !fin.CollectorStalled {
		t.Fatalf("stalled state=%+v", fin)
	}
	wantHorizon := now.Add(-(60 + 2*10 + 15) * time.Second)
	if fin.FinalizedTo.Sub(wantHorizon).Abs() > time.Millisecond {
		t.Fatalf("stalled finalized_to=%v, want horizon ~%v", fin.FinalizedTo, wantHorizon)
	}
	// 停止后 → 不再 pending，finalized_to 为 stopped_at。
	stopped := now.Add(-time.Minute)
	session.StoppedAt = &stopped
	fin = s.continuousFinalizationState(session, now)
	if fin.Pending || fin.CollectorStalled {
		t.Fatalf("stopped state=%+v", fin)
	}
}

func TestContinuousRepairDryRunAndApply(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	createCorrectnessSession(t, s, "cps-repair", "")
	now := time.Now().UTC().Truncate(time.Millisecond)
	// 同一逻辑窗口（同起止时间+信号）在不同 batch 中各出现一次 → 重复。
	for _, bid := range []string{"cpb-rep-a", "cpb-rep-b"} {
		objectKey := "continuous/cps-repair/" + bid + ".json"
		if err := s.DB.Create(&model.ProfileBatch{
			BID: bid, SessionSID: "cps-repair", StartTime: now.Add(-30 * time.Second),
			EndTime: now.Add(-20 * time.Second), ReceivedAt: now, CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Create(&model.ProfileWindow{
			SessionSID: "cps-repair", BatchBID: bid, WindowStart: now.Add(-30 * time.Second),
			WindowEnd: now.Add(-20 * time.Second), SignalType: "cpu_profile", SampleCount: 5,
			ObjectKey: objectKey, CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		payload := fmt.Sprintf(`{"session_sid":"cps-repair","batch_id":%q,"start_time":%q,"end_time":%q,"windows":[{"window_start":%q,"window_end":%q,"samples":[{"count":5,"stack":["main"]}]}]}`,
			bid, now.Add(-30*time.Second).Format(time.RFC3339Nano), now.Add(-20*time.Second).Format(time.RFC3339Nano),
			now.Add(-30*time.Second).Format(time.RFC3339Nano), now.Add(-20*time.Second).Format(time.RFC3339Nano))
		if err := s.Storage.PutObject(context.Background(), s.Config.Storage.Bucket, objectKey,
			strings.NewReader(payload), int64(len(payload)), "application/json"); err != nil {
			t.Fatal(err)
		}
	}
	// dry-run
	report, err := s.runContinuousRepair(context.Background(), "dry-run", "cps-repair", nil, nil)
	if err != nil {
		t.Fatalf("dry-run err=%v", err)
	}
	if report.Groups != 1 || report.Excluded != 1 {
		t.Fatalf("dry-run report=%+v, want 1 group 1 excluded", report)
	}
	var afterDry int64
	_ = s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ?", "cps-repair").Count(&afterDry).Error
	if afterDry != 2 {
		t.Fatalf("dry-run must not modify data, windows=%d", afterDry)
	}
	// apply
	report, err = s.runContinuousRepair(context.Background(), "apply", "cps-repair", nil, nil)
	if err != nil {
		t.Fatalf("apply err=%v", err)
	}
	if report.Excluded != 1 || report.AuditRows != 1 || report.Kept != 1 {
		t.Fatalf("apply report=%+v", report)
	}
	var kept int64
	_ = s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ?", "cps-repair").Count(&kept).Error
	if kept != 1 {
		t.Fatalf("after apply windows=%d, want 1", kept)
	}
	var audits int64
	if err := s.DB.Model(&model.ContinuousRepairAudit{}).Where("run_id = ?", report.RunID).Count(&audits).Error; err != nil || audits != 1 {
		t.Fatalf("audit rows=%d err=%v, want 1", audits, err)
	}
	// 保留的 canonical 窗口已回填 window_id。
	var keptWindow model.ProfileWindow
	if err := s.DB.Where("session_sid = ?", "cps-repair").First(&keptWindow).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(keptWindow.WindowID, "cpw-legacy-") {
		t.Fatalf("kept window_id=%q, want cpw-legacy- prefix", keptWindow.WindowID)
	}
}

func TestContinuousRepairEndpointRequiresPlatformAdmin(t *testing.T) {
	s := newTestAPIServer(t)
	router := gin.New()
	router.POST("/api/v1/internal/continuous/repair", s.RepairContinuousDuplicates)
	body := `{"mode":"dry-run"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/repair", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "operator-1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("operator repair status=%d body=%s, want 403", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/repair", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "admin-1")
	req.Header.Set("Drop-User-Role", RolePlatformAdmin)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("platform admin repair status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}
