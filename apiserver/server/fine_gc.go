// ============================================================
// server/fine_gc.go — 阶段六：细粒度 GC（profile_windows/profile_batches 瘦身）
// ============================================================
// CONTINUOUS_FINE_ROW_GC_MODE=off|observe|enforce（默认 off）：
//   - observe：只统计候选与阻塞原因，不删除（Release 6B/6C/6D 观察期）。
//   - enforce：小事务 + 固定批次清理 window/batch 元数据与 staging 对象。
//
// 清理条件必须全部成立（pqFineGCCandidateBlockReason）：
//   1. window/batch 早于 2 小时（CONTINUOUS_HOT_METADATA_RETENTION_MINUTES=120）
//   2. batch 的每种实际信号均存在 active、validated、reconciled 的 raw
//      Block lineage（per-signal，禁止共享 member）
//   3. 对应 coverage segments 已提交且区间/样本统计对账通过
//   4. 当前模式为 enforce（parquet mode 与 GC mode 都必须是 enforce）
//   5. v1 24h 回滚观察期已经结束（Release 6D 运营时序：先 enforce 观察
//      24h，覆盖门禁通过后才把 GC 切到 enforce；重启安全，不重复计时）
//   6. 对象没有未处理的迁移失败
//
// 清理过程：
//   1. 事务内删除 window 和 batch 元数据（FK CASCADE 兜底，兼容 NOT VALID）
//   2. 提交后删除 staging/v1 对象；失败进入可重试异常记录
//   3. 不允许对象已删但仍保留 active 查询引用（元数据先删、对象后删）
//   4. 停止生成新的 ContinuousWindowSummary（由 cleanupContinuousRetention
//      按 enforce 模式跳过），旧摘要继续按 168h 过期
// ============================================================

package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
)

// fineGCEnforceEnabled 细粒度 GC 是否进入清理模式（parquet 与 GC 双 enforce）。
func (s *APIServer) fineGCEnforceEnabled() bool {
	if s == nil || s.Config == nil {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(s.Config.ContinuousParquet.FineRowGCMode))
	return mode == "enforce" && s.pqModeOf() == "enforce"
}

// fineGCObserveEnabled GC 是否至少处于 observe（统计）模式。
func (s *APIServer) fineGCObserveEnabled() bool {
	if s == nil || s.Config == nil {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(s.Config.ContinuousParquet.FineRowGCMode))
	return mode == "observe" || mode == "enforce"
}

// pqRunFineRowGC 一轮细粒度 GC 调度。
func (s *APIServer) pqRunFineRowGC(ctx context.Context) {
	if s.DB == nil {
		return
	}
	if !s.fineGCObserveEnabled() {
		return
	}
	// v2 未写入（off）或 v1 仍是唯一查询源时不清理元数据
	if !pqModeEnabled(s.pqModeOf()) {
		return
	}
	cfg := s.Config.ContinuousParquet
	hotRetention := time.Duration(cfg.HotMetadataRetentionMinutes) * time.Minute
	if hotRetention <= 0 {
		hotRetention = 120 * time.Minute
	}
	batch := cfg.FineRowGCBatch
	if batch <= 0 {
		batch = 1000
	}
	cutoff := time.Now().Add(-hotRetention)

	// 候选：早于热保留期的 batch（未压缩 staging + 已压缩 v1 块成员），
	// 按 created_at 升序分批处理（每轮固定上限，观察磁盘/锁/延迟）。
	var candidates []model.ProfileBatch
	if err := s.DB.WithContext(ctx).
		Where("created_at < ?", cutoff).
		Order("created_at ASC").Limit(batch).Find(&candidates).Error; err != nil {
		return
	}
	for i := range candidates {
		b := &candidates[i]
		reason := s.pqFineGCCandidateBlockReason(ctx, b, cutoff)
		if reason == "" {
			incFineGCCandidate(b.BID)
			if s.fineGCEnforceEnabled() {
				s.pqFineGCRemoveBatch(ctx, b)
			}
			continue
		}
		incFineGCBlocked(reason, b.BID)
	}
}

// pqFineGCCandidateBlockReason 返回阻塞原因；空串表示候选通过。
func (s *APIServer) pqFineGCCandidateBlockReason(ctx context.Context, b *model.ProfileBatch, cutoff time.Time) string {
	if b == nil || b.BID == "" {
		return "invalid_batch"
	}
	if b.CreatedAt.After(cutoff) {
		return "hot_retention"
	}
	// 条件 6：对象没有未处理的迁移失败
	if s.pqHasUnresolvedMigrationFailures(ctx, "batch", b.BID) {
		return "migration_failure"
	}
	// 条件 2：每种实际信号的永久 passed migration receipt
	if !s.pqBatchCoveredByValidatedRaw(ctx, b) {
		return "no_validated_raw_lineage"
	}
	// 条件 3：receipt 区间覆盖全部 window
	if ok, reason := s.pqBatchCoverageReconciled(ctx, b); !ok {
		return reason
	}
	return ""
}

// pqBatchCoverageReconciled 检查 batch 的全部 window 都被同信号 passed
// migration receipt 覆盖。raw 对账成功已经证明源窗口完整性；这里不再用
// Timeline coverage 的重叠总量近似删除安全性。
func (s *APIServer) pqBatchCoverageReconciled(ctx context.Context, b *model.ProfileBatch) (bool, string) {
	var windows []model.ProfileWindow
	if err := s.DB.WithContext(ctx).Where("batch_bid = ?", b.BID).Find(&windows).Error; err != nil {
		return false, "window_query_error"
	}
	if len(windows) == 0 {
		// 无 window 引用的 batch：无需覆盖对账，但需要 lineage 已确认（上方已查）
		return true, ""
	}
	bySignal := map[string][]model.ContinuousMigrationReceipt{}
	for i := range windows {
		w := &windows[i]
		signal := pqLedgerSignalForWindow(w.SignalType)
		if signal == "" {
			continue
		}
		receipts, ok := bySignal[signal]
		if !ok {
			var err error
			receipts, err = s.pqPassedMigrationReceipts(ctx, b.BID, signal)
			if err != nil {
				return false, "receipt_query_error"
			}
			bySignal[signal] = receipts
		}
		if !pqReceiptsCoverRange(append([]model.ContinuousMigrationReceipt(nil), receipts...), w.WindowStart, w.WindowEnd) {
			return false, "receipt_interval_mismatch"
		}
	}
	return true, ""
}

// pqFineGCRemoveBatch 清理单个 batch：事务内删 window+batch 元数据，
// 提交后删源对象（仅未压缩 staging 删对象；已压缩 batch 的对象属于 v1
// 块，由 pqReclaimV1Blocks 处理）。
func (s *APIServer) pqFineGCRemoveBatch(ctx context.Context, b *model.ProfileBatch) {
	// 行级领取：事务内 FOR UPDATE SKIP LOCKED，只有实际删除该行的实例才会
	// 在提交后删除对象。
	claimed := false
	claimedBatch := *b
	// 1) 事务内清理 window + batch 元数据
	txErr := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked model.ProfileBatch
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("bid = ?", b.BID).First(&locked).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.Where("batch_bid = ?", locked.BID).Delete(&model.ProfileWindow{}).Error; err != nil {
			return err
		}
		res := tx.Where("bid = ?", locked.BID).Delete(&model.ProfileBatch{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil // 已被并发清理
		}
		claimed = true
		claimedBatch = locked
		return nil
	})
	if txErr != nil {
		incFineGCFailure("metadata_delete", b.BID)
		s.Logger.Warn("细粒度 GC 元数据清理失败（对象保持不动）",
			zap.String("bid", b.BID), zap.Error(txErr))
		return
	}
	if !claimed {
		return
	}
	incFineGCDeleted(b.BID)
	// 2) 提交后删除 staging 对象（block_id 为空才是 staging 对象；已压缩
	//    batch 的 object_key 指向 v1 块对象，禁止在这里删）
	if claimedBatch.BlockID == "" && claimedBatch.ObjectKey != "" && s.StorageConnected() {
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, claimedBatch.ObjectKey); err != nil {
			s.recordContinuousMigrationFailure(ctx, "batch", claimedBatch.BID, claimedBatch.SessionSID, claimedBatch.ObjectKey, "object_delete", err.Error())
			return
		}
		incContinuousReclaimedBytes(int64(claimedBatch.PayloadBytes))
	}
}

// pqReclaimOrphanWindows 修复历史遗留 orphan window（迁移 019 清理后仍可能
// 由旧路径产生；观察/修复用）。observe/enforce 都只统计，不自动删除——
// 由 019 外键与隔离逻辑保证新 orphan 不再产生。
func (s *APIServer) pqCountOrphanWindows(ctx context.Context) int64 {
	var count int64
	err := s.DB.WithContext(ctx).Model(&model.ProfileWindow{}).
		Where("NOT EXISTS (SELECT 1 FROM profile_batches b WHERE b.bid = profile_windows.batch_bid)").
		Count(&count).Error
	if err != nil {
		return 0
	}
	return count
}
