// ============================================================
// server/receipt_gc_test.go — 阶段六：migration receipt 回收测试
// ============================================================
// 覆盖：
//   - 有 batch 的旧 receipt 不删除
//   - batch 已删除但未到期的不删除
//   - batch 已删除且超过保留期的删除
//   - 分批上限有效
//   - receipt 删除不影响 coverage segment 与 Parquet 查询
// ============================================================

package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// receiptGCSeed 造一个 batch（可选）与一个 receipt。
func receiptGCSeed(t *testing.T, s *APIServer, id string, batchExists bool, updatedAt time.Time) {
	t.Helper()
	if batchExists {
		batch := model.ProfileBatch{
			BID: "receipt-batch-" + id, SessionSID: "s1",
			StartTime: time.Now().Add(-2 * time.Hour), EndTime: time.Now().Add(-2 * time.Hour).Add(time.Minute),
			SignalTypes: mustJSONBytes([]string{"cpu_profile"}), Status: "ready",
			CreatedAt: time.Now().Add(-2 * time.Hour),
		}
		if err := s.DB.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
	}
	receipt := model.ContinuousMigrationReceipt{
		Tenant: "default", SourceKind: "batch",
		SourceRef: "receipt-batch-" + id, SessionSID: "s1",
		SignalType: "cpu", BlockID: "block-" + id,
		BucketStart: time.Now().Add(-2 * time.Hour), BucketEnd: time.Now().Add(-2 * time.Hour).Add(time.Hour),
		StartTime: time.Now().Add(-2 * time.Hour), EndTime: time.Now().Add(-2 * time.Hour).Add(time.Minute),
		SampleCount: 1, ValueTotal: 1, RowCount: 1,
		Status: "passed", CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}
	if err := s.DB.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
}

func receiptGCCount(t *testing.T, s *APIServer, sourceRef string) int64 {
	t.Helper()
	var n int64
	if err := s.DB.Model(&model.ContinuousMigrationReceipt{}).Where("source_ref = ?", sourceRef).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPQReceiptGCKeepsReceiptWithBatch 有 batch 的旧 receipt 不删除。
func TestPQReceiptGCKeepsReceiptWithBatch(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	s.Config.ContinuousParquet.MigrationReceiptRetentionHours = 72
	old := time.Now().Add(-100 * time.Hour)
	receiptGCSeed(t, s, "keep", true, old)

	s.pqRunMigrationReceiptGC(ctx)
	if n := receiptGCCount(t, s, "receipt-batch-keep"); n != 1 {
		t.Fatalf("receipt with existing batch must NOT be deleted: count=%d", n)
	}
}

// TestPQReceiptGCKeepsUnexpiredReceipt batch 已删除但未到期的不删除。
func TestPQReceiptGCKeepsUnexpiredReceipt(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	s.Config.ContinuousParquet.MigrationReceiptRetentionHours = 72
	fresh := time.Now().Add(-10 * time.Hour)
	receiptGCSeed(t, s, "fresh", false, fresh)

	s.pqRunMigrationReceiptGC(ctx)
	if n := receiptGCCount(t, s, "receipt-batch-fresh"); n != 1 {
		t.Fatalf("receipt within retention must NOT be deleted: count=%d", n)
	}
}

// TestPQReceiptGCDeletesExpiredOrphanReceipt batch 已删除且超过保留期的删除。
func TestPQReceiptGCDeletesExpiredOrphanReceipt(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	s.Config.ContinuousParquet.MigrationReceiptRetentionHours = 72
	old := time.Now().Add(-100 * time.Hour)
	receiptGCSeed(t, s, "expired", false, old)

	s.pqRunMigrationReceiptGC(ctx)
	if n := receiptGCCount(t, s, "receipt-batch-expired"); n != 0 {
		t.Fatalf("expired orphan receipt must be deleted: count=%d", n)
	}
	// 删除计数累计
	if deleted := metricMigrationReceiptGCDeletedTotal; deleted < 1 {
		t.Fatalf("receipt GC deleted counter must be bumped: %d", deleted)
	}
}

// TestPQReceiptGCBatchLimit 分批上限有效（每轮最多处理 FineRowGCBatch 条）。
func TestPQReceiptGCBatchLimit(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	s.Config.ContinuousParquet.MigrationReceiptRetentionHours = 72
	s.Config.ContinuousParquet.FineRowGCBatch = 3
	old := time.Now().Add(-100 * time.Hour)
	for i := 0; i < 5; i++ {
		receiptGCSeed(t, s, fmt.Sprintf("l%d", i), false, old)
	}

	s.pqRunMigrationReceiptGC(ctx)
	var remain int64
	if err := s.DB.Model(&model.ContinuousMigrationReceipt{}).
		Where("source_kind = ?", "batch").Count(&remain).Error; err != nil {
		t.Fatal(err)
	}
	if remain != 2 {
		t.Fatalf("batch limit must cap deletion: remain=%d want 2", remain)
	}
}

// TestPQReceiptGCDoesNotAffectCoverageOrQueries receipt 删除不影响 coverage
// segment 与 Parquet 查询：旧孤儿 receipt 被回收，但块 lineage/coverage 保持。
func TestPQReceiptGCDoesNotAffectCoverageOrQueries(t *testing.T) {
	s := pqTestServer(t)
	ctx := context.Background()
	s.Config.ContinuousParquet.MigrationReceiptRetentionHours = 72
	now := time.Now().UTC()
	hour := now.Truncate(time.Hour).Add(-3 * time.Hour)
	batchID := "b-receipt-cov"

	batch := model.ProfileBatch{
		BID: batchID, SessionSID: "s1",
		StartTime: hour.Add(time.Minute), EndTime: hour.Add(2 * time.Minute),
		SignalTypes: mustJSONBytes([]string{"cpu_profile"}), Status: "ready",
		PayloadBytes: 10, CreatedAt: now.Add(-3 * time.Hour),
	}
	if err := s.DB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	// window 行：coverage segment 在块激活时从 window 重建，必须存在
	if err := s.DB.Create(&model.ProfileWindow{
		SessionSID: "s1", BatchBID: batchID, ObjectKey: "k",
		WindowStart: hour.Add(time.Minute), WindowEnd: hour.Add(2 * time.Minute),
		SignalType: "cpu_profile", SampleCount: 2, CreatedAt: now.Add(-3 * time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 旧的孤儿 receipt（updated_at 远超保留期）
	old := now.Add(-100 * time.Hour)
	if err := s.DB.Create(&model.ContinuousMigrationReceipt{
		Tenant: "default", SourceKind: "batch", SourceRef: batchID, SessionSID: "s1",
		SignalType: "cpu", BlockID: "block-cov",
		BucketStart: hour, BucketEnd: hour.Add(time.Hour),
		StartTime: hour.Add(time.Minute), EndTime: hour.Add(2 * time.Minute),
		SampleCount: 1, ValueTotal: 1, RowCount: 1,
		Status: "passed", CreatedAt: old, UpdatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// 构建 raw 块（激活时自动重建 coverage segments，member 引用 batch，
	// 并生成一条新的 passed receipt）
	s.Config.ContinuousParquet.Mode = "enforce"
	rows := []pqCPURow{{Timestamp: hour.Add(time.Minute).UnixMilli(), SessionSID: "s1", Value: 2, ProfileType: "cpu_profile"}}
	members := []model.ContinuousParquetBlockMember{{
		SourceKind: "batch", SourceRef: batchID, SessionSID: "s1",
		StartTime: hour.Add(time.Minute), EndTime: hour.Add(2 * time.Minute),
		SampleCount: 2, ValueTotal: 2, RowCount: 1,
	}}
	built, err := s.pqWriteSignalBlock(ctx, "default", hour, hour.Add(time.Hour),
		model.ContinuousParquetSignalCPU, model.ContinuousParquetResolutionRaw, "",
		rows, nil, nil, nil, map[string]bool{"s1": true}, nil, members)
	if err != nil || !built {
		t.Fatalf("raw block build failed: built=%v err=%v", built, err)
	}
	var block model.ContinuousParquetBlock
	if err := s.DB.Where("bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ?",
		hour, model.ContinuousParquetSignalCPU, model.ContinuousParquetResolutionRaw, model.ContinuousParquetStatusActive).
		First(&block).Error; err != nil {
		t.Fatal(err)
	}

	// 删除 batch（模拟细粒度 GC 已清理 batch）：旧 receipt 与块构建生成的
	// 新 receipt 都变成孤儿；只有旧的超过保留期
	if err := s.DB.Where("bid = ?", batchID).Delete(&model.ProfileBatch{}).Error; err != nil {
		t.Fatal(err)
	}

	s.pqRunMigrationReceiptGC(ctx)

	// 旧的孤儿 receipt 被删除
	var oldReceiptCount int64
	if err := s.DB.Model(&model.ContinuousMigrationReceipt{}).
		Where("source_ref = ? AND block_id = ?", batchID, "block-cov").Count(&oldReceiptCount).Error; err != nil {
		t.Fatal(err)
	}
	if oldReceiptCount != 0 {
		t.Fatalf("expired orphan receipt must be deleted: count=%d", oldReceiptCount)
	}
	// 块构建生成的新 receipt（保留期内）保留
	var newReceiptCount int64
	if err := s.DB.Model(&model.ContinuousMigrationReceipt{}).
		Where("source_ref = ? AND block_id = ?", batchID, block.BlockID).Count(&newReceiptCount).Error; err != nil {
		t.Fatal(err)
	}
	if newReceiptCount != 1 {
		t.Fatalf("fresh receipt within retention must be kept: count=%d", newReceiptCount)
	}

	// coverage segment 仍可用
	if !s.pqCoverageCovered(ctx, "s1", model.ContinuousParquetSignalCPU,
		hour.Add(time.Minute), hour.Add(2*time.Minute)) {
		t.Fatal("coverage segment must survive receipt GC")
	}
	// Parquet 查询仍可用
	found, err := s.pqFindBestBlock(ctx, "default", hour, model.ContinuousParquetSignalCPU)
	if err != nil || found == nil {
		t.Fatalf("parquet block query must survive receipt GC: block=%v err=%v", found, err)
	}
}
