// ============================================================
// server/detection.go — 检测→触发深度诊断：MVP 判异 + 触发循环
// ============================================================
// 对应 docs/detection-trigger-pipeline-design.md 的 MVP 阶段：
//   仅 sched_latency 一个信号（io_latency/io_syscall_latency 复用同一套
//   histogram 判异逻辑，顺带一起接上；db_snapshot 判异方式不同，留给迭代2）。
//   固定阈值（SentinelRule.FloorValue），不做滚动中位数/MAD。
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
	"errors"
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

// evaluateSentinelRules 遍历全部启用的哨兵规则，逐条判异。
func (s *APIServer) evaluateSentinelRules() {
	var rules []model.SentinelRule
	if err := s.DB.Where("enabled = ?", true).Find(&rules).Error; err != nil {
		s.Logger.Warn("哨兵规则加载失败", zap.Error(err))
		return
	}
	for _, rule := range rules {
		s.evaluateSentinelRule(rule)
	}
}

// evaluateSentinelRule 对单条规则跑一次判异；命中且通过全部闸门时触发一次
// 深度诊断任务。每一步的跳过原因都写一条 DetectionEvent，方便事后排查。
func (s *APIServer) evaluateSentinelRule(rule model.SentinelRule) {
	taskKind, ok := detectionSignalTaskKind[rule.Signal]
	if !ok {
		return // 信号类型暂未接入判异逻辑（如 db_snapshot，见迭代2），跳过
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

	if observed <= rule.FloorValue {
		return // 未超过阈值，正常范围，不记录事件（避免表随每个 tick 无限增长）
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
		return true // 查询失败时保守处理：当作"有活跃任务"，不新建任务
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
