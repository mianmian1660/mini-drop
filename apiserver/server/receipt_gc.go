// ============================================================
// server/receipt_gc.go — 阶段六：migration receipt 回收
// ============================================================
// migration receipt（continuous_migration_receipts）是 raw Block lineage 的
// 永久凭证，不会随 raw Block 生命周期删除；当对应 profile_batches.bid 已
// 不存在（细粒度 GC 已清理 batch）且超过保留期后，receipt 就失去存在意义，
// 需要回收，否则会随业务长期运行无界增长。
//
// 回收条件必须全部成立：
//   1. source_kind = 'batch'
//   2. updated_at 早于 now - CONTINUOUS_MIGRATION_RECEIPT_RETENTION_HOURS
//      （默认 72h；revoked 会更新 updated_at，同样受保留期保护）
//   3. 对应 profile_batches.bid 已不存在（仍有 ProfileBatch 的 receipt
//      无论多久都绝不删除）
// passed/revoked receipt 均可回收；每轮最多处理
// CONTINUOUS_FINE_ROW_GC_BATCH 条，按 updated_at 升序分批删除。
// ============================================================

package server

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
)

// pqMigrationReceiptRetention 返回 receipt 保留时长（默认 72h）。
func (s *APIServer) pqMigrationReceiptRetention() time.Duration {
	if s == nil || s.Config == nil {
		return 72 * time.Hour
	}
	hours := s.Config.ContinuousParquet.MigrationReceiptRetentionHours
	if hours <= 0 {
		return 72 * time.Hour
	}
	return time.Duration(hours) * time.Hour
}

// pqRunMigrationReceiptGC 一轮 migration receipt 回收。
// 在 Parquet 维护周期内调用；只处理 source_kind='batch'，且对应
// profile_batches.bid 已不存在、超过保留期的 receipt。
func (s *APIServer) pqRunMigrationReceiptGC(ctx context.Context) {
	if s == nil || s.DB == nil || s.Config == nil {
		return
	}
	cfg := s.Config.ContinuousParquet
	batch := cfg.FineRowGCBatch
	if batch <= 0 {
		batch = 1000
	}
	cutoff := time.Now().Add(-s.pqMigrationReceiptRetention())

	var candidates []model.ContinuousMigrationReceipt
	if err := s.DB.WithContext(ctx).
		Where("source_kind = ? AND updated_at < ?", "batch", cutoff).
		Where("NOT EXISTS (SELECT 1 FROM profile_batches b WHERE b.bid = continuous_migration_receipts.source_ref)").
		Order("updated_at ASC, id ASC").
		Limit(batch).
		Find(&candidates).Error; err != nil {
		incMigrationReceiptGCFailure()
		s.Logger.Warn("migration receipt GC 候选查询失败", zap.Error(err))
		return
	}
	if len(candidates) == 0 {
		return
	}
	ids := make([]uint, 0, len(candidates))
	for i := range candidates {
		ids = append(ids, candidates[i].ID)
	}
	if err := s.DB.WithContext(ctx).
		Where("id IN ?", ids).
		Delete(&model.ContinuousMigrationReceipt{}).Error; err != nil {
		incMigrationReceiptGCFailure()
		s.Logger.Warn("migration receipt GC 删除失败（对象保持不动）", zap.Error(err))
		return
	}
	incMigrationReceiptGCDeleted(int64(len(candidates)))
}
