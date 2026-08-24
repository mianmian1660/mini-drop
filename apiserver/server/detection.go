// ============================================================
// server/detection.go — 检测→触发深度诊断：判异 + 触发循环
// ============================================================
// 对应 docs/detection-trigger-pipeline-design.md：
//   sched_latency/io_latency/io_syscall_latency 三个信号复用同一套 histogram 判异逻辑
//   （db_snapshot 判异方式不同，留给 §10.1）。
//   判异 = 静态下限（SentinelRule.FloorValue）+ 滚动中位数/MAD 稳健基线（§10.2/§4.1）
//   + 持续性判断（§10.3，过滤单点抖动），三道闸门都要过才算真正的异常。
//   规则本身通过 sentinel_rules 表管理，本阶段没有前端/API 做规则增删，
//   只能直接写库或在测试里 seed（见 detection_test.go）。
//
// 判异→触发的整条链路照抄 executeScheduledTask（schedule.go）已经验证过的模式：
// 数据质量闸门 → 阈值判断 → 冷却期 → 磁盘预算闸门 → 活跃任务重叠检查 →
// createTaskWithOutbox（MasterTaskTID 指向 sentinel_rules.sid，触发出的诊断
// 直接复用 GetTimeline/ScheduleTimeline.js 查看，不需要新的前端时间轴）。
// ============================================================

package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// detectionSignalTaskKind 把哨兵规则的信号映射到触发时创建的诊断 TaskKind。
var detectionSignalTaskKind = map[string]string{
	"sched_latency":      TaskKindEBPFSched,
	"io_latency":         TaskKindEBPFIO,
	"io_syscall_latency": TaskKindEBPFIO,
}

// detectionSignalEvent 对应 TaskKind 默认参数里的 bpftrace event（见 task_kind.go）。
var detectionSignalEvent = map[string]string{
	"sched_latency":      "sched",
	"io_latency":         "io",
	"io_syscall_latency": "io",
}

const (
	detectionEvalInterval         = 30 * time.Second
	detectionLookback             = 5 * time.Minute
	detectionCoverageMinRatio     = 0.9
	detectionCoverageTolerance    = 5 * time.Second
	detectionDiagnosisDurationSec = 60
	// detectionBaselineWindowSize 滚动基线保留的最近窗口值个数（对应文档 §4.2 的 N=100）。
	detectionBaselineWindowSize = 100
	// detectionDefaultKFactor SentinelRule.KFactor 未设置时的默认判异灵敏度（对应 §4.1 的 K=5）。
	detectionDefaultKFactor = 5.0
	// madToStdDevFactor 把 MAD 换算成等效标准差的标准换算常数（正态分布下 MAD ≈ 0.6745σ）。
	madToStdDevFactor = 1.4826
)

// startAnomalyDetector 后台判异循环，写法对齐 server.go 里已有的 ticker 惯例
// （如 startTaskPoller，server.go:298-305）。
func (s *APIServer) startAnomalyDetector() {
	ticker := time.NewTicker(detectionEvalInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.evaluateSentinelRules()
	}
}

// detectionHealthState 判异循环自检状态（单实例内存态，无需持久化，见 §10.6）。
// 写法对齐 storage_status.go 的 storageMonitorState：后台 ticker goroutine 写，
// GetDetectionHealth 这个 HTTP handler 读，用同一把锁避免竞态。
type detectionHealthState struct {
	mu                  sync.Mutex
	lastEvalAt          time.Time
	lastSuccessAt       time.Time
	consecutiveFailures int
	lastError           string
}

// detectionHealthFailureAlertThreshold 连续失败达到这个次数时升级为 Error 级别日志——
// 单次失败可能是瞬时抖动，连续失败才说明哨兵已经失效，值得报警而不是淹没在 Warn 里。
const detectionHealthFailureAlertThreshold = 3

// evaluateSentinelRules 遍历全部启用的哨兵规则，逐条判异。规则加载本身失败时更新
// 自检状态（§10.6）：之前这里只有一条 Warn 日志，数据库持续故障时哨兵会静默永远
// 不触发，没有任何信号提示"哨兵已经失效"——GetDetectionHealth 把这个状态暴露成一个
// 可轮询的端点，配合下面的连续失败升级日志，运维不需要盯日志也能发现哨兵失效。
func (s *APIServer) evaluateSentinelRules() {
	var rules []model.SentinelRule
	err := s.DB.Where("enabled = ?", true).Find(&rules).Error

	s.detectionHealth.mu.Lock()
	s.detectionHealth.lastEvalAt = time.Now()
	if err != nil {
		s.detectionHealth.consecutiveFailures++
		s.detectionHealth.lastError = err.Error()
		failures := s.detectionHealth.consecutiveFailures
		s.detectionHealth.mu.Unlock()

		s.Logger.Warn("哨兵规则加载失败", zap.Error(err), zap.Int("consecutive_failures", failures))
		if failures >= detectionHealthFailureAlertThreshold {
			s.Logger.Error("哨兵判异循环连续失败，可能已经失效", zap.Int("consecutive_failures", failures), zap.Error(err))
		}
		return
	}
	s.detectionHealth.consecutiveFailures = 0
	s.detectionHealth.lastSuccessAt = time.Now()
	s.detectionHealth.lastError = ""
	s.detectionHealth.mu.Unlock()

	for _, rule := range rules {
		s.evaluateSentinelRule(rule)
	}
}

// detectionHealthSnapshot 供 GetDetectionHealth 读取当前自检状态。
func (s *APIServer) detectionHealthSnapshot() (lastEvalAt, lastSuccessAt time.Time, consecutiveFailures int, lastError string) {
	s.detectionHealth.mu.Lock()
	defer s.detectionHealth.mu.Unlock()
	return s.detectionHealth.lastEvalAt, s.detectionHealth.lastSuccessAt, s.detectionHealth.consecutiveFailures, s.detectionHealth.lastError
}

// evaluateSentinelRule 对单条规则跑一次判异；命中且通过全部闸门时触发一次
// 深度诊断任务。每一步的跳过原因都写一条 DetectionEvent，方便事后排查。
func (s *APIServer) evaluateSentinelRule(rule model.SentinelRule) {
	if rule.Signal == "db_snapshot" {
		s.evaluateDBSnapshotRule(rule) // 判异方式和触发动作都不同，见 §10.1 独立实现
		return
	}

	taskKind, ok := detectionSignalTaskKind[rule.Signal]
	if !ok {
		return // 未知信号类型，跳过
	}

	// 用 UTC 与窗口写入的时区保持一致，否则 SQLite 按字符串比较时间会错位（CST vs UTC）。
	now := time.Now().UTC().UTC()
	q := ProfileQuery{
		Host:       rule.TargetIP,
		From:       now.Add(-detectionLookback),
		To:         now,
		CanReadAll: true, // 检测器是系统级后台任务，不受用户所有权过滤限制
	}

	result, _, err := s.queryNativeContinuousHistogram(context.Background(), q, rule.Signal)
	if err != nil || result == nil {
		s.Logger.Debug("哨兵规则暂无可判异数据", zap.String("rule_sid", rule.SID),
			zap.String("target_ip", q.Host), zap.Time("from", q.From), zap.Time("to", q.To), zap.Error(err))
		return // 没有数据可判异（session 未运行/无 backend 数据），不算异常也不算错误，不记录事件
	}
	trend, _ := result["trend"].([]gin.H)
	if len(trend) == 0 {
		s.Logger.Debug("哨兵规则 histogram trend 为空", zap.String("rule_sid", rule.SID))
		return
	}
	latest := trend[len(trend)-1]
	observed, _ := latest[rule.Metric].(float64)

	if !s.detectionCoverageOK(q, rule.Signal) {
		s.Logger.Debug("哨兵规则采样覆盖率不足", zap.String("rule_sid", rule.SID))
		s.recordDetectionEvent(rule, observed, "skipped_low_coverage", "")
		return
	}

	// 无论这次是否异常，都用「加入本次观测值之前」的滚动基线打分，再把本次观测值纳入
	// 基线（§10.2/§4.2 步骤3）——中位数+MAD 本身对离群点稳健，纳入少数异常值不会显著
	// 污染基线，但如果只在触发时才更新基线，正常波动样本会系统性偏少，基线反而不准。
	baselineMedian, baselineMAD := s.detectionUpdateBaseline(rule.SID, observed)

	if observed <= rule.FloorValue {
		return // 未超过阈值，正常范围，不记录事件（避免表随每个 tick 无限增长）
	}

	if !detectionPersistentEnough(trend, rule) {
		s.Logger.Debug("哨兵规则单点超阈值但持续性不足", zap.String("rule_sid", rule.SID))
		s.recordDetectionEvent(rule, observed, "skipped_low_persistence", "")
		return
	}

	if baselineMAD > 0 {
		kFactor := rule.KFactor
		if kFactor <= 0 {
			kFactor = detectionDefaultKFactor
		}
		score := math.Abs(observed-baselineMedian) / (madToStdDevFactor * baselineMAD)
		if score <= kFactor {
			s.Logger.Debug("哨兵规则超静态下限但未偏离滚动基线", zap.String("rule_sid", rule.SID), zap.Float64("score", score))
			s.recordDetectionEvent(rule, observed, "skipped_low_deviation", "")
			return
		}
	}

	if s.detectionInCooldown(rule) {
		s.recordDetectionEvent(rule, observed, "skipped_cooldown", "")
		return
	}

	if ok, _, _ := s.canStartCollection(CollectionSourceScheduled); !ok {
		s.recordDetectionEvent(rule, observed, "skipped_low_disk", "")
		return
	}

	if s.detectionHasActiveTask(rule.TargetIP, taskKind) {
		s.recordDetectionEvent(rule, observed, "skipped_overlap", "")
		return
	}

	childTID, err := s.triggerDetectionDiagnosis(rule, taskKind, observed)
	if err != nil {
		s.Logger.Error("哨兵规则触发诊断任务失败", zap.String("rule_sid", rule.SID), zap.Error(err))
		return
	}
	s.recordDetectionEvent(rule, observed, "fired", childTID)
	s.markDetectionFired(rule)
}

// detectionCoverageOK 复用 continuousTimelineCoverage（continuous.go:982）判断
// 判异窗口内的采样是否完整。查询逻辑与 queryNativeContinuousHistogram 内部拉取
// model.ProfileWindow 的方式一致（continuous.go:2299-2306），这里单独拉一次是
// 因为该函数没有对外暴露中间的 []model.ProfileWindow，不值得为此改动既有签名。
func (s *APIServer) detectionCoverageOK(q ProfileQuery, signal string) bool {
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	if err := s.DB.Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", signal).
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Order("window_start ASC").
		Find(&windows).Error; err != nil || len(windows) == 0 {
		return false
	}
	_, coverage := continuousTimelineCoverage(windows, q.From, q.To, time.Time{}, detectionCoverageTolerance)
	ratio, _ := coverage["ratio"].(float64)
	return ratio >= detectionCoverageMinRatio
}

// evaluateDBSnapshotRule 对 db_snapshot 信号做判异（见
// docs/detection-trigger-pipeline-design.md §10.1）。和 histogram 类信号（sched/io
// latency）的两处关键差异：
//
//   - 数据结构不同：queryNativeContinuousDBSnapshot 返回的是整个 lookback 窗口聚合后的
//     digest 排行/锁等待列表，不是 histogram 那种逐窗口 trend 序列，没有"最新一个窗口"
//     的概念，判异对象是"当前聚合快照 vs 上一个等长窗口的聚合快照"。
//   - 触发动作不同：script_diagnostic 这个 TaskKind 目前是"仅声明契约，Runner 未接入"
//     （task_kind.go:199-205），建出来的任务永远跑不完、永远卡在已创建状态。db_snapshot
//     命中因此只记一条 DetectionEvent（status=fired_no_action），不创建 HotmethodTask，
//     等 Runner 真正接入后再补上创建任务这一步——不做一个"能触发但触发了也没用"的假功能。
//
// rule.Metric 取值：
//
//	"lock_wait" — 当前窗口内最大 wait_seconds 超过 FloorValue 即触发（长事务阻塞不需要
//	              看基线，超过几秒本身就是问题，见 §10.1 设计理由）。
//	"digest"    — 当前窗口 total_latency_us 最高的 digest：若同一 digest 文本在上一个
//	              等长窗口里也出现过，比较环比倍数（复用 KFactor 字段，默认5倍）；若上一
//	              窗口没有这条 digest（新出现的慢查询），只要现值超过 FloorValue 就算命中
//	              ——不同 digest 正常耗时水位天差地别，不能像延迟类信号一样共用一个绝对
//	              阈值，所以主判断是环比而不是绝对值，FloorValue 只做"绝对值太小不值得算"
//	              的下限过滤（避免从 100us 变到 500us 这种噪声被环比放大成"5倍暴涨"）。
func (s *APIServer) evaluateDBSnapshotRule(rule model.SentinelRule) {
	now := time.Now().UTC()
	q := ProfileQuery{
		Host:       rule.TargetIP,
		From:       now.Add(-detectionLookback),
		To:         now,
		CanReadAll: true,
	}

	current, _, err := s.queryNativeContinuousDBSnapshot(context.Background(), q)
	if err != nil || current == nil {
		s.Logger.Debug("哨兵规则暂无可判异的 db_snapshot 数据", zap.String("rule_sid", rule.SID), zap.Error(err))
		return
	}

	if !s.detectionCoverageOK(q, "db_snapshot") {
		s.recordDetectionEvent(rule, 0, "skipped_low_coverage", "")
		return
	}

	var observed float64
	var hit bool
	switch rule.Metric {
	case "lock_wait":
		observed, hit = detectionMaxLockWait(current)
		hit = hit && observed > rule.FloorValue
	case "digest":
		prevQ := q
		prevQ.To, prevQ.From = q.From, q.From.Add(-detectionLookback)
		previous, _, prevErr := s.queryNativeContinuousDBSnapshot(context.Background(), prevQ)
		if prevErr != nil {
			previous = nil
		}
		kFactor := rule.KFactor
		if kFactor <= 0 {
			kFactor = detectionDefaultKFactor
		}
		observed, hit = detectionTopDigestSpike(current, previous, rule.FloorValue, kFactor)
	default:
		s.Logger.Warn("哨兵规则 db_snapshot metric 不支持", zap.String("rule_sid", rule.SID), zap.String("metric", rule.Metric))
		return
	}

	if !hit {
		return // 未命中，不记录事件（避免表随每个 tick 无限增长，与 histogram 分支一致）
	}
	if s.detectionInCooldown(rule) {
		s.recordDetectionEvent(rule, observed, "skipped_cooldown", "")
		return
	}
	s.recordDetectionEvent(rule, observed, "fired_no_action", "")
	s.markDetectionFired(rule)
}

// detectionMaxLockWait 从 queryNativeContinuousDBSnapshot 的结果里取最大 wait_seconds。
func detectionMaxLockWait(snapshot gin.H) (max float64, ok bool) {
	lockWaits, _ := snapshot["lock_waits"].([]gin.H)
	for _, item := range lockWaits {
		waitSeconds, _ := item["wait_seconds"].(uint64)
		if v := float64(waitSeconds); v > max {
			max = v
			ok = true
		}
	}
	return max, ok
}

// detectionTopDigestSpike 取当前快照里耗时最高的 digest（queryNativeContinuousDBSnapshot
// 已按 total_latency_us 降序排好，见 continuous.go:2736-2738），和上一个等长窗口里同一条
// digest 文本对比环比倍数。返回值是当前 digest 的 total_latency_us（供审计记录展示）。
func detectionTopDigestSpike(current, previous gin.H, floorValue, kFactor float64) (observed float64, hit bool) {
	currentDigests, _ := current["digests"].([]gin.H)
	if len(currentDigests) == 0 {
		return 0, false
	}
	top := currentDigests[0]
	digestText, _ := top["digest_text"].(string)
	totalLatencyUs, _ := top["total_latency_us"].(uint64)
	observed = float64(totalLatencyUs)
	if observed <= floorValue {
		return observed, false
	}

	var previousLatency float64
	var seenBefore bool
	if previous != nil {
		if prevDigests, ok := previous["digests"].([]gin.H); ok {
			for _, item := range prevDigests {
				if prevText, _ := item["digest_text"].(string); prevText == digestText {
					prevLatency, _ := item["total_latency_us"].(uint64)
					previousLatency = float64(prevLatency)
					seenBefore = true
					break
				}
			}
		}
	}

	if !seenBefore || previousLatency <= 0 {
		return observed, true // 新出现的慢 digest，上一窗口没有可比基线，超过静态下限即命中
	}
	return observed, observed >= previousLatency*kFactor
}

// detectionPersistentEnough 判断最新超阈值是否是"持续性异常"而不是单点抖动（见
// docs/detection-trigger-pipeline-design.md §10.3）：统计 trend 数组末尾最近
// PersistenceWindows 个窗口里，有多少个超过 FloorValue，达到 PersistenceMinHits 才算数。
// 两个字段默认 1/1（GORM 建表默认值），等价于"只看最新一个窗口"，与升级前的 MVP 行为一致。
func detectionPersistentEnough(trend []gin.H, rule model.SentinelRule) bool {
	n := rule.PersistenceWindows
	if n <= 0 {
		n = 1
	}
	if n > len(trend) {
		n = len(trend)
	}
	minHits := rule.PersistenceMinHits
	if minHits <= 0 {
		minHits = 1
	}

	hits := 0
	for _, window := range trend[len(trend)-n:] {
		v, _ := window[rule.Metric].(float64)
		if v > rule.FloorValue {
			hits++
		}
	}
	return hits >= minHits
}

// detectionUpdateBaseline 用「当前」滚动基线（不含本次观测值）算出中位数/MAD 供打分，
// 然后把本次观测值纳入滚动窗口（超出 detectionBaselineWindowSize 丢最旧）并持久化——
// 无论本次是否判定为异常都纳入（见 §10.2/§4.2 步骤3的理由）。读取/更新失败时静默返回
// 0,0，调用方据此退化为只看 FloorValue（不影响判异的可用性，只是少一层灵敏度）。
func (s *APIServer) detectionUpdateBaseline(ruleSID string, observed float64) (medianBefore, madBefore float64) {
	var state model.DetectionState
	err := s.DB.Where("rule_sid = ?", ruleSID).First(&state).Error
	var prevValues []float64
	if err == nil && len(state.RecentValues) > 0 {
		if unmarshalErr := json.Unmarshal(state.RecentValues, &prevValues); unmarshalErr != nil {
			s.Logger.Warn("解析滚动基线失败", zap.String("rule_sid", ruleSID), zap.Error(unmarshalErr))
			prevValues = nil
		}
	}
	medianBefore, madBefore = detectionMedianMAD(prevValues)

	newValues := append(append([]float64{}, prevValues...), observed)
	if len(newValues) > detectionBaselineWindowSize {
		newValues = newValues[len(newValues)-detectionBaselineWindowSize:]
	}
	payload, marshalErr := json.Marshal(newValues)
	if marshalErr != nil {
		s.Logger.Warn("序列化滚动基线失败", zap.String("rule_sid", ruleSID), zap.Error(marshalErr))
		return
	}

	if errors.Is(err, gorm.ErrRecordNotFound) {
		if createErr := s.DB.Create(&model.DetectionState{RuleSID: ruleSID, RecentValues: payload, UpdatedAt: time.Now()}).Error; createErr != nil {
			s.Logger.Warn("写入滚动基线失败", zap.String("rule_sid", ruleSID), zap.Error(createErr))
		}
		return
	}
	if err != nil {
		s.Logger.Warn("读取 detection_state 失败", zap.String("rule_sid", ruleSID), zap.Error(err))
		return
	}
	if updateErr := s.DB.Model(&state).Updates(map[string]interface{}{"recent_values": payload, "updated_at": time.Now()}).Error; updateErr != nil {
		s.Logger.Warn("更新滚动基线失败", zap.String("rule_sid", ruleSID), zap.Error(updateErr))
	}
	return
}

// detectionMedianMAD 计算稳健基线：中位数 + MAD（median absolute deviation，中位数绝对
// 偏差）。用中位数/MAD 而不是均值/标准差，是因为延迟类指标长尾右偏，均值和标准差会被
// 少数极端值本身拽高（"异常值污染了自己的判定基准"），见文档 §4.1。
func detectionMedianMAD(values []float64) (median, mad float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	median = detectionMedianOf(sorted)

	devs := make([]float64, len(sorted))
	for i, v := range sorted {
		devs[i] = math.Abs(v - median)
	}
	sort.Float64s(devs)
	mad = detectionMedianOf(devs)
	return
}

func detectionMedianOf(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// detectionInCooldown 检查规则是否仍在冷却期内（DetectionState.LastFiredAt）。
func (s *APIServer) detectionInCooldown(rule model.SentinelRule) bool {
	var state model.DetectionState
	if err := s.DB.Where("rule_sid = ?", rule.SID).First(&state).Error; err != nil {
		return false // 无历史记录，不在冷却期
	}
	if state.LastFiredAt == nil {
		return false
	}
	return time.Since(*state.LastFiredAt) < time.Duration(rule.CooldownSeconds)*time.Second
}

// detectionHasActiveTask 对齐 executeScheduledTask 的单飞检查（schedule.go:121-127）：
// 同一目标+TaskKind 已有活跃任务在跑时不重复触发。
func (s *APIServer) detectionHasActiveTask(targetIP, taskKind string) bool {
	var active int64
	if err := s.DB.Model(&model.HotmethodTask{}).
		Where("target_ip = ? AND task_kind = ? AND status IN ?", targetIP, taskKind,
			[]int{TaskStatusCreated, TaskStatusRunning, TaskStatusUploading}).
		Count(&active).Error; err != nil {
		// 查询失败时保守处理：当作"有活跃任务"，不新建任务；但这个保守分支之前完全
		// 静默，数据库持续故障时哨兵会一直"看起来正常"地跳过触发，没有任何日志线索
		// （见 §10.6）。补一条 Warn——单次失败不升级，evaluateSentinelRules 的
		// consecutive_failures 计数器只统计规则加载本身的失败，这里的失败不会重复计数，
		// 只是让排查时能看到"跳过是因为这个查询失败"而不是误以为判异逻辑本身有问题。
		s.Logger.Warn("哨兵检查活跃任务失败，保守跳过触发", zap.String("target_ip", targetIP), zap.String("task_kind", taskKind), zap.Error(err))
		return true
	}
	return active > 0
}

// triggerDetectionDiagnosis 创建一次哨兵触发的深度诊断任务，MasterTaskTID 指向
// sentinel_rules.sid——GetTimeline 只按 master_task_tid 查询，不关心父对象类型，
// 触发出的诊断因此可以直接用现有 GetTimeline/ScheduleTimeline.js 查看。
func (s *APIServer) triggerDetectionDiagnosis(rule model.SentinelRule, taskKind string, observed float64) (string, error) {
	tid := util.GenTID()
	now := time.Now()

	triggerCtx, err := util.MarshalJSONB(map[string]interface{}{
		"trigger_source": "detector",
		"rule_sid":       rule.SID,
		"signal":         rule.Signal,
		"metric":         rule.Metric,
		"observed_value": observed,
		"floor_value":    rule.FloorValue,
	})
	if err != nil {
		return "", err
	}

	task := &model.HotmethodTask{
		TID:            tid,
		Name:           rule.Name + "（哨兵触发）",
		TaskKind:       taskKind,
		Type:           TaskTypeBPF,
		ProfilerType:   ProfilerBPF,
		TargetIP:       rule.TargetIP,
		Status:         0,
		StatusInfo:     "哨兵规则触发",
		AnalysisStatus: 0,
		UID:            rule.UID,
		UserName:       rule.UserName,
		MasterTaskTID:  rule.SID,
		TriggerContext: triggerCtx,
		CreateTime:     now,
		DeadlineUnixMS: now.Add(time.Duration(detectionDiagnosisDurationSec+30) * time.Second).UnixMilli(),
	}

	req := CreateTaskReq{
		Name:         task.Name,
		TaskKind:     taskKind,
		TaskType:     TaskTypeBPF,
		ProfilerType: ProfilerBPF,
		TargetIP:     rule.TargetIP,
		Duration:     detectionDiagnosisDurationSec,
		Frequency:    1,
		Event:        detectionSignalEvent[rule.Signal],
	}

	if err := s.createTaskWithOutbox(task, req); err != nil {
		return "", err
	}
	return tid, nil
}

// markDetectionFired 更新规则的滚动状态缓存（目前只用 LastFiredAt 做冷却期）。
func (s *APIServer) markDetectionFired(rule model.SentinelRule) {
	now := time.Now()
	var state model.DetectionState
	err := s.DB.Where("rule_sid = ?", rule.SID).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := s.DB.Create(&model.DetectionState{RuleSID: rule.SID, LastFiredAt: &now, UpdatedAt: now}).Error; err != nil {
			s.Logger.Warn("写入 detection_state 失败", zap.String("rule_sid", rule.SID), zap.Error(err))
		}
		return
	}
	if err := s.DB.Model(&state).Updates(map[string]interface{}{"last_fired_at": &now, "updated_at": now}).Error; err != nil {
		s.Logger.Warn("更新 detection_state 失败", zap.String("rule_sid", rule.SID), zap.Error(err))
	}
}

// recordDetectionEvent 写一条判异审计记录，触发的和被闸门跳过的都记，方便排查
// "为什么这次该触发没触发"。
func (s *APIServer) recordDetectionEvent(rule model.SentinelRule, observed float64, status, childTID string) {
	event := model.DetectionEvent{
		RuleSID:       rule.SID,
		EvaluatedAt:   time.Now(),
		Signal:        rule.Signal,
		Metric:        rule.Metric,
		ObservedValue: observed,
		FloorValue:    rule.FloorValue,
		Status:        status,
		ChildTID:      childTID,
	}
	if err := s.DB.Create(&event).Error; err != nil {
		s.Logger.Warn("写入 detection_event 失败", zap.String("rule_sid", rule.SID), zap.Error(err))
	}
}
