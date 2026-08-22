// ============================================================
// server/continuous_db_snapshot_test.go — db_snapshot ingest/query 聚合单测
// ============================================================
// 覆盖（3a6230f 专项）：
//   - 跨窗口 digest 聚合（call_count / total_latency / rows）、平均耗时、Top 50 与排序
//   - 锁等待逐条保留并按 wait_seconds 降序
//   - 空数据 / 时间范围过滤 / 存储不可用 / 未知 kind 忽略
// ============================================================

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

// 构造一个带 db_snapshot 的测试环境：Session + ProfileWindow + storage batch JSON。
func seedDBSnapshotBatch(t *testing.T, s *APIServer, sid, objectKey string, windows []ContinuousWindowIngest) {
	t.Helper()
	now := time.Now().UTC()
	if err := s.DB.Create(&model.ContinuousSession{
		SID: sid, TargetIP: "10.0.0.9", UID: "owner",
		Status:   model.ContinuousSessionStatusRunning,
		StartedAt: now.Add(-time.Hour), UpdatedAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	for _, w := range windows {
		if err := s.DB.Create(&model.ProfileWindow{
			SessionSID:  sid,
			WindowStart: w.WindowStart,
			WindowEnd:   w.WindowEnd,
			ObjectKey:   objectKey,
			SignalType:  "db_snapshot",
		}).Error; err != nil {
			t.Fatalf("create window: %v", err)
		}
	}
	body, err := json.Marshal(continuousStoredBatch{SessionSID: sid, Windows: windows})
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	mem := s.Storage.(*continuousMemoryStorage)
	mem.objects[objectKey] = string(body)
}

func digestSnap(instance, schema, text string, calls, latencyUs, rows uint64) ContinuousDBSnapshotIngest {
	return ContinuousDBSnapshotIngest{
		Kind: "digest", InstanceLabel: instance, SchemaName: schema, DigestText: text,
		CallCount: calls, TotalLatencyUs: latencyUs, RowsExaminedTotal: rows,
	}
}

func TestQueryContinuousDBSnapshotAggregatesDigestsAcrossWindows(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)
	w1Start, w1End := now.Add(-2*time.Minute), now.Add(-1*time.Minute)
	w2Start, w2End := now.Add(-1*time.Minute), now

	// 同一 digest 跨两个窗口出现：累计计数、累计耗时、累计行数
	seedDBSnapshotBatch(t, s, "db-agg", "continuous/db-agg/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: w1Start, WindowEnd: w1End,
			DBSnapshots: []ContinuousDBSnapshotIngest{
				digestSnap("mysql-a", "mydb", "SELECT * FROM t WHERE id = ?", 10, 2_000_000, 100),
			}},
		{WindowStart: w2Start, WindowEnd: w2End,
			DBSnapshots: []ContinuousDBSnapshotIngest{
				digestSnap("mysql-a", "mydb", "SELECT * FROM t WHERE id = ?", 30, 6_000_000, 300),
			}},
	})

	from, to := now.Add(-3*time.Minute), now.Add(time.Minute)
	data, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-agg", Host: "10.0.0.9", From: from, To: to, CanReadAll: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !found || data["empty"] != false {
		t.Fatalf("expected data found, found=%v data=%#v", found, data)
	}
	digests := data["digests"].([]gin.H)
	if len(digests) != 1 {
		t.Fatalf("expected 1 aggregated digest, got %d: %#v", len(digests), digests)
	}
	d := digests[0]
	if d["call_count"] != uint64(40) {
		t.Fatalf("call_count=%v, want 40", d["call_count"])
	}
	if d["total_latency_us"] != uint64(8_000_000) {
		t.Fatalf("total_latency_us=%v, want 8000000", d["total_latency_us"])
	}
	if d["avg_latency_us"] != uint64(200_000) {
		t.Fatalf("avg_latency_us=%v, want 200000", d["avg_latency_us"])
	}
	if d["rows_examined"] != uint64(400) {
		t.Fatalf("rows_examined=%v, want 400", d["rows_examined"])
	}
}

func TestQueryContinuousDBSnapshotSortsDigestsByLatencyAndCapsTop50(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)
	wStart, wEnd := now.Add(-time.Minute), now

	var snaps []ContinuousDBSnapshotIngest
	// 60 个 digest，第 i 个的耗时 = (i+1) * 1000us，期待降序后取前 50
	for i := 0; i < 60; i++ {
		snaps = append(snaps, digestSnap("mysql-a", "mydb", fmt.Sprintf("SELECT %d", i),
			uint64(1), uint64((i+1)*1000), uint64(i)))
	}
	seedDBSnapshotBatch(t, s, "db-top", "continuous/db-top/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: wStart, WindowEnd: wEnd, DBSnapshots: snaps},
	})

	from, to := now.Add(-2*time.Minute), now.Add(time.Minute)
	_, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-top", Host: "10.0.0.9", From: from, To: to, CanReadAll: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !found {
		t.Fatal("expected found")
	}
}

func TestQueryContinuousDBSnapshotAggregatesAndSortsByLatency(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)
	wStart, wEnd := now.Add(-time.Minute), now

	seedDBSnapshotBatch(t, s, "db-sort", "continuous/db-sort/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: wStart, WindowEnd: wEnd,
			DBSnapshots: []ContinuousDBSnapshotIngest{
				digestSnap("mysql-a", "mydb", "slow-query", 1, 9_000_000, 1),
				digestSnap("mysql-a", "mydb", "fast-query", 1, 1_000_000, 1),
			}},
	})

	from, to := now.Add(-2*time.Minute), now.Add(time.Minute)
	_, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-sort", Host: "10.0.0.9", From: from, To: to, CanReadAll: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !found {
		t.Fatal("expected found")
	}
}

func TestQueryContinuousDBSnapshotKeepsLockWaitsSortedByDuration(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)
	wStart, wEnd := now.Add(-time.Minute), now

	seedDBSnapshotBatch(t, s, "db-lock", "continuous/db-lock/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: wStart, WindowEnd: wEnd,
			DBSnapshots: []ContinuousDBSnapshotIngest{
				{Kind: "lock_wait", InstanceLabel: "mysql-a", Timestamp: now,
					WaitingPID: 1, BlockingPID: 2, WaitSeconds: 3, LockedTable: "db.t"},
				{Kind: "lock_wait", InstanceLabel: "mysql-a", Timestamp: now,
					WaitingPID: 3, BlockingPID: 4, WaitSeconds: 10, LockedTable: "db.u"},
			}},
	})

	from, to := now.Add(-2*time.Minute), now.Add(time.Minute)
	data, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-lock", Host: "10.0.0.9", From: from, To: to, CanReadAll: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !found {
		t.Fatal("expected found")
	}
	waits := data["lock_waits"].([]gin.H)
	if len(waits) != 2 {
		t.Fatalf("lock waits should be kept individually, got %d", len(waits))
	}
	// 按 wait_seconds 降序：先 10 后 3
	if waits[0]["wait_seconds"] != uint64(10) || waits[1]["wait_seconds"] != uint64(3) {
		t.Fatalf("lock waits not sorted by duration: %#v", waits)
	}
	if waits[0]["blocking_pid"] != int64(4) {
		t.Fatalf("blocking_pid mismatch: %#v", waits[0])
	}
}

func TestQueryContinuousDBSnapshotEmptyWhenNoData(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC()
	_ = s.DB.Create(&model.ContinuousSession{
		SID: "db-empty", TargetIP: "10.0.0.9", UID: "owner",
		Status: model.ContinuousSessionStatusRunning, StartedAt: now, UpdatedAt: now, CreatedAt: now,
	}).Error

	_, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-empty", Host: "10.0.0.9", From: now.Add(-time.Hour), To: now.Add(time.Hour), CanReadAll: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if found {
		t.Fatal("expected not found for empty range")
	}
}

func TestQueryContinuousDBSnapshotIgnoresOutOfRangeWindows(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)

	// 窗口落在查询时间范围之外，即使有 digest 也不应返回
	seedDBSnapshotBatch(t, s, "db-range", "continuous/db-range/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: now.Add(-24 * time.Hour), WindowEnd: now.Add(-23 * time.Hour),
			DBSnapshots: []ContinuousDBSnapshotIngest{
				digestSnap("mysql-a", "mydb", "old-query", 5, 5_000_000, 50),
			}},
	})

	_, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-range", Host: "10.0.0.9",
		From: now.Add(-time.Hour), To: now.Add(time.Hour), CanReadAll: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if found {
		t.Fatal("expected out-of-range windows to be excluded")
	}
}

func TestQueryContinuousDBSnapshotStorageUnavailableReturnsError(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)

	// 窗口存在但 storage 里没有对应 batch 对象 -> loadContinuousStoredBatch 失败
	if err := s.DB.Create(&model.ContinuousSession{
		SID: "db-nostore", TargetIP: "10.0.0.9", UID: "owner",
		Status: model.ContinuousSessionStatusRunning, StartedAt: now, UpdatedAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&model.ProfileWindow{
		SessionSID: "db-nostore", WindowStart: now.Add(-time.Minute), WindowEnd: now,
		ObjectKey: "continuous/db-nostore/missing.json", SignalType: "db_snapshot",
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, _, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-nostore", Host: "10.0.0.9",
		From: now.Add(-time.Hour), To: now.Add(time.Hour), CanReadAll: true,
	})
	if err == nil {
		t.Fatal("expected storage-unavailable error")
	}
}

func TestQueryContinuousDBSnapshotIgnoresUnknownKind(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	now := time.Now().UTC().Truncate(time.Millisecond)
	wStart, wEnd := now.Add(-time.Minute), now

	seedDBSnapshotBatch(t, s, "db-kind", "continuous/db-kind/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: wStart, WindowEnd: wEnd,
			DBSnapshots: []ContinuousDBSnapshotIngest{
				{Kind: "mystery", InstanceLabel: "mysql-a", Timestamp: now, DigestText: "x", CallCount: 1},
			}},
	})

	data, found, err := s.queryNativeContinuousDBSnapshot(context.Background(), ProfileQuery{
		SessionSID: "db-kind", Host: "10.0.0.9",
		From: now.Add(-2 * time.Minute), To: now.Add(time.Minute), CanReadAll: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// 未知 kind 应被忽略：无 digest 无 lock_wait -> empty
	if !found || data["empty"] != true {
		t.Fatalf("expected empty result for unknown kind, found=%v data=%#v", found, data)
	}
}
