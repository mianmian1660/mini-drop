// ============================================================
// server/coverage.go — 阶段六：精确覆盖区间（continuous_coverage_segments）
// ============================================================
// Parquet raw Block 激活时在同一事务内按 session、信号、小时重建覆盖区间：
//   - 连续或间隔不超过 5 秒的 window 合并为一个 segment，真实缺口原样保留；
//   - segment 独立于 raw Block 生命周期，保留 30 天
//     （CONTINUOUS_COVERAGE_RETENTION_HOURS），raw 降采样后精确缺口不丢失；
//   - Timeline 与细粒度 GC 依赖 segment 判断"区间/样本统计对账"。
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
// 为了保持跨小时连续性，重建范围取 [hour-1h, hour+1h)，替换与该范围相交的
// 旧 segment；相邻小时激活会再次重建并幂等收敛。
func (s *APIServer) pqRebuildCoverageSegmentsTx(tx *gorm.DB, tenant string, hourStart time.Time, signalType string, sourceBlock string, sourceVersion int) error {
	types := pqV1SignalTypesFor(signalType)
	if len(types) == 0 {
		return nil
	}
	now := time.Now()
	from := hourStart.Add(-time.Hour)
	to := hourStart.Add(2 * time.Hour)

	var windows []model.ProfileWindow
	if err := tx.Where("window_start >= ? AND window_start < ? AND signal_type IN ?", from, to, types).
		Order("session_sid ASC, window_start ASC").Find(&windows).Error; err != nil {
		return err
	}
	bySession := map[string][]model.ProfileWindow{}
	for _, w := range windows {
		if w.SessionSID == "" || w.WindowStart.IsZero() || w.WindowEnd.IsZero() || !w.WindowStart.Before(w.WindowEnd) {
			continue
		}
		bySession[w.SessionSID] = append(bySession[w.SessionSID], w)
	}

	// 替换与该范围相交的旧 segment（保持跨小时连续）
	for sid := range bySession {
		if err := tx.Where("session_sid = ? AND signal_type = ? AND segment_start < ? AND segment_end > ?",
			sid, signalType, to, from).Delete(&model.ContinuousCoverageSegment{}).Error; err != nil {
			return err
		}
	}

	// 合并 window → segment
	for sid, ws := range bySession {
		var segs []model.ContinuousCoverageSegment
		for _, w := range ws {
			last := len(segs) - 1
			if last >= 0 && w.WindowStart.Sub(segs[last].SegmentEnd) <= pqCoverageMergeTolerance {
				if w.WindowEnd.After(segs[last].SegmentEnd) {
					segs[last].SegmentEnd = w.WindowEnd
				}
				segs[last].SampleCount = addContinuousCount(segs[last].SampleCount, w.SampleCount)
				continue
			}
			segs = append(segs, model.ContinuousCoverageSegment{
				Tenant:        tenant,
				SessionSID:    sid,
				SignalType:    signalType,
				SegmentStart:  w.WindowStart,
				SegmentEnd:    w.WindowEnd,
				SampleCount:   w.SampleCount,
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

// pqCoverageSegmentsFor 读取某 (session, signal, [from,to)) 的覆盖区间。
func (s *APIServer) pqCoverageSegmentsFor(ctx context.Context, sessionSID, signalType string, from, to time.Time) ([]model.ContinuousCoverageSegment, error) {
	var segments []model.ContinuousCoverageSegment
	err := s.DB.WithContext(ctx).
		Where("session_sid = ? AND signal_type = ? AND segment_start < ? AND segment_end > ?",
			sessionSID, signalType, to, from).
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
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousCoverageSegment{}).
		Where("session_sid = ? AND signal_type = ? AND segment_start < ? AND segment_end > ?",
			sessionSID, signalType, to, from).
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
