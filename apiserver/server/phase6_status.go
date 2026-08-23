// ============================================================
// server/phase6_status.go — 阶段六：状态快照与指标刷新
// ============================================================
// 为 /api/v1/storage/status 与 Prometheus 提供：
//   - 热 window/batch 数与最老时间
//   - GC observe/enforce 候选、已删、失败和阻塞原因
//   - orphan 数、migration failure/quarantine 数、coverage segment 数
//   - 各信号 Parquet 覆盖率和对账失败数
//   - v1 fallback 次数、Parquet 查询错误与耗时
// ============================================================

package server

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// pqPhase6Status 阶段六状态快照（storage/status 响应片段）。
type pqPhase6Status struct {
	FineGCMode            string                      `json:"fine_gc_mode"`
	HotWindowCount        int64                       `json:"hot_window_count"`
	HotBatchCount         int64                       `json:"hot_batch_count"`
	HotWindowOldestAt     *time.Time                  `json:"hot_window_oldest_at"`
	HotBatchOldestAt      *time.Time                  `json:"hot_batch_oldest_at"`
	OrphanWindowCount     int64                       `json:"orphan_window_count"`
	MigrationFailures     int64                       `json:"migration_failures"`
	MigrationQuarantined  int64                       `json:"migration_quarantined"`
	CoverageSegments      int64                       `json:"coverage_segments"`
	FineGCCandidates      int64                       `json:"fine_gc_candidates_total"`
	FineGCDeleted         int64                       `json:"fine_gc_deleted_total"`
	FineGCFailures        int64                       `json:"fine_gc_failures_total"`
	FineGCBlocked         map[string]int64            `json:"fine_gc_blocked_by_reason"`
	ReconcileFailed       int64                       `json:"reconcile_failed"`
	ReconcileQuarantined  int64                       `json:"reconcile_quarantined"`
	SignalCoverage        map[string]pqSignalCoverage `json:"signal_coverage"`
	ParquetV1Fallback     int64                       `json:"parquet_v1_fallback_total"`
	ParquetQueryErrors    int64                       `json:"parquet_query_errors_total"`
	ParquetQueryLatencyMs int64                       `json:"parquet_query_latency_ms"`
}

// pqSignalCoverage 单信号覆盖情况。
type pqSignalCoverage struct {
	ActiveBlocks         int64 `json:"active_blocks"`
	ReconcileFailed      int64 `json:"reconcile_failed"`
	ReconcileQuarantined int64 `json:"reconcile_quarantined"`
	PassedBlocks         int64 `json:"reconciled_passed_blocks"`
}

// pqPhase6StatusSnapshot 计算阶段六状态快照。
func (s *APIServer) pqPhase6StatusSnapshot(ctx context.Context) pqPhase6Status {
	out := pqPhase6Status{
		FineGCMode:     s.Config.ContinuousParquet.FineRowGCMode,
		FineGCBlocked:  map[string]int64{},
		SignalCoverage: map[string]pqSignalCoverage{},
	}
	cfg := s.Config.ContinuousParquet
	hotRetention := time.Duration(cfg.HotMetadataRetentionMinutes) * time.Minute
	if hotRetention <= 0 {
		hotRetention = 120 * time.Minute
	}
	cutoff := time.Now().Add(-hotRetention)

	// 热表计数与最老时间
	var oldestWindow model.ProfileWindow
	if err := s.DB.WithContext(ctx).Model(&model.ProfileWindow{}).
		Where("window_start >= ?", cutoff).Order("window_start ASC").Limit(1).First(&oldestWindow).Error; err == nil {
		out.HotWindowOldestAt = &oldestWindow.WindowStart
	}
	_ = s.DB.WithContext(ctx).Model(&model.ProfileWindow{}).
		Where("window_start >= ?", cutoff).Count(&out.HotWindowCount).Error
	var oldestBatch model.ProfileBatch
	if err := s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
		Where("created_at >= ?", cutoff).Order("created_at ASC").Limit(1).First(&oldestBatch).Error; err == nil {
		out.HotBatchOldestAt = &oldestBatch.CreatedAt
	}
	_ = s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
		Where("created_at >= ?", cutoff).Count(&out.HotBatchCount).Error

	// orphan window
	out.OrphanWindowCount = s.pqCountOrphanWindows(ctx)

	// 迁移失败与覆盖区间
	out.MigrationFailures, out.MigrationQuarantined = s.pqCountMigrationFailures(ctx)
	out.CoverageSegments = s.pqCountCoverageSegments(ctx)

	// 对账失败/隔离 + 各信号覆盖
	for _, signal := range []string{
		model.ContinuousParquetSignalCPU,
		model.ContinuousParquetSignalMetrics,
		model.ContinuousParquetSignalHistogram,
		model.ContinuousParquetSignalDB,
	} {
		cov := pqSignalCoverage{}
		_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
			Where("signal_type = ? AND status = ?", signal, model.ContinuousParquetStatusActive).
			Count(&cov.ActiveBlocks).Error
		_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
			Where("signal_type = ? AND status = ? AND reconcile_status = ?",
				signal, model.ContinuousParquetStatusActive, model.ContinuousParquetReconcilePassed).
			Count(&cov.PassedBlocks).Error
		_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
			Where("signal_type = ? AND status = ? AND reconcile_status = ?",
				signal, model.ContinuousParquetStatusActive, model.ContinuousParquetReconcileFailed).
			Count(&cov.ReconcileFailed).Error
		_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
			Where("signal_type = ? AND status = ? AND reconcile_status = ?",
				signal, model.ContinuousParquetStatusActive, model.ContinuousParquetReconcileQuarantined).
			Count(&cov.ReconcileQuarantined).Error
		out.SignalCoverage[signal] = cov
	}
	// 单独汇总对账失败/隔离总数（跨信号）
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("status = ? AND reconcile_status = ?", model.ContinuousParquetStatusActive, model.ContinuousParquetReconcileFailed).
		Count(&out.ReconcileFailed).Error
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("status = ? AND reconcile_status = ?", model.ContinuousParquetStatusActive, model.ContinuousParquetReconcileQuarantined).
		Count(&out.ReconcileQuarantined).Error

	// GC 计数
	out.FineGCCandidates = atomic.LoadInt64(&metricFineGCCandidatesTotal)
	out.FineGCDeleted = atomic.LoadInt64(&metricFineGCDeletedTotal)
	out.FineGCFailures = atomic.LoadInt64(&metricFineGCFailuresTotal)
	metricFineGCBlockedMu.Lock()
	for reason, count := range metricFineGCBlocked {
		out.FineGCBlocked[reason] = count
	}
	metricFineGCBlockedMu.Unlock()

	out.ParquetV1Fallback = atomic.LoadInt64(&metricParquetV1FallbackTotal)
	out.ParquetQueryErrors = atomic.LoadInt64(&metricParquetQueryErrorsTotal)
	out.ParquetQueryLatencyMs = atomic.LoadInt64(&metricParquetQueryLatencyMs)
	return out
}

// pqRefreshPhase6Metrics 刷新阶段六 gauge 指标（worker 周期调用）。
func (s *APIServer) pqRefreshPhase6Metrics(ctx context.Context) {
	snap := s.pqPhase6StatusSnapshot(ctx)
	if snap.HotWindowOldestAt != nil {
		atomic.StoreInt64(&metricHotWindowOldestMs, snap.HotWindowOldestAt.UnixMilli())
	}
	if snap.HotBatchOldestAt != nil {
		atomic.StoreInt64(&metricHotBatchOldestMs, snap.HotBatchOldestAt.UnixMilli())
	}
	atomic.StoreInt64(&metricHotWindowCount, snap.HotWindowCount)
	atomic.StoreInt64(&metricHotBatchCount, snap.HotBatchCount)
	atomic.StoreInt64(&metricOrphanWindowCount, snap.OrphanWindowCount)
	atomic.StoreInt64(&metricCoverageSegments, snap.CoverageSegments)
	atomic.StoreInt64(&metricReconcileFailed, snap.ReconcileFailed)
	atomic.StoreInt64(&metricReconcileQuarantined, snap.ReconcileQuarantined)
	// enforce 候选数（通过全部清理条件的 batch；observe 模式统计用）
	if s.fineGCObserveEnabled() {
		minutes := s.Config.ContinuousParquet.HotMetadataRetentionMinutes
		if minutes <= 0 {
			minutes = 120
		}
		cutoff := time.Now().Add(-time.Duration(minutes) * time.Minute)
		var count int64
		_ = s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
			Where("created_at < ?", cutoff).Count(&count).Error
		atomic.StoreInt64(&metricFineGCEnforceCandidates, count)
	}
}
