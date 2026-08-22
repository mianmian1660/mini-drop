// ============================================================
// server/continuous_block_test.go — 阶段三：内建持续剖析块存储单测
// ============================================================
// 覆盖（题目要求的 6 组场景）：
//   - 60 个分钟 batch 合并为一个小时 gzip 块（块内 60 成员、映射更新、源对象删除）
//   - CPU / io-sched / db_snapshot 在块内查询结果与原分钟 JSON 一致
//   - 最近未合并（热数据）与已合并数据混合查询
//   - 迟到 batch 触发版本替换（superseded + 15 分钟宽限后删旧对象）
//   - 到期 batch 的块重写、最后成员删除、源对象删除失败重试
//   - 并发 ingest/compaction（幂等）与低磁盘跳过
// ============================================================

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
)

func enableBlockCompactor(s *APIServer) {
	s.Config.ContinuousBlock = config.ContinuousBlockConfig{
		Enabled: true, WindowSec: 3600, CompactionDelaySec: 600, CompactionIntervalSec: 300,
	}
	s.Config.StorageDisk = config.StorageDiskConfig{Path: tTempDir(), MinFreeBytes: 1 << 30}
}

func tTempDir() string {
	return "/tmp"
}

// blockSeedSession 创建用于块测试的 ContinuousSession。
func blockSeedSession(t *testing.T, s *APIServer, sid, ip string) {
	t.Helper()
	now := time.Now().UTC()
	if err := s.DB.Create(&model.ContinuousSession{
		SID: sid, TargetIP: ip, UID: "owner", RetentionHours: 24,
		Status: model.ContinuousSessionStatusRunning, StartedAt: now.Add(-3 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
}

// blockCreateWithRetry 兼容 SQLite 内存库在并发 compaction/ingest 时的
// "table is locked" 瞬时锁（生产 PostgreSQL 无此问题），短退避重试。
func blockCreateWithRetry(create func() error) error {
	var err error
	for attempt := 0; attempt < 200; attempt++ {
		err = create()
		if err == nil || !strings.Contains(err.Error(), "table is locked") {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// blockSeedBatch 直接写 ProfileBatch + ProfileWindow 行并存放分钟对象，
// 模拟 ingest 成功后的状态（含 payload_bytes，与真实 ingest 一致）。
func blockSeedBatch(t *testing.T, s *APIServer, sid, ip, bid string, start, end time.Time, windows []ContinuousWindowIngest) {
	t.Helper()
	objectKey := continuousBatchObjectKey(sid, bid)
	payload := continuousStoredBatch{SessionSID: sid, BatchID: bid, StartTime: start, EndTime: end, Windows: windows}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := s.Storage.PutObject(context.Background(), s.Config.Storage.Bucket, objectKey, bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
		t.Fatalf("put payload: %v", err)
	}
	batch := model.ProfileBatch{
		BID: bid, SessionSID: sid, TargetIP: ip, ObjectKey: objectKey,
		StartTime: start, EndTime: end, WindowCount: uint32(len(windows)),
		SampleCount: uint64(len(windows)), SchemaVersion: 1, Status: model.ContinuousBatchStatusReady,
		PayloadBytes: uint64(len(body)), ReceivedAt: time.Now(), CreatedAt: time.Now(),
	}
	if err := blockCreateWithRetry(func() error { return s.DB.Create(&batch).Error }); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	for _, w := range windows {
		for _, signal := range continuousWindowSignalRows(w) {
			window := model.ProfileWindow{
				SessionSID: sid, BatchBID: bid,
				WindowStart: w.WindowStart, WindowEnd: w.WindowEnd,
				ObjectKey: objectKey, SignalType: signal.SignalType,
				SampleCount: continuousWindowSampleCount(w, signal.SignalType),
				CreatedAt:   time.Now(),
			}
			if err := blockCreateWithRetry(func() error { return s.DB.Create(&window).Error }); err != nil {
				t.Fatalf("create window: %v", err)
			}
		}
	}
}

// blockSeedCPUWindow 构造一个含 CPU 样本的窗口。
func blockSeedCPUWindow(start, end time.Time, stackCount int) ContinuousWindowIngest {
	samples := make([]ContinuousStackSample, 0, stackCount)
	for i := 0; i < stackCount; i++ {
		samples = append(samples, ContinuousStackSample{
			Stack: []string{"main", fmt.Sprintf("foo_%d", i)}, Count: 1,
		})
	}
	return ContinuousWindowIngest{
		WindowStart: start, WindowEnd: end, SignalType: "cpu_profile", Samples: samples,
	}
}

// blockSeedIOWindow 构造一个含 io_latency 直方图的窗口。
func blockSeedIOWindow(start, end time.Time) ContinuousWindowIngest {
	return ContinuousWindowIngest{
		WindowStart: start, WindowEnd: end, SignalType: "io_latency",
		Histograms: []ContinuousHistogramIngest{{
			SignalType: "io_latency", Backend: "ebpf", Unit: "us", EventCount: 100,
			Buckets: []ContinuousHistogramBucket{
				{Range: "0-100", Low: 0, High: 100, Count: 40},
				{Range: "100-500", Low: 100, High: 500, Count: 60},
			},
			Summary: ContinuousHistogramSummary{Min: 1, Max: 400, P50: 80, P95: 300, P99: 380},
		}},
	}
}

// blockSeedDBWindow 构造一个含 db_snapshot（digest + lock_wait）的窗口。
func blockSeedDBWindow(start, end time.Time) ContinuousWindowIngest {
	return ContinuousWindowIngest{
		WindowStart: start, WindowEnd: end, SignalType: "db_snapshot",
		DBSnapshots: []ContinuousDBSnapshotIngest{
			{Kind: "digest", InstanceLabel: "mysql-a", SchemaName: "mydb", DigestText: "SELECT * FROM t WHERE id = ?",
				CallCount: 10, TotalLatencyUs: 2_000_000, RowsExaminedTotal: 100},
			{Kind: "lock_wait", InstanceLabel: "mysql-a", Timestamp: start,
				WaitingPID: 11, WaitingQuery: "UPDATE t", BlockingPID: 22, BlockingQuery: "SELECT", WaitSeconds: 5, LockedTable: "t"},
		},
	}
}

// blockCompactRun 执行一轮 compaction（幂等可重复调用）。
func blockCompactRun(t *testing.T, s *APIServer) {
	t.Helper()
	s.runContinuousBlockCompaction(context.Background())
}

// blockCompactRunConverged 反复运行 compaction 直到收敛（没有未压缩 batch）。
// SQLite 内存库并发写会瞬时抛 "table is locked"（生产 PostgreSQL 无此问题），
// 重跑几轮即可看到最新状态。
func blockCompactRunConverged(t *testing.T, s *APIServer) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		s.runContinuousBlockCompaction(context.Background())
		var unblocked int64
		if err := s.DB.Model(&model.ProfileBatch{}).Where("(block_id IS NULL OR block_id = '')").Count(&unblocked).Error; err != nil || unblocked == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("compaction did not converge")
}

func blockQuerySnapshot(t *testing.T, s *APIServer, sid, ip string, from, to time.Time) (aggregateTotal float64, histEvent uint64, digestCount int) {
	t.Helper()
	q := ProfileQuery{SessionSID: sid, Host: ip, From: from, To: to, CanReadAll: true}
	agg, found, err := s.queryNativeContinuousAggregate(context.Background(), q)
	if err != nil {
		t.Fatalf("aggregate query: %v", err)
	}
	if found {
		aggregateTotal = agg.Total
	}
	hist, found, err := s.queryNativeContinuousHistogram(context.Background(), q, "io_latency")
	if err != nil {
		t.Fatalf("histogram query: %v", err)
	}
	if found {
		histEvent = hist["event_count"].(uint64)
	}
	snap, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), q)
	if err != nil {
		t.Fatalf("db snapshot query: %v", err)
	}
	if found {
		digestCount = len(snap["digests"].([]gin.H))
	}
	return
}

// ---------------------------------------------------------------------------
// 1) 60 个分钟 batch 合并为一个小时 gzip 块
// ---------------------------------------------------------------------------

func TestContinuousCompactionMergesMinuteBatchesIntoHourBlock(t *testing.T) {
	s := newTestAPIServer(t)
	devLogger, _ := zap.NewDevelopment()
	s.Logger = devLogger
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.101"
	blockSeedSession(t, s, "cps-block-merge", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	const batchCount = 60
	for i := 0; i < batchCount; i++ {
		start := bucketStart.Add(time.Duration(i) * time.Minute)
		end := start.Add(time.Minute)
		wStart, wEnd := start.Add(10*time.Second), start.Add(20*time.Second)
		blockSeedBatch(t, s, "cps-block-merge", ip, fmt.Sprintf("cpb-merge-%02d", i), start, end,
			[]ContinuousWindowIngest{blockSeedCPUWindow(wStart, wEnd, 2)})
	}

	blockCompactRun(t, s)

	var blocks []model.ContinuousProfileBlock
	if err := s.DB.Find(&blocks).Error; err != nil {
		t.Fatalf("load blocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected exactly 1 block, got %d", len(blocks))
	}
	blk := blocks[0]
	if blk.Status != model.ContinuousBlockStatusActive || blk.Version != 1 || blk.BatchCount != batchCount {
		t.Fatalf("unexpected block: %+v", blk)
	}
	if !strings.HasPrefix(blk.ObjectKey, "continuous-blocks/cps-block-merge/") || !strings.HasSuffix(blk.ObjectKey, ".json.gz") {
		t.Fatalf("unexpected block object key: %s", blk.ObjectKey)
	}

	// 全部 batch 已映射到块；源对象删除成功后 source_object_key 被清空
	//（删除失败才保留，由 sweep 重试——见 delretry 用例）。
	var batches []model.ProfileBatch
	if err := s.DB.Find(&batches).Error; err != nil {
		t.Fatalf("load batches: %v", err)
	}
	for _, b := range batches {
		if b.BlockID != blk.BlockID || b.ObjectKey != blk.ObjectKey || b.SourceObjectKey != "" || b.CompactedAt == nil {
			t.Fatalf("batch %s not fully compacted: %+v", b.BID, b)
		}
	}

	// 全部 window 的 object_key 指向块
	var windowCount int64
	if err := s.DB.Model(&model.ProfileWindow{}).Where("object_key <> ?", blk.ObjectKey).Count(&windowCount).Error; err != nil {
		t.Fatalf("count windows: %v", err)
	}
	if windowCount != 0 {
		t.Fatalf("%d windows still point to old object keys", windowCount)
	}

	// 源分钟对象已删除，块对象存在且含 60 个成员
	mem := s.Storage.(*continuousMemoryStorage)
	for i := 0; i < batchCount; i++ {
		key := continuousBatchObjectKey("cps-block-merge", fmt.Sprintf("cpb-merge-%02d", i))
		if _, exists := mem.objects[key]; exists {
			t.Fatalf("source object %s should be deleted", key)
		}
	}
	if _, exists := mem.objects[blk.ObjectKey]; !exists {
		t.Fatalf("block object %s missing", blk.ObjectKey)
	}
	blockObj, err := s.loadContinuousBlockObject(context.Background(), blk.ObjectKey)
	if err != nil {
		t.Fatalf("load block object: %v", err)
	}
	if len(blockObj.Batches) != batchCount {
		t.Fatalf("block contains %d batches, want %d", len(blockObj.Batches), batchCount)
	}
	if blk.BytesAfter <= 0 || blk.BytesBefore <= 0 {
		t.Fatalf("block byte accounting missing: %+v", blk)
	}
}

// ---------------------------------------------------------------------------
// 2) CPU / io-sched / db_snapshot 块内查询结果与原分钟 JSON 一致
// ---------------------------------------------------------------------------

func TestContinuousBlockQueryMatchesMinuteJSON(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.102"
	blockSeedSession(t, s, "cps-block-query", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	start, end := bucketStart.Add(10*time.Minute), bucketStart.Add(11*time.Minute)
	wStart, wEnd := start.Add(10*time.Second), start.Add(20*time.Second)
	blockSeedBatch(t, s, "cps-block-query", ip, "cpb-query-1", start, end, []ContinuousWindowIngest{
		blockSeedCPUWindow(wStart, wEnd, 3),
		blockSeedIOWindow(wStart, wEnd),
		blockSeedDBWindow(wStart, wEnd),
	})

	from, to := bucketStart, bucketStart.Add(time.Hour)
	beforeTotal, beforeHist, beforeDigest := blockQuerySnapshot(t, s, "cps-block-query", ip, from, to)

	blockCompactRun(t, s)

	afterTotal, afterHist, afterDigest := blockQuerySnapshot(t, s, "cps-block-query", ip, from, to)
	if beforeTotal != afterTotal {
		t.Fatalf("aggregate total before=%v after=%v", beforeTotal, afterTotal)
	}
	if beforeHist != afterHist {
		t.Fatalf("histogram event_count before=%v after=%v", beforeHist, afterHist)
	}
	if beforeDigest != afterDigest {
		t.Fatalf("db digest count before=%v after=%v", beforeDigest, afterDigest)
	}

	// 再确认具体值非零（防止两边都是 0 假通过）
	if afterTotal != 3 {
		t.Fatalf("aggregate total=%v, want 3", afterTotal)
	}
	if afterHist != 100 {
		t.Fatalf("histogram event_count=%v, want 100", afterHist)
	}
	if afterDigest != 1 {
		t.Fatalf("db digest count=%v, want 1", afterDigest)
	}
}

// ---------------------------------------------------------------------------
// 3) 最近未合并（热数据）与已合并数据混合查询
// ---------------------------------------------------------------------------

func TestContinuousMixedHotAndCompactedQuery(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.103"
	blockSeedSession(t, s, "cps-block-mixed", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	// 冷：2 小时前的小时桶（会被合并）
	oldStart, oldEnd := bucketStart.Add(5*time.Minute), bucketStart.Add(6*time.Minute)
	blockSeedBatch(t, s, "cps-block-mixed", ip, "cpb-mixed-old", oldStart, oldEnd,
		[]ContinuousWindowIngest{blockSeedCPUWindow(oldStart.Add(10*time.Second), oldStart.Add(20*time.Second), 2)})
	// 热：最近 1 分钟内的小时桶（未封存，不合并）
	hotStart, hotEnd := now.Add(-30*time.Second), now.Add(-20*time.Second)
	blockSeedBatch(t, s, "cps-block-mixed", ip, "cpb-mixed-hot", hotStart, hotEnd,
		[]ContinuousWindowIngest{blockSeedCPUWindow(hotStart, hotEnd, 4)})

	blockCompactRun(t, s)

	// 旧桶合并为块；热 batch 仍是热数据
	var blocks []model.ContinuousProfileBlock
	if err := s.DB.Find(&blocks).Error; err != nil {
		t.Fatalf("load blocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	var hotBatch model.ProfileBatch
	if err := s.DB.Where("bid = ?", "cpb-mixed-hot").First(&hotBatch).Error; err != nil {
		t.Fatalf("load hot batch: %v", err)
	}
	if hotBatch.BlockID != "" || !strings.HasPrefix(hotBatch.ObjectKey, "continuous/") {
		t.Fatalf("hot batch should stay uncompacted: %+v", hotBatch)
	}

	from, to := bucketStart, now.Add(time.Minute)
	total, _, _ := blockQuerySnapshot(t, s, "cps-block-mixed", ip, from, to)
	// 旧窗口样本数 2 + 热窗口样本数 4 = 6
	if total != 6 {
		t.Fatalf("mixed aggregate total=%v, want 6", total)
	}
}

// ---------------------------------------------------------------------------
// 4) 迟到 batch 触发版本替换（superseded + 宽限删除）
// ---------------------------------------------------------------------------

func TestContinuousLateBatchTriggersVersionReplacement(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.104"
	blockSeedSession(t, s, "cps-block-late", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	for i := 0; i < 5; i++ {
		start := bucketStart.Add(time.Duration(i) * time.Minute)
		blockSeedBatch(t, s, "cps-block-late", ip, fmt.Sprintf("cpb-late-%d", i), start, start.Add(time.Minute),
			[]ContinuousWindowIngest{blockSeedCPUWindow(start.Add(10*time.Second), start.Add(20*time.Second), 1)})
	}
	blockCompactRun(t, s)

	var v1 model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).First(&v1).Error; err != nil {
		t.Fatalf("load v1: %v", err)
	}
	if v1.Version != 1 || v1.BatchCount != 5 {
		t.Fatalf("unexpected v1: %+v", v1)
	}

	// 迟到 batch：同一小时桶，晚于首轮 compaction 到达
	lateStart := bucketStart.Add(6 * time.Minute)
	blockSeedBatch(t, s, "cps-block-late", ip, "cpb-late-late", lateStart, lateStart.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(lateStart.Add(10*time.Second), lateStart.Add(20*time.Second), 2)})
	blockCompactRun(t, s)

	var v2 model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).First(&v2).Error; err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if v2.Version != 2 || v2.BatchCount != 6 || v2.BlockID == v1.BlockID {
		t.Fatalf("unexpected v2: %+v", v2)
	}
	// 旧块 superseded，并记录 replaced_by
	var v1Row model.ContinuousProfileBlock
	if err := s.DB.Where("block_id = ?", v1.BlockID).First(&v1Row).Error; err != nil {
		t.Fatalf("load superseded v1: %v", err)
	}
	if v1Row.Status != model.ContinuousBlockStatusSuperseded || v1Row.ReplacedBy != v2.BlockID || v1Row.SupersededAt == nil {
		t.Fatalf("unexpected superseded row: %+v", v1Row)
	}

	// 全部 6 个 batch 映射到 v2
	var batches []model.ProfileBatch
	if err := s.DB.Find(&batches).Error; err != nil {
		t.Fatalf("load batches: %v", err)
	}
	for _, b := range batches {
		if b.BlockID != v2.BlockID || b.ObjectKey != v2.ObjectKey {
			t.Fatalf("batch %s not remapped to v2: %+v", b.BID, b)
		}
	}

	// 宽限期内旧块对象仍在
	mem := s.Storage.(*continuousMemoryStorage)
	if _, exists := mem.objects[v1.ObjectKey]; !exists {
		t.Fatalf("v1 object should still exist within grace period")
	}
	// 把 superseded_at 拨到 15 分钟宽限之外，sweep 回收旧对象
	past := time.Now().Add(-30 * time.Minute)
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Where("block_id = ?", v1.BlockID).
		Update("superseded_at", past).Error; err != nil {
		t.Fatalf("update superseded_at: %v", err)
	}
	s.sweepContinuousBlockCleanup(context.Background())
	if _, exists := mem.objects[v1.ObjectKey]; exists {
		t.Fatalf("v1 object should be swept after grace")
	}
	var leftover int64
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Where("block_id = ?", v1.BlockID).Count(&leftover).Error; err != nil {
		t.Fatalf("count v1 row: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("v1 row should be deleted after sweep, found %d", leftover)
	}
}

// ---------------------------------------------------------------------------
// 5) 到期 batch 的块重写、最后成员删除
// ---------------------------------------------------------------------------

func TestContinuousExpiredBatchRewriteAndBlockDeletion(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.105"
	blockSeedSession(t, s, "cps-block-expire", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	expireA := bucketStart.Add(1 * time.Minute)
	expireB := bucketStart.Add(2 * time.Minute)
	blockSeedBatch(t, s, "cps-block-expire", ip, "cpb-exp-a", expireA, expireA.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(expireA.Add(10*time.Second), expireA.Add(20*time.Second), 1)})
	blockSeedBatch(t, s, "cps-block-expire", ip, "cpb-exp-b", expireB, expireB.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(expireB.Add(10*time.Second), expireB.Add(20*time.Second), 1)})
	blockCompactRun(t, s)

	var v1 model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).First(&v1).Error; err != nil {
		t.Fatalf("load v1: %v", err)
	}
	if v1.BatchCount != 2 {
		t.Fatalf("unexpected v1 batch count: %+v", v1)
	}

	// 模拟 retention 完成：A 到期且窗口已删（冷层摘要假设已完成）
	expired := now.Add(-25 * time.Hour)
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "cpb-exp-a").Update("end_time", expired).Error; err != nil {
		t.Fatalf("expire A: %v", err)
	}
	if err := s.DB.Where("batch_bid = ?", "cpb-exp-a").Delete(&model.ProfileWindow{}).Error; err != nil {
		t.Fatalf("delete A windows: %v", err)
	}
	blockCompactRun(t, s)

	// v2 只含 B；A 的 batch 行被删除；v1 superseded
	var v2 model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).First(&v2).Error; err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if v2.Version != 2 || v2.BatchCount != 1 {
		t.Fatalf("unexpected v2: %+v", v2)
	}
	var removed int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "cpb-exp-a").Count(&removed).Error; err != nil {
		t.Fatalf("count A: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expired batch A row should be removed")
	}
	blockObj, err := s.loadContinuousBlockObject(context.Background(), v2.ObjectKey)
	if err != nil {
		t.Fatalf("load v2 object: %v", err)
	}
	if len(blockObj.Batches) != 1 || blockObj.Batches[0].BatchID != "cpb-exp-b" {
		t.Fatalf("unexpected v2 members: %+v", blockObj.Batches)
	}

	// 最后成员 B 也到期 → 整块删除（行 + 对象）
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "cpb-exp-b").Update("end_time", expired).Error; err != nil {
		t.Fatalf("expire B: %v", err)
	}
	if err := s.DB.Where("batch_bid = ?", "cpb-exp-b").Delete(&model.ProfileWindow{}).Error; err != nil {
		t.Fatalf("delete B windows: %v", err)
	}
	blockCompactRun(t, s)

	mem := s.Storage.(*continuousMemoryStorage)
	if _, exists := mem.objects[v2.ObjectKey]; exists {
		t.Fatalf("v2 object should be deleted after last member expiry")
	}
	var blocks int64
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Where("status = ?", model.ContinuousBlockStatusActive).Count(&blocks).Error; err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if blocks != 0 {
		t.Fatalf("no active block rows should remain, found %d", blocks)
	}
	var v2Left int64
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Where("block_id = ?", v2.BlockID).Count(&v2Left).Error; err != nil {
		t.Fatalf("count v2 row: %v", err)
	}
	if v2Left != 0 {
		t.Fatalf("v2 block row should be deleted, found %d", v2Left)
	}
}

// ---------------------------------------------------------------------------
// 6) 源对象删除失败：保留 source_object_key，sweep 重试成功后清空
// ---------------------------------------------------------------------------

type blockFailingDeleteStorage struct {
	*continuousMemoryStorage
	failMinuteDeletes bool
	failBlockDeletes  bool
}

func (f *blockFailingDeleteStorage) DeleteObject(ctx context.Context, bucket, key string) error {
	if f.failMinuteDeletes && strings.HasPrefix(key, "continuous/") {
		return fmt.Errorf("simulated source delete failure")
	}
	if f.failBlockDeletes && strings.HasPrefix(key, continuousBlockPrefix) {
		return fmt.Errorf("simulated block delete failure")
	}
	return f.continuousMemoryStorage.DeleteObject(ctx, bucket, key)
}

func TestContinuousSourceDeleteFailureRetainsSourceKey(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	mem := &blockFailingDeleteStorage{continuousMemoryStorage: newContinuousMemoryStorage(), failMinuteDeletes: true}
	s.Storage = mem
	ip := "10.0.0.106"
	blockSeedSession(t, s, "cps-block-delretry", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	start := bucketStart.Add(3 * time.Minute)
	blockSeedBatch(t, s, "cps-block-delretry", ip, "cpb-delretry-1", start, start.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(start.Add(10*time.Second), start.Add(20*time.Second), 1)})

	blockCompactRun(t, s)

	// 块已创建，但源对象删除失败 → source_object_key 保留
	var batch model.ProfileBatch
	if err := s.DB.Where("bid = ?", "cpb-delretry-1").First(&batch).Error; err != nil {
		t.Fatalf("load batch: %v", err)
	}
	if batch.SourceObjectKey == "" || batch.BlockID == "" {
		t.Fatalf("source_object_key should be retained after delete failure: %+v", batch)
	}
	if _, exists := mem.objects[batch.SourceObjectKey]; !exists {
		t.Fatalf("minute object should still exist after failed delete")
	}

	// 恢复删除能力，sweep 重试 → 源对象删除且 source_object_key 清空
	mem.failMinuteDeletes = false
	s.sweepContinuousBlockCleanup(context.Background())
	if err := s.DB.Where("bid = ?", "cpb-delretry-1").First(&batch).Error; err != nil {
		t.Fatalf("reload batch: %v", err)
	}
	if batch.SourceObjectKey != "" {
		t.Fatalf("source_object_key should be cleared after successful sweep: %+v", batch)
	}
	if _, exists := mem.objects[batch.SourceObjectKey]; exists {
		t.Fatalf("minute object should be deleted by sweep")
	}
}

func TestContinuousEmptyBlockDeleteIsRecoverable(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	mem := &blockFailingDeleteStorage{continuousMemoryStorage: newContinuousMemoryStorage()}
	s.Storage = mem
	ip := "10.0.0.116"
	blockSeedSession(t, s, "cps-block-delete-state", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	start := bucketStart.Add(3 * time.Minute)
	blockSeedBatch(t, s, "cps-block-delete-state", ip, "cpb-delete-state-1", start, start.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(start.Add(10*time.Second), start.Add(20*time.Second), 1)})
	blockCompactRun(t, s)

	var block model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).First(&block).Error; err != nil {
		t.Fatalf("load active block: %v", err)
	}
	expired := now.Add(-25 * time.Hour)
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "cpb-delete-state-1").Update("end_time", expired).Error; err != nil {
		t.Fatalf("expire batch: %v", err)
	}
	if err := s.DB.Where("batch_bid = ?", "cpb-delete-state-1").Delete(&model.ProfileWindow{}).Error; err != nil {
		t.Fatalf("delete windows: %v", err)
	}

	mem.failBlockDeletes = true
	blockCompactRun(t, s)
	var deleting model.ContinuousProfileBlock
	if err := s.DB.Where("block_id = ?", block.BlockID).First(&deleting).Error; err != nil {
		t.Fatalf("deleting registration must remain for retry: %v", err)
	}
	if deleting.Status != model.ContinuousBlockStatusDeleting {
		t.Fatalf("expected deleting status after object failure, got %q", deleting.Status)
	}
	if _, exists := mem.objects[block.ObjectKey]; !exists {
		t.Fatalf("failed object deletion must leave the object intact")
	}
	var batchCount int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("bid = ?", "cpb-delete-state-1").Count(&batchCount).Error; err != nil {
		t.Fatalf("count detached batch: %v", err)
	}
	if batchCount != 0 {
		t.Fatalf("expired batch reference must be detached before object deletion")
	}

	mem.failBlockDeletes = false
	s.sweepContinuousBlockCleanup(context.Background())
	var blockCount int64
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Where("block_id = ?", block.BlockID).Count(&blockCount).Error; err != nil {
		t.Fatalf("count block registration: %v", err)
	}
	if blockCount != 0 {
		t.Fatalf("deleting registration should be removed after successful sweep")
	}
	if _, exists := mem.objects[block.ObjectKey]; exists {
		t.Fatalf("block object should be removed after successful sweep")
	}
}

func TestContinuousOrphanBlockSweepUsesGracePeriod(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	mem := newContinuousMemoryStorage()
	s.Storage = mem
	ctx := context.Background()
	oldKey := continuousBlockPrefix + "orphan/old.json.gz"
	newKey := continuousBlockPrefix + "orphan/new.json.gz"
	registeredKey := continuousBlockPrefix + "registered/old.json.gz"
	for _, key := range []string{oldKey, newKey, registeredKey} {
		if err := mem.PutObject(ctx, "", key, strings.NewReader("payload"), 7, "application/gzip"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	mem.modified[oldKey] = time.Now().Add(-continuousBlockOrphanGrace - time.Minute)
	mem.modified[registeredKey] = time.Now().Add(-continuousBlockOrphanGrace - time.Minute)
	if err := s.DB.Create(&model.ContinuousProfileBlock{
		BlockID: "cblk-registered", SessionSID: "cps-registered", ObjectKey: registeredKey,
		BucketStart: time.Now().Add(-2 * time.Hour), BucketEnd: time.Now().Add(-time.Hour),
		Status: model.ContinuousBlockStatusSuperseded, SupersededAt: func() *time.Time { v := time.Now(); return &v }(),
	}).Error; err != nil {
		t.Fatalf("create registered block: %v", err)
	}

	s.sweepContinuousBlockCleanup(ctx)
	if _, exists := mem.objects[oldKey]; exists {
		t.Fatalf("old unregistered block should be reclaimed")
	}
	if _, exists := mem.objects[newKey]; !exists {
		t.Fatalf("new unregistered block must be protected by grace period")
	}
	if _, exists := mem.objects[registeredKey]; !exists {
		t.Fatalf("registered block must not be treated as orphan")
	}
}

func TestContinuousUnreadableLateBatchDoesNotRewriteBlock(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	mem := newContinuousMemoryStorage()
	s.Storage = mem
	ip := "10.0.0.117"
	blockSeedSession(t, s, "cps-block-unreadable-late", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	firstStart := bucketStart.Add(time.Minute)
	blockSeedBatch(t, s, "cps-block-unreadable-late", ip, "cpb-readable", firstStart, firstStart.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(firstStart.Add(10*time.Second), firstStart.Add(20*time.Second), 1)})
	blockCompactRun(t, s)
	var original model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).First(&original).Error; err != nil {
		t.Fatalf("load original block: %v", err)
	}

	lateStart := bucketStart.Add(2 * time.Minute)
	blockSeedBatch(t, s, "cps-block-unreadable-late", ip, "cpb-unreadable", lateStart, lateStart.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(lateStart.Add(10*time.Second), lateStart.Add(20*time.Second), 1)})
	lateKey := continuousBatchObjectKey("cps-block-unreadable-late", "cpb-unreadable")
	delete(mem.objects, lateKey)
	delete(mem.modified, lateKey)
	blockCompactRun(t, s)

	var active model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).First(&active).Error; err != nil {
		t.Fatalf("load active block: %v", err)
	}
	if active.BlockID != original.BlockID || active.Version != original.Version {
		t.Fatalf("unreadable late batch must not rewrite the existing block: old=%+v new=%+v", original, active)
	}
	var blockCount int64
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Count(&blockCount).Error; err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if blockCount != 1 {
		t.Fatalf("unreadable late batch created block churn: %d rows", blockCount)
	}
}

// ---------------------------------------------------------------------------
// 7) 并发 ingest/compaction（幂等：重复 compaction 不产生重复块）
// ---------------------------------------------------------------------------

func TestContinuousCompactionIdempotentAndConcurrentIngest(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.107"
	blockSeedSession(t, s, "cps-block-cc", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		start := bucketStart.Add(time.Duration(i) * time.Minute)
		blockSeedBatch(t, s, "cps-block-cc", ip, fmt.Sprintf("cpb-cc-%d", i), start, start.Add(time.Minute),
			[]ContinuousWindowIngest{blockSeedCPUWindow(start.Add(10*time.Second), start.Add(20*time.Second), 1)})
	}
	blockCompactRun(t, s)
	// 无变化重复 compaction：不得产生第二个 active 块
	blockCompactRun(t, s)
	var active int64
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Where("status = ?", model.ContinuousBlockStatusActive).Count(&active).Error; err != nil {
		t.Fatalf("count active blocks: %v", err)
	}
	if active != 1 {
		t.Fatalf("expected 1 active block after duplicate run, got %d", active)
	}

	// 并发：compaction 与 ingest 同时进行（迟到 batch 到达），最终块包含全部 batch
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.runContinuousBlockCompaction(context.Background())
	}()
	go func() {
		defer wg.Done()
		for i := 3; i < 6; i++ {
			start := bucketStart.Add(time.Duration(i) * time.Minute)
			blockSeedBatch(t, s, "cps-block-cc", ip, fmt.Sprintf("cpb-cc-%d", i), start, start.Add(time.Minute),
				[]ContinuousWindowIngest{blockSeedCPUWindow(start.Add(10*time.Second), start.Add(20*time.Second), 1)})
		}
	}()
	wg.Wait()
	// 再收敛跑几轮把并发期间到达的迟到 batch 收进来（SQLite 瞬时锁兼容）
	blockCompactRunConverged(t, s)

	var final model.ContinuousProfileBlock
	if err := s.DB.Where("status = ?", model.ContinuousBlockStatusActive).Order("version DESC").First(&final).Error; err != nil {
		t.Fatalf("load final block: %v", err)
	}
	if final.BatchCount != 6 {
		t.Fatalf("final block should contain 6 batches, got %d", final.BatchCount)
	}
	var unblocked int64
	if err := s.DB.Model(&model.ProfileBatch{}).Where("(block_id IS NULL OR block_id = '')").Count(&unblocked).Error; err != nil {
		t.Fatalf("count unblocked: %v", err)
	}
	if unblocked != 0 {
		t.Fatalf("all batches should be compacted, %d remain", unblocked)
	}
}

// ---------------------------------------------------------------------------
// 8) 低磁盘跳过 compaction（不影响 ingest）
// ---------------------------------------------------------------------------

func TestContinuousLowDiskSkipsCompaction(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.108"
	blockSeedSession(t, s, "cps-block-lowdisk", ip)

	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	start := bucketStart.Add(3 * time.Minute)
	blockSeedBatch(t, s, "cps-block-lowdisk", ip, "cpb-lowdisk-1", start, start.Add(time.Minute),
		[]ContinuousWindowIngest{blockSeedCPUWindow(start.Add(10*time.Second), start.Add(20*time.Second), 1)})

	oldFree := storageFreeBytes
	storageFreeBytes = func(string) (uint64, error) { return 512 * 1024 * 1024, nil } // 低于 1GiB
	defer func() { storageFreeBytes = oldFree }()

	blockCompactRun(t, s)

	var blocks int64
	if err := s.DB.Model(&model.ContinuousProfileBlock{}).Count(&blocks).Error; err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if blocks != 0 {
		t.Fatalf("low disk must skip compaction, found %d blocks", blocks)
	}
	// 热数据仍在，查询无空窗
	q := ProfileQuery{SessionSID: "cps-block-lowdisk", Host: ip, From: bucketStart, To: bucketStart.Add(time.Hour), CanReadAll: true}
	if _, found, err := s.queryNativeContinuousAggregate(context.Background(), q); err != nil || !found {
		t.Fatalf("hot data should remain queryable after skipped compaction, found=%v err=%v", found, err)
	}
}

// ---------------------------------------------------------------------------
// 9) 编解码往返 + 块 key 解析
// ---------------------------------------------------------------------------

func TestContinuousBlockCodecRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour)
	member := continuousBlockBatch{
		BatchID: "cpb-codec-1", StartTime: bucketStart.Add(time.Minute), EndTime: bucketStart.Add(2 * time.Minute),
		Payload: continuousStoredBatch{
			SessionSID: "cps-codec", BatchID: "cpb-codec-1",
			Windows: []ContinuousWindowIngest{blockSeedCPUWindow(bucketStart.Add(70*time.Second), bucketStart.Add(80*time.Second), 2)},
		},
	}
	body, rawSize, err := buildContinuousBlock("cps-codec", bucketStart, bucketStart.Add(time.Hour), now, 1, []continuousBlockBatch{member})
	if err != nil {
		t.Fatalf("build block: %v", err)
	}
	if len(body) == 0 || rawSize <= 0 {
		t.Fatalf("empty block body (compressed=%d raw=%d)", len(body), rawSize)
	}
	block, err := parseContinuousBlock(mustGunzipForTest(body))
	if err != nil {
		t.Fatalf("parse block: %v", err)
	}
	if block.Schema != continuousBlockSchemaV1 || block.Version != 1 || block.SessionSID != "cps-codec" ||
		len(block.Batches) != 1 || block.Batches[0].BatchID != "cpb-codec-1" {
		t.Fatalf("unexpected parsed block: %+v", block)
	}
	if block.Batches[0].Checksum == "" || block.Checksum == "" {
		t.Fatalf("checksums missing: %+v", block)
	}
	// 篡改后必须校验失败
	tampered := strings.Replace(string(mustGunzipForTest(body)), "cpb-codec-1", "cpb-codec-9", 1)
	if _, err := parseContinuousBlock([]byte(tampered)); err == nil {
		t.Fatalf("tampered block should fail checksum")
	}

	// 对象 key 结构：continuous-blocks/{sid}/{YYYY}/{MM}/{DD}/{HH}/{block}.json.gz
	key := continuousBlockObjectKey("cps-codec", "cblk-abc", bucketStart)
	if !looksLikeContinuousBlockKey(key) {
		t.Fatalf("key should look like block key: %s", key)
	}
	if sid := continuousSessionSIDFromObjectKey(key); sid != "cps-codec" {
		t.Fatalf("SID extraction from block key = %q, want cps-codec", sid)
	}
}

func mustGunzipForTest(body []byte) []byte {
	raw, err := gunzipBytes(body)
	if err != nil {
		panic(err)
	}
	return raw
}

// ingestContinuousBatchViaHTTP 走真实 HTTP ingest（含 payload_bytes 写入）。
func ingestContinuousBatchViaHTTP(t *testing.T, s *APIServer, sid, bid string, start, end time.Time, windows []ContinuousWindowIngest) {
	t.Helper()
	reqBody, err := json.Marshal(ContinuousBatchIngestReq{
		SessionSID: sid, BatchID: bid, StartTime: start, EndTime: end,
		WindowCount: uint32(len(windows)), SampleCount: uint64(len(windows)),
		SchemaVersion: 1, SignalTypes: []string{"cpu_profile"}, Windows: windows,
	})
	if err != nil {
		t.Fatalf("marshal ingest: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/continuous/batches", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	profileRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatalf("ingest status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestContinuousIngestRecordsPayloadBytes 校验真实 ingest 写入 payload_bytes，
// 供 compactor 磁盘余量检查使用。
func TestContinuousIngestRecordsPayloadBytes(t *testing.T) {
	s := newTestAPIServer(t)
	enableBlockCompactor(s)
	s.Storage = newContinuousMemoryStorage()
	ip := "10.0.0.109"
	blockSeedSession(t, s, "cps-block-payloadbytes", ip)

	now := time.Now().UTC()
	start, end := now.Add(-30*time.Second), now.Add(-20*time.Second)
	ingestContinuousBatchViaHTTP(t, s, "cps-block-payloadbytes", "cpb-pb-1", start, end,
		[]ContinuousWindowIngest{blockSeedCPUWindow(start, end, 1)})

	var batch model.ProfileBatch
	if err := s.DB.Where("bid = ?", "cpb-pb-1").First(&batch).Error; err != nil {
		t.Fatalf("load batch: %v", err)
	}
	if batch.PayloadBytes == 0 {
		t.Fatalf("payload_bytes not recorded: %+v", batch)
	}
}
