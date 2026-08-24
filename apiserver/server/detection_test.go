// ============================================================
// server/detection_test.go — 检测→触发深度诊断 MVP 单测
// ============================================================
// 用硬编码的 sentinel_rules 行验证整条链路：
//   histogram trend 超过 FloorValue → 数据质量闸门通过 → 创建诊断任务
//   （MasterTaskTID 指向 rule.SID）→ 写一条 fired 的 DetectionEvent。
// 同时覆盖三个不触发的分支：未超阈值、冷却期内、采样覆盖率不足。
// 复用 continuous_block_test.go 里已有的 newTestAPIServer / blockSeedSession /
// blockSeedBatch 等 test helper，不重复造轮子。
// ============================================================

package server

import (
	"testing"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// detectionSeedSchedWindow 构造一个含 sched_latency 直方图的窗口，P99 可指定，
// 便于分别测试"超阈值"和"未超阈值"两种场景。
func detectionSeedSchedWindow(start, end time.Time, p99 float64) ContinuousWindowIngest {
	return ContinuousWindowIngest{
		WindowStart: start, WindowEnd: end, SignalType: "sched_latency",
		Histograms: []ContinuousHistogramIngest{{
			SignalType: "sched_latency", Backend: "bpftrace", Unit: "us", EventCount: 200,
			Buckets: []ContinuousHistogramBucket{
				{Range: "0-5000", Low: 0, High: 5000, Count: 150},
				{Range: "5000-50000", Low: 5000, High: 50000, Count: 50},
			},
			Summary: ContinuousHistogramSummary{Min: 100, Max: p99 * 1.1, P50: p99 / 8, P95: p99 * 0.9, P99: p99},
		}},
	}
}

// detectionSeedRule 插入一条哨兵规则：sched_latency p99 固定阈值。
func detectionSeedRule(t *testing.T, s *APIServer, sid, ip string, floorValue float64, cooldownSeconds int) model.SentinelRule {
	t.Helper()
	rule := model.SentinelRule{
		SID: sid, Name: "调度延迟哨兵-" + sid, TargetIP: ip,
		Signal: "sched_latency", Metric: "p99", FloorValue: floorValue,
		CooldownSeconds: cooldownSeconds, Enabled: true,
		UID: "detector-owner", UserName: "detector-owner",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.DB.Create(&rule).Error; err != nil {
		t.Fatalf("seed sentinel rule: %v", err)
	}
	return rule
}

// TestDetectionFiresWhenObservedExceedsFloor 覆盖主链路：超阈值 → 触发诊断任务，
// 且诊断任务的 MasterTaskTID 指向规则的 SID（GetTimeline 复用的关键前提）。
func TestDetectionFiresWhenObservedExceedsFloor(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	enableBlockCompactor(s) // 顺带把 StorageDisk 配成可用路径，让 canStartCollection 走真实通过路径

	ip := "10.0.0.201"
	sid := "cps-detect-fire"
	blockSeedSession(t, s, sid, ip)

	now := time.Now().UTC()
	windowStart, windowEnd := now.Add(-90*time.Second), now.Add(-30*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-fire", windowStart.Add(-10*time.Second), windowEnd.Add(10*time.Second),
		[]ContinuousWindowIngest{detectionSeedSchedWindow(windowStart, windowEnd, 41000)}) // P99=41ms，远超阈值

	rule := detectionSeedRule(t, s, "sr-detect-fire", ip, 5000 /* 5ms */, 900)

	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	if err := s.DB.Where("rule_sid = ?", rule.SID).Find(&events).Error; err != nil {
		t.Fatalf("load detection events: %v", err)
	}
	if len(events) != 1 || events[0].Status != "fired" {
		t.Fatalf("expected exactly one fired event, got %+v", events)
	}
	if events[0].ChildTID == "" {
		t.Fatalf("fired event should carry child_tid")
	}

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", events[0].ChildTID).First(&task).Error; err != nil {
		t.Fatalf("load triggered task: %v", err)
	}
	if task.MasterTaskTID != rule.SID {
		t.Fatalf("expected master_task_tid=%s, got %s", rule.SID, task.MasterTaskTID)
	}
	if task.TaskKind != TaskKindEBPFSched {
		t.Fatalf("expected task_kind=%s, got %s", TaskKindEBPFSched, task.TaskKind)
	}
	if len(task.TriggerContext) == 0 {
		t.Fatalf("triggered task should carry trigger_context")
	}

	var outboxCount int64
	s.DB.Model(&model.Outbox{}).Where("aggregate_id = ?", task.TID).Count(&outboxCount)
	if outboxCount != 1 {
		t.Fatalf("expected outbox dispatch entry for triggered task, got %d", outboxCount)
	}

	// 冷却期内的第二次判异不应重复触发。
	s.evaluateSentinelRule(rule)
	var eventsAfterSecondTick []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Order("id ASC").Find(&eventsAfterSecondTick)
	if len(eventsAfterSecondTick) != 2 || eventsAfterSecondTick[1].Status != "skipped_cooldown" {
		t.Fatalf("expected second tick to be skipped_cooldown, got %+v", eventsAfterSecondTick)
	}
}

// TestDetectionDoesNotFireBelowFloor 覆盖：观测值未超阈值时不触发、不写事件、不建任务。
func TestDetectionDoesNotFireBelowFloor(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	ip := "10.0.0.202"
	sid := "cps-detect-normal"
	blockSeedSession(t, s, sid, ip)

	now := time.Now().UTC()
	windowStart, windowEnd := now.Add(-90*time.Second), now.Add(-30*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-normal", windowStart.Add(-10*time.Second), windowEnd.Add(10*time.Second),
		[]ContinuousWindowIngest{detectionSeedSchedWindow(windowStart, windowEnd, 2000)}) // P99=2ms，低于阈值

	rule := detectionSeedRule(t, s, "sr-detect-normal", ip, 5000, 900)
	s.evaluateSentinelRule(rule)

	var count int64
	s.DB.Model(&model.DetectionEvent{}).Where("rule_sid = ?", rule.SID).Count(&count)
	if count != 0 {
		t.Fatalf("expected no detection event when observed value is within normal range, got %d", count)
	}
	var taskCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", rule.SID).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("expected no task created when observed value is within normal range, got %d", taskCount)
	}
}

// TestDetectionSkipsLowCoverage 覆盖数据质量闸门：判异窗口内采样覆盖率不足时，
// 即便观测值超阈值也不应触发，只记录 skipped_low_coverage。
func TestDetectionSkipsLowCoverage(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	ip := "10.0.0.203"
	sid := "cps-detect-gap"
	blockSeedSession(t, s, sid, ip)

	now := time.Now().UTC()
	// 只喂 10 秒的窗口数据，覆盖 detectionLookback（5 分钟）的比例远低于 90% 闸门。
	windowStart, windowEnd := now.Add(-20*time.Second), now.Add(-10*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-gap", windowStart, windowEnd,
		[]ContinuousWindowIngest{detectionSeedSchedWindow(windowStart, windowEnd, 41000)})

	rule := detectionSeedRule(t, s, "sr-detect-gap", ip, 5000, 900)
	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Find(&events)
	if len(events) != 1 || events[0].Status != "skipped_low_coverage" {
		t.Fatalf("expected skipped_low_coverage event, got %+v", events)
	}
	var taskCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", rule.SID).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("expected no task created when coverage is insufficient, got %d", taskCount)
	}
}
