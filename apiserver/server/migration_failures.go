// ============================================================
// server/migration_failures.go — 阶段六：细粒度迁移失败异常记录
// ============================================================
// continuous_migration_failures 记录 source kind/ref、session、object key、
// 错误类型、首次/最近出现时间、重试次数和状态：
//   - retrying：可恢复，后台按周期重试
//   - quarantined：连续失败 ≥ 上限 且跨越 ≥ 30 分钟后隔离（fail-closed，
//     不伪造 Parquet 数据或覆盖率）
//   - resolved：源恢复（重试成功）
//   - purged：清理完成（如对象最终删除成功）
// 对象确实不存在时，以异常记录替代失效的细粒度元数据（window 行删除后
// 该记录仍可审计）。
// ============================================================

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// recordContinuousMigrationFailure upsert 一条迁移失败记录。
// quarantined 记录保持隔离，不因重复失败回到 retrying。
func (s *APIServer) recordContinuousMigrationFailure(ctx context.Context, sourceKind, sourceRef, sessionSID, objectKey, errorType, errorMessage string) {
	if sourceKind == "" || sourceRef == "" {
		return
	}
	now := time.Now()
	failure := model.ContinuousMigrationFailure{
		SourceKind:   sourceKind,
		SourceRef:    sourceRef,
		SessionSID:   sessionSID,
		ObjectKey:    objectKey,
		ErrorType:    errorType,
		ErrorMessage: errorMessage,
		FirstSeenAt:  now,
		LastSeenAt:   now,
		Status:       model.ContinuousMigrationFailureRetrying,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "source_kind"}, {Name: "source_ref"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"session_sid":   sessionSID,
			"object_key":    objectKey,
			"error_type":    errorType,
			"error_message": errorMessage,
			"last_seen_at":  now,
			"retry_count":   gorm.Expr("retry_count + 1"),
			"status": gorm.Expr("CASE WHEN status = ? THEN status ELSE ? END",
				model.ContinuousMigrationFailureQuarantined, model.ContinuousMigrationFailureRetrying),
			"updated_at": now,
		}),
	}).Create(&failure).Error
	if err != nil {
		s.Logger.Warn("登记 continuous migration failure 失败",
			zap.String("source_kind", sourceKind), zap.String("source_ref", sourceRef), zap.Error(err))
	}
	incContinuousMigrationFailure(errorType)
}

// pqProcessMigrationFailures 一轮失败重试/隔离：
//  1. retrying 且达到重试间隔 → 按类型重试（object_delete 重删对象；
//     missing_object 重读对象）。
//  2. 连续失败 ≥ 上限且跨越 ≥ 30 分钟 → quarantined；window 级 missing_object
//     同时删除失效 window 行（异常记录替代失效元数据）。
//  3. quarantined 的 object_delete 仍可重试对象删除，成功 → purged。
func (s *APIServer) pqProcessMigrationFailures(ctx context.Context, limit int) {
	if limit <= 0 {
		limit = 100
	}
	cfg := s.Config.ContinuousParquet
	retryLimit := cfg.MigrationFailureRetryLimit
	if retryLimit <= 0 {
		retryLimit = 3
	}
	now := time.Now()
	retryAfter := 10 * time.Minute

	var pending []model.ContinuousMigrationFailure
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND (last_seen_at IS NULL OR last_seen_at < ?)",
			model.ContinuousMigrationFailureRetrying, now.Add(-retryAfter)).
		Order("last_seen_at ASC").Limit(limit).Find(&pending).Error; err != nil {
		return
	}
	for i := range pending {
		f := &pending[i]
		// 隔离判定：连续失败 ≥ 上限 且跨越 ≥ 30 分钟
		if f.RetryCount >= retryLimit && now.Sub(f.FirstSeenAt) >= 30*time.Minute {
			s.pqQuarantineMigrationFailure(ctx, f)
			continue
		}
		s.pqRetryMigrationFailure(ctx, f)
	}

	// quarantined 的 object_delete：仍尝试删对象（成功后 purged）
	var quarantined []model.ContinuousMigrationFailure
	if err := s.DB.WithContext(ctx).
		Where("status = ?", model.ContinuousMigrationFailureQuarantined).
		Order("last_seen_at ASC").Limit(limit).Find(&quarantined).Error; err == nil {
		for i := range quarantined {
			f := &quarantined[i]
			if f.ErrorType == "object_delete" && f.ObjectKey != "" && s.StorageConnected() {
				if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, f.ObjectKey); err == nil {
					incContinuousReclaimedBytes(int64(f.RetryCount) * 1024)
					_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
						Where("id = ? AND status = ?", f.ID, model.ContinuousMigrationFailureQuarantined).
						Updates(map[string]interface{}{
							"status": model.ContinuousMigrationFailurePurged, "updated_at": time.Now(),
						}).Error
				} else {
					_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
						Where("id = ?", f.ID).
						Updates(map[string]interface{}{
							"last_seen_at": now, "updated_at": now,
						}).Error
				}
			}
		}
	}
}

// pqRetryMigrationFailure 按错误类型重试一次。
func (s *APIServer) pqRetryMigrationFailure(ctx context.Context, f *model.ContinuousMigrationFailure) {
	now := time.Now()
	var nextStatus = model.ContinuousMigrationFailureRetrying
	var nextError = f.ErrorMessage

	switch f.ErrorType {
	case "object_delete":
		if f.ObjectKey != "" && s.StorageConnected() {
			if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, f.ObjectKey); err == nil {
				nextStatus = model.ContinuousMigrationFailurePurged
				nextError = ""
			} else {
				nextError = err.Error()
			}
		}
	case "missing_object", "read_error":
		// 源对象是否恢复（回读校验）
		if f.ObjectKey != "" && s.StorageConnected() {
			if _, err := s.Storage.StatObject(ctx, s.Config.Storage.Bucket, f.ObjectKey); err == nil {
				nextStatus = model.ContinuousMigrationFailureResolved
				nextError = ""
			} else {
				nextError = err.Error()
			}
		}
	default:
		// 未知类型：更新 last_seen 即可
	}
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
		Where("id = ? AND status = ?", f.ID, model.ContinuousMigrationFailureRetrying).
		Updates(map[string]interface{}{
			"status": nextStatus, "error_message": nextError,
			"retry_count":  gorm.Expr("retry_count + 1"),
			"last_seen_at": now, "updated_at": now,
		}).Error
}

// pqQuarantineMigrationFailure 隔离失败记录；window 级 missing_object/
// orphan_window 在隔离时删除失效 window 行（异常记录替代失效元数据，
// 不伪造 Parquet 数据或覆盖率）。
func (s *APIServer) pqQuarantineMigrationFailure(ctx context.Context, f *model.ContinuousMigrationFailure) {
	now := time.Now()
	if f.Status == model.ContinuousMigrationFailureQuarantined {
		return
	}
	err := s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
		Where("id = ? AND status = ?", f.ID, model.ContinuousMigrationFailureRetrying).
		Updates(map[string]interface{}{
			"status": model.ContinuousMigrationFailureQuarantined, "updated_at": now,
		}).Error
	if err != nil {
		s.Logger.Warn("隔离 migration failure 失败", zap.Uint("id", f.ID), zap.Error(err))
		return
	}
	incContinuousMigrationQuarantine()
	if f.SourceKind == "window" && f.SourceRef != "" {
		// SourceRef 可能是 "window-<id>" 或 "<id>"，兼容两种格式
		ref := strings.TrimPrefix(f.SourceRef, "window-")
		var id uint
		if _, err := fmt.Sscanf(ref, "%d", &id); err == nil {
			if err := s.DB.WithContext(ctx).Where("id = ?", id).Delete(&model.ProfileWindow{}).Error; err != nil {
				s.Logger.Warn("隔离后删除失效 window 失败",
					zap.String("source_ref", f.SourceRef), zap.Error(err))
			}
		}
	}
	s.Logger.Warn("continuous migration failure 已隔离",
		zap.String("source_kind", f.SourceKind), zap.String("source_ref", f.SourceRef),
		zap.String("error_type", f.ErrorType), zap.String("status", "quarantined"))
}

// pqCountMigrationFailures 失败/隔离计数（storage/status 用）。
func (s *APIServer) pqCountMigrationFailures(ctx context.Context) (retrying, quarantined int64) {
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
		Where("status = ?", model.ContinuousMigrationFailureRetrying).Count(&retrying).Error
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
		Where("status = ?", model.ContinuousMigrationFailureQuarantined).Count(&quarantined).Error
	return
}

// pqHasUnresolvedMigrationFailures 判断是否存在未处理失败（对象没有未处理
// 迁移失败是细粒度 GC 的清理条件之一）。
func (s *APIServer) pqHasUnresolvedMigrationFailures(ctx context.Context, sourceKind, sourceRef string) bool {
	if sourceRef == "" {
		return false
	}
	var count int64
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
		Where("source_kind = ? AND source_ref = ? AND status IN ?",
			sourceKind, sourceRef, []string{
				model.ContinuousMigrationFailureRetrying,
				model.ContinuousMigrationFailureQuarantined,
			}).
		Count(&count).Error
	return count > 0
}

// pqHasAnyUnresolvedMigrationFailures 是否存在任何 retrying 失败（GC 阻塞用）。
func (s *APIServer) pqHasAnyUnresolvedMigrationFailures(ctx context.Context) bool {
	var count int64
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationFailure{}).
		Where("status = ?", model.ContinuousMigrationFailureRetrying).Count(&count).Error
	return count > 0
}

// errMissingObject 兼容 StatObject 的 NotExist 判定。
func errMissingObject(err error) bool {
	if err == nil {
		return false
	}
	// minio-go 的 NoSuchKey / 404；SQLite 测试无此语义
	return errors.Is(err, gorm.ErrRecordNotFound) || strings.Contains(strings.ToLower(err.Error()), "no such key") ||
		strings.Contains(strings.ToLower(err.Error()), "not found")
}
