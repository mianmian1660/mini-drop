// ============================================================
// server/phase6_test.go — 阶段六：细粒度目录瘦身测试
// ============================================================
// 覆盖：
//   - 信号映射修正（python_rss → metrics）
//   - per-signal lineage（batch 只登记实际贡献的信号）
//   - 对账门禁（pqFindBestBlock 只接受 reconcile_status=passed）
//   - coverage segment 合并（≤5s 间隔合并，真实缺口保留）
//   - 细粒度 GC observe 不删 / enforce 只删完整覆盖数据
//   - 迁移失败重试与隔离
// ============================================================

package server

import (
	"context"
	"testing"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// TestPQSignalMappingMetrics 信号映射：python_rss window → metrics v2 行。
func TestPQSignalMappingMetrics(t *testing.T) {
	s := pqTestServer(t)
	window := model.ProfileWindow{
		SessionSID: "s1", SignalType: "python_rss", WindowStart: time.Now().Add(-time.Minute),
	}
	in := ContinuousWindowIngest{
		WindowStart: window.WindowStart, WindowEnd: window.WindowStart.Add(time.Minute),
		Metrics: []ContinuousMetricIngest{
			{PID: 1, Metric: "rss_bytes", Value: 4096, Unit: "bytes", Comm: "python"},
		},
		Labels: map[string]interface{}{"agent_id": "a1"},
	}
	batch := &continuousStoredBatch{Windows: []ContinuousWindowIngest{in}}
	rows := pqCollectWindowRows(s, window, batch, &in)
	if len(rows.CPU) != 0 {
		t.Fatalf("python_rss must not produce CPU rows: %d", len(rows.CPU))
	}
	if len(rows.Metrics) != 1 || rows.Metrics[0].Metric != "rss_bytes" || rows.Metrics[0].Value != 4096 {
		t.Fatalf("python_rss must map to metrics rows: %+v", rows.Metrics)
	}
	if got := pqLedgerSignalForWindow("python_rss"); got != model.ContinuousParquetSignalMetrics {
		t.Fatalf("pqLedgerSignalForWindow(python_rss) = %q", got)
	}
}

// TestPQPerSignalLineage 每个信号只登记实际贡献的 batch member。
func TestPQPerSignalLineage(t *testing.T) {
	acc := map[string]map[string]*pqSignalMemberAcc{}
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	pqAccumulateSignalMembers(acc, model.ContinuousParquetSignalCPU, "s1", "b-cpu",
		[]pqCPURow{{Timestamp: start.UnixMilli(), SessionSID: "s1", Value: 3}}, start, start.Add(time.Minute))
	pqAccumulateSignalMembers(acc, model.ContinuousParquetSignalMetrics, "s1", "b-rss",
		[]pqMetricRow{{Timestamp: start.UnixMilli(), SessionSID: "s1", Count: 1, Value: 10}}, start, start.Add(time.Minute))

	cpuMembers := pqMemberRowsFor(acc[model.ContinuousParquetSignalCPU])
	if len(cpuMembers) != 1 || cpuMembers[0].SourceRef != "b-cpu" || cpuMembers[0].SampleCount != 3 {
		t.Fatalf("cpu lineage wrong: %+v", cpuMembers)
	}
	if len(cpuMembers) > 0 && cpuMembers[0].RowCount != 1 {
		t.Fatalf("cpu member row_count wrong: %+v", cpuMembers[0])
	}
	if got := pqMemberRowsFor(acc[model.ContinuousParquetSignalHistogram]); got != nil {
		t.Fatalf("histogram must have no members (no rows contributed): %+v", got)
	}
	// 同一 batch 跨信号不能共享 member
	pqAccumulateSignalMembers(acc, model.ContinuousParquetSignalMetrics, "s1", "b-cpu",
		[]pqMetricRow{{Timestamp: start.UnixMilli(), SessionSID: "s1", Count: 1, Value: 5}}, start, start.Add(time.Minute))
	metricMembers := pqMemberRowsFor(acc[model.ContinuousParquetSignalMetrics])
	if len(metricMembers) != 2 {
		t.Fatalf("metrics must track both contributing batches: %+v", metricMembers)
	}
}

// TestPQFindBestBlockRequiresReconcile 对账门禁：pending/failed 块不可查询。
func TestPQFindBestBlockRequiresReconcile(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	hour := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	key := pqBlockKey{Tenant: "default", BucketStart: hour, SignalType: model.ContinuousParquetSignalCPU, Resolution: model.ContinuousParquetResolutionRaw}
	if _, err := s.pqCreateBuildingBlock(ctx, key, "pq-rec-1", hour.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	// 直接置 active + pending（模拟未对账）
	if err := s.DB.Model(&model.ContinuousParquetBlock{}).Where("block_id = ?", "pq-rec-1").
		Updates(map[string]interface{}{"status": "active", "validation": "passed", "reconcile_status": "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	if block, err := s.pqFindBestBlock(ctx, "default", hour, model.ContinuousParquetSignalCPU); err != nil || block != nil {
		t.Fatalf("pending reconcile block must be invisible to queries: block=%+v err=%v", block, err)
	}
	if s.pqCoverageForHour(ctx, "default", hour, model.ContinuousParquetSignalCPU) {
		t.Fatal("pending reconcile block must not count as coverage")
	}
	// 对账通过后可查
	if err := s.DB.Model(&model.ContinuousParquetBlock{}).Where("block_id = ?", "pq-rec-1").
		Updates(map[string]interface{}{"reconcile_status": "passed"}).Error; err != nil {
		t.Fatal(err)
	}
	if block, err := s.pqFindBestBlock(ctx, "default", hour, model.ContinuousParquetSignalCPU); err != nil || block == nil {
		t.Fatalf("reconciled block must be visible: block=%v err=%v", block, err)
	}
}

// TestPQCoverageSegmentMerge coverage segment：间隔 ≤5s 合并，>5s 缺口保留。
func TestPQCoverageSegmentMerge(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	windows := []model.ProfileWindow{
		{SessionSID: "s1", SignalType: "cpu_profile", WindowStart: hour.Add(time.Minute), WindowEnd: hour.Add(2 * time.Minute), SampleCount: 1},
		{SessionSID: "s1", SignalType: "cpu_profile", WindowStart: hour.Add(2*time.Minute + 4*time.Second), WindowEnd: hour.Add(3 * time.Minute), SampleCount: 2},
		{SessionSID: "s1", SignalType: "cpu_profile", WindowStart: hour.Add(10 * time.Minute), WindowEnd: hour.Add(11 * time.Minute), SampleCount: 4},
	}
	for _, w := range windows {
		if err := s.DB.Create(&w).Error; err != nil {
			t.Fatal(err)
		}
	}
	tx := s.DB.Begin()
	if err := s.pqRebuildCoverageSegmentsTx(tx, "default", hour, model.ContinuousParquetSignalCPU, "pq-cov-1", 1); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()

	var segments []model.ContinuousCoverageSegment
	if err := s.DB.Where("session_sid = ? AND signal_type = ?", "s1", model.ContinuousParquetSignalCPU).
		Order("segment_start ASC").Find(&segments).Error; err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("expected 2 segments (5s merge + real gap), got %d: %+v", len(segments), segments)
	}
	if segments[0].SampleCount != 3 {
		t.Fatalf("merged segment sample_count = %d, want 3", segments[0].SampleCount)
	}
	// 覆盖判定
	if !s.pqCoverageCovered(context.Background(), "s1", model.ContinuousParquetSignalCPU,
		hour.Add(time.Minute), hour.Add(3*time.Minute)) {
		t.Fatal("merged range must be covered")
	}
	if s.pqCoverageCovered(context.Background(), "s1", model.ContinuousParquetSignalCPU,
		hour.Add(3*time.Minute), hour.Add(10*time.Minute)) {
		t.Fatal("real gap must NOT be covered")
	}
}

// TestPQFineGCObserveAndEnforce 细粒度 GC：observe 不删，enforce 只删完整覆盖。
func TestPQFineGCObserveAndEnforce(t *testing.T) {
	s := pqTestServer(t)
	setDiskFree(t, 100<<30, 80<<30, 20<<30, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	hour := now.Truncate(time.Hour).Add(-3 * time.Hour)
	objKey := "continuous/s1/b1.json"

	// 源 batch（staging，未压缩）+ window（样本量与 v2 对齐）
	batch := model.ProfileBatch{
		BID: "b1", SessionSID: "s1", ObjectKey: objKey,
		StartTime: hour.Add(time.Minute), EndTime: hour.Add(2 * time.Minute),
		SignalTypes: mustJSONBytes([]string{"cpu_profile"}), Status: "ready",
		PayloadBytes: 100, CreatedAt: now.Add(-3 * time.Hour),
	}
	if err := s.DB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	window := model.ProfileWindow{
		SessionSID: "s1", BatchBID: "b1", ObjectKey: objKey,
		WindowStart: hour.Add(time.Minute), WindowEnd: hour.Add(2 * time.Minute),
		SignalType: "cpu_profile", SampleCount: 2, CreatedAt: now.Add(-3 * time.Hour),
	}
	if err := s.DB.Create(&window).Error; err != nil {
		t.Fatal(err)
	}

	// 构建 raw 块（激活时自动重建 coverage segments）
	s.Config.ContinuousParquet.Mode = "enforce"
	rows := []pqCPURow{{Timestamp: hour.Add(time.Minute).UnixMilli(), SessionSID: "s1", Value: 2, ProfileType: "cpu_profile"}}
	members := []model.ContinuousParquetBlockMember{{
		SourceKind: "batch", SourceRef: "b1", SessionSID: "s1",
		StartTime: hour.Add(time.Minute), EndTime: hour.Add(2 * time.Minute),
		SampleCount: 2, ValueTotal: 2, RowCount: 1,
	}}
	built, err := s.pqWriteSignalBlock(ctx, "default", hour, hour.Add(time.Hour),
		model.ContinuousParquetSignalCPU, model.ContinuousParquetResolutionRaw, "",
		rows, nil, nil, nil, map[string]bool{"s1": true}, nil, members)
	if err != nil || !built {
		t.Fatalf("raw block build failed: built=%v err=%v", built, err)
	}

	// observe：不删除
	s.Config.ContinuousParquet.FineRowGCMode = "observe"
	s.pqRunFineRowGC(ctx)
	if count := s.pqCountOrphanWindows(ctx); count != 0 {
		t.Fatalf("unexpected orphan windows: %d", count)
	}
	var remain int64
	if err := s.DB.Model(&model.ProfileWindow{}).Where("batch_bid = ?", "b1").Count(&remain).Error; err != nil || remain != 1 {
		t.Fatalf("observe must not delete windows: count=%d err=%v", remain, err)
	}
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "b1").Count(&remain).Error; err != nil || remain != 1 {
		t.Fatalf("observe must not delete batches: count=%d err=%v", remain, err)
	}

	// enforce：完整覆盖数据被清理（window + batch + staging 对象）
	s.Config.ContinuousParquet.FineRowGCMode = "enforce"
	s.pqRunFineRowGC(ctx)
	if err := s.DB.Model(&model.ProfileWindow{}).Where("batch_bid = ?", "b1").Count(&remain).Error; err != nil || remain != 0 {
		t.Fatalf("enforce must delete covered windows: count=%d err=%v", remain, err)
	}
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "b1").Count(&remain).Error; err != nil || remain != 0 {
		t.Fatalf("enforce must delete covered batches: count=%d err=%v", remain, err)
	}
	// v2 raw 块仍保留（查询源不变）
	var blockCount int64
	if err := s.DB.Model(&model.ContinuousParquetBlock{}).
		Where("status = ?", model.ContinuousParquetStatusActive).Count(&blockCount).Error; err != nil || blockCount != 1 {
		t.Fatalf("v2 raw block must survive fine GC: count=%d err=%v", blockCount, err)
	}
}

// TestPQFineGCBlockedWithoutCoverage 无完整覆盖时 GC 不删除（阻塞原因统计）。
func TestPQFineGCBlockedWithoutCoverage(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	batch := model.ProfileBatch{
		BID: "b-orphan", SessionSID: "s1", ObjectKey: "continuous/s1/b-orphan.json",
		StartTime: now.Add(-3 * time.Hour), EndTime: now.Add(-3 * time.Hour).Add(time.Minute),
		SignalTypes: mustJSONBytes([]string{"cpu_profile"}), Status: "ready",
		PayloadBytes: 10, CreatedAt: now.Add(-3 * time.Hour),
	}
	if err := s.DB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	s.Config.ContinuousParquet.Mode = "enforce"
	s.Config.ContinuousParquet.FineRowGCMode = "enforce"
	s.pqRunFineRowGC(ctx)
	// 无 v2 lineage → 阻塞，batch 保留
	var remain int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "b-orphan").Count(&remain).Error; err != nil || remain != 1 {
		t.Fatalf("uncovered batch must not be GC'd: count=%d err=%v", remain, err)
	}
}

// TestPQMigrationFailureQuarantine 迁移失败重试上限与隔离。
func TestPQMigrationFailureQuarantine(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	firstSeen := time.Now().Add(-45 * time.Minute)
	failure := model.ContinuousMigrationFailure{
		SourceKind: "window", SourceRef: "window-1", SessionSID: "s1",
		ObjectKey: "continuous/s1/missing.json", ErrorType: "missing_object",
		ErrorMessage: "no such key", FirstSeenAt: firstSeen, LastSeenAt: time.Now().Add(-15 * time.Minute),
		RetryCount: 3, Status: model.ContinuousMigrationFailureRetrying,
	}
	if err := s.DB.Create(&failure).Error; err != nil {
		t.Fatal(err)
	}
	s.pqProcessMigrationFailures(ctx, 100)
	var updated model.ContinuousMigrationFailure
	if err := s.DB.Where("id = ?", failure.ID).First(&updated).Error; err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.ContinuousMigrationFailureQuarantined {
		t.Fatalf("3 failures spanning 45min must quarantine: status=%s", updated.Status)
	}
	// 新失败从 retrying 开始
	s.recordContinuousMigrationFailure(ctx, "batch", "b-x", "s1", "continuous/s1/b-x.json", "object_delete", "boom")
	var fresh model.ContinuousMigrationFailure
	if err := s.DB.Where("source_kind = ? AND source_ref = ?", "batch", "b-x").First(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	if fresh.Status != model.ContinuousMigrationFailureRetrying || fresh.RetryCount != 0 {
		t.Fatalf("new failure must start retrying: status=%s retry=%d", fresh.Status, fresh.RetryCount)
	}
	// 重复记录累加 retry_count 且不回退 quarantine
	s.recordContinuousMigrationFailure(ctx, "batch", "b-x", "s1", "continuous/s1/b-x.json", "object_delete", "boom2")
	if err := s.DB.Where("source_kind = ? AND source_ref = ?", "batch", "b-x").First(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	if fresh.RetryCount != 1 {
		t.Fatalf("re-record must bump retry_count: %d", fresh.RetryCount)
	}
}

// TestPQHourBoundaryNoDoubleCount 小时边界不重复、不漏样本（半开区间）。
func TestPQHourBoundaryNoDoubleCount(t *testing.T) {
	s := pqTestServer(t)
	setDiskFree(t, 100<<30, 80<<30, 20<<30, nil)
	s.Config.ContinuousParquet.Mode = "prefer"
	ctx := context.Background()
	hour := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)

	// hour 10 的 raw 块（含 10:59:50 边界样本）
	rows := []pqCPURow{
		{Timestamp: hour.Add(10 * time.Minute).UnixMilli(), SessionSID: "s1", Frames: []pqCPUFrame{{Function: "a"}}, Value: 1, ProfileType: "cpu_profile"},
		{Timestamp: hour.Add(59*time.Minute + 50*time.Second).UnixMilli(), SessionSID: "s1", Frames: []pqCPUFrame{{Function: "b"}}, Value: 1, ProfileType: "cpu_profile"},
	}
	result, err := writeParquetPartGeneric(s, ctx, parquetObjectKeyV2("default", hour, "cpu", "raw", "pq-bd-0", 0), rows)
	if err != nil {
		t.Fatal(err)
	}
	key := pqBlockKey{Tenant: "default", BucketStart: hour, SignalType: model.ContinuousParquetSignalCPU, Resolution: model.ContinuousParquetResolutionRaw}
	if _, err := s.pqCreateBuildingBlock(ctx, key, "pq-bd-0", hour.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	stats := pqBlockStats{RowCount: 2, SampleTotal: 2, ValueTotal: 2, BytesTotal: result.SizeBytes}
	if err := s.pqRegisterActiveBlock(ctx, key, "pq-bd-0", hour.Add(time.Hour), 1, result, stats, nil); err != nil {
		t.Fatal(err)
	}

	q := ProfileQuery{SessionSID: "s1", Host: "10.0.0.1", OwnerUIDs: []string{"owner"},
		From: hour, To: hour.Add(time.Hour + 30*time.Minute), ProfileType: "cpu"}
	// 无 s1 session 行 → 授权为空；补一个 session
	_ = s.DB.Create(&model.ContinuousSession{SID: "s1", TargetIP: "10.0.0.1", UID: "owner", ServiceName: "svc", CreatedAt: hour, UpdatedAt: hour}).Error

	agg, found, err := s.pqQueryAggregateMixed(ctx, q)
	if err != nil || !found {
		t.Fatalf("mixed query failed: found=%v err=%v", found, err)
	}
	// 期望 total=2：10:00 样本 + 10:59:50 边界样本（只计一次，11 点小时无数据）
	if agg.Total != 2 {
		t.Fatalf("hour boundary must not double count: total=%v", agg.Total)
	}
}
