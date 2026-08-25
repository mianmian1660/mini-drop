// ============================================================
// server/golden_query_test.go — 阶段八：跨存储 golden query 一致性
// ============================================================
// 同一份源数据（v1 热窗口 JSON）分别走：
//   - v1 查询（queryNativeContinuousAggregate，mode=off）
//   - v2 查询（pqQueryAggregateMixed，mode=prefer + pqBuildRawHour 构建 raw 块）
// 对比 Total / TopN / 调用树 / 去重语义 / 诊断字段，保证 raw、v1、Parquet
// 使用同一 dedupe key 且结果逐项一致。
// ============================================================

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// goldenSeedBatch 构造一份含 CPU samples + Memray profile（含跨窗口重复投递）
// + RSS metrics 的 batch 对象，写入内存存储并登记 profile_windows 行。
// 返回 hour（窗口所在 UTC 小时）。
func goldenSeedBatch(t *testing.T, s *APIServer, hour time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	_ = s.DB.Create(&model.AgentInfo{
		Hostname: "node-golden", IPAddr: "10.0.0.1", UID: "owner", Online: true, LastSeen: now,
	}).Error
	_ = s.DB.Create(&model.ContinuousSession{
		SID: "cps-golden", Name: "golden", TargetIP: "10.0.0.1", ServiceName: "hotmethod",
		SampleRateHz: 19, AggregationWindowSec: 10, UploadBatchSec: 60, RetentionHours: 24,
		Status: model.ContinuousSessionStatusRunning, UID: "owner",
		StartedAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}).Error

	// 窗口 A：CPU samples + Memray profile（memray-1-100）+ RSS metrics。
	// 窗口 B：同一 Memray profile 重复投递（验证跨窗口去重）。
	windowA := ContinuousWindowIngest{
		WindowStart: hour.Add(2 * time.Minute),
		WindowEnd:   hour.Add(3 * time.Minute),
		SignalType:  "cpu_profile",
		Samples: []ContinuousStackSample{
			{PID: 100, ProcessStartMs: 1000, Comm: "app", Exe: "/usr/bin/app", Runtime: "native", Count: 10, Stack: []string{"main", "busy"}},
			{PID: 100, ProcessStartMs: 1000, Comm: "app", Exe: "/usr/bin/app", Runtime: "native", Count: 5, Stack: []string{"main", "idle"}},
		},
		Profiles: []ContinuousProfileIngest{{
			SignalType: "python_memory", ProfileID: "memray-1-100", Backend: "memray", Unit: "bytes",
			Samples: []ContinuousStackSample{
				{PID: 200, ProcessStartMs: 2000, Comm: "python3", Exe: "/usr/bin/python3", Runtime: "python", Count: 4096, Stack: []string{"allocA"}},
				{PID: 200, ProcessStartMs: 2000, Comm: "python3", Exe: "/usr/bin/python3", Runtime: "python", Count: 2048, Stack: []string{"allocB"}},
			},
		}, {
			SignalType: "python_memory", ProfileID: "memray-1-failed", Backend: "memray", Unit: "bytes",
		}},
		Metrics: []ContinuousMetricIngest{
			{Metric: "rss_bytes", Timestamp: hour.Add(2*time.Minute + 30*time.Second), PID: 200, ProcessStartMs: 2000, Comm: "python3", Exe: "/usr/bin/python3", Runtime: "python", Value: 1048576, Unit: "bytes"},
		},
		RSSTruncated: 3,
	}
	windowB := ContinuousWindowIngest{
		WindowStart: hour.Add(4 * time.Minute),
		WindowEnd:   hour.Add(5 * time.Minute),
		SignalType:  "cpu_profile",
		Profiles: []ContinuousProfileIngest{{
			SignalType: "python_memory", ProfileID: "memray-1-100", Backend: "memray", Unit: "bytes",
			Samples: []ContinuousStackSample{
				{PID: 200, ProcessStartMs: 2000, Comm: "python3", Exe: "/usr/bin/python3", Runtime: "python", Count: 4096, Stack: []string{"allocA"}},
			},
		}},
	}

	batch := continuousStoredBatch{
		SessionSID:    "cps-golden",
		BatchID:       "cpb-golden",
		TargetIP:      "10.0.0.1",
		StartTime:     hour.Add(2 * time.Minute),
		EndTime:       hour.Add(5 * time.Minute),
		SchemaVersion: 3,
		Windows:       []ContinuousWindowIngest{windowA, windowB},
	}
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	objectKey := continuousBatchObjectKey("cps-golden", "cpb-golden")
	if err := s.Storage.PutObject(ctx, s.Config.Storage.Bucket, objectKey, bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
		t.Fatal(err)
	}
	created := hour.Add(6 * time.Minute)
	// 窗口 A：cpu_profile 行 + python_memory 行（memray profile 独立信号行，
	// 符合 Agent 协议）+ python_rss 行。
	for _, window := range []ContinuousWindowIngest{windowA} {
		for _, signalType := range []string{"cpu_profile", "python_memory", "python_rss"} {
			if err := s.DB.Create(&model.ProfileWindow{
				SessionSID: "cps-golden", BatchBID: "cpb-golden", ObjectKey: objectKey,
				WindowStart: window.WindowStart, WindowEnd: window.WindowEnd,
				SignalType: signalType, CreatedAt: created,
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	// 窗口 B：python_memory 行（memray profile 重复投递）。
	if err := s.DB.Create(&model.ProfileWindow{
		SessionSID: "cps-golden", BatchBID: "cpb-golden", ObjectKey: objectKey,
		WindowStart: windowB.WindowStart, WindowEnd: windowB.WindowEnd,
		SignalType: "python_memory", CreatedAt: created,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

// goldenQuery 构造查询参数（host=10.0.0.1，覆盖整个小时）。
func goldenQuery(hour time.Time, profileType string) ProfileQuery {
	return ProfileQuery{
		Host:        "10.0.0.1",
		From:        hour,
		To:          hour.Add(time.Hour),
		ProfileType: profileType,
		CanReadAll:  true,
		MaxNodes:    5000,
	}
}

// goldenAssertAggregateEqual 对比 v1/v2 聚合结果的关键字段。
func goldenAssertAggregateEqual(t *testing.T, label string, v1, v2 continuousAggregate) {
	t.Helper()
	if v1.Total != v2.Total {
		t.Fatalf("%s: Total 不一致 v1=%v v2=%v", label, v1.Total, v2.Total)
	}
	if len(v1.Top) != len(v2.Top) {
		t.Fatalf("%s: Top 数量不一致 v1=%d v2=%d", label, len(v1.Top), len(v2.Top))
	}
	for key, item1 := range v1.Top {
		item2, ok := v2.Top[key]
		if !ok {
			t.Fatalf("%s: v2 缺少 Top 项 %q", label, key)
		}
		if item1.Value != item2.Value || item1.Self != item2.Self {
			t.Fatalf("%s: Top[%q] 不一致 v1=%+v v2=%+v", label, key, item1, item2)
		}
	}
	// 调用树逐节点对比。
	var walk func(n1, n2 *continuousTreeNode, path string)
	walk = func(n1, n2 *continuousTreeNode, path string) {
		if n1.Value != n2.Value || n1.Self != n2.Self {
			t.Fatalf("%s: 节点 %s 不一致 v1=(%v,%v) v2=(%v,%v)", label, path, n1.Value, n1.Self, n2.Value, n2.Self)
		}
		if len(n1.Children) != len(n2.Children) {
			t.Fatalf("%s: 节点 %s 子节点数不一致 v1=%d v2=%d", label, path, len(n1.Children), len(n2.Children))
		}
		for name, child1 := range n1.Children {
			child2, ok := n2.Children[name]
			if !ok {
				t.Fatalf("%s: v2 缺少子节点 %s/%s", label, path, name)
			}
			walk(child1, child2, path+"/"+name)
		}
	}
	walk(v1.Root, v2.Root, "root")
}

// TestGoldenQueryCPUCrossStorage 同一 CPU 数据 v1 vs v2 聚合一致。
func TestGoldenQueryCPUCrossStorage(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)
	ctx := context.Background()
	q := goldenQuery(hour, "cpu")

	// v1 查询（mode=off）。
	v1Agg, v1Found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil || !v1Found {
		t.Fatalf("v1 query: found=%v err=%v", v1Found, err)
	}
	if v1Agg.Total != 15 {
		t.Fatalf("v1 Total=%v want 15", v1Agg.Total)
	}

	// 构建 v2 raw 块并查询（mode=prefer）。
	s.Config.ContinuousParquet.Mode = "prefer"
	built, err := s.pqBuildRawHour(ctx, "default", hour)
	if err != nil || !built {
		t.Fatalf("pqBuildRawHour: built=%v err=%v", built, err)
	}
	v2Agg, v2Found, err := s.pqQueryAggregateMixed(ctx, q)
	if err != nil || !v2Found {
		t.Fatalf("v2 query: found=%v err=%v", v2Found, err)
	}
	goldenAssertAggregateEqual(t, "cpu", v1Agg, v2Agg)
}

// TestGoldenQueryMemoryCrossStorage 同一 Memray 数据 v1 vs v2 聚合一致
// （含跨窗口重复投递去重）。
func TestGoldenQueryMemoryCrossStorage(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)
	ctx := context.Background()
	q := goldenQuery(hour, "memory")

	v1Agg, v1Found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil || !v1Found {
		t.Fatalf("v1 query: found=%v err=%v", v1Found, err)
	}
	// 跨窗口重复 profile 的每条 stack 只计一次，但同一 profile 内不同 stack 都保留。
	if v1Agg.Total != 6144 {
		t.Fatalf("v1 memory Total=%v want 6144（跨窗口去重且保留多栈）", v1Agg.Total)
	}

	s.Config.ContinuousParquet.Mode = "prefer"
	built, err := s.pqBuildRawHour(ctx, "default", hour)
	if err != nil || !built {
		t.Fatalf("pqBuildRawHour: built=%v err=%v", built, err)
	}
	v2Agg, v2Found, err := s.pqQueryAggregateMixed(ctx, q)
	if err != nil || !v2Found {
		t.Fatalf("v2 query: found=%v err=%v", v2Found, err)
	}
	goldenAssertAggregateEqual(t, "memory", v1Agg, v2Agg)
}

// TestGoldenQueryRSSCrossStorage 同一 RSS 数据 v1 vs v2 时序一致。
func TestGoldenQueryRSSCrossStorage(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)
	ctx := context.Background()
	q := goldenQuery(hour, "memory")

	// v1 时序。
	v1Series, v1Truncated, v1Found, err := s.queryNativeContinuousTimeseries(ctx, q, "rss_bytes", 20)
	if err != nil || !v1Found {
		t.Fatalf("v1 timeseries: found=%v err=%v", v1Found, err)
	}
	if len(v1Series) != 1 || v1Series[0].Peak != 1048576 {
		t.Fatalf("v1 series=%+v", v1Series)
	}
	if v1Truncated != 3 {
		t.Fatalf("v1 rss_truncated=%d want 3", v1Truncated)
	}

	// v2 时序（构建 metrics 块）。
	s.Config.ContinuousParquet.Mode = "prefer"
	built, err := s.pqBuildRawHour(ctx, "default", hour)
	if err != nil || !built {
		t.Fatalf("pqBuildRawHour: built=%v err=%v", built, err)
	}
	v2Series, v2Truncated, v2Found, err := s.pqQueryTimeseriesMixed(ctx, q, "rss_bytes", 20)
	if err != nil || !v2Found {
		t.Fatalf("v2 timeseries: found=%v err=%v", v2Found, err)
	}
	if len(v2Series) != 1 || v2Series[0].Peak != 1048576 {
		t.Fatalf("v2 series=%+v", v2Series)
	}
	if v2Truncated != 3 {
		t.Fatalf("v2 rss_truncated=%d want 3", v2Truncated)
	}
	if v1Series[0].PID != v2Series[0].PID || v1Series[0].ProcessStartMs != v2Series[0].ProcessStartMs ||
		v1Series[0].Exe != v2Series[0].Exe {
		t.Fatalf("series 身份不一致 v1=%+v v2=%+v", v1Series[0], v2Series[0])
	}
}

func TestGoldenQueryFailedMemoryProfilePreservedInParquet(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)
	ctx := context.Background()
	s.Config.ContinuousParquet.Mode = "prefer"
	if _, err := s.pqBuildRawHour(ctx, "default", hour); err != nil {
		t.Fatal(err)
	}
	profiles, found, err := s.queryMemoryProfilesMixed(ctx, goldenQuery(hour, "memory"))
	if err != nil || !found {
		t.Fatalf("profiles found=%v err=%v", found, err)
	}
	for _, profile := range profiles {
		if profile.ProfileID == "memray-1-failed" {
			if profile.Status != "failed" || profile.Reason == "" {
				t.Fatalf("failed profile=%+v", profile)
			}
			return
		}
	}
	t.Fatalf("failed Memray profile missing after Parquet compaction: %+v", profiles)
}

// TestGoldenQueryParquetProfileIDPreserved v2 写入保留 profile_id，
// 降采样后仍可查询（profile 去重键不因降采样丢失）。
func TestGoldenQueryParquetProfileIDPreserved(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)
	ctx := context.Background()
	s.Config.ContinuousParquet.Mode = "prefer"
	if _, err := s.pqBuildRawHour(ctx, "default", hour); err != nil {
		t.Fatal(err)
	}
	// 读取 raw 块行，确认 profile_id 保留。
	block, err := s.pqFindBestBlock(ctx, "default", hour, model.ContinuousParquetSignalCPU)
	if err != nil || block == nil {
		t.Fatalf("find block: %v", err)
	}
	files, err := s.pqLoadBlockFiles(ctx, block.BlockID)
	if err != nil || len(files) == 0 {
		t.Fatalf("load files: %v", err)
	}
	rows, err := readParquetRows[pqCPURow](s, ctx, files[0].ObjectKey, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundProfile := false
	for _, row := range rows {
		if row.ProfileID == "memray-1-100" {
			foundProfile = true
			if row.PID != 200 || row.ProcessStartMs != 2000 || row.Exe != "/usr/bin/python3" {
				t.Fatalf("profile 行进程身份丢失: %+v", row)
			}
		}
	}
	if !foundProfile {
		t.Fatalf("parquet 未保留 profile_id: %+v", rows)
	}
	// 降采样到 5m 后 profile_id 仍保留。
	downsampled := downsampleCPURows(rows, model.ContinuousParquetResolution5m)
	foundAfter := false
	for _, row := range downsampled {
		if row.ProfileID == "memray-1-100" {
			foundAfter = true
		}
	}
	if !foundAfter {
		t.Fatalf("降采样后 profile_id 丢失: %+v", downsampled)
	}
}

// TestGoldenQueryV1V2DedupeKeyConsistency 同一 profile 在 v1 与 v2 使用
// 相同 dedupe key（profile_id + pid + start + exe）。
func TestGoldenQueryV1V2DedupeKeyConsistency(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)

	// v1 侧：两个窗口同一 profile → 只消费一次。
	seenV1 := map[string]bool{}
	windowA := ContinuousWindowIngest{Profiles: []ContinuousProfileIngest{{
		SignalType: "python_memory", ProfileID: "memray-1-100", Backend: "memray",
		Samples: []ContinuousStackSample{{PID: 200, ProcessStartMs: 2000, Exe: "/usr/bin/python3", Count: 4096, Stack: []string{"allocA"}}},
	}}}
	windowB := ContinuousWindowIngest{Profiles: []ContinuousProfileIngest{{
		SignalType: "python_memory", ProfileID: "memray-1-100", Backend: "memray",
		Samples: []ContinuousStackSample{{PID: 200, ProcessStartMs: 2000, Exe: "/usr/bin/python3", Count: 4096, Stack: []string{"allocA"}}},
	}}}
	fromA := continuousProfileSamplesForQuery(windowA, ProfileQuery{ProfileType: "memory"}, seenV1)
	fromB := continuousProfileSamplesForQuery(windowB, ProfileQuery{ProfileType: "memory"}, seenV1)
	if len(fromA) != 1 || len(fromB) != 0 {
		t.Fatalf("v1 dedupe: A=%d B=%d", len(fromA), len(fromB))
	}

	// v2 侧：同一 key 在 Parquet 行间去重。
	seenV2 := map[string]bool{}
	row := pqCPURow{ProfileID: "memray-1-100", PID: 200, ProcessStartMs: 2000, Exe: "/usr/bin/python3", Value: 4096}
	sample := pqSampleFromCPURow(row)
	if continuousProfileSeen(seenV2, row.ProfileID, []ContinuousStackSample{sample}) {
		t.Fatal("v2 first profile must not be seen")
	}
	if !continuousProfileSeen(seenV2, row.ProfileID, []ContinuousStackSample{sample}) {
		t.Fatal("v2 duplicate profile must be seen")
	}
	// 不同进程身份不误去重。
	rowOther := pqCPURow{ProfileID: "memray-1-100", PID: 300, ProcessStartMs: 3000, Exe: "/usr/bin/python3", Value: 4096}
	if continuousProfileSeen(seenV2, rowOther.ProfileID, []ContinuousStackSample{pqSampleFromCPURow(rowOther)}) {
		t.Fatal("v2 different process identity must not dedupe")
	}
}

// TestGoldenQuerySummaryConsistency 冷层摘要与原始聚合的 TopN 总量一致
// （同一 dedupe 语义下摘要不双计）。
func TestGoldenQuerySummaryConsistency(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)
	ctx := context.Background()
	q := goldenQuery(hour, "cpu")

	// 原始聚合。
	agg, found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil || !found {
		t.Fatalf("aggregate: found=%v err=%v", found, err)
	}
	// 手工构造同内容摘要（模拟 downsampleContinuousWindows 产物）。
	items := make([]ProfileTopItem, 0, len(agg.Top))
	for _, item := range agg.Top {
		items = append(items, *item)
	}
	topJSON, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&model.ContinuousWindowSummary{
		SessionSID: "cps-golden", SignalType: "cpu_profile",
		BucketStart: hour, BucketEnd: hour.Add(time.Hour),
		SampleCount: uint64(agg.Total), TopSelfJSON: topJSON,
	}).Error; err != nil {
		t.Fatal(err)
	}
	summary, found, err := s.queryNativeContinuousSummary(ctx, q)
	if err != nil || !found {
		t.Fatalf("summary: found=%v err=%v", found, err)
	}
	if summary.Total != agg.Total {
		t.Fatalf("summary Total=%v want %v", summary.Total, agg.Total)
	}
	if len(summary.Items) != len(agg.Top) {
		t.Fatalf("summary items=%d want %d", len(summary.Items), len(agg.Top))
	}
	for _, item := range summary.Items {
		orig, ok := agg.Top[item.Name]
		if !ok {
			t.Fatalf("summary 含未知函数 %q", item.Name)
		}
		if item.Self != orig.Self {
			t.Fatalf("summary[%q].Self=%v want %v", item.Name, item.Self, orig.Self)
		}
	}
}

// TestGoldenQueryStorageSourceFields 查询响应携带 storage_source /
// resolution / diagnostics 字段（前端一致性契约）。
func TestGoldenQueryStorageSourceFields(t *testing.T) {
	s := pqTestServer(t)
	hour := time.Date(2026, 8, 24, 14, 0, 0, 0, time.UTC)
	goldenSeedBatch(t, s, hour)
	ctx := context.Background()
	q := goldenQuery(hour, "cpu")

	// v1 模式：storage_source=parquet_v1。
	statsV1 := s.pqQueryStatsFor(ctx, q)
	if statsV1.StorageSource != "parquet_v1" {
		t.Fatalf("v1 storage_source=%q", statsV1.StorageSource)
	}
	// v2 模式：构建后 storage_source=parquet_v2。
	s.Config.ContinuousParquet.Mode = "prefer"
	if _, err := s.pqBuildRawHour(ctx, "default", hour); err != nil {
		t.Fatal(err)
	}
	statsV2 := s.pqQueryStatsFor(ctx, q)
	if statsV2.StorageSource != "parquet_v2" {
		t.Fatalf("v2 storage_source=%q", statsV2.StorageSource)
	}
	if statsV2.ResolutionSeconds != 60 {
		t.Fatalf("v2 resolution=%d want 60", statsV2.ResolutionSeconds)
	}
	if statsV2.EarliestAvailable == nil {
		t.Fatalf("v2 earliest_available_at 缺失")
	}
	_ = statsV1 // 保持引用
}
