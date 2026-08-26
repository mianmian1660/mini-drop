// ============================================================
// server/parquet_ledger.go — 阶段五：v2 目录账本状态机
// ============================================================
// 状态机固定为：
//
//	building → validating → active
//	                  ↘ failed
//	active → superseded → deleting → deleted（墓碑保留元数据）
//
// 不变量（017 迁移）：
//   - (tenant,bucket_start,signal,resolution) 仅允许一个 active 版本
//   - object key 全局唯一
//   - active 必须是 validation='passed'
//
// 事务语义：上传/校验成功后才在单事务内 退役旧 active → 插入新 active →
// 登记 block_files/members；任一步失败整体回滚，未登记对象由 sweep 兜底。
// ============================================================

package server

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// pqNewBlockID 生成 v2 逻辑块 ID。
func pqNewBlockID() string {
	return "pq-" + util.GenTID()[4:]
}

// pqBlockKey 定位一个逻辑分区。
type pqBlockKey struct {
	Tenant      string
	BucketStart time.Time
	SignalType  string
	Resolution  string
}

// pqFindActiveBlock 查找分区的 active 块（不存在返回 nil）。
func (s *APIServer) pqFindActiveBlock(ctx context.Context, key pqBlockKey) (*model.ContinuousParquetBlock, error) {
	var block model.ContinuousParquetBlock
	err := s.DB.WithContext(ctx).
		Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ?",
			key.Tenant, key.BucketStart, key.SignalType, key.Resolution, model.ContinuousParquetStatusActive).
		First(&block).Error
	if err == nil {
		return &block, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return nil, err
}

// pqCreateBuildingBlock 登记 building 块行（校验未通过前不可见）。
func (s *APIServer) pqCreateBuildingBlock(ctx context.Context, key pqBlockKey, blockID string, bucketEnd time.Time, version int) (*model.ContinuousParquetBlock, error) {
	now := time.Now()
	block := model.ContinuousParquetBlock{
		BlockID:     blockID,
		Tenant:      key.Tenant,
		BucketStart: key.BucketStart,
		BucketEnd:   bucketEnd,
		SignalType:  key.SignalType,
		Resolution:  key.Resolution,
		Version:     version,
		Status:      model.ContinuousParquetStatusBuilding,
		Validation:  model.ContinuousParquetValidationPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.DB.WithContext(ctx).Create(&block).Error; err != nil {
		return nil, err
	}
	return &block, nil
}

// pqMarkBlockFailed 构建/校验失败：building/validating → failed。
// 行保留供诊断；对象由 sweep 回收。
func (s *APIServer) pqMarkBlockFailed(ctx context.Context, blockID, reason string) error {
	return s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("block_id = ? AND status IN ?", blockID,
			[]string{model.ContinuousParquetStatusBuilding, model.ContinuousParquetStatusValidating}).
		Updates(map[string]interface{}{
			"status": model.ContinuousParquetStatusFailed, "validation": model.ContinuousParquetValidationFailed,
			"delete_reason": reason, "updated_at": time.Now(),
		}).Error
}

// pqRegisterActiveBlock 单事务内把 building 块提升为 active：
//   - 退役旧 active（如果有）→ superseded
//   - building 行更新为 active（validation=passed），不回插新行
//   - 登记 block_files（物理 shard）与 members（lineage）
//
// 依赖唯一索引 uq_cpq_active_partition：旧块必须先退役，再更新新块。
func (s *APIServer) pqRegisterActiveBlock(ctx context.Context, key pqBlockKey, blockID string, bucketEnd time.Time,
	version int, result parquetWriteResult, stats pqBlockStats, members []model.ContinuousParquetBlockMember) error {
	now := time.Now()
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var oldBlock model.ContinuousParquetBlock
		oldFound := false
		if err := tx.Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ?",
			key.Tenant, key.BucketStart, key.SignalType, key.Resolution, model.ContinuousParquetStatusActive).
			First(&oldBlock).Error; err == nil {
			oldFound = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if oldFound {
			result := tx.Model(&model.ContinuousParquetBlock{}).
				Where("block_id = ? AND status = ?", oldBlock.BlockID, model.ContinuousParquetStatusActive).
				Updates(map[string]interface{}{
					"status": model.ContinuousParquetStatusSuperseded, "superseded_at": now,
					"replaced_by": blockID, "updated_at": now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("旧 active v2 块 %s 状态已变更", oldBlock.BlockID)
			}
		}

		boundaries, err := marshalRowGroupBoundaries(result.RowGroupBoundary)
		if err != nil {
			return err
		}
		updates := map[string]interface{}{
			"bucket_end":           bucketEnd,
			"status":               model.ContinuousParquetStatusActive,
			"validation":           model.ContinuousParquetValidationPassed,
			"reconcile_status":     model.ContinuousParquetReconcilePassed,
			"reconciled_at":        now,
			"reconcile_error":      "",
			"member_count":         len(members),
			"row_count":            stats.RowCount,
			"value_total":          stats.ValueTotal,
			"sample_total":         stats.SampleTotal,
			"session_count":        stats.SessionCount,
			"process_count":        stats.ProcessCount,
			"bytes_total":          stats.BytesTotal,
			"first_row_time":       stats.FirstRowTime,
			"last_row_time":        stats.LastRowTime,
			"row_group_boundaries": boundaries,
			"updated_at":           now,
		}
		rowResult := tx.Model(&model.ContinuousParquetBlock{}).
			Where("block_id = ? AND status = ?", blockID, model.ContinuousParquetStatusBuilding).
			Updates(updates)
		if rowResult.Error != nil {
			return rowResult.Error
		}
		if rowResult.RowsAffected != 1 {
			return fmt.Errorf("building 块 %s 状态已变更，无法提升为 active", blockID)
		}

		file := model.ContinuousParquetBlockFile{
			BlockID:       blockID,
			PartIndex:     0,
			ObjectKey:     result.ObjectKey,
			SizeBytes:     result.SizeBytes,
			SHA256:        result.SHA256,
			RowGroupCount: result.RowGroupCount,
			RowCount:      result.RowCount,
			CreatedAt:     now,
		}
		if err := tx.Create(&file).Error; err != nil {
			return err
		}
		for i := range members {
			members[i].ID = 0
			members[i].BlockID = blockID
			if members[i].CreatedAt.IsZero() {
				members[i].CreatedAt = now
			}
			if err := tx.Create(&members[i]).Error; err != nil {
				return err
			}
		}
		if err := s.pqPersistMigrationReceiptsTx(tx, key, blockID, bucketEnd, members); err != nil {
			return fmt.Errorf("登记永久 migration receipt 失败: %w", err)
		}
		if key.Resolution == model.ContinuousParquetResolutionRaw {
			if err := s.pqRebuildCoverageSegmentsTx(tx, key.Tenant, key.BucketStart, key.SignalType, blockID, version); err != nil {
				return fmt.Errorf("重建 coverage segments 失败: %w", err)
			}
		}
		return nil
	})
}

// pqBlockStats v2 块统计（登记 + 对账用）。
type pqBlockStats struct {
	RowCount     int64
	ValueTotal   uint64
	SampleTotal  uint64
	SessionCount int
	ProcessCount int
	BytesTotal   int64
	FirstRowTime time.Time
	LastRowTime  time.Time
}

// pqMultiPartRegister 多 part 块登记：part 文件行按 part_index 递增登记。
func (s *APIServer) pqMultiPartRegister(ctx context.Context, blockID string, results []parquetWriteResult) error {
	now := time.Now()
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i, result := range results {
			file := model.ContinuousParquetBlockFile{
				BlockID:       blockID,
				PartIndex:     i,
				ObjectKey:     result.ObjectKey,
				SizeBytes:     result.SizeBytes,
				SHA256:        result.SHA256,
				RowGroupCount: result.RowGroupCount,
				RowCount:      result.RowCount,
				CreatedAt:     now,
			}
			if err := tx.Create(&file).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// sweep：对象回收
// ---------------------------------------------------------------------------

// pqDeleteBlockObject 删除块对象（失败由 sweep 重试）。
func (s *APIServer) pqDeleteBlockObject(ctx context.Context, objectKey string) error {
	if !s.StorageConnected() {
		return errProfileUnavailable
	}
	return s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, objectKey)
}

// pqSupersedeAndDeleteBlock 把 active 块标记 superseded 并排入删除
// （供保留过期/配额回收使用；调用方先确保新版本已 active 或该分区
// 不再需要数据）。返回 (是否执行了状态变更)。
func (s *APIServer) pqSupersedeAndDeleteBlock(ctx context.Context, block *model.ContinuousParquetBlock, reason string) error {
	now := time.Now()
	return s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("block_id = ? AND status = ?", block.BlockID, model.ContinuousParquetStatusActive).
		Updates(map[string]interface{}{
			"status": model.ContinuousParquetStatusSuperseded, "superseded_at": now,
			"delete_reason": reason, "updated_at": now,
		}).Error
}

// pqReclaimExpiredBlocks 回收过期分区：先删除最老 1h，其次 5m，最后 raw
// （配额回收顺序按题目约定：先 superseded/staging，再最老 1h）。
// 返回回收的字节数。
func (s *APIServer) pqReclaimExpiredBlocks(ctx context.Context, resolution string, keepAfter time.Time, limit int) (int64, error) {
	var blocks []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND resolution = ? AND bucket_start < ?", model.ContinuousParquetStatusActive, resolution, keepAfter).
		Order("bucket_start ASC").
		Limit(limit).
		Find(&blocks).Error; err != nil {
		return 0, err
	}
	var reclaimed int64
	for i := range blocks {
		blk := &blocks[i]
		if err := s.pqSupersedeAndDeleteBlock(ctx, blk, "retention_expired"); err != nil {
			s.Logger.Warn("v2 块过期标记失败", zap.String("block_id", blk.BlockID), zap.Error(err))
			continue
		}
		reclaimed += blk.BytesTotal
	}
	return reclaimed, nil
}

// pqSweepCleanup 回收 v2 对象：
//  1. deleting 块：删除对象 → 墓碑化（status=deleted, tombstone_at）。
//  2. superseded 块：宽限后删除对象 → 墓碑化。
//  3. failed/building/validating 孤儿块：超时后删除对象与行。
//  4. 未登记 v2 对象：超时后删除。
func (s *APIServer) pqSweepCleanup(ctx context.Context, limit int) {
	if s.DB == nil || !s.StorageConnected() {
		return
	}
	if limit <= 0 {
		limit = 200
	}
	now := time.Now()
	grace := 15 * time.Minute

	// 1) deleting → 删对象 → deleted 墓碑
	var deleting []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).Where("status = ?", model.ContinuousParquetStatusDeleting).
		Limit(limit).Find(&deleting).Error; err != nil {
		s.Logger.Warn("v2 sweep: 查询 deleting 块失败", zap.Error(err))
		return
	}
	for i := range deleting {
		blk := &deleting[i]
		if err := s.pqDeleteBlockObjectsByPrefix(ctx, blk, limit); err != nil {
			s.Logger.Warn("v2 sweep: 删除 deleting 块对象失败", zap.String("block_id", blk.BlockID), zap.Error(err))
			continue
		}
		if err := s.pqTombstoneBlock(ctx, blk, "deleting_sweep"); err != nil {
			s.Logger.Warn("v2 sweep: 墓碑化失败", zap.String("block_id", blk.BlockID), zap.Error(err))
		}
	}

	// 2) superseded 宽限后删除
	graceCutoff := now.Add(-grace)
	var superseded []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND superseded_at IS NOT NULL AND superseded_at < ?",
			model.ContinuousParquetStatusSuperseded, graceCutoff).
		Limit(limit).Find(&superseded).Error; err != nil {
		s.Logger.Warn("v2 sweep: 查询 superseded 块失败", zap.Error(err))
		return
	}
	for i := range superseded {
		blk := &superseded[i]
		if err := s.pqDeleteBlockObjectsByPrefix(ctx, blk, limit); err != nil {
			s.Logger.Warn("v2 sweep: 删除 superseded 块对象失败", zap.String("block_id", blk.BlockID), zap.Error(err))
			continue
		}
		if err := s.pqTombstoneBlock(ctx, blk, "superseded_sweep"); err != nil {
			s.Logger.Warn("v2 sweep: 墓碑化 superseded 失败", zap.String("block_id", blk.BlockID), zap.Error(err))
		}
	}

	// 3) failed/building/validating 孤儿块：超时清理
	staleCutoff := now.Add(-2 * time.Hour)
	var stale []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("status IN ? AND created_at < ?",
			[]string{model.ContinuousParquetStatusFailed, model.ContinuousParquetStatusBuilding, model.ContinuousParquetStatusValidating}, staleCutoff).
		Limit(limit).Find(&stale).Error; err != nil {
		s.Logger.Warn("v2 sweep: 查询孤儿块失败", zap.Error(err))
		return
	}
	for i := range stale {
		blk := &stale[i]
		_ = s.pqDeleteBlockObjectsByPrefix(ctx, blk, limit)
		if err := s.DB.WithContext(ctx).Where("block_id = ?", blk.BlockID).Delete(&model.ContinuousParquetBlock{}).Error; err != nil {
			s.Logger.Warn("v2 sweep: 删除孤儿块行失败", zap.String("block_id", blk.BlockID), zap.Error(err))
		}
	}
}

// pqDeleteBlockObjectsByPrefix 删除块的全部物理对象（按 block_id 前缀匹配
// object key，来自 block_files 登记）。
func (s *APIServer) pqDeleteBlockObjectsByPrefix(ctx context.Context, blk *model.ContinuousParquetBlock, limit int) error {
	var files []model.ContinuousParquetBlockFile
	if err := s.DB.WithContext(ctx).Where("block_id = ?", blk.BlockID).Limit(limit).Find(&files).Error; err != nil {
		return err
	}
	for _, file := range files {
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, file.ObjectKey); err != nil {
			return err
		}
		incContinuousReclaimedBytes(file.SizeBytes)
	}
	if len(files) == 0 && blk.BlockID != "" {
		// 无登记（异常块）：尝试直接删 block_id 前缀对象
		objects, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, "continuous/v2/")
		if err != nil {
			return err
		}
		for _, object := range objects {
			if containsBlockID(object.Name, blk.BlockID) {
				if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, object.Name); err != nil {
					return err
				}
				incContinuousReclaimedBytes(object.Size)
			}
		}
	}
	return nil
}

func containsBlockID(objectKey, blockID string) bool {
	// 对象 key 形如 .../{block-id}-{part}.parquet
	base := path.Base(objectKey)
	return blockID != "" && strings.HasPrefix(base, blockID+"-") && strings.HasSuffix(base, ".parquet")
}

// pqTombstoneBlock 墓碑化：status=deleted + tombstone_at + 删除 members/files。
func (s *APIServer) pqTombstoneBlock(ctx context.Context, blk *model.ContinuousParquetBlock, reason string) error {
	now := time.Now()
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.ContinuousParquetBlock{}).
			Where("block_id = ?", blk.BlockID).
			Updates(map[string]interface{}{
				"status": model.ContinuousParquetStatusDeleted, "deleted_at": now,
				"delete_reason": reason, "updated_at": now,
			}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ContinuousParquetBlockFile{}).
			Where("block_id = ?", blk.BlockID).
			Updates(map[string]interface{}{"deleted_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Where("block_id = ?", blk.BlockID).
			Delete(&model.ContinuousParquetRuntimeDiagnostic{}).Error; err != nil {
			return err
		}
		return tx.Model(&model.ContinuousParquetBlockMember{}).
			Where("block_id = ?", blk.BlockID).Delete(&model.ContinuousParquetBlockMember{}).Error
	})
}

// pqLoadBlockFiles 加载块的全部物理 part（按 part_index 排序）。
func (s *APIServer) pqLoadBlockFiles(ctx context.Context, blockID string) ([]model.ContinuousParquetBlockFile, error) {
	var files []model.ContinuousParquetBlockFile
	if err := s.DB.WithContext(ctx).Where("block_id = ?", blockID).
		Order("part_index ASC").Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

// pqLockPartition 对 (tenant, bucket) 加 PostgreSQL 会话级 advisory lock
// 实现 v2 构建单飞；SQLite（单测）退化为 noop。
func (s *APIServer) pqLockPartition(ctx context.Context, lockKey string) (func(), error) {
	if s.DB == nil || s.DB.Dialector.Name() != "postgres" {
		return func() {}, nil
	}
	sqlDB, err := s.DB.DB()
	if err != nil {
		return nil, err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", lockKey); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", lockKey)
		_ = conn.Close()
	}, nil
}

// pqUpsertBlockFileOnConflict 幂等登记 part 文件（对象已存在时按 sha256
// 校验；不一致报错，防止部分上传被错误复用）。
