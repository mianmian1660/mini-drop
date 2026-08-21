package server

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
)

func (s *APIServer) startRetentionCleaner() {
	if s == nil || s.Config == nil || !s.Config.Retention.Enabled {
		return
	}
	interval := time.Duration(s.Config.Retention.CleanupIntervalSec) * time.Second
	if interval <= 0 {
		interval = time.Hour
	}
	time.Sleep(30 * time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		s.runRetentionCleanup(ctx)
		cancel()
		<-ticker.C
	}
}

func (s *APIServer) runRetentionCleanup(ctx context.Context) {
	if s == nil || s.DB == nil {
		return
	}
	limit := s.retentionBatchLimit()
	if s.StorageConnected() {
		s.cleanupExpiredArtifacts(ctx, []string{model.ArtifactKindRaw, model.ArtifactKindIntermediate, model.ArtifactKindLog}, time.Duration(s.Config.Retention.RawRetentionHours)*time.Hour, limit)
		s.cleanupExpiredArtifacts(ctx, []string{model.ArtifactKindResult, model.ArtifactKindManifest}, time.Duration(s.Config.Retention.ResultRetentionHours)*time.Hour, limit)
		s.cleanupDeletedTaskArtifacts(ctx, limit)
	}
	s.cleanupAllContinuousRetention(ctx, limit)
}

func (s *APIServer) retentionBatchLimit() int {
	if s == nil || s.Config == nil || s.Config.Retention.BatchLimit <= 0 {
		return 200
	}
	return s.Config.Retention.BatchLimit
}

func (s *APIServer) cleanupExpiredArtifacts(ctx context.Context, kinds []string, age time.Duration, limit int) {
	if age <= 0 || limit <= 0 {
		return
	}
	cutoff := time.Now().Add(-age)
	var artifacts []model.Artifact
	if err := s.DB.Where("kind IN ? AND created_at < ?", kinds, cutoff).
		Order("created_at ASC, id ASC").
		Limit(limit).
		Find(&artifacts).Error; err != nil {
		s.Logger.Warn("查询过期 artifact 失败", zap.Error(err))
		return
	}
	s.cleanupArtifactRows(ctx, artifacts)
}

func (s *APIServer) cleanupDeletedTaskArtifacts(ctx context.Context, limit int) {
	if limit <= 0 {
		return
	}
	var tids []string
	if err := s.DB.Raw(
		"SELECT DISTINCT a.task_tid FROM artifacts a JOIN hotmethod_tasks t ON t.tid = a.task_tid WHERE t.deleted_at IS NOT NULL LIMIT ?",
		limit,
	).Scan(&tids).Error; err != nil {
		s.Logger.Warn("查询已软删任务 artifact 失败", zap.Error(err))
		return
	}
	for _, tid := range tids {
		s.cleanupTaskArtifacts(ctx, tid, false)
	}
}

func (s *APIServer) cleanupTaskArtifacts(ctx context.Context, tid string, includeUntrackedObjects bool) {
	if tid == "" {
		return
	}
	var artifacts []model.Artifact
	if err := s.DB.Where("task_tid = ?", tid).Find(&artifacts).Error; err != nil {
		s.Logger.Warn("查询任务 artifact 失败", zap.String("tid", tid), zap.Error(err))
		return
	}
	known := map[string]bool{}
	for _, artifact := range artifacts {
		known[artifact.ObjectKey] = true
	}
	s.cleanupArtifactRows(ctx, artifacts)
	if includeUntrackedObjects && s.StorageConnected() {
		objects, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, tid+"/")
		if err != nil {
			s.Logger.Warn("列出待删除任务对象失败", zap.String("tid", tid), zap.Error(err))
			return
		}
		for _, obj := range objects {
			if obj.Name == "" || known[obj.Name] || isKernelSymbolObjectKey(obj.Name) {
				continue
			}
			if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, obj.Name); err != nil {
				s.Logger.Warn("删除未登记任务对象失败", zap.String("tid", tid), zap.String("object_key", obj.Name), zap.Error(err))
			}
		}
	}
}

func (s *APIServer) cleanupArtifactRows(ctx context.Context, artifacts []model.Artifact) {
	for _, artifact := range artifacts {
		if artifact.ID == 0 || artifact.ObjectKey == "" {
			continue
		}
		if isKernelSymbolObjectKey(artifact.ObjectKey) {
			if err := s.DB.Delete(&model.Artifact{}, artifact.ID).Error; err != nil {
				s.Logger.Warn("删除共享 kallsyms artifact 引用失败", zap.Uint("artifact_id", artifact.ID), zap.Error(err))
				continue
			}
			s.cleanupOrphanKernelSymbol(ctx, artifact.ObjectKey)
			continue
		}
		if s.StorageConnected() {
			if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, artifact.ObjectKey); err != nil {
				s.Logger.Warn("删除过期 artifact 对象失败", zap.String("object_key", artifact.ObjectKey), zap.Error(err))
				continue
			}
		}
		if err := s.DB.Delete(&model.Artifact{}, artifact.ID).Error; err != nil {
			s.Logger.Warn("删除过期 artifact 元数据失败", zap.Uint("artifact_id", artifact.ID), zap.Error(err))
		}
	}
}

func (s *APIServer) cleanupOrphanKernelSymbol(ctx context.Context, objectKey string) {
	var refs int64
	if err := s.DB.Model(&model.Artifact{}).Where("object_key = ?", objectKey).Count(&refs).Error; err != nil {
		s.Logger.Warn("统计 kallsyms 引用失败", zap.String("object_key", objectKey), zap.Error(err))
		return
	}
	if refs > 0 {
		return
	}
	if s.StorageConnected() {
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, objectKey); err != nil {
			s.Logger.Warn("删除孤儿 kallsyms 对象失败", zap.String("object_key", objectKey), zap.Error(err))
			return
		}
	}
	if err := s.DB.Where("object_key = ?", objectKey).Delete(&model.KernelSymbolFile{}).Error; err != nil {
		s.Logger.Warn("删除 kallsyms 账本失败", zap.String("object_key", objectKey), zap.Error(err))
	}
}

func (s *APIServer) cleanupAllContinuousRetention(ctx context.Context, limit int) {
	var sessions []model.ContinuousSession
	query := s.DB.Order("created_at ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Find(&sessions).Error; err != nil {
		s.Logger.Warn("查询 ContinuousSession 清理列表失败", zap.Error(err))
		return
	}
	for _, session := range sessions {
		s.cleanupContinuousRetention(ctx, session)
	}
	s.cleanupContinuousSummaries(ctx)
}

// cleanupContinuousSummaries 清理过期的冷层摘要（ContinuousWindowSummary）。
// 保留期是全局配置（Retention.ContinuousSummaryRetentionHours），不像原始
// 数据那样按 session 各自的 RetentionHours 走——摘要已经和具体 session 的
// 生命周期弱相关（session 停止/删除后摘要还应该按自己的节奏过期）。
func (s *APIServer) cleanupContinuousSummaries(ctx context.Context) {
	hours := s.Config.Retention.ContinuousSummaryRetentionHours
	if hours <= 0 {
		hours = 168
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	if err := s.DB.WithContext(ctx).Where("bucket_start < ?", cutoff).Delete(&model.ContinuousWindowSummary{}).Error; err != nil {
		s.Logger.Warn("Native Continuous Profiling 冷层摘要清理失败", zap.Error(err))
	}
}
