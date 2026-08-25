// ============================================================
// server/schedule_worker.go — 间隔型周期计划 DB 轮询调度 worker
// ============================================================
// 新任务（interval_seconds > 0）不再注册进程内 cron，改由本 worker 定期
// 领取到期计划：
//   - 轮询 enabled 且 next_run_at <= now 的间隔型计划；
//   - 用计划的 next_run_at 作为 scheduledAt 触发（复用 schedule_triggers
//     (schedule_id, scheduled_at) 唯一约束防止多 API 实例重复触发）；
//   - 触发成功后原子推进 next_run_at（跳过错过的多个周期，避免重启后爆发）；
//   - 停用/删除/重启/执行失败都能正确处理。
// ============================================================

package server

import (
	"time"

	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
)

// scheduleWorkerInterval 轮询周期。远小于最小间隔（1 分钟），保证触发及时
// 又不至于空转太多。
const scheduleWorkerInterval = 5 * time.Second

// startScheduleWorker 启动间隔型周期计划轮询 worker（server.go 中调用）。
func (s *APIServer) startScheduleWorker() {
	ticker := time.NewTicker(scheduleWorkerInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.pollDueSchedules()
	}
}

// pollDueSchedules 领取所有到期（next_run_at <= now）的启用间隔型计划。
func (s *APIServer) pollDueSchedules() {
	now := time.Now()
	var schedules []model.ScheduleTask
	if err := s.DB.
		Where("enabled = ? AND interval_seconds > 0 AND next_run_at IS NOT NULL AND next_run_at <= ?", true, now).
		Find(&schedules).Error; err != nil {
		s.Logger.Warn("轮询到期周期计划失败", zap.Error(err))
		return
	}
	for _, sch := range schedules {
		s.triggerIntervalSchedule(sch, now)
	}
}

// triggerIntervalSchedule 触发一个到期计划：在计划的 next_run_at 创建采集
// 任务，然后原子推进 next_run_at 到下一个未来槽位。
func (s *APIServer) triggerIntervalSchedule(sch model.ScheduleTask, now time.Time) {
	if sch.NextRunAt == nil {
		return
	}
	scheduledAt := *sch.NextRunAt
	// 执行触发（内部用 schedule_triggers 唯一约束去重；失败会把 trigger 标
	// failed 并记录日志，不影响推进 next_run_at，避免失败计划卡死轮询）。
	s.executeScheduledTaskAt(sch, scheduledAt)

	// 推进 next_run_at：从本次 scheduledAt 起按间隔推进，跳过所有已错过的
	// 槽位，落到第一个严格晚于 now 的未来槽位（重启错过多个周期时不爆发）。
	next := scheduledAt
	interval := time.Duration(sch.IntervalSeconds) * time.Second
	for !next.After(now) {
		next = next.Add(interval)
	}
	updates := map[string]interface{}{"next_run_at": next, "updated_at": time.Now()}
	if err := s.DB.Model(&model.ScheduleTask{}).Where("sid = ?", sch.SID).Updates(updates).Error; err != nil {
		s.Logger.Warn("推进周期计划 next_run_at 失败", zap.String("sid", sch.SID), zap.Error(err))
	}
}
