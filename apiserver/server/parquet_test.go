// ============================================================
// server/parquet_test.go — 阶段五 v2 单元测试（SQLite 内存库）
// ============================================================

package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
)

func pqTestConfig() *config.Config {
	return &config.Config{
		Storage: config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900},
		ContinuousParquet: config.ContinuousParquetConfig{
			Mode:                  "off",
			Tenant:                "default",
			RawRetentionHours:     24,
			Res5mRetentionHours:   168,
			Res1hRetentionHours:   720,
			QuotaBytes:            4 << 30,
			QuotaTargetBytes:      3600 << 20,
			StagingMinutesRetention: 120,
			RowGroupTargetBytes:   16 << 20,
			MaxPartBytes:          128 << 20,
			MinFreeReserve:        512 << 20,
			RecoverHysteresisBytes: 512 << 20,
			RecoveryChecks:        2,
			V1RollbackWindowHours: 24,
			V1DeleteBatch:         200,
		},
		StorageDisk: config.StorageDiskConfig{
			Path: "/tmp", WarningFreeBytes: 8 << 30, CriticalFreeBytes: 4 << 30, MinFreeBytes: 1 << 30,
		},
	}
}

func pqTestServer(t *testing.T) *APIServer {
	t.Helper()
	s := newTestAPIServer(t)
	s.Config = pqTestConfig()
	s.Storage = newContinuousMemoryStorage()
	return s
}

// ---------------------------------------------------------------------------
// frames 协议
// ---------------------------------------------------------------------------

func TestPQFramesFromLegacyStack(t *testing.T) {
	frames := framesFromLegacyStack([]string{
		"runtime.main",
		"0x7f1234 [libc.so.6]",
		"[unknown]",
		"",
	})
	if len(frames) != 4 {
		t.Fatalf("expected 4 frames, got %d", len(frames))
	}
	if frames[0].Function != "runtime.main" || !frames[0].Resolved {
		t.Errorf("frame0: %+v", frames[0])
	}
	if frames[1].Address != 0x7f1234 || frames[1].MappingFile != "libc.so.6" || frames[1].Resolved {
		t.Errorf("frame1: %+v", frames[1])
	}
	if frames[2].Resolved {
		t.Errorf("frame2 should be unresolved: %+v", frames[2])
	}
	// display name 兼容旧格式
	if got := frameDisplayName(frames[1]); got != "0x7f1234 [libc.so.6]" {
		t.Errorf("display name: %q", got)
	}
}

func TestPQSensitiveLabels(t *testing.T) {
	labels := map[string]interface{}{
		"hostname":    "node-a",
		"env":         "prod",
		"db_password": "hunter2",
		"token":       "abc",
		"credentials": map[string]interface{}{"x": 1},
		"nested":      []interface{}{1, 2},
		"count":       float64(42),
		"enabled":     true,
	}
	clean := sanitizeContinuousLabels(labels)
	if clean["hostname"] != "node-a" {
		t.Errorf("hostname lost")
	}
	if _, ok := clean["db_password"]; ok {
		t.Errorf("db_password 未被剥离")
	}
	if _, ok := clean["token"]; ok {
		t.Errorf("token 未被剥离")
	}
	if _, ok := clean["credentials"]; ok {
		t.Errorf("credentials 未被剥离")
	}
	if _, ok := clean["nested"]; ok {
		t.Errorf("嵌套结构不应进入 Parquet")
	}
	if clean["count"] != "42" || clean["enabled"] != "true" {
		t.Errorf("标量转换失败: %+v", clean)
	}
}

// ---------------------------------------------------------------------------
// Parquet round-trip（写 → 读）
// ---------------------------------------------------------------------------

func TestPQCPURoundTrip(t *testing.T) {
	s := pqTestServer(t)
	pqAvgRowBytesEstimate.Store(2048)
	rows := []pqCPURow{
		{
			Timestamp: 1_700_000_000_000, SessionSID: "cps-1", Service: "hotmethod", Agent: "a1",
			PID: 100, ProcessStartMs: 1000, Comm: "app", Exe: "/usr/bin/app",
			Backend: "perf", Runtime: "native",
			Labels: map[string]string{"env": "prod"},
			Frames: []pqCPUFrame{
				{Function: "main", File: "main.go", Line: 42, Address: 0x1000, MappingFile: "/usr/bin/app", BuildID: "abc", NormalizedOffset: 0x1000, Resolved: true},
				{Function: "runtime.main", Resolved: true},
			},
			Value: 5, Unit: "samples", ProfileType: "cpu_profile",
		},
		{
			Timestamp: 1_700_000_001_000, SessionSID: "cps-1", Service: "hotmethod", Agent: "a1",
			PID: 100, ProcessStartMs: 1000, Comm: "app", Exe: "/usr/bin/app",
			Backend: "perf", Runtime: "native", Value: 3, Unit: "samples", ProfileType: "cpu_profile",
		},
	}
	key := parquetObjectKeyV2("default", time.UnixMilli(1_700_000_000_000), "cpu", "raw", "test-block", 0)
	result, err := writeParquetPartGeneric(s, context.Background(), key, rows)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if result.RowCount != 2 || result.SizeBytes <= 0 || result.SHA256 == "" {
		t.Fatalf("result: %+v", result)
	}
	if result.RowGroupCount < 1 {
		t.Fatalf("row groups: %d", result.RowGroupCount)
	}
	got, err := readParquetRows[pqCPURow](s, context.Background(), key, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows: %d", len(got))
	}
	// 排序校验：timestamp 升序
	if got[0].Timestamp > got[1].Timestamp {
		t.Errorf("未按 timestamp 排序")
	}
	if len(got[0].Frames) != 2 {
		t.Errorf("frames 丢失: %+v", got[0].Frames)
	}
	if got[0].Frames[0].Function != "main" || got[0].Frames[0].Line != 42 {
		t.Errorf("frame 内容不符: %+v", got[0].Frames[0])
	}
	if got[1].Value != 3 {
		t.Errorf("value: %d", got[1].Value)
	}
	// 时间范围 row group 选择：查询区间不含任何行时应返回空
	gotNone, err := readParquetRows[pqCPURow](s, context.Background(), key, 1, 2)
	if err != nil {
		t.Fatalf("read none: %v", err)
	}
	if len(gotNone) != 0 {
		t.Fatalf("预期空结果, got %d", len(gotNone))
	}
}

func TestPQDownsampleCPU(t *testing.T) {
	// base 对齐 5 分钟桶（1_699_999_800_000 ms 可被 300000 整除）
	base := int64(1_699_999_800_000)
	rows := []pqCPURow{
		{Timestamp: base, SessionSID: "s1", PID: 1, Frames: []pqCPUFrame{{Function: "a"}}, Value: 2, Unit: "samples"},
		{Timestamp: base + 60_000, SessionSID: "s1", PID: 1, Frames: []pqCPUFrame{{Function: "a"}}, Value: 3, Unit: "samples"},
		{Timestamp: base + 120_000, SessionSID: "s1", PID: 1, Frames: []pqCPUFrame{{Function: "b"}}, Value: 4, Unit: "samples"},
	}
	out := downsampleCPURows(rows, model.ContinuousParquetResolution5m)
	if len(out) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(out))
	}
	if out[0].Value != 5 {
		t.Errorf("group0 value: %d (want 5)", out[0].Value)
	}
	if out[1].Value != 4 {
		t.Errorf("group1 value: %d (want 4)", out[1].Value)
	}
	if out[0].Timestamp != base {
		t.Errorf("bucket 时间未对齐: %d want %d", out[0].Timestamp, base)
	}
}

func TestPQDownsampleMetricCounter(t *testing.T) {
	base := int64(1_699_999_800_000)
	rows := []pqMetricRow{
		{Timestamp: base, SessionSID: "s1", PID: 1, Metric: "db_questions_total", MetricKind: "counter", Value: 100},
		{Timestamp: base + 60_000, SessionSID: "s1", PID: 1, Metric: "db_questions_total", MetricKind: "counter", Value: 150},
		{Timestamp: base + 120_000, SessionSID: "s1", PID: 1, Metric: "db_questions_total", MetricKind: "counter", Value: 30}, // 回绕
		{Timestamp: base + 180_000, SessionSID: "s1", PID: 1, Metric: "db_questions_total", MetricKind: "counter", Value: 60},
	}
	out := downsampleMetricRows(rows, model.ContinuousParquetResolution5m)
	if len(out) != 1 {
		t.Fatalf("expected 1 group, got %d", len(out))
	}
	// delta = (150-100) + 回绕后(30) + (60-30) = 110
	if out[0].Delta != 110 {
		t.Errorf("counter delta: %d (want 110)", out[0].Delta)
	}

	gaugeRows := []pqMetricRow{
		{Timestamp: base, SessionSID: "s1", PID: 1, Metric: "rss_bytes", MetricKind: "gauge", Value: 10},
		{Timestamp: base + 60_000, SessionSID: "s1", PID: 1, Metric: "rss_bytes", MetricKind: "gauge", Value: 30},
		{Timestamp: base + 120_000, SessionSID: "s1", PID: 1, Metric: "rss_bytes", MetricKind: "gauge", Value: 20},
	}
	gout := downsampleMetricRows(gaugeRows, model.ContinuousParquetResolution5m)
	if gout[0].Min != 10 || gout[0].Max != 30 || gout[0].Count != 3 || gout[0].Last != 20 || gout[0].Sum != 60 {
		t.Errorf("gauge 聚合不符: %+v", gout[0])
	}
}

func TestPQDownsampleHistogram(t *testing.T) {
	base := int64(1_699_999_800_000)
	rows := []pqHistogramRow{
		{Timestamp: base, SessionSID: "s1", SignalType: "io_latency", BucketLow: 0, BucketHigh: 10, Count: 2, EventCount: 2, P50: 5, P95: 8, P99: 9, Min: 1, Max: 9},
		{Timestamp: base + 60_000, SessionSID: "s1", SignalType: "io_latency", BucketLow: 0, BucketHigh: 10, Count: 3, EventCount: 3, P50: 6, P95: 9, P99: 10, Min: 2, Max: 10},
	}
	out := downsampleHistogramRows(rows, model.ContinuousParquetResolution5m)
	if len(out) != 1 {
		t.Fatalf("expected 1 merged bucket, got %d", len(out))
	}
	if out[0].Count != 5 || out[0].EventCount != 5 {
		t.Errorf("count 合并不符: %+v", out[0])
	}
	if out[0].Min != 1 || out[0].Max != 10 {
		t.Errorf("min/max 合并不符: %+v", out[0])
	}
	// 加权平均 P50 = (5*2 + 6*3)/5 = 5.6
	if out[0].P50 != 5.6 {
		t.Errorf("P50 重算: %v (want 5.6)", out[0].P50)
	}
}

func TestPQDownsampleDB(t *testing.T) {
	base := int64(1_699_999_800_000)
	rows := []pqDBRow{
		{Timestamp: base, SessionSID: "s1", Kind: "digest", Instance: "mysql-a", SchemaName: "app", DigestText: "SELECT", CallCount: 10, TotalLatencyUs: 1000, RowsExaminedTotal: 500},
		{Timestamp: base + 60_000, SessionSID: "s1", Kind: "digest", Instance: "mysql-a", SchemaName: "app", DigestText: "SELECT", CallCount: 5, TotalLatencyUs: 200, RowsExaminedTotal: 100},
		{Timestamp: base + 120_000, SessionSID: "s1", Kind: "lock_wait", Instance: "mysql-a", LockedTable: "users", WaitingPID: 1, BlockingPID: 2, WaitingQuery: "UPDATE", WaitSeconds: 3},
		{Timestamp: base + 180_000, SessionSID: "s1", Kind: "lock_wait", Instance: "mysql-a", LockedTable: "users", WaitingPID: 1, BlockingPID: 2, WaitingQuery: "UPDATE", WaitSeconds: 8},
	}
	out := downsampleDBRows(rows, model.ContinuousParquetResolution5m)
	if len(out) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(out))
	}
	digest := out[0]
	if digest.CallCount != 15 || digest.TotalLatencyUs != 1200 || digest.RowsExaminedTotal != 600 {
		t.Errorf("digest 聚合不符: %+v", digest)
	}
	lockWait := out[1]
	if lockWait.OccurrenceCount != 2 || lockWait.MaxWaitSeconds != 8 || lockWait.WaitSeconds != 8 {
		t.Errorf("lock_wait 聚合不符: %+v", lockWait)
	}
}

// ---------------------------------------------------------------------------
// 目录账本状态机
// ---------------------------------------------------------------------------

func TestPQBlockStateMachine(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	hourStart := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	key := pqBlockKey{Tenant: "default", BucketStart: hourStart, SignalType: model.ContinuousParquetSignalCPU, Resolution: model.ContinuousParquetResolutionRaw}

	// 构建 building 块
	blockID := "pq-test-block-1"
	if _, err := s.pqCreateBuildingBlock(ctx, key, blockID, hourStart.Add(time.Hour), 1); err != nil {
		t.Fatalf("create building: %v", err)
	}
	// 未校验 → 不可见
	if active, _ := s.pqFindActiveBlock(ctx, key); active != nil {
		t.Fatalf("building 块不应可见")
	}

	// 登记 active（校验通过）
	result := parquetWriteResult{ObjectKey: "continuous/v2/default/date=2026-08-23/hour=10/signal=cpu/resolution=raw/pq-test-block-1-00.parquet",
		SizeBytes: 1234, SHA256: "abc", RowCount: 10, RowGroupCount: 1}
	stats := pqBlockStats{RowCount: 10, SampleTotal: 10, ValueTotal: 10, SessionCount: 1, ProcessCount: 1, BytesTotal: 1234,
		FirstRowTime: hourStart, LastRowTime: hourStart.Add(time.Minute)}
	members := []model.ContinuousParquetBlockMember{{SourceKind: "batch", SourceRef: "b1"}}
	if err := s.pqRegisterActiveBlock(ctx, key, blockID, hourStart.Add(time.Hour), 1, result, stats, members); err != nil {
		t.Fatalf("register active: %v", err)
	}
	active, _ := s.pqFindActiveBlock(ctx, key)
	if active == nil || active.Validation != model.ContinuousParquetValidationPassed {
		t.Fatalf("active 块缺失或未通过校验")
	}

	// 第二个 active 版本 → 旧版本 superseded，新版本 active
	blockID2 := "pq-test-block-2"
	if _, err := s.pqCreateBuildingBlock(ctx, key, blockID2, hourStart.Add(time.Hour), 2); err != nil {
		t.Fatalf("create building v2: %v", err)
	}
	result2 := result
	result2.ObjectKey = "continuous/v2/default/date=2026-08-23/hour=10/signal=cpu/resolution=raw/pq-test-block-2-00.parquet"
	if err := s.pqRegisterActiveBlock(ctx, key, blockID2, hourStart.Add(time.Hour), 2, result2, stats, members); err != nil {
		t.Fatalf("register active v2: %v", err)
	}
	active2, _ := s.pqFindActiveBlock(ctx, key)
	if active2 == nil || active2.BlockID != blockID2 {
		t.Fatalf("新 active 块不符")
	}
	var superseded model.ContinuousParquetBlock
	if err := s.DB.Where("block_id = ?", blockID).First(&superseded).Error; err != nil {
		t.Fatalf("旧块应保留为 superseded: %v", err)
	}
	if superseded.Status != model.ContinuousParquetStatusSuperseded || superseded.ReplacedBy != blockID2 {
		t.Errorf("旧块状态: %s replaced_by=%s", superseded.Status, superseded.ReplacedBy)
	}

	// 墓碑化
	if err := s.pqTombstoneBlock(ctx, active2, "test"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	var tomb model.ContinuousParquetBlock
	if err := s.DB.Where("block_id = ?", blockID2).First(&tomb).Error; err != nil {
		t.Fatalf("墓碑行应保留: %v", err)
	}
	if tomb.Status != model.ContinuousParquetStatusDeleted || tomb.TombstoneAt == nil {
		t.Errorf("墓碑状态: %s", tomb.Status)
	}
}

// ---------------------------------------------------------------------------
// 容量门禁
// ---------------------------------------------------------------------------

func TestPQRequiredFreeFormula(t *testing.T) {
	s := pqTestServer(t)
	setDiskFree(t, 40<<30, 20<<30, 20<<30, nil)
	defer setDiskFree(t, 40<<30, 20<<30, 20<<30, nil)

	// 无待压缩/历史：required = max(critical=4GiB, min+reserve=1.5GiB) = 4GiB
	required := s.requiredFreeBytes(context.Background())
	if required != 4<<30 {
		t.Errorf("required_free: %d (want %d)", required, uint64(4<<30))
	}

	// 有待压缩 batch：required 升高
	now := time.Now()
	_ = s.DB.Create(&model.ProfileBatch{
		BID: "b-pending", SessionSID: "cps-1", ObjectKey: "k1", StartTime: now.Add(-time.Hour), EndTime: now.Add(-time.Minute),
		BlockID: "", PayloadBytes: 3 << 30, Status: model.ContinuousBatchStatusReady, ReceivedAt: now,
	}).Error
	required2 := s.requiredFreeBytes(context.Background())
	if required2 <= required {
		t.Errorf("有待压缩输入时 required_free 应升高: %d -> %d", required, required2)
	}

	// 低于 required_free → 拒收
	setDiskFree(t, 40<<30, required2-1, 40<<30-(required2-1), nil)
	ok, _, snap := s.collectionCapacityOK(CollectionSourceOneShot)
	if ok {
		t.Fatalf("低于 required_free 应拒收（available=%d required=%d）", snap.AvailableBytes, required2)
	}
}

// ---------------------------------------------------------------------------
// 连续配额
// ---------------------------------------------------------------------------

func TestPQContinuousQuota(t *testing.T) {
	s := pqTestServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.ProfileBatch{
		BID: "b1", SessionSID: "cps-1", ObjectKey: "k1", StartTime: now.Add(-time.Minute), EndTime: now,
		PayloadBytes: 500, Status: model.ContinuousBatchStatusReady, ReceivedAt: now,
	}).Error
	_ = s.DB.Create(&model.ContinuousParquetBlock{
		BlockID: "pq-1", Tenant: "default", BucketStart: now.Truncate(time.Hour), BucketEnd: now.Truncate(time.Hour).Add(time.Hour),
		SignalType: "cpu", Resolution: "raw", Status: model.ContinuousParquetStatusActive,
		Validation: model.ContinuousParquetValidationPassed, BytesTotal: 2500, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}).Error
	snap := s.continuousQuotaSnapshot(context.Background())
	if snap.StagingBytes != 500 {
		t.Errorf("staging bytes: %d", snap.StagingBytes)
	}
	if snap.V2BlockBytes != 2500 {
		t.Errorf("v2 bytes: %d", snap.V2BlockBytes)
	}
	if snap.UsedBytes != 3000 {
		t.Errorf("used: %d", snap.UsedBytes)
	}
	if snap.QuotaBytes != 4<<30 {
		t.Errorf("quota: %d", snap.QuotaBytes)
	}
}

// ---------------------------------------------------------------------------
// 对象 key 布局
// ---------------------------------------------------------------------------

func TestPQObjectKeyLayout(t *testing.T) {
	hour := time.Date(2026, 8, 23, 10, 30, 0, 0, time.UTC)
	key := parquetObjectKeyV2("default", hour, "cpu", "raw", "pq-abc", 0)
	want := "continuous/v2/default/date=2026-08-23/hour=10/signal=cpu/resolution=raw/pq-abc-00.parquet"
	if key != want {
		t.Fatalf("key: %q want %q", key, want)
	}
}

// ---------------------------------------------------------------------------
// 降采样调度（raw → 5m → 1h）
// ---------------------------------------------------------------------------

func TestPQDownsampleLifecycle(t *testing.T) {
	s := pqTestServer(t)
	// 确保 maintenance 空间足够
	setDiskFree(t, 100<<30, 80<<30, 20<<30, nil)

	ctx := context.Background()
	now := time.Now().UTC()
	hourStart := now.Truncate(time.Hour).Add(-time.Hour)
	key := pqBlockKey{Tenant: "default", BucketStart: hourStart, SignalType: model.ContinuousParquetSignalCPU, Resolution: model.ContinuousParquetResolutionRaw}

	// 先登记一个 raw 块（含物理文件）
	rows := []pqCPURow{
		{Timestamp: hourStart.Add(time.Minute).UnixMilli(), SessionSID: "s1", PID: 1,
			Frames: []pqCPUFrame{{Function: "a"}}, Value: 3, Unit: "samples"},
		{Timestamp: hourStart.Add(2 * time.Minute).UnixMilli(), SessionSID: "s1", PID: 1,
			Frames: []pqCPUFrame{{Function: "a"}}, Value: 4, Unit: "samples"},
	}
	pqAvgRowBytesEstimate.Store(512)
	result, err := writeParquetPartGeneric(s, ctx, parquetObjectKeyV2("default", hourStart, "cpu", "raw", "pq-raw", 0), rows)
	if err != nil {
		t.Fatalf("write raw part: %v", err)
	}
	if _, err := s.pqCreateBuildingBlock(ctx, key, "pq-raw", hourStart.Add(time.Hour), 1); err != nil {
		t.Fatalf("create building raw: %v", err)
	}
	stats := pqBlockStats{RowCount: result.RowCount, SampleTotal: uint64(len(rows)), ValueTotal: 7, BytesTotal: result.SizeBytes,
		FirstRowTime: hourStart.Add(time.Minute), LastRowTime: hourStart.Add(2 * time.Minute)}
	if err := s.pqRegisterActiveBlock(ctx, key, "pq-raw", hourStart.Add(time.Hour), 1, result, stats, nil); err != nil {
		t.Fatalf("register raw: %v", err)
	}

	// 构建 5m 降采样
	ok, err := s.pqBuildDownsample(ctx, "default", hourStart, model.ContinuousParquetResolutionRaw, model.ContinuousParquetResolution5m)
	if err != nil || !ok {
		t.Fatalf("build 5m: ok=%v err=%v", ok, err)
	}
	fiveMin, err := s.pqFindActiveBlock(ctx, pqBlockKey{Tenant: "default", BucketStart: hourStart, SignalType: model.ContinuousParquetSignalCPU, Resolution: model.ContinuousParquetResolution5m})
	if err != nil || fiveMin == nil {
		t.Fatalf("5m block missing: %v", err)
	}
	// 5m 应为 1 组（5 分钟桶），value=7
	files, _ := s.pqLoadBlockFiles(ctx, fiveMin.BlockID)
	got, err := readParquetRows[pqCPURow](s, ctx, files[0].ObjectKey, 0, 0)
	if err != nil {
		t.Fatalf("read 5m: %v", err)
	}
	if len(got) != 1 || got[0].Value != 7 {
		t.Fatalf("5m rows: %+v", got)
	}
}

var _ = fmt.Sprintf
