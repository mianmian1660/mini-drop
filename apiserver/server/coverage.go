// ============================================================
// server/coverage.go — 阶段六：精确覆盖区间（continuous_coverage_segments）
// ============================================================
// Parquet raw Block 激活时在同一事务内按 session、信号、小时重建覆盖区间：
//   - 连续或间隔不超过 5 秒的 window 合并为一个 segment，真实缺口原样保留；
//   - segment 独立于 raw Block 生命周期，保留 30 天
//     （CONTINUOUS_COVERAGE_RETENTION_HOURS），raw 降采样后精确缺口不丢失；
//   - segment 只服务 Timeline；细粒度 GC 使用永久 migration receipt。
// ============================================================

package server

import (
	"context"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
)

// pqCoverageMergeTolerance 相邻 window 合并的最大间隔。
const pqCoverageMergeTolerance = 5 * time.Second

// pqRebuildCoverageSegmentsTx 在激活事务内重建某 (hour, signal) 的覆盖区间。
// segment 严格限制在当前小时，并保留原始 signal subtype；它只用于 Timeline，
// 不再作为 GC 删除证明。
func (s *APIServer) pqRebuildCoverageSegmentsTx(tx *gorm.DB, tenant string, hourStart time.Time, signalType string, sourceBlock string, sourceVersion int) error {
	types := pqV1SignalTypesFor(signalType)
	if len(types) == 0 {
		return nil
	}
	now := time.Now()
	from := hourStart
	to := hourStart.Add(time.Hour)

	var windows []model.ProfileWindow
	if err := tx.Where("window_start < ? AND window_end > ? AND signal_type IN ?", to, from, types).
		Order("session_sid ASC, window_start ASC").Find(&windows).Error; err != nil {
		return err
	}
	// 统一数据源（均裁剪到当前小时）：
	//   1. 优先热 window（激活事务内窗口尚在，含精确 signal subtype）；
	//   2. 若 window 已被细粒度 GC 清理，退化为该 block 的持久 lineage
	//      members（continuous_parquet_block_members 不随 window 清理删除，
	//      含 session/时间/样本），按 block 的 canonical 信号归属；
	//   3. 两者皆无时保留已有 coverage 不清空（防止启动重建把真实覆盖区间
	//      误删成 0，导致 Timeline 出现新增缺口）。
	type coverageSource struct {
		sessionSID  string
		signalType  string
		start, end  time.Time
		sampleCount uint64
	}
	var sources []coverageSource
	for _, w := range windows {
		if w.SessionSID == "" || w.WindowStart.IsZero() || w.WindowEnd.IsZero() || !w.WindowStart.Before(w.WindowEnd) {
			continue
		}
		st, en := w.WindowStart, w.WindowEnd
		if st.Before(from) {
			st = from
		}
		if en.After(to) {
			en = to
		}
		sources = append(sources, coverageSource{w.SessionSID, w.SignalType, st, en, w.SampleCount})
	}
	if len(sources) == 0 {
		var members []model.ContinuousParquetBlockMember
		if err := tx.Where("block_id = ?", sourceBlock).Order("start_time ASC").Find(&members).Error; err != nil {
			return err
		}
		for _, m := range members {
			if m.SessionSID == "" || m.StartTime.IsZero() || m.EndTime.IsZero() || !m.StartTime.Before(m.EndTime) {
				continue
			}
			st, en := m.StartTime, m.EndTime
			if st.Before(from) {
				st = from
			}
			if en.After(to) {
				en = to
			}
			// members 不存信号归属，raw 块每个 block 是单一 canonical 信号
			// （cpu/histogram/metrics/db），用其首个 v1 signal subtype 标记。
			sources = append(sources, coverageSource{m.SessionSID, types[0], st, en, m.SampleCount})
		}
	}
	if len(sources) == 0 {
		return nil
	}

	// 有数据源才清掉该 canonical signal 当前小时的旧段并重建；
	// 无数据源（window 与 member 均缺失）时保留已有 coverage。
	deleteTypes := append(append([]string{}, types...), signalType)
	if err := tx.Where("tenant = ? AND signal_type IN ? AND segment_start >= ? AND segment_start < ?",
		tenant, deleteTypes, from, to).Delete(&model.ContinuousCoverageSegment{}).Error; err != nil {
		return err
	}

	// 按 (session, signal) 分组合并 → segment
	bySeries := map[string][]coverageSource{}
	for _, sg := range sources {
		key := sg.sessionSID + "\x00" + sg.signalType
		bySeries[key] = append(bySeries[key], sg)
	}
	for _, series := range bySeries {
		var segs []model.ContinuousCoverageSegment
		for _, sg := range series {
			last := len(segs) - 1
			if last >= 0 && sg.start.Sub(segs[last].SegmentEnd) <= pqCoverageMergeTolerance {
				if sg.end.After(segs[last].SegmentEnd) {
					segs[last].SegmentEnd = sg.end
				}
				segs[last].SampleCount = addContinuousCount(segs[last].SampleCount, sg.sampleCount)
				continue
			}
			segs = append(segs, model.ContinuousCoverageSegment{
				Tenant:        tenant,
				SessionSID:    sg.sessionSID,
				SignalType:    sg.signalType,
				SegmentStart:  sg.start,
				SegmentEnd:    sg.end,
				SampleCount:   sg.sampleCount,
				SourceBlock:   sourceBlock,
				SourceVersion: sourceVersion,
				Resolution:    model.ContinuousParquetResolutionRaw,
				CreatedAt:     now,
				UpdatedAt:     now,
			})
		}
		for i := range segs {
			if err := tx.Create(&segs[i]).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// pqRebuildActiveCoverageCatalog 在新版本启动时从 active raw block 重建派生的
// Timeline catalog。receipt/Parquet 数据不受影响。
func (s *APIServer) pqRebuildActiveCoverageCatalog(ctx context.Context, tenant string) {
	var blocks []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("tenant = ? AND resolution = ? AND status = ? AND validation = ? AND reconcile_status = ?",
			tenant, model.ContinuousParquetResolutionRaw, model.ContinuousParquetStatusActive,
			model.ContinuousParquetValidationPassed, model.ContinuousParquetReconcilePassed).
		Order("bucket_start ASC").Find(&blocks).Error; err != nil {
		return
	}
	for i := range blocks {
		block := &blocks[i]
		if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return s.pqRebuildCoverageSegmentsTx(tx, tenant, block.BucketStart, block.SignalType, block.BlockID, block.Version)
		}); err != nil {
			s.Logger.Warn("启动时重建 coverage catalog 失败", zap.String("block_id", block.BlockID), zap.Error(err))
		}
	}
}

// pqCoverageSegmentsFor 读取某 (session, signal, [from,to)) 的覆盖区间。
func (s *APIServer) pqCoverageSegmentsFor(ctx context.Context, sessionSID, signalType string, from, to time.Time) ([]model.ContinuousCoverageSegment, error) {
	var segments []model.ContinuousCoverageSegment
	types := pqV1SignalTypesFor(signalType)
	if len(types) == 0 {
		types = []string{signalType}
	}
	err := s.DB.WithContext(ctx).
		Where("session_sid = ? AND signal_type IN ? AND segment_start < ? AND segment_end > ?",
			sessionSID, types, to, from).
		Order("segment_start ASC").Find(&segments).Error
	return segments, err
}

// pqCoverageCovered 判断 (session, signal) 的 window 区间是否被 segment
// 完整覆盖（区间对账：segment 必须覆盖 window 的 [start,end)，允许 ≤5s
// 的合并容差，与 segment 生成规则一致）。
func (s *APIServer) pqCoverageCovered(ctx context.Context, sessionSID, signalType string, start, end time.Time) bool {
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return false
	}
	segments, err := s.pqCoverageSegmentsFor(ctx, sessionSID, signalType, start, end)
	if err != nil {
		return false
	}
	cursor := start
	for _, seg := range segments {
		// seg 起点晚于游标超过容差 → 中间有真实缺口
		if seg.SegmentStart.After(cursor.Add(pqCoverageMergeTolerance)) {
			return false
		}
		if seg.SegmentEnd.After(cursor) {
			cursor = seg.SegmentEnd
		}
		if !cursor.Before(end) {
			return true
		}
	}
	return !cursor.Before(end)
}

// pqCoverageSampleTotalFor 某 (session, signal, [from,to)) 的 segment 样本总量
// （统计对账：GC 用 segment 样本量核对被清理 window 的样本量）。
func (s *APIServer) pqCoverageSampleTotalFor(ctx context.Context, sessionSID, signalType string, from, to time.Time) uint64 {
	var total uint64
	types := pqV1SignalTypesFor(signalType)
	if len(types) == 0 {
		types = []string{signalType}
	}
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousCoverageSegment{}).
		Where("session_sid = ? AND signal_type IN ? AND segment_start < ? AND segment_end > ?",
			sessionSID, types, to, from).
		Select("COALESCE(SUM(sample_count),0)").Scan(&total).Error
	return total
}

// pqReclaimExpiredCoverage 回收超过保留期的覆盖区间（默认 30 天）。
func (s *APIServer) pqReclaimExpiredCoverage(ctx context.Context, limit int) int64 {
	if limit <= 0 {
		limit = 500
	}
	cfg := s.Config.ContinuousParquet
	hours := cfg.CoverageRetentionHours
	if hours <= 0 {
		hours = 720
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	var segments []model.ContinuousCoverageSegment
	if err := s.DB.WithContext(ctx).Where("segment_end < ?", cutoff).
		Order("segment_end ASC").Limit(limit).Find(&segments).Error; err != nil {
		return 0
	}
	var removed int64
	for i := range segments {
		if err := s.DB.WithContext(ctx).Delete(&segments[i]).Error; err != nil {
			s.Logger.Warn("删除过期 coverage segment 失败",
				zap.Uint("id", segments[i].ID), zap.Error(err))
			continue
		}
		removed++
	}
	return removed
}

// pqCountCoverageSegments 覆盖区间总数（storage/status 用）。
func (s *APIServer) pqCountCoverageSegments(ctx context.Context) int64 {
	var count int64
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousCoverageSegment{}).Count(&count).Error
	return count
}
