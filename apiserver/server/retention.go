package server

import (
	"context"
	"strings"
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
		s.cleanupHistoricalUnreferencedObjects(ctx)
	}
	s.cleanupAllContinuousRetention(ctx, limit)
}

// cleanupHistoricalUnreferencedObjects is a deliberately narrow migration
// sweep. The database remains authoritative: only old objects absent from both
// artifact and kernel-symbol ledgers can be removed. It never touches the last
// seven days, so an interrupted historical upload remains recoverable.
func (s *APIServer) cleanupHistoricalUnreferencedObjects(ctx context.Context) {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	type candidate struct {
		key    string
		size   int64
		prefix string
	}
	candidates := []candidate{}
	for _, prefix := range []string{"tid-", "kernel-symbols/"} {
		objects, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, prefix)
		if err != nil {
			s.Logger.Warn("历史对象扫尾列举失败", zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		for _, object := range objects {
			if object.Name == "" || !object.LastModified.Before(cutoff) ||
				(prefix == "tid-" && !strings.HasPrefix(object.Name, "tid-")) ||
				(prefix == "kernel-symbols/" && !isKernelSymbolObjectKey(object.Name)) {
				continue
			}
			var artifactRefs, symbolRefs int64
			if s.DB.Model(&model.Artifact{}).Where("object_key = ?", object.Name).Count(&artifactRefs).Error != nil ||
				s.DB.Model(&model.KernelSymbolFile{}).Where("object_key = ?", object.Name).Count(&symbolRefs).Error != nil {
				s.Logger.Warn("历史对象扫尾引用检查失败", zap.String("object_key", object.Name))
				continue
			}
			if artifactRefs == 0 && symbolRefs == 0 {
				candidates = append(candidates, candidate{object.Name, object.Size, prefix})
			}
		}
	}
	if len(candidates) == 0 {
		return
	}
	var total int64
	byPrefix := map[string]int{}
	for _, c := range candidates {
		total += c.size
		byPrefix[c.prefix]++
	}
	s.Logger.Info("历史对象扫尾计划", zap.String("event", "gc_historical_sweep_plan"), zap.Int("objects", len(candidates)), zap.Int64("bytes", total), zap.Any("prefix_counts", byPrefix))
	deleted, deletedBytes := 0, int64(0)
	for _, candidate := range candidates {
		// Recheck immediately before deletion: a late metadata transaction wins.
		var refs int64
		if err := s.DB.Model(&model.Artifact{}).Where("object_key = ?", candidate.key).Count(&refs).Error; err != nil || refs != 0 {
			continue
		}
		if err := s.DB.Model(&model.KernelSymbolFile{}).Where("object_key = ?", candidate.key).Count(&refs).Error; err != nil || refs != 0 {
			continue
		}
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, candidate.key); err != nil {
			s.Logger.Warn("历史对象扫尾删除失败", zap.String("object_key", candidate.key), zap.Error(err))
			continue
		}
		deleted++
		deletedBytes += candidate.size
	}
	s.Logger.Info("历史对象扫尾完成", zap.String("event", "gc_historical_sweep_completed"), zap.Int("objects", deleted), zap.Int64("bytes", deletedBytes), zap.Any("prefix_counts", byPrefix))
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
