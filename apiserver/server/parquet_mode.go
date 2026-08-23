// ============================================================
// server/parquet_mode.go — 阶段五：模式机与后台 worker
// ============================================================
// CONTINUOUS_PARQUET_MODE：
//   - off：不写入 v2、不启动 v2 worker；查询完全走 v1（Release A）。
//   - shadow：v2 双写 + 每完成小时自动对账；v1 仍是唯一查询源（Release B）。
//   - prefer：按 coverage map 优先 v2、缺口回退 v1，继续双写（Release C）。
//   - enforce：停止生成 v1 小时块；分钟 JSON 仅作 staging；既有 v1 保留
//     24h 回滚窗口后按 200 对象/批分批删除（Release D）。
//
// worker 调度：
//   - 构建已封存小时（hour < now - delay）的 raw 块（四类信号）
//   - 降采样：raw 到龄 → 5m；5m 到龄 → 1h
//   - 保留回收：1h 超期 / 配额超限
//   - 每小时对账（shadow/prefer）
// ============================================================

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
)

// pqMode 规范化模式名。
func pqMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "shadow", "prefer", "enforce":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "off"
	}
}

// pqModeEnabled v2 是否写入（shadow/prefer/enforce）。
func pqModeEnabled(mode string) bool { return mode == "shadow" || mode == "prefer" || mode == "enforce" }

// pqModeQueryV2 v2 是否参与查询（prefer/enforce）。
func pqModeQueryV2(mode string) bool { return mode == "prefer" || mode == "enforce" }

// startParquetWorkers 启动 v2 worker（按模式）。
func (s *APIServer) startParquetWorkers() {
	if s == nil || s.DB == nil || s.Config == nil {
		return
	}
	mode := pqMode(s.Config.ContinuousParquet.Mode)
	if !pqModeEnabled(mode) {
		return
	}
	interval := time.Duration(s.Config.ContinuousParquet.BlockIntervalSec) * time.Second
	if interval <= 0 {
		interval = 300 * time.Second
	}
	time.Sleep(10 * time.Second)
	s.Logger.Info("continuous parquet v2 worker 已启动",
		zap.String("mode", mode),
		zap.Int("block_interval_sec", s.Config.ContinuousParquet.BlockIntervalSec),
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		s.runParquetCycle(ctx, mode)
		cancel()
	}
}

// runParquetCycle 一轮 v2 调度。
func (s *APIServer) runParquetCycle(ctx context.Context, mode string) {
	cfg := s.Config.ContinuousParquet
	if s.DB == nil || !s.StorageConnected() {
		return
	}
	// 容量门禁：halted 时只做回收/对账，不构建
	halted := s.capacityHalted()

	// 1) 构建已封存小时的 raw 块（shadow/prefer/enforce 都构建 v2）
	if !halted {
		s.pqBuildSealedRawHours(ctx, cfg.Tenant)
	}

	// 2) 降采样与保留回收
	s.pqDownsampleAndRetain(ctx, cfg.Tenant)

	// 3) shadow/prefer：每小时对账
	if mode == "shadow" || mode == "prefer" {
		s.pqReconcileHours(ctx)
	}

	// 4) enforce：v1 回滚窗口后分批删除
	if mode == "enforce" {
		s.pqReclaimV1Blocks(ctx)
	}

	// 5) 配额回收
	s.pqEnforceQuota(ctx)

	// 6) sweep 回收对象
	s.pqSweepCleanup(ctx, 200)
}

// pqSealedHours 需要构建的封存小时列表（已封存、未过期、尚无 active raw）。
func (s *APIServer) pqSealedRawHours(ctx context.Context, tenant string, now time.Time, limit int) []time.Time {
	delay := time.Duration(s.Config.ContinuousBlock.CompactionDelaySec) * time.Second
	if delay <= 0 {
		delay = 600 * time.Second
	}
	sealedCutoff := now.Add(-delay)

	// 已存在 active raw 的分区（避免重复构建）
	var existingBuckets []time.Time
	if err := s.DB.WithContext(ctx).
		Model(&model.ContinuousParquetBlock{}).
		Where("tenant = ? AND resolution = ? AND status = ?",
			tenant, model.ContinuousParquetResolutionRaw, model.ContinuousParquetStatusActive).
		Pluck("bucket_start", &existingBuckets).Error; err != nil {
		return nil
	}
	covered := map[int64]bool{}
	for _, bucket := range existingBuckets {
		covered[bucket.Unix()] = true
	}

	// 有窗口数据但尚无 raw 块的小时
	type hourRow struct {
		Bucket time.Time
	}
	var hours []hourRow
	trunc := pqHourTruncExpr(s.DB.Dialector.Name(), "window_start")
	if err := s.DB.WithContext(ctx).
		Model(&model.ProfileWindow{}).
		Select(fmt.Sprintf("%s AS bucket", trunc)).
		Where("window_start < ?", sealedCutoff).
		Group("bucket").
		Order("bucket DESC").
		Limit(limit).Scan(&hours).Error; err != nil {
		return nil
	}
	out := make([]time.Time, 0, len(hours))
	for _, hour := range hours {
		if covered[hour.Bucket.Unix()] {
			continue
		}
		out = append(out, hour.Bucket)
	}
	return out
}

// pqBuildSealedRawHours 构建所有待建 raw 小时块。
func (s *APIServer) pqBuildSealedRawHours(ctx context.Context, tenant string) {
	hours := s.pqSealedRawHours(ctx, tenant, time.Now(), 12)
	for _, hour := range hours {
		if _, err := s.pqBuildRawHour(ctx, tenant, hour); err != nil {
			s.Logger.Warn("v2 raw 块构建失败", zap.Time("hour", hour), zap.Error(err))
			continue
		}
	}
}

// pqDownsampleAndRetain 降采样调度与保留回收：
//   - raw 到龄(24h) 且无 5m → 构建 5m
//   - 5m 到龄(7d) 且无 1h → 构建 1h
//   - 1h 超期(30d) → 删除
//   - raw/5m 在下一层已 active 后按宽限删除
func (s *APIServer) pqDownsampleAndRetain(ctx context.Context, tenant string) {
	cfg := s.Config.ContinuousParquet
	now := time.Now()
	rawAge := time.Duration(cfg.RawRetentionHours) * time.Hour
	res5mAge := time.Duration(cfg.Res5mRetentionHours) * time.Hour
	res1hAge := time.Duration(cfg.Res1hRetentionHours) * time.Hour

	// 1) raw 到龄 → 5m
	var rawBlocks []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("tenant = ? AND resolution = ? AND status = ? AND bucket_start < ?",
			tenant, model.ContinuousParquetResolutionRaw, model.ContinuousParquetStatusActive, now.Add(-rawAge)).
		Order("bucket_start ASC").Limit(50).Find(&rawBlocks).Error; err == nil {
		for i := range rawBlocks {
			block := &rawBlocks[i]
			// 已有 5m 则跳过构建（进入删除判定）
			hasTarget := false
			var count int64
			if err := s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
				Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ?",
					tenant, block.BucketStart, block.SignalType, model.ContinuousParquetResolution5m, model.ContinuousParquetStatusActive).
				Count(&count).Error; err == nil && count > 0 {
				hasTarget = true
			}
			if !hasTarget {
				if _, err := s.pqBuildDownsample(ctx, tenant, block.BucketStart,
					model.ContinuousParquetResolutionRaw, model.ContinuousParquetResolution5m); err != nil {
					s.Logger.Warn("v2 5m 降采样失败", zap.String("block_id", block.BlockID), zap.Error(err))
					continue
				}
			}
			// raw 宽限 15 分钟后删除（5m 已 active 的前提下）
			s.pqDeleteLayerAfterGrace(ctx, block, model.ContinuousParquetResolution5m)
		}
	}

	// 2) 5m 到龄 → 1h
	var fiveMinBlocks []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("tenant = ? AND resolution = ? AND status = ? AND bucket_start < ?",
			tenant, model.ContinuousParquetResolution5m, model.ContinuousParquetStatusActive, now.Add(-res5mAge)).
		Order("bucket_start ASC").Limit(50).Find(&fiveMinBlocks).Error; err == nil {
		for i := range fiveMinBlocks {
			block := &fiveMinBlocks[i]
			var count int64
			if err := s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
				Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ?",
					tenant, block.BucketStart, block.SignalType, model.ContinuousParquetResolution1h, model.ContinuousParquetStatusActive).
				Count(&count).Error; err == nil && count == 0 {
				if _, err := s.pqBuildDownsample(ctx, tenant, block.BucketStart,
					model.ContinuousParquetResolution5m, model.ContinuousParquetResolution1h); err != nil {
					s.Logger.Warn("v2 1h 降采样失败", zap.String("block_id", block.BlockID), zap.Error(err))
					continue
				}
			}
			s.pqDeleteLayerAfterGrace(ctx, block, model.ContinuousParquetResolution1h)
		}
	}

	// 3) 1h 超期 → 删除
	_, _ = s.pqReclaimExpiredBlocks(ctx, model.ContinuousParquetResolution1h, now.Add(-res1hAge), 50)
}

// pqDeleteLayerAfterGrace 源层在目标层已 active 且超过 15 分钟宽限后删除。
func (s *APIServer) pqDeleteLayerAfterGrace(ctx context.Context, source *model.ContinuousParquetBlock, targetResolution string) {
	if source.Status != model.ContinuousParquetStatusActive {
		return
	}
	var count int64
	if err := s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ? AND validation = ?",
			source.Tenant, source.BucketStart, source.SignalType, targetResolution,
			model.ContinuousParquetStatusActive, model.ContinuousParquetValidationPassed).
		Count(&count).Error; err != nil || count == 0 {
		return
	}
	if source.CreatedAt.Add(15 * time.Minute).After(time.Now()) {
		return
	}
	if err := s.pqSupersedeAndDeleteBlock(ctx, source, "layer_superseded"); err != nil {
		s.Logger.Warn("v2 源层删除标记失败", zap.String("block_id", source.BlockID), zap.Error(err))
	}
}

// pqReconcileHours shadow/prefer 每小时对账：对已 active 的 raw 块
// 校验统计（v1 vs v2）。失败计数入指标。
func (s *APIServer) pqReconcileHours(ctx context.Context) {
	var blocks []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND validation = ?", model.ContinuousParquetStatusActive, model.ContinuousParquetValidationPassed).
		Order("bucket_start DESC").Limit(100).Find(&blocks).Error; err != nil {
		return
	}
	for i := range blocks {
		block := &blocks[i]
		key := pqBlockKey{
			Tenant: block.Tenant, BucketStart: block.BucketStart,
			SignalType: block.SignalType, Resolution: block.Resolution,
		}
		stats := pqBlockStats{
			RowCount: block.RowCount, ValueTotal: block.ValueTotal,
			SampleTotal: block.SampleTotal, SessionCount: block.SessionCount,
			ProcessCount: block.ProcessCount, BytesTotal: block.BytesTotal,
			FirstRowTime: block.FirstRowTime, LastRowTime: block.LastRowTime,
		}
		s.pqShadowReconcile(ctx, key, block.BlockID, stats)
	}
}

// pqEnforceQuota 配额回收：超过硬配额先回收 staging/superseded，再最老 1h。
func (s *APIServer) pqEnforceQuota(ctx context.Context) {
	ok, snap := s.continuousQuotaOK(ctx)
	if ok {
		return
	}
	s.Logger.Warn("continuous 配额超限，触发回收",
		zap.Int64("used_bytes", snap.UsedBytes), zap.Int64("quota_bytes", snap.QuotaBytes))
	s.pqReclaimForQuota(ctx)
}

// pqReclaimV1Blocks enforce：v1 回滚窗口后按批删除。
func (s *APIServer) pqReclaimV1Blocks(ctx context.Context) {
	cfg := s.Config.ContinuousParquet
	rollbackWindow := time.Duration(cfg.V1RollbackWindowHours) * time.Hour
	if rollbackWindow <= 0 {
		rollbackWindow = 24 * time.Hour
	}
	cutoff := time.Now().Add(-rollbackWindow)
	batch := cfg.V1DeleteBatch
	if batch <= 0 {
		batch = 200
	}
	var blocks []model.ContinuousProfileBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND bucket_end < ?", model.ContinuousBlockStatusActive, cutoff).
		Order("bucket_start ASC").Limit(batch).Find(&blocks).Error; err != nil {
		return
	}
	for i := range blocks {
		blk := &blocks[i]
		s.deleteContinuousBlock(ctx, blk, nil)
	}
}

// pqCoverageSnapshot v2 覆盖情况（/storage/status 用）。
type pqCoverageSnapshot struct {
	ByResolution map[string]int `json:"by_resolution"`
	ActiveBlocks int            `json:"active_blocks"`
	FailedBlocks int            `json:"failed_blocks"`
	ValidationBacklog int       `json:"validation_backlog"`
	ShadowFailures   int64      `json:"shadow_failures"`
	EarliestActiveAt *time.Time `json:"earliest_active_at"`
}

func (s *APIServer) pqCoverageSnapshot(ctx context.Context) pqCoverageSnapshot {
	out := pqCoverageSnapshot{
		ByResolution: map[string]int{
			model.ContinuousParquetResolutionRaw: 0,
			model.ContinuousParquetResolution5m:  0,
			model.ContinuousParquetResolution1h:  0,
		},
	}
	var blocks []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ?", model.ContinuousParquetStatusActive).
		Select("resolution, bucket_start").Find(&blocks).Error; err != nil {
		return out
	}
	for _, block := range blocks {
		out.ActiveBlocks++
		out.ByResolution[block.Resolution]++
		if out.EarliestActiveAt == nil || block.BucketStart.Before(*out.EarliestActiveAt) {
			t := block.BucketStart
			out.EarliestActiveAt = &t
		}
	}
	var failed int64
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("status = ?", model.ContinuousParquetStatusFailed).Count(&failed).Error
	out.FailedBlocks = int(failed)
	var backlog int64
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("status IN ?", []string{model.ContinuousParquetStatusBuilding, model.ContinuousParquetStatusValidating}).
		Count(&backlog).Error
	out.ValidationBacklog = int(backlog)
	out.ShadowFailures = parquetShadowFailures.Load()
	return out
}

// pqModeOf 便捷取规范化模式。
func (s *APIServer) pqModeOf() string { return pqMode(s.Config.ContinuousParquet.Mode) }

// pqEarliestAvailableAt 实际最早可查询时间（v2 覆盖的 min bucket_start）。
func (s *APIServer) pqEarliestAvailableAt(ctx context.Context) *time.Time {
	snap := s.pqCoverageSnapshot(ctx)
	return snap.EarliestActiveAt
}

// pqCoverageForHour 判断某小时是否有 active v2 块（prefer 查询用）。
func (s *APIServer) pqCoverageForHour(ctx context.Context, tenant string, hourStart time.Time, signalType string) bool {
	var count int64
	if err := s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND status = ? AND validation = ?",
			tenant, hourStart, signalType, model.ContinuousParquetStatusActive, model.ContinuousParquetValidationPassed).
		Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}

// pqFindBestBlock 按 raw→5m→1h 优先级找某小时的 active 块。
func (s *APIServer) pqFindBestBlock(ctx context.Context, tenant string, hourStart time.Time, signalType string) (*model.ContinuousParquetBlock, error) {
	for _, resolution := range model.ContinuousParquetResolutions {
		var block model.ContinuousParquetBlock
		err := s.DB.WithContext(ctx).
			Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ? AND validation = ?",
				tenant, hourStart, signalType, resolution,
				model.ContinuousParquetStatusActive, model.ContinuousParquetValidationPassed).
			First(&block).Error
		if err == nil {
			return &block, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	return nil, nil
}

// pqErrNotImplemented 未实现路径占位（后续 Release 填）。
func pqErrNotImplemented() error { return fmt.Errorf("尚未实现") }
