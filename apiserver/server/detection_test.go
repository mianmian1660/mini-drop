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
	"context"
	"encoding/json"
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
	// 覆盖 detectionLookback（5 分钟）的 90% 以上，否则会被数据质量闸门 skipped_low_coverage。
	windowStart, windowEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-fire", windowStart, windowEnd,
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
	// 覆盖率需 ≥90%（5 分钟 lookback），否则会先走 skipped_low_coverage 分支而非阈值判断。
	windowStart, windowEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-normal", windowStart, windowEnd,
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

// detectionSeedRuleWithPersistence 同 detectionSeedRule，额外指定持续性判断参数
// （见 docs/detection-trigger-pipeline-design.md §10.3）。
func detectionSeedRuleWithPersistence(t *testing.T, s *APIServer, sid, ip string, floorValue float64, cooldownSeconds, persistenceWindows, persistenceMinHits int) model.SentinelRule {
	t.Helper()
	rule := model.SentinelRule{
		SID: sid, Name: "调度延迟哨兵-" + sid, TargetIP: ip,
		Signal: "sched_latency", Metric: "p99", FloorValue: floorValue,
		CooldownSeconds: cooldownSeconds, Enabled: true,
		PersistenceWindows: persistenceWindows, PersistenceMinHits: persistenceMinHits,
		UID: "detector-owner", UserName: "detector-owner",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.DB.Create(&rule).Error; err != nil {
		t.Fatalf("seed sentinel rule: %v", err)
	}
	return rule
}

// TestDetectionSkipsLowPersistence 覆盖 §10.3：最新窗口超阈值，但要求连续 2 个窗口
// 都超阈值（PersistenceMinHits=2）时，只有 1 个窗口命中的单点抖动不应触发。
func TestDetectionSkipsLowPersistence(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	ip := "10.0.0.204"
	sid := "cps-detect-spike"
	blockSeedSession(t, s, sid, ip)

	now := time.Now().UTC()
	// 两个相邻窗口合计覆盖 280s/300s（>90%）：第一个窗口正常，第二个（最新）单点冲高。
	w1Start, w1End := now.Add(-290*time.Second), now.Add(-150*time.Second)
	w2Start, w2End := now.Add(-150*time.Second), now.Add(-10*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-spike", w1Start, w2End, []ContinuousWindowIngest{
		detectionSeedSchedWindow(w1Start, w1End, 2000),  // 正常
		detectionSeedSchedWindow(w2Start, w2End, 41000), // 单点冲高
	})

	rule := detectionSeedRuleWithPersistence(t, s, "sr-detect-spike", ip, 5000, 900, 2, 2)
	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Find(&events)
	if len(events) != 1 || events[0].Status != "skipped_low_persistence" {
		t.Fatalf("expected skipped_low_persistence event, got %+v", events)
	}
	var taskCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", rule.SID).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("expected no task created for an isolated spike, got %d", taskCount)
	}
}

// detectionSeedBaseline 直接预置滚动基线（跳过多轮 evaluateSentinelRule 累积过程本身——
// 那是 detectionUpdateBaseline 的实现细节，不是这两个测试要验证的对象），只验证"已有
// 基线 + 新观测值超阈值"时，§10.2 的 KFactor/score 判断是否按预期工作。
func detectionSeedBaseline(t *testing.T, s *APIServer, ruleSID string, values []float64) {
	t.Helper()
	payload, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	if err := s.DB.Create(&model.DetectionState{RuleSID: ruleSID, RecentValues: payload, UpdatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed detection state: %v", err)
	}
}

// TestDetectionSkipsLowDeviationFromBaseline 覆盖 §10.2 此前完全没有测试覆盖的分支：
// 观测值超过静态下限（FloorValue），但没有显著偏离滚动基线时，应判定为正常波动而不是
// 异常。基线 median=4000/MAD=500，观测值 5200 的 score=|5200-4000|/(1.4826*500)≈1.62，
// 远低于默认 KFactor=5。
func TestDetectionSkipsLowDeviationFromBaseline(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	ip := "10.0.0.206"
	sid := "cps-detect-baseline-low"
	blockSeedSession(t, s, sid, ip)

	rule := detectionSeedRule(t, s, "sr-detect-baseline-low", ip, 5000, 900)
	detectionSeedBaseline(t, s, rule.SID, []float64{3000, 4000, 5000, 3500, 4500})

	now := time.Now().UTC()
	windowStart, windowEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-baseline-low", windowStart, windowEnd,
		[]ContinuousWindowIngest{detectionSeedSchedWindow(windowStart, windowEnd, 5200)})

	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Find(&events)
	if len(events) != 1 || events[0].Status != "skipped_low_deviation" {
		t.Fatalf("expected skipped_low_deviation event, got %+v", events)
	}
	var taskCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", rule.SID).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("expected no task created for a value within normal baseline deviation, got %d", taskCount)
	}
}

// TestDetectionFiresWhenDeviatesFromBaseline 是上一个测试的镜像：同样的基线
// （median=4000/MAD=500），但观测值 41000 远远偏离基线（score≈49.9 >> KFactor=5），
// 应该正常触发——确保 §10.2 的判断不是单向只会跳过，两条分支都要经过验证。
func TestDetectionFiresWhenDeviatesFromBaseline(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	enableBlockCompactor(s)

	ip := "10.0.0.207"
	sid := "cps-detect-baseline-high"
	blockSeedSession(t, s, sid, ip)

	rule := detectionSeedRule(t, s, "sr-detect-baseline-high", ip, 5000, 900)
	detectionSeedBaseline(t, s, rule.SID, []float64{3000, 4000, 5000, 3500, 4500})

	now := time.Now().UTC()
	windowStart, windowEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-baseline-high", windowStart, windowEnd,
		[]ContinuousWindowIngest{detectionSeedSchedWindow(windowStart, windowEnd, 41000)})

	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Find(&events)
	if len(events) != 1 || events[0].Status != "fired" {
		t.Fatalf("expected fired event for a value far outside baseline deviation, got %+v", events)
	}
}

// TestDetectionFiresWhenPersistent 覆盖 §10.3 的另一面：连续 2 个窗口都超阈值时，
// 即便 PersistenceMinHits=2，也应该正常触发。
func TestDetectionFiresWhenPersistent(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()
	enableBlockCompactor(s)

	ip := "10.0.0.205"
	sid := "cps-detect-sustained"
	blockSeedSession(t, s, sid, ip)

	now := time.Now().UTC()
	w1Start, w1End := now.Add(-290*time.Second), now.Add(-150*time.Second)
	w2Start, w2End := now.Add(-150*time.Second), now.Add(-10*time.Second)
	blockSeedBatch(t, s, sid, ip, "cpb-detect-sustained", w1Start, w2End, []ContinuousWindowIngest{
		detectionSeedSchedWindow(w1Start, w1End, 38000), // 持续偏高
		detectionSeedSchedWindow(w2Start, w2End, 41000), // 仍然偏高
	})

	rule := detectionSeedRuleWithPersistence(t, s, "sr-detect-sustained", ip, 5000, 900, 2, 2)
	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Find(&events)
	if len(events) != 1 || events[0].Status != "fired" {
		t.Fatalf("expected fired event for a sustained anomaly, got %+v", events)
	}
}

// seedDBSnapshotWindowOnly 只追加一个 ProfileWindow + storage batch，不重新创建 Session——
// seedDBSnapshotBatch（continuous_db_snapshot_test.go）每次调用都会新建一条
// ContinuousSession，同一个 sid 调两次会撞主键，db_snapshot 环比测试需要在同一个 session
// 下喂两个不同时间段的窗口（"上一个等长窗口" vs "当前窗口"），所以拆出这个更细粒度的版本。
func seedDBSnapshotWindowOnly(t *testing.T, s *APIServer, sid, objectKey string, window ContinuousWindowIngest) {
	t.Helper()
	if err := s.DB.Create(&model.ProfileWindow{
		SessionSID: sid, WindowStart: window.WindowStart, WindowEnd: window.WindowEnd,
		ObjectKey: objectKey, SignalType: "db_snapshot",
	}).Error; err != nil {
		t.Fatalf("create window: %v", err)
	}
	body, err := json.Marshal(continuousStoredBatch{SessionSID: sid, Windows: []ContinuousWindowIngest{window}})
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	mem := s.Storage.(*continuousMemoryStorage)
	mem.objects[objectKey] = string(body)
}

// TestDetectionDBSnapshotLockWaitFires 覆盖 §10.1 的 lock_wait 分支：锁等待秒数超过
// FloorValue 时应命中，写 fired_no_action（不建诊断任务，见 evaluateDBSnapshotRule 注释：
// script_diagnostic 的 Runner 还没接入，建出来的任务永远跑不完）。
func TestDetectionDBSnapshotLockWaitFires(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	now := time.Now().UTC().Truncate(time.Millisecond)
	wStart, wEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	seedDBSnapshotBatch(t, s, "db-detect-lock", "continuous/db-detect-lock/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: wStart, WindowEnd: wEnd, DBSnapshots: []ContinuousDBSnapshotIngest{
			{Kind: "lock_wait", InstanceLabel: "mysql-a", Timestamp: now,
				WaitingPID: 1, BlockingPID: 2, WaitSeconds: 8, LockedTable: "db.t"},
		}},
	})

	rule := model.SentinelRule{
		SID: "sr-detect-lock", Name: "锁等待哨兵", TargetIP: "10.0.0.9",
		Signal: "db_snapshot", Metric: "lock_wait", FloorValue: 2, CooldownSeconds: 900, Enabled: true,
		UID: "detector-owner", UserName: "detector-owner", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&rule).Error; err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Find(&events)
	if len(events) != 1 || events[0].Status != "fired_no_action" {
		t.Fatalf("expected fired_no_action event, got %+v", events)
	}
	if events[0].ObservedValue != 8 {
		t.Fatalf("expected observed_value=8 (wait_seconds), got %v", events[0].ObservedValue)
	}
	var taskCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", rule.SID).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("db_snapshot hit should never create a task (script_diagnostic Runner not wired up), got %d", taskCount)
	}
}

// TestDetectionDBSnapshotLockWaitBelowFloorDoesNotFire 覆盖负向分支：锁等待秒数没超过
// FloorValue 时不应触发、不应记录事件。
func TestDetectionDBSnapshotLockWaitBelowFloorDoesNotFire(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	now := time.Now().UTC().Truncate(time.Millisecond)
	wStart, wEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	seedDBSnapshotBatch(t, s, "db-detect-lock-ok", "continuous/db-detect-lock-ok/cpb-1.json", []ContinuousWindowIngest{
		{WindowStart: wStart, WindowEnd: wEnd, DBSnapshots: []ContinuousDBSnapshotIngest{
			{Kind: "lock_wait", InstanceLabel: "mysql-a", Timestamp: now,
				WaitingPID: 1, BlockingPID: 2, WaitSeconds: 1, LockedTable: "db.t"},
		}},
	})

	rule := model.SentinelRule{
		SID: "sr-detect-lock-ok", Name: "锁等待哨兵", TargetIP: "10.0.0.9",
		Signal: "db_snapshot", Metric: "lock_wait", FloorValue: 2, CooldownSeconds: 900, Enabled: true,
		UID: "detector-owner", UserName: "detector-owner", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&rule).Error; err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	s.evaluateSentinelRule(rule)

	var count int64
	s.DB.Model(&model.DetectionEvent{}).Where("rule_sid = ?", rule.SID).Count(&count)
	if count != 0 {
		t.Fatalf("expected no event when wait_seconds is within floor, got %d", count)
	}
}

// TestDetectionDBSnapshotDigestSpikeFires 覆盖 §10.1 的 digest 分支：同一条 digest 的
// total_latency_us 相比上一个等长窗口暴涨超过 KFactor 倍时应命中。基线窗口
// total_latency_us=100_000（100ms/10次调用），当前窗口=600_000（600ms），
// 环比=6倍 > 默认 KFactor=5，应该触发。
func TestDetectionDBSnapshotDigestSpikeFires(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	now := time.Now().UTC().Truncate(time.Millisecond)
	sid := "db-detect-digest"
	if err := s.DB.Create(&model.ContinuousSession{
		SID: sid, TargetIP: "10.0.0.9", UID: "owner",
		Status: model.ContinuousSessionStatusRunning, StartedAt: now.Add(-time.Hour), UpdatedAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}

	digestText := "SELECT * FROM orders WHERE id = ?"
	// 上一个等长窗口（detectionLookback 之前的 5 分钟内）：耗时正常。
	prevStart, prevEnd := now.Add(-9*time.Minute), now.Add(-6*time.Minute)
	seedDBSnapshotWindowOnly(t, s, sid, "continuous/db-detect-digest/prev.json", ContinuousWindowIngest{
		WindowStart: prevStart, WindowEnd: prevEnd,
		DBSnapshots: []ContinuousDBSnapshotIngest{digestSnap("mysql-a", "mydb", digestText, 10, 100_000, 100)},
	})
	// 当前窗口（覆盖 detectionLookback 90% 以上，否则会被 skipped_low_coverage 挡住）：暴涨到 6 倍。
	currStart, currEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	seedDBSnapshotWindowOnly(t, s, sid, "continuous/db-detect-digest/curr.json", ContinuousWindowIngest{
		WindowStart: currStart, WindowEnd: currEnd,
		DBSnapshots: []ContinuousDBSnapshotIngest{digestSnap("mysql-a", "mydb", digestText, 10, 600_000, 100)},
	})

	rule := model.SentinelRule{
		SID: "sr-detect-digest", Name: "慢查询哨兵", TargetIP: "10.0.0.9",
		Signal: "db_snapshot", Metric: "digest", FloorValue: 50_000, KFactor: 5,
		CooldownSeconds: 900, Enabled: true,
		UID: "detector-owner", UserName: "detector-owner", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&rule).Error; err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	s.evaluateSentinelRule(rule)

	var events []model.DetectionEvent
	s.DB.Where("rule_sid = ?", rule.SID).Find(&events)
	if len(events) != 1 || events[0].Status != "fired_no_action" {
		t.Fatalf("expected fired_no_action event for a 6x digest spike, got %+v", events)
	}
	if events[0].ObservedValue != 600_000 {
		t.Fatalf("expected observed_value=600000 (current total_latency_us), got %v", events[0].ObservedValue)
	}
}

// TestDetectionDBSnapshotDigestMildIncreaseDoesNotFire 覆盖负向分支：同一条 digest 耗时
// 有上升但没到 KFactor 倍（2倍 < 默认 5倍），不应触发。
func TestDetectionDBSnapshotDigestMildIncreaseDoesNotFire(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	now := time.Now().UTC().Truncate(time.Millisecond)
	sid := "db-detect-digest-mild"
	if err := s.DB.Create(&model.ContinuousSession{
		SID: sid, TargetIP: "10.0.0.9", UID: "owner",
		Status: model.ContinuousSessionStatusRunning, StartedAt: now.Add(-time.Hour), UpdatedAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}

	digestText := "SELECT * FROM orders WHERE id = ?"
	prevStart, prevEnd := now.Add(-9*time.Minute), now.Add(-6*time.Minute)
	seedDBSnapshotWindowOnly(t, s, sid, "continuous/db-detect-digest-mild/prev.json", ContinuousWindowIngest{
		WindowStart: prevStart, WindowEnd: prevEnd,
		DBSnapshots: []ContinuousDBSnapshotIngest{digestSnap("mysql-a", "mydb", digestText, 10, 100_000, 100)},
	})
	currStart, currEnd := now.Add(-290*time.Second), now.Add(-10*time.Second)
	seedDBSnapshotWindowOnly(t, s, sid, "continuous/db-detect-digest-mild/curr.json", ContinuousWindowIngest{
		WindowStart: currStart, WindowEnd: currEnd,
		DBSnapshots: []ContinuousDBSnapshotIngest{digestSnap("mysql-a", "mydb", digestText, 10, 200_000, 100)}, // 仅2倍
	})

	rule := model.SentinelRule{
		SID: "sr-detect-digest-mild", Name: "慢查询哨兵", TargetIP: "10.0.0.9",
		Signal: "db_snapshot", Metric: "digest", FloorValue: 50_000, KFactor: 5,
		CooldownSeconds: 900, Enabled: true,
		UID: "detector-owner", UserName: "detector-owner", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&rule).Error; err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	s.evaluateSentinelRule(rule)

	var count int64
	s.DB.Model(&model.DetectionEvent{}).Where("rule_sid = ?", rule.SID).Count(&count)
	if count != 0 {
		t.Fatalf("expected no event for a mild (2x) increase below KFactor, got %d", count)
	}
}

// TestCleanupDetectionEventsRemovesOnlyExpired 覆盖 §10.4：过期审计记录应被清理，
// 未过期的应保留——照抄 continuous.go 的 cutoff 模式，验证边界不要删多/删少。
func TestCleanupDetectionEventsRemovesOnlyExpired(t *testing.T) {
	s := newTestAPIServer(t)
	s.Config.Retention.DetectionEventRetentionHours = 24 // 保留 1 天，方便测试构造边界

	now := time.Now()
	old := model.DetectionEvent{RuleSID: "sr-old", EvaluatedAt: now.Add(-48 * time.Hour), Signal: "sched_latency", Status: "fired"}
	recent := model.DetectionEvent{RuleSID: "sr-recent", EvaluatedAt: now.Add(-1 * time.Hour), Signal: "sched_latency", Status: "fired"}
	if err := s.DB.Create(&old).Error; err != nil {
		t.Fatalf("seed old event: %v", err)
	}
	if err := s.DB.Create(&recent).Error; err != nil {
		t.Fatalf("seed recent event: %v", err)
	}

	s.cleanupDetectionEvents(context.Background())

	var remaining []model.DetectionEvent
	if err := s.DB.Find(&remaining).Error; err != nil {
		t.Fatalf("query remaining events: %v", err)
	}
	if len(remaining) != 1 || remaining[0].RuleSID != "sr-recent" {
		t.Fatalf("expected only the recent event to survive, got %+v", remaining)
	}
}

// TestEvaluateSentinelRulesUpdatesHealthOnSuccess 覆盖 §10.6：正常跑完一轮判异循环后，
// 自检状态应该反映"刚成功过一次"，consecutive_failures 归零。
func TestEvaluateSentinelRulesUpdatesHealthOnSuccess(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newContinuousMemoryStorage()

	s.evaluateSentinelRules()

	lastEvalAt, lastSuccessAt, consecutiveFailures, lastError := s.detectionHealthSnapshot()
	if lastEvalAt.IsZero() || lastSuccessAt.IsZero() {
		t.Fatalf("expected lastEvalAt/lastSuccessAt to be set after a successful loop")
	}
	if consecutiveFailures != 0 {
		t.Fatalf("expected consecutive_failures=0 after success, got %d", consecutiveFailures)
	}
	if lastError != "" {
		t.Fatalf("expected no lastError after success, got %q", lastError)
	}
}
