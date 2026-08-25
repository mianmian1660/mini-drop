// ============================================================
// server/coverage_bands.go — 阶段九：按信号覆盖率、状态可信度与
// 长时间范围服务端聚合（coverage_bands）
// ============================================================
// 解决的问题：
//   1. 覆盖率按当前选中信号独立计算，不再被其他信号的数据"填绿"；
//   2. 零样本窗口（target_idle/no_events/unavailable/failed/unknown）
//      不再作为有效覆盖：只有 sample_count>0 或明确 collected 才算；
//   3. 启动/停止/上传等待边界明确：startup_grace / shutdown_grace /
//      pending_upload 不产生真实缺口，也不降低 finalized 覆盖率；
//   4. 长时间范围服务端聚合为最多 600 个 coverage_bands，前端不再
//      拼接数千条 gap 生成 DOM。
// 兼容性：现有 coverage/gaps/pending 字段保留旧口径，新字段
// （signal_coverage / coverage_bands / status_summary / boundary）
// 增量加入，旧客户端继续使用旧字段。
// ============================================================

package server

import (
	"math"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// continuousCoverageStatus 覆盖状态分类（服务端权威口径，前端只做展示）。
type continuousCoverageStatus string

const (
	continuousCoverageHealthy         continuousCoverageStatus = "healthy"          // 有有效数据（绿）
	continuousCoverageRealGap         continuousCoverageStatus = "real_gap"         // 确认缺少数据（红）
	continuousCoveragePendingUpload   continuousCoverageStatus = "pending_upload"   // 数据整理中（黄）
	continuousCoverageTargetIdle      continuousCoverageStatus = "target_idle"      // 目标暂时空闲（灰）
	continuousCoverageStartupGrace    continuousCoverageStatus = "startup_grace"    // 正在启动采集（灰）
	continuousCoverageShutdownGrace   continuousCoverageStatus = "shutdown_grace"   // 停止收尾中（灰）
	continuousCoverageCollectorFailed continuousCoverageStatus = "collector_failed" // 采集异常（红）
	continuousCoverageUnknown         continuousCoverageStatus = "unknown"          // 状态未知（灰）
)

// continuousCoverageBandLimit 每个信号最多返回的 coverage_bands 数量。
const continuousCoverageBandLimit = 600

// continuousCoverageGapDetailLimit 详细缺口最多返回条数（其余用数量汇总）。
const continuousCoverageGapDetailLimit = 20

// continuousCoverageBand 时间桶聚合后的可视化区间。
type continuousCoverageBand struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	Status          string    `json:"status"`
	SampleCount     uint64    `json:"sample_count"`
	DurationSeconds float64   `json:"duration_seconds"`
	CoveredSeconds  float64   `json:"covered_seconds"`
	GapSeconds      float64   `json:"gap_seconds"`
}

// continuousGraceInterval 启动/停止收尾 grace 区间。
type continuousGraceInterval struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Seconds float64   `json:"seconds"`
}

// continuousCoverageBoundary 启动/停止边界（Timeline 顶层 boundary 字段）。
type continuousCoverageBoundary struct {
	StartupGrace  *continuousGraceInterval `json:"startup_grace"`
	ShutdownGrace *continuousGraceInterval `json:"shutdown_grace"`
	ActiveFrom    time.Time                `json:"active_from"`
	ActiveTo      time.Time                `json:"active_to"`
}

// continuousStatusSummary 服务端生成的稳定用户可读文案，前端只负责展示。
type continuousStatusSummary struct {
	Status      string `json:"status"`
	Label       string `json:"label"`
	Explanation string `json:"explanation"`
	Suggestion  string `json:"suggestion"`
}

// continuousSignalCoverage 单个信号的覆盖率结果（signal_coverage 条目）。
type continuousSignalCoverage struct {
	Coverage       gin.H                    `json:"coverage"`
	Gaps           []continuousTimelineGap  `json:"gaps"`
	GapCountTotal  int                      `json:"gap_count_total"`
	Status         string                   `json:"status"`
	CoverageBands  []continuousCoverageBand `json:"coverage_bands"`
	BandLimit      int                      `json:"band_limit"`
	BandResolution float64                  `json:"band_resolution_seconds"`
	StatusSummary  continuousStatusSummary  `json:"status_summary"`
}

// continuousStatusInterval 内部状态区间（带状态与样本数）。
type continuousStatusInterval struct {
	start, end  time.Time
	status      continuousCoverageStatus
	sampleCount uint64
}

// continuousSessionBoundaries Session 级启动/停止 grace 区间。启动 grace 以
// 任一信号的首个有效数据到达为终点（采集器已工作，其他信号无数据即真实
// 缺口，不能被启动期"填灰"）；停止 grace 以最后有效数据为起点。
type continuousSessionBoundaries struct {
	startupGrace  *continuousStatusInterval
	shutdownGrace *continuousStatusInterval
}

// continuousCoverageStats 区间集合的统计结果。
type continuousCoverageStats struct {
	coveredSeconds   float64
	redSeconds       float64
	redFailedSeconds float64
	graySeconds      float64
	pendingSeconds   float64
	sampleCount      uint64
}

// continuousTimelineGrace 启动/停止/上传等待的统一 grace：
// upload_batch_sec + 2×aggregation_window_sec + 15 秒。
func continuousTimelineGrace(session model.ContinuousSession) time.Duration {
	return time.Duration(firstNonZeroUint32(session.UploadBatchSec, 60))*time.Second +
		2*time.Duration(firstNonZeroUint32(session.AggregationWindowSec, 10))*time.Second +
		15*time.Second
}

// continuousWindowCoverageStatus 按窗口样本数与 signal_status 分类。
// 有效覆盖只允许 sample_count>0 或明确 collected；target_idle/no_events
// 为灰色；unavailable/failed 为红色；unknown 为灰色（历史数据无法确认）。
func continuousWindowCoverageStatus(w model.ProfileWindow) continuousCoverageStatus {
	if w.SampleCount > 0 || w.SignalStatus == "collected" {
		return continuousCoverageHealthy
	}
	switch w.SignalStatus {
	case "target_idle", "no_events":
		return continuousCoverageTargetIdle
	case "unavailable", "failed":
		return continuousCoverageCollectorFailed
	default:
		return continuousCoverageUnknown
	}
}

// continuousSegmentCoverageStatus 历史压缩 segment 无 signal_status，
// 按样本数推导：>0 视为有效覆盖，否则状态未知（灰色）。
func continuousSegmentCoverageStatus(seg model.ContinuousCoverageSegment) continuousCoverageStatus {
	if seg.SampleCount > 0 {
		return continuousCoverageHealthy
	}
	return continuousCoverageUnknown
}

// continuousSignalTypesForTimeline 把请求信号展开为 v1 window signal_type
// 集合。v2 canonical 名（cpu/metrics/histogram/db）复用 pqV1SignalTypesFor；
// v1 精确类型（cpu_profile/io_latency/io_syscall_latency/sched_latency/
// db_snapshot 等）原样返回，保证"当前选中信号"不被同族其他信号填绿。
func continuousSignalTypesForTimeline(signal string) []string {
	switch signal {
	case model.ContinuousParquetSignalCPU, model.ContinuousParquetSignalMetrics,
		model.ContinuousParquetSignalHistogram, model.ContinuousParquetSignalDB:
		return pqV1SignalTypesFor(signal)
	}
	return []string{signal}
}

// continuousTimelineSignalsFor 返回需要计算 signal_coverage 的信号列表。
// 显式请求时只算该信号；否则按 session.signals 展开为 v1 类型。
func continuousTimelineSignalsFor(session model.ContinuousSession, requested string) []string {
	if requested != "" {
		return continuousSignalTypesForTimeline(requested)
	}
	var signals []string
	_ = util.UnmarshalJSONB(session.Signals, &signals)
	seen := map[string]bool{}
	out := make([]string, 0, len(signals))
	for _, sig := range signals {
		for _, t := range continuousSignalTypesForTimeline(sig) {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	if len(out) == 0 {
		out = []string{"cpu_profile"}
	}
	return out
}

// filterMergedBySignal 按 v1 signal_type 集合过滤合并后的窗口列表。
func filterMergedBySignal(merged []model.ProfileWindow, types []string) []model.ProfileWindow {
	if len(types) == 0 {
		return merged
	}
	allowed := map[string]bool{}
	for _, t := range types {
		allowed[t] = true
	}
	out := make([]model.ProfileWindow, 0, len(merged))
	for _, w := range merged {
		if allowed[w.SignalType] {
			out = append(out, w)
		}
	}
	return out
}

// continuousSessionBoundariesFor 计算 Session 级启动/停止 grace 区间
// （基于全部信号的合并窗口，与具体信号无关）。
func continuousSessionBoundariesFor(merged []model.ProfileWindow, session model.ContinuousSession,
	from, to time.Time, grace time.Duration) continuousSessionBoundaries {
	startedAt := session.StartedAt
	if startedAt.IsZero() {
		startedAt = session.CreatedAt
	}
	firstData, lastData := time.Time{}, time.Time{}
	for _, w := range merged {
		if firstData.IsZero() || w.WindowStart.Before(firstData) {
			firstData = w.WindowStart
		}
		if w.WindowEnd.After(lastData) {
			lastData = w.WindowEnd
		}
	}
	var b continuousSessionBoundaries
	// 启动 grace：等待首个有效窗口。首个数据到达后不再延伸，避免与数据
	// 区间重叠；无数据时覆盖 [startedAt, startedAt+grace]。
	startupEnd := startedAt.Add(grace)
	if !firstData.IsZero() && firstData.Before(startupEnd) {
		startupEnd = firstData
	}
	if startupEnd.After(from) && startedAt.Before(to) {
		s, e := startedAt, startupEnd
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		if s.Before(e) {
			b.startupGrace = &continuousStatusInterval{start: s, end: e, status: continuousCoverageStartupGrace}
		}
	}
	// 停止收尾 grace：最后有效数据到 stopped_at 之间，灰色不计缺口。
	if session.StoppedAt != nil {
		lastDataEnd := lastData
		if lastDataEnd.IsZero() {
			lastDataEnd = startedAt
		}
		if session.StoppedAt.After(lastDataEnd) {
			s, e := lastDataEnd, *session.StoppedAt
			if s.Before(from) {
				s = from
			}
			if e.After(to) {
				e = to
			}
			if s.Before(e) {
				b.shutdownGrace = &continuousStatusInterval{start: s, end: e, status: continuousCoverageShutdownGrace}
			}
		}
	}
	return b
}

// continuousStatusIntervalsFor 构建 [from,to] 内的完整状态区间集合：
// 数据区间（按窗口状态分类）+ 启动 grace + 停止收尾 grace + pending tail
// + 真实缺口。返回的区间互不重叠（grace 已按数据边界裁剪）。
func continuousStatusIntervalsFor(merged []model.ProfileWindow, from, to, finalizedTo time.Time,
	session model.ContinuousSession, boundaries continuousSessionBoundaries) []continuousStatusInterval {
	intervals := make([]continuousStatusInterval, 0, len(merged)+4)
	for _, w := range merged {
		start, end := w.WindowStart, w.WindowEnd
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if !start.Before(end) {
			continue
		}
		intervals = append(intervals, continuousStatusInterval{start: start, end: end, status: continuousWindowCoverageStatus(w), sampleCount: w.SampleCount})
	}
	if boundaries.startupGrace != nil {
		intervals = append(intervals, *boundaries.startupGrace)
	}
	if boundaries.shutdownGrace != nil {
		intervals = append(intervals, *boundaries.shutdownGrace)
	}

	// pending tail：running Session 的最终化边界之后到查询终点，黄色等待
	// 上传，不降低 finalized 覆盖率。起点不早于启动 grace 结束，避免重叠。
	finalTo := to
	if !finalizedTo.IsZero() && finalizedTo.Before(finalTo) {
		finalTo = finalizedTo
	}
	if finalTo.Before(from) {
		finalTo = from
	}
	pendingStart := finalTo
	if boundaries.startupGrace != nil && boundaries.startupGrace.end.After(pendingStart) {
		pendingStart = boundaries.startupGrace.end
	}
	if pendingStart.Before(to) {
		intervals = append(intervals, continuousStatusInterval{start: pendingStart, end: to, status: continuousCoveragePendingUpload})
	}

	// 真实缺口：最终化域内、grace 之外、无数据。非缺口区间（数据+grace+
	// pending）合并后取补集，间隔 ≤ tolerance 的缝隙不视为缺口。
	nonGap := mergeStatusIntervals(intervals, pqCoverageMergeTolerance)
	cursor := from
	for _, iv := range nonGap {
		if iv.start.Sub(cursor) > pqCoverageMergeTolerance {
			s, e := cursor, iv.start
			if e.After(finalTo) {
				e = finalTo
			}
			if s.Before(e) {
				intervals = append(intervals, continuousStatusInterval{start: s, end: e, status: continuousCoverageRealGap})
			}
		}
		if iv.end.After(cursor) {
			cursor = iv.end
		}
	}
	if finalTo.Sub(cursor) > pqCoverageMergeTolerance {
		intervals = append(intervals, continuousStatusInterval{start: cursor, end: finalTo, status: continuousCoverageRealGap})
	}
	return intervals
}

// mergeStatusIntervals 合并相邻（间隔 ≤ tolerance）且状态相同的区间。
// 相同起点时高优先级状态在前（防御性排序，正常输入无重叠）。
func mergeStatusIntervals(intervals []continuousStatusInterval, tolerance time.Duration) []continuousStatusInterval {
	if len(intervals) == 0 {
		return nil
	}
	sorted := make([]continuousStatusInterval, len(intervals))
	copy(sorted, intervals)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start.Equal(sorted[j].start) {
			return coverageStatusPriority(sorted[i].status) > coverageStatusPriority(sorted[j].status)
		}
		return sorted[i].start.Before(sorted[j].start)
	})
	merged := make([]continuousStatusInterval, 0, len(sorted))
	for _, iv := range sorted {
		last := len(merged) - 1
		if last >= 0 && iv.status == merged[last].status && iv.start.Sub(merged[last].end) <= tolerance {
			if iv.end.After(merged[last].end) {
				merged[last].end = iv.end
			}
			merged[last].sampleCount += iv.sampleCount
			continue
		}
		merged = append(merged, iv)
	}
	return merged
}

// coverageStatusPriority 桶内状态优先级：真实缺口 > 采集失败 > 空闲/无事件
// > 等待上传 > 有数据。灰色类（target_idle/startup_grace/shutdown_grace/
// unknown）同级。
func coverageStatusPriority(status continuousCoverageStatus) int {
	switch status {
	case continuousCoverageRealGap:
		return 5
	case continuousCoverageCollectorFailed:
		return 4
	case continuousCoverageTargetIdle, continuousCoverageStartupGrace, continuousCoverageShutdownGrace, continuousCoverageUnknown:
		return 3
	case continuousCoveragePendingUpload:
		return 2
	default:
		return 1
	}
}

// continuousCoverageStatsOf 统计区间集合的覆盖/红/灰/黄秒数与样本数。
func continuousCoverageStatsOf(intervals []continuousStatusInterval) continuousCoverageStats {
	var st continuousCoverageStats
	for _, iv := range intervals {
		d := iv.end.Sub(iv.start).Seconds()
		st.sampleCount += iv.sampleCount
		switch iv.status {
		case continuousCoverageHealthy:
			st.coveredSeconds += d
		case continuousCoverageRealGap:
			st.redSeconds += d
		case continuousCoverageCollectorFailed:
			st.redSeconds += d
			st.redFailedSeconds += d
		case continuousCoveragePendingUpload:
			st.pendingSeconds += d
		default:
			st.graySeconds += d
		}
	}
	return st
}

// continuousOverallStatus 汇总状态：mixed > collector_failed > real_gap >
// pending_upload > target_idle > healthy > unknown。有数据时黄色/灰色不掩盖
// "数据正常"——正常 running Session 的尾部 pending 是常态，仅当等待/空闲
// 占主导（超过有效数据）时才提升为对应状态。
func continuousOverallStatus(st continuousCoverageStats) string {
	if st.redSeconds > 0 {
		if st.coveredSeconds > 0 {
			return "mixed"
		}
		if st.redFailedSeconds >= st.redSeconds {
			return "collector_failed"
		}
		return "real_gap"
	}
	if st.coveredSeconds > 0 {
		if st.pendingSeconds > st.coveredSeconds {
			return "pending_upload"
		}
		if st.graySeconds > st.coveredSeconds {
			return "target_idle"
		}
		return "healthy"
	}
	if st.pendingSeconds > 0 {
		return "pending_upload"
	}
	if st.graySeconds > 0 {
		return "target_idle"
	}
	return "unknown"
}

// continuousStatusSummaryText 状态 → 稳定中文文案（label/explanation/suggestion）。
func continuousStatusSummaryText(status string) (label, explanation, suggestion string) {
	switch status {
	case "healthy":
		return "数据正常", "这段时间已经收到有效采集数据", ""
	case "real_gap":
		return "确认缺少数据", "这段时间已经超过等待时间，仍没有收到数据", "请检查 Agent 状态与网络上传"
	case "pending_upload":
		return "数据整理中", "采集器已经工作，数据还在上传或整理", "稍后刷新；如果持续超过上传周期，请检查 Agent 状态"
	case "target_idle":
		return "目标暂时空闲", "目标存在，但这段时间没有可采集活动", "无需处理"
	case "startup_grace":
		return "正在启动采集", "采集器刚启动，正在准备首批数据", "稍等片刻后刷新"
	case "shutdown_grace":
		return "停止收尾中", "采集已经停止，正在完成最后数据整理", "无需处理"
	case "collector_failed":
		return "采集异常", "采集过程出现错误，需要检查 Agent", "请检查 Agent 日志与采集能力"
	case "unknown":
		return "状态未知", "这段历史数据缺少足够状态信息", "可尝试重新采集"
	case "mixed":
		return "部分异常", "部分时段缺少数据或状态异常", "请查看下方色带定位具体时段"
	}
	return status, "", ""
}

// continuousStatusSummaryFor 生成 status_summary 对象。
func continuousStatusSummaryFor(status string) continuousStatusSummary {
	label, explanation, suggestion := continuousStatusSummaryText(status)
	return continuousStatusSummary{Status: status, Label: label, Explanation: explanation, Suggestion: suggestion}
}

// continuousSignalCoverageV2 计算单个信号的覆盖率、状态分类、coverage_bands
// 与 status_summary。merged 必须已按该信号过滤（filterMergedBySignal）；
// boundaries 为 Session 级启动/停止 grace（continuousSessionBoundariesFor）。
func continuousSignalCoverageV2(merged []model.ProfileWindow, from, to, finalizedTo time.Time,
	session model.ContinuousSession, boundaries continuousSessionBoundaries) continuousSignalCoverage {
	intervals := continuousStatusIntervalsFor(merged, from, to, finalizedTo, session, boundaries)
	mergedIntervals := mergeStatusIntervals(intervals, pqCoverageMergeTolerance)
	st := continuousCoverageStatsOf(mergedIntervals)

	totalSeconds := to.Sub(from).Seconds()
	// 覆盖率 = 有效数据 /（总时长 - 灰色 - 黄色）：黄色和灰色不降低
	// finalized coverage。
	denominator := totalSeconds - st.graySeconds - st.pendingSeconds
	ratio := 0.0
	if denominator > 0 {
		ratio = st.coveredSeconds / denominator
		if ratio > 1 {
			ratio = 1
		}
	}
	coverage := gin.H{
		"from": from, "to": to,
		"total_seconds":   totalSeconds,
		"covered_seconds": st.coveredSeconds,
		"gap_seconds":     st.redSeconds,
		"gray_seconds":    st.graySeconds,
		"pending_seconds": st.pendingSeconds,
		"ratio":           ratio,
	}

	// 详细缺口：最长的前 20 个红色区间（real_gap + collector_failed），
	// 其余用 gap_count_total 汇总。
	var gaps []continuousTimelineGap
	for _, iv := range mergedIntervals {
		if iv.status == continuousCoverageRealGap || iv.status == continuousCoverageCollectorFailed {
			gaps = append(gaps, continuousTimelineGap{
				Start: iv.start, End: iv.end,
				DurationSeconds: iv.end.Sub(iv.start).Seconds(),
				Type:            string(iv.status),
			})
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].DurationSeconds > gaps[j].DurationSeconds })
	gapCountTotal := len(gaps)
	if len(gaps) > continuousCoverageGapDetailLimit {
		gaps = gaps[:continuousCoverageGapDetailLimit]
	}

	bands, resolution := continuousCoverageBands(mergedIntervals, from, to)
	status := continuousOverallStatus(st)
	return continuousSignalCoverage{
		Coverage:       coverage,
		Gaps:           gaps,
		GapCountTotal:  gapCountTotal,
		Status:         status,
		CoverageBands:  bands,
		BandLimit:      continuousCoverageBandLimit,
		BandResolution: resolution,
		StatusSummary:  continuousStatusSummaryFor(status),
	}
}

// continuousCoverageBands 把状态区间聚合为最多 600 个可视化色带。
// 区间数不超过上限时直接返回（短范围保持细粒度）；超过时按时间桶聚合，
// 桶内状态取优先级最高者，覆盖/缺口秒数按桶内实际统计，不改变覆盖率数值。
func continuousCoverageBands(intervals []continuousStatusInterval, from, to time.Time) ([]continuousCoverageBand, float64) {
	if len(intervals) <= continuousCoverageBandLimit {
		bands := make([]continuousCoverageBand, 0, len(intervals))
		for _, iv := range intervals {
			bands = append(bands, continuousCoverageBand{
				Start:           iv.start,
				End:             iv.end,
				Status:          string(iv.status),
				SampleCount:     iv.sampleCount,
				DurationSeconds: iv.end.Sub(iv.start).Seconds(),
				CoveredSeconds:  secondsOfStatus(iv, continuousCoverageHealthy),
				GapSeconds:      secondsOfStatus(iv, continuousCoverageRealGap) + secondsOfStatus(iv, continuousCoverageCollectorFailed),
			})
		}
		return bands, 0
	}
	total := to.Sub(from).Seconds()
	resolution := niceBucketResolution(total / continuousCoverageBandLimit)
	grid := time.Duration(resolution * float64(time.Second))
	byBucket := map[int64]*continuousCoverageBand{}
	for _, iv := range intervals {
		cur := iv.start
		for cur.Before(iv.end) {
			bucketStart := cur.Truncate(grid)
			bucketEnd := bucketStart.Add(grid)
			if bucketEnd.After(to) {
				bucketEnd = to
			}
			segEnd := iv.end
			if segEnd.After(bucketEnd) {
				segEnd = bucketEnd
			}
			key := bucketStart.Unix()
			band := byBucket[key]
			if band == nil {
				band = &continuousCoverageBand{Start: bucketStart, End: bucketEnd, Status: string(iv.status)}
				byBucket[key] = band
			}
			if coverageStatusPriority(iv.status) > coverageStatusPriority(continuousCoverageStatus(band.Status)) {
				band.Status = string(iv.status)
			}
			band.SampleCount += iv.sampleCount
			band.DurationSeconds += segEnd.Sub(cur).Seconds()
			if iv.status == continuousCoverageHealthy {
				band.CoveredSeconds += segEnd.Sub(cur).Seconds()
			}
			if iv.status == continuousCoverageRealGap || iv.status == continuousCoverageCollectorFailed {
				band.GapSeconds += segEnd.Sub(cur).Seconds()
			}
			cur = segEnd
		}
	}
	keys := make([]int64, 0, len(byBucket))
	for k := range byBucket {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	bands := make([]continuousCoverageBand, 0, len(keys))
	for _, k := range keys {
		bands = append(bands, *byBucket[k])
	}
	return bands, resolution
}

// secondsOfStatus 区间内某状态的秒数（区间状态唯一时即区间时长）。
func secondsOfStatus(iv continuousStatusInterval, status continuousCoverageStatus) float64 {
	if iv.status == status {
		return iv.end.Sub(iv.start).Seconds()
	}
	return 0
}

// niceBucketResolution 把原始分辨率向上取整到"好看"的桶分辨率。
func niceBucketResolution(seconds float64) float64 {
	steps := []float64{1, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400}
	for _, step := range steps {
		if seconds <= step {
			return step
		}
	}
	return math.Ceil(seconds/86400) * 86400
}

// continuousCoverageBoundaryFor 计算启动/停止边界（Timeline 顶层 boundary）。
func continuousCoverageBoundaryFor(merged []model.ProfileWindow, session model.ContinuousSession,
	from, to time.Time, grace time.Duration) continuousCoverageBoundary {
	startedAt := session.StartedAt
	if startedAt.IsZero() {
		startedAt = session.CreatedAt
	}
	activeFrom := from
	if startedAt.After(activeFrom) {
		activeFrom = startedAt
	}
	activeTo := to
	if session.StoppedAt != nil && session.StoppedAt.Before(activeTo) {
		activeTo = *session.StoppedAt
	}
	b := continuousCoverageBoundary{ActiveFrom: activeFrom, ActiveTo: activeTo}
	boundaries := continuousSessionBoundariesFor(merged, session, from, to, grace)
	if boundaries.startupGrace != nil {
		b.StartupGrace = &continuousGraceInterval{
			Start: boundaries.startupGrace.start, End: boundaries.startupGrace.end,
			Seconds: boundaries.startupGrace.end.Sub(boundaries.startupGrace.start).Seconds(),
		}
	}
	if boundaries.shutdownGrace != nil {
		b.ShutdownGrace = &continuousGraceInterval{
			Start: boundaries.shutdownGrace.start, End: boundaries.shutdownGrace.end,
			Seconds: boundaries.shutdownGrace.end.Sub(boundaries.shutdownGrace.start).Seconds(),
		}
	}
	return b
}