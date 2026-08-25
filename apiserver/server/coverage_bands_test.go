// ============================================================
// server/coverage_bands_test.go — 阶段九：按信号覆盖率、状态可信度
// 与长时间范围聚合单测
// ============================================================
// 覆盖计划要求：
//   1. 多信号独立覆盖：CPU 有数据、IO 无数据，CPU 覆盖率不能替 IO；
//   2. 零样本窗口：sample_count=0 不计绿色；target_idle/no_events 灰色；
//      failed/unavailable 红色；
//   3. 启动/停止边界：grace 内不生成真实缺口；grace 外生成；stopped
//      Session 不产生 pending tail；
//   4. 上传等待：pending 区间不降低 finalized coverage；status_summary
//      返回"数据整理中"；
//   5. 长范围聚合：最多 600 个 band；聚合前后覆盖/缺口秒数与比例一致；
//      状态优先级正确；
//   6. 历史兼容：无 signal_status 的旧窗口按样本数推导。
// ============================================================

package server

import (
	"testing"
	"time"

	"github.com/mini-drop/apiserver/model"
)

func coverageTestSession(startedAt time.Time) model.ContinuousSession {
	return model.ContinuousSession{
		SID: "cps-cov", StartedAt: startedAt,
		UploadBatchSec: 60, AggregationWindowSec: 10,
	}
}

func coverageTestBoundaries(merged []model.ProfileWindow, session model.ContinuousSession, from, to time.Time) continuousSessionBoundaries {
	return continuousSessionBoundariesFor(merged, session, from, to, continuousTimelineGrace(session))
}

// 1. 多信号独立覆盖：CPU 有数据、IO 无数据时，CPU 覆盖率不能替 IO 覆盖率。
func TestContinuousSignalCoverageIsIndependentPerSignal(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	merged := []model.ProfileWindow{
		{SignalType: "cpu_profile", WindowStart: base.Add(10 * time.Second), WindowEnd: base.Add(20 * time.Second), SampleCount: 100},
		{SignalType: "cpu_profile", WindowStart: base.Add(20 * time.Second), WindowEnd: base.Add(30 * time.Second), SampleCount: 100},
	}
	// to = finalizedTo = 30s：无 pending tail，避免正常尾部等待掩盖状态。
	from, to := base, base.Add(30*time.Second)
	finalizedTo := base.Add(30 * time.Second)
	boundaries := coverageTestBoundaries(merged, session, from, to)

	cpu := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if cpu.Coverage["ratio"].(float64) != 1.0 {
		t.Fatalf("cpu ratio=%v, want 1.0（20s 数据 / 20s 最终化域）", cpu.Coverage["ratio"])
	}
	if cpu.Status != "healthy" {
		t.Fatalf("cpu status=%q, want healthy", cpu.Status)
	}

	io := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("io_latency")),
		from, to, finalizedTo, session, boundaries)
	if io.Coverage["ratio"].(float64) != 0 {
		t.Fatalf("io ratio=%v, want 0（无数据不能被 CPU 填绿）", io.Coverage["ratio"])
	}
	if io.Status != "real_gap" {
		t.Fatalf("io status=%q, want real_gap", io.Status)
	}
	if io.GapCountTotal != 1 {
		t.Fatalf("io gap_count_total=%d, want 1", io.GapCountTotal)
	}
	// IO 的真实缺口应为 [10s, 30s]（启动 grace 到最终化边界）。
	if len(io.Gaps) != 1 || io.Gaps[0].Start != base.Add(10*time.Second) || io.Gaps[0].End != base.Add(30*time.Second) {
		t.Fatalf("io gaps=%+v, want [10s,30s]", io.Gaps)
	}
}

// 2a. 零样本窗口不计绿色：sample_count=0 且状态 unknown 的窗口是灰色。
func TestContinuousZeroSampleWindowIsNotCovered(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	merged := []model.ProfileWindow{
		{SignalType: "cpu_profile", WindowStart: base.Add(10 * time.Second), WindowEnd: base.Add(20 * time.Second), SampleCount: 0, SignalStatus: "unknown"},
		{SignalType: "cpu_profile", WindowStart: base.Add(20 * time.Second), WindowEnd: base.Add(30 * time.Second), SampleCount: 0, SignalStatus: "unknown"},
	}
	from, to := base, base.Add(30*time.Second)
	finalizedTo := base.Add(30 * time.Second)
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if sc.Coverage["covered_seconds"].(float64) != 0 {
		t.Fatalf("covered_seconds=%v, want 0（零样本窗口不计绿色）", sc.Coverage["covered_seconds"])
	}
	// 启动 grace [0,10s] + 数据 [10s,30s] 均为灰色（unknown 零样本）。
	if sc.Coverage["gray_seconds"].(float64) != 30 {
		t.Fatalf("gray_seconds=%v, want 30（启动 grace 10s + unknown 零样本 20s）", sc.Coverage["gray_seconds"])
	}
	if sc.Status != "target_idle" {
		t.Fatalf("status=%q, want target_idle（灰色不产生真实缺口）", sc.Status)
	}
}

// 2b. target_idle / no_events 进入灰色状态，不产生真实缺口。
func TestContinuousTargetIdleAndNoEventsAreGray(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	merged := []model.ProfileWindow{
		{SignalType: "cpu_profile", WindowStart: base.Add(10 * time.Second), WindowEnd: base.Add(20 * time.Second), SampleCount: 0, SignalStatus: "target_idle"},
		{SignalType: "cpu_profile", WindowStart: base.Add(20 * time.Second), WindowEnd: base.Add(30 * time.Second), SampleCount: 0, SignalStatus: "no_events"},
	}
	from, to := base, base.Add(30*time.Second)
	finalizedTo := base.Add(30 * time.Second)
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if sc.Coverage["gap_seconds"].(float64) != 0 {
		t.Fatalf("gap_seconds=%v, want 0（target_idle/no_events 不产生真实缺口）", sc.Coverage["gap_seconds"])
	}
	if sc.Coverage["gray_seconds"].(float64) != 30 {
		t.Fatalf("gray_seconds=%v, want 30（启动 grace 10s + 空闲 20s）", sc.Coverage["gray_seconds"])
	}
}

// 2c. failed / unavailable 进入红色状态（collector_failed），计入缺口。
func TestContinuousFailedAndUnavailableAreRed(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	merged := []model.ProfileWindow{
		{SignalType: "cpu_profile", WindowStart: base.Add(10 * time.Second), WindowEnd: base.Add(20 * time.Second), SampleCount: 0, SignalStatus: "failed"},
		{SignalType: "cpu_profile", WindowStart: base.Add(20 * time.Second), WindowEnd: base.Add(30 * time.Second), SampleCount: 0, SignalStatus: "unavailable"},
	}
	from, to := base, base.Add(60*time.Second)
	finalizedTo := base.Add(30 * time.Second)
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if sc.Coverage["gap_seconds"].(float64) != 20 {
		t.Fatalf("gap_seconds=%v, want 20（failed/unavailable 为红色缺口）", sc.Coverage["gap_seconds"])
	}
	if sc.Status != "collector_failed" {
		t.Fatalf("status=%q, want collector_failed", sc.Status)
	}
	if sc.StatusSummary.Label != "采集异常" {
		t.Fatalf("status_summary.label=%q, want 采集异常", sc.StatusSummary.Label)
	}
}

// 3a. 启动 grace 内不生成真实缺口；grace 外生成真实缺口。
func TestContinuousStartupGraceBoundary(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	grace := continuousTimelineGrace(session) // 60 + 2*10 + 15 = 95s
	// 无任何数据：启动 grace 覆盖 [0, 95s]，之后到最终化边界是真实缺口。
	merged := []model.ProfileWindow{}
	from, to := base, base.Add(300*time.Second)
	finalizedTo := base.Add(300 * time.Second)
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if sc.Coverage["gray_seconds"].(float64) != grace.Seconds() {
		t.Fatalf("gray_seconds=%v, want %v（启动 grace 灰色）", sc.Coverage["gray_seconds"], grace.Seconds())
	}
	if sc.Coverage["gap_seconds"].(float64) != (300-grace.Seconds()) {
		t.Fatalf("gap_seconds=%v, want %v（grace 外真实缺口）", sc.Coverage["gap_seconds"], 300-grace.Seconds())
	}
	if sc.Status != "real_gap" {
		t.Fatalf("status=%q, want real_gap", sc.Status)
	}
	// boundary 应暴露启动 grace 区间。
	boundary := continuousCoverageBoundaryFor(merged, session, from, to, grace)
	if boundary.StartupGrace == nil || boundary.StartupGrace.Seconds != grace.Seconds() {
		t.Fatalf("boundary.startup_grace=%+v, want %v 秒", boundary.StartupGrace, grace.Seconds())
	}
}

// 3b. 停止收尾 grace：最后有效数据到 stopped_at 之间灰色，不产生缺口；
// stopped Session 不产生 pending tail。
func TestContinuousShutdownGraceAndNoPendingTail(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	stoppedAt := base.Add(120 * time.Second)
	session := coverageTestSession(base)
	session.StoppedAt = &stoppedAt
	merged := []model.ProfileWindow{
		{SignalType: "cpu_profile", WindowStart: base.Add(10 * time.Second), WindowEnd: base.Add(20 * time.Second), SampleCount: 100},
		{SignalType: "cpu_profile", WindowStart: base.Add(20 * time.Second), WindowEnd: base.Add(30 * time.Second), SampleCount: 100},
	}
	from, to := base, stoppedAt
	finalizedTo := stoppedAt
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if sc.Coverage["pending_seconds"].(float64) != 0 {
		t.Fatalf("pending_seconds=%v, want 0（stopped Session 无 pending tail）", sc.Coverage["pending_seconds"])
	}
	// 停止收尾 grace = [30s, 120s] 灰色 + 启动 grace [0,10s]，均不产生真实缺口。
	if sc.Coverage["gray_seconds"].(float64) != 100 {
		t.Fatalf("gray_seconds=%v, want 100（启动 grace 10s + 停止收尾 90s）", sc.Coverage["gray_seconds"])
	}
	if sc.Coverage["gap_seconds"].(float64) != 0 {
		t.Fatalf("gap_seconds=%v, want 0", sc.Coverage["gap_seconds"])
	}
	boundary := continuousCoverageBoundaryFor(merged, session, from, to, continuousTimelineGrace(session))
	if boundary.ShutdownGrace == nil || boundary.ShutdownGrace.Seconds != 90 {
		t.Fatalf("boundary.shutdown_grace=%+v, want 90 秒", boundary.ShutdownGrace)
	}
}

// 4. 上传等待：pending 区间不降低 finalized coverage；status_summary
// 返回"数据整理中"。
func TestContinuousPendingUploadDoesNotLowerCoverage(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	merged := []model.ProfileWindow{
		{SignalType: "cpu_profile", WindowStart: base.Add(10 * time.Second), WindowEnd: base.Add(20 * time.Second), SampleCount: 100},
		{SignalType: "cpu_profile", WindowStart: base.Add(20 * time.Second), WindowEnd: base.Add(30 * time.Second), SampleCount: 100},
	}
	from, to := base, base.Add(300*time.Second)
	finalizedTo := base.Add(30 * time.Second) // 数据只到 30s，之后 270s 是 pending
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if sc.Coverage["pending_seconds"].(float64) != 270 {
		t.Fatalf("pending_seconds=%v, want 270", sc.Coverage["pending_seconds"])
	}
	if sc.Coverage["ratio"].(float64) != 1.0 {
		t.Fatalf("ratio=%v, want 1.0（pending 不降低 finalized coverage）", sc.Coverage["ratio"])
	}
	if sc.Status != "pending_upload" {
		t.Fatalf("status=%q, want pending_upload", sc.Status)
	}
	if sc.StatusSummary.Label != "数据整理中" {
		t.Fatalf("status_summary.label=%q, want 数据整理中", sc.StatusSummary.Label)
	}
	// 色带应包含 pending_upload 状态。
	foundPending := false
	for _, band := range sc.CoverageBands {
		if band.Status == "pending_upload" {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatalf("coverage_bands 缺少 pending_upload 色带: %+v", sc.CoverageBands)
	}
}

// 5a. 长范围聚合：任意查询最多返回 600 个 band。
func TestContinuousCoverageBandsRespectLimit(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	// 24 小时范围，每 10 秒一个窗口（8640 个窗口），中间随机挖空制造缺口。
	merged := make([]model.ProfileWindow, 0, 8640)
	for i := 0; i < 8640; i++ {
		start := base.Add(time.Duration(i) * 10 * time.Second)
		if i%7 == 0 {
			continue // 挖空制造缺口
		}
		merged = append(merged, model.ProfileWindow{
			SignalType: "cpu_profile", WindowStart: start, WindowEnd: start.Add(10 * time.Second), SampleCount: 100,
		})
	}
	from, to := base, base.Add(24*time.Hour)
	finalizedTo := to
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if len(sc.CoverageBands) > continuousCoverageBandLimit {
		t.Fatalf("bands=%d, want <= %d", len(sc.CoverageBands), continuousCoverageBandLimit)
	}
	if sc.BandLimit != continuousCoverageBandLimit {
		t.Fatalf("band_limit=%d, want %d", sc.BandLimit, continuousCoverageBandLimit)
	}
	if sc.BandResolution <= 0 {
		t.Fatalf("band_resolution_seconds=%v, want > 0（长范围应提升桶分辨率）", sc.BandResolution)
	}
}

// 5b. 聚合前后覆盖秒数、缺口秒数和比例一致。
func TestContinuousCoverageBandsPreserveStats(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	// 24 小时范围，每 10 秒一个窗口，中间 (12h, 13h) 挖空（359 个窗口）。
	merged := make([]model.ProfileWindow, 0, 8640)
	for i := 0; i < 8640; i++ {
		start := base.Add(time.Duration(i) * 10 * time.Second)
		if start.After(base.Add(12*time.Hour)) && start.Before(base.Add(13*time.Hour)) {
			continue
		}
		merged = append(merged, model.ProfileWindow{
			SignalType: "cpu_profile", WindowStart: start, WindowEnd: start.Add(10 * time.Second), SampleCount: 100,
		})
	}
	from, to := base, base.Add(24*time.Hour)
	finalizedTo := to
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	// 精确统计：挖空窗口从 43210s 到 46790s，缺口 [43210s, 46800s] = 3590s。
	if sc.Coverage["covered_seconds"].(float64) != 86400-3590 {
		t.Fatalf("covered_seconds=%v, want %v", sc.Coverage["covered_seconds"], 86400-3590)
	}
	if sc.Coverage["gap_seconds"].(float64) != 3590 {
		t.Fatalf("gap_seconds=%v, want 3590", sc.Coverage["gap_seconds"])
	}
	// 色带统计与精确统计一致。
	var bandCovered, bandGap float64
	for _, band := range sc.CoverageBands {
		bandCovered += band.CoveredSeconds
		bandGap += band.GapSeconds
	}
	if bandCovered != sc.Coverage["covered_seconds"].(float64) || bandGap != sc.Coverage["gap_seconds"].(float64) {
		t.Fatalf("band stats covered=%v gap=%v, want %v/%v", bandCovered, bandGap,
			sc.Coverage["covered_seconds"], sc.Coverage["gap_seconds"])
	}
}

// 5c. 状态优先级：真实缺口 > 采集失败 > 空闲 > 等待上传 > 有数据。
func TestContinuousCoverageBandsStatusPriority(t *testing.T) {
	if coverageStatusPriority(continuousCoverageRealGap) <= coverageStatusPriority(continuousCoverageCollectorFailed) ||
		coverageStatusPriority(continuousCoverageCollectorFailed) <= coverageStatusPriority(continuousCoverageTargetIdle) ||
		coverageStatusPriority(continuousCoverageTargetIdle) <= coverageStatusPriority(continuousCoveragePendingUpload) ||
		coverageStatusPriority(continuousCoveragePendingUpload) <= coverageStatusPriority(continuousCoverageHealthy) {
		t.Fatalf("status priority order violated: real_gap=%d failed=%d idle=%d pending=%d healthy=%d",
			coverageStatusPriority(continuousCoverageRealGap), coverageStatusPriority(continuousCoverageCollectorFailed),
			coverageStatusPriority(continuousCoverageTargetIdle), coverageStatusPriority(continuousCoveragePendingUpload),
			coverageStatusPriority(continuousCoverageHealthy))
	}
	// 灰色类同级。
	if coverageStatusPriority(continuousCoverageStartupGrace) != coverageStatusPriority(continuousCoverageTargetIdle) ||
		coverageStatusPriority(continuousCoverageShutdownGrace) != coverageStatusPriority(continuousCoverageTargetIdle) ||
		coverageStatusPriority(continuousCoverageUnknown) != coverageStatusPriority(continuousCoverageTargetIdle) {
		t.Fatalf("gray statuses must share priority")
	}
}

// 6. 历史兼容：无 signal_status 的旧窗口按样本数推导（>0 覆盖，=0 未知）。
func TestContinuousLegacyWindowDerivesStatusFromSamples(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	session := coverageTestSession(base)
	merged := []model.ProfileWindow{
		{SignalType: "cpu_profile", WindowStart: base.Add(10 * time.Second), WindowEnd: base.Add(20 * time.Second), SampleCount: 100}, // 旧行无 signal_status
		{SignalType: "cpu_profile", WindowStart: base.Add(20 * time.Second), WindowEnd: base.Add(30 * time.Second), SampleCount: 0},   // 旧行零样本
	}
	from, to := base, base.Add(60*time.Second)
	finalizedTo := base.Add(30 * time.Second)
	boundaries := coverageTestBoundaries(merged, session, from, to)
	sc := continuousSignalCoverageV2(filterMergedBySignal(merged, continuousSignalTypesForTimeline("cpu_profile")),
		from, to, finalizedTo, session, boundaries)
	if sc.Coverage["covered_seconds"].(float64) != 10 {
		t.Fatalf("covered_seconds=%v, want 10（旧行按样本数推导）", sc.Coverage["covered_seconds"])
	}
	// 启动 grace [0,10s] + 旧零样本行 [20s,30s] 均为灰色。
	if sc.Coverage["gray_seconds"].(float64) != 20 {
		t.Fatalf("gray_seconds=%v, want 20（启动 grace 10s + 旧零样本 10s）", sc.Coverage["gray_seconds"])
	}
}

// 6b. 历史压缩 segment 无 signal_status，按样本数推导。
func TestContinuousSegmentDerivesStatusFromSamples(t *testing.T) {
	if got := continuousSegmentCoverageStatus(model.ContinuousCoverageSegment{SampleCount: 5}); got != continuousCoverageHealthy {
		t.Fatalf("segment with samples status=%q, want healthy", got)
	}
	if got := continuousSegmentCoverageStatus(model.ContinuousCoverageSegment{SampleCount: 0}); got != continuousCoverageUnknown {
		t.Fatalf("segment without samples status=%q, want unknown", got)
	}
}

// 6c. 信号展开：v2 canonical 名复用 pqV1SignalTypesFor，v1 精确类型原样。
func TestContinuousSignalTypesForTimeline(t *testing.T) {
	if got := continuousSignalTypesForTimeline("io_latency"); len(got) != 1 || got[0] != "io_latency" {
		t.Fatalf("io_latency types=%v, want [io_latency]（精确类型不被同族填绿）", got)
	}
	if got := continuousSignalTypesForTimeline("histogram"); len(got) != 3 {
		t.Fatalf("histogram types=%v, want 3 个 v1 类型", got)
	}
	if got := continuousSignalTypesForTimeline("cpu_profile"); len(got) != 1 || got[0] != "cpu_profile" {
		t.Fatalf("cpu_profile types=%v, want [cpu_profile]", got)
	}
}

// 6d. 旧 coverage/gaps 字段仍存在：legacy 计算路径不受影响。
func TestContinuousLegacyCoverageFieldsStillPresent(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	windows := []model.ProfileWindow{
		{WindowStart: base, WindowEnd: base.Add(10 * time.Second), SampleCount: 100},
		{WindowStart: base.Add(15 * time.Second), WindowEnd: base.Add(25 * time.Second), SampleCount: 100},
	}
	gaps, coverage := continuousTimelineCoverage(windows, base, base.Add(40*time.Second), base.Add(25*time.Second), 5*time.Second)
	if len(gaps) != 0 {
		t.Fatalf("legacy gaps=%+v, want 0（pending tail 不计缺口）", gaps)
	}
	if _, ok := coverage["ratio"]; !ok {
		t.Fatalf("legacy coverage missing ratio: %+v", coverage)
	}
	if _, ok := coverage["covered_seconds"]; !ok {
		t.Fatalf("legacy coverage missing covered_seconds: %+v", coverage)
	}
}