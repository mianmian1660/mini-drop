// ============================================================
// server/schedule.go — 周期性深度采样管理处理器（旧 schedule API 兼容）
// 包含：创建/列表/删除定时任务 + cron 调度器
// ============================================================

package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// CreateScheduleReq 创建定时任务请求
type CreateScheduleReq struct {
	Name         string `json:"name" binding:"required"`
	CronExpr     string `json:"cron_expr" binding:"required"` // 如 "*/5 * * * *"
	TaskKind     string `json:"task_kind"`
	TaskType     uint32 `json:"task_type"`
	ProfilerType uint32 `json:"profiler_type"`
	TargetIP     string `json:"target_ip" binding:"required"`
	TargetPID    int32  `json:"target_pid"`
	Duration     uint64 `json:"duration"`
	Frequency    uint32 `json:"frequency"`
	Callgraph    string `json:"callgraph"`
	Event        string `json:"event"`
	Subprocess   bool   `json:"subprocess"`
	PprofURL     string `json:"pprof_url"`
	// WindowSeconds 可选：前端按"窗口预设"下发时携带的窗口周期（秒），用于校验
	// Duration 不会超出窗口本身，避免相邻窗口重叠。不下发则跳过该校验（自定义 cron 场景）。
	WindowSeconds uint64 `json:"window_seconds"`
}

// ----------------------------------------------------------
// initCron 初始化 cron 调度器并恢复已启用的定时任务（W5）
// ----------------------------------------------------------
func (s *APIServer) initCron() {
	s.Cron = cron.New() // 标准 5 字段 cron（分 时 日 月 周），无需秒级

	// 从数据库恢复所有已启用的定时任务
	var schedules []model.ScheduleTask
	if err := s.DB.Where("enabled = ?", true).Find(&schedules).Error; err != nil {
		s.Logger.Warn("恢复定时任务失败", zap.Error(err))
	} else {
		for _, sch := range schedules {
			s.addCronJob(sch)
		}
		s.Logger.Info("已恢复周期性深度采样计划", zap.Int("count", len(schedules)))
	}

	s.Cron.Start()
	s.Logger.Info("Cron 调度器已启动")
}

// addCronJob 向 cron 调度器添加一个定时任务，返回 EntryID
func (s *APIServer) addCronJob(sch model.ScheduleTask) cron.EntryID {
	entryID, err := s.Cron.AddFunc(sch.CronExpr, func() {
		s.executeScheduledTask(sch)
	})
	if err != nil {
		s.Logger.Error("添加 cron 任务失败",
			zap.String("sid", sch.SID),
			zap.String("cron", sch.CronExpr),
			zap.Error(err),
		)
		return 0
	}
	if s.CronJobs == nil {
		s.CronJobs = make(map[string]cron.EntryID)
	}
	s.CronJobs[sch.SID] = entryID
	s.Logger.Info("cron 任务已添加", zap.String("sid", sch.SID), zap.Int("entryID", int(entryID)))
	return entryID
}

// removeCronJob 从 cron 调度器中移除定时任务
func (s *APIServer) removeCronJob(sid string) {
	if entryID, ok := s.CronJobs[sid]; ok {
		s.Cron.Remove(entryID)
		delete(s.CronJobs, sid)
		s.Logger.Info("cron 任务已移除", zap.String("sid", sid), zap.Int("entryID", int(entryID)))
	}
}

// executeScheduledTask 执行定时任务（创建实际的采集任务）
func (s *APIServer) executeScheduledTask(sch model.ScheduleTask) {
	scheduledAt := time.Now().Truncate(time.Minute)
	s.Logger.Info("触发定时任务",
		zap.String("sid", sch.SID),
		zap.String("name", sch.Name),
		zap.Time("scheduled_at", scheduledAt),
	)
	trigger, claimed := s.claimScheduleTrigger(sch.SID, scheduledAt)
	if !claimed {
		s.Logger.Info("定时任务触发已存在，跳过重复创建",
			zap.String("sid", sch.SID),
			zap.Time("scheduled_at", scheduledAt),
			zap.String("child_tid", trigger.ChildTID))
		return
	}

	// 解析任务参数
	var params PerfParams
	if len(sch.RequestParams) > 0 {
		if err := util.UnmarshalJSONB(sch.RequestParams, &params); err != nil {
			s.Logger.Warn("解析定时任务参数失败", zap.String("sid", sch.SID), zap.Error(err))
		}
	}

	// 创建采集任务
	tid := util.GenTID()
	now := time.Now()

	task := &model.HotmethodTask{
		TID:            tid,
		Name:           sch.Name + " (定时)",
		TaskKind:       sch.TaskKind,
		Type:           sch.TaskType,
		ProfilerType:   sch.ProfilerType,
		TargetIP:       sch.TargetIP,
		RequestParams:  sch.RequestParams,
		Status:         0,
		StatusInfo:     "定时任务触发",
		AnalysisStatus: 0,
		UID:            sch.UID,
		UserName:       sch.UserName,
		MasterTaskTID:  sch.SID,
		CreateTime:     scheduledAt,
		DeadlineUnixMS: int64(now.Add(time.Duration(params.Duration+30) * time.Second).UnixMilli()),
	}

	req := CreateTaskReq{
		Name:         task.Name,
		TaskKind:     sch.TaskKind,
		TaskType:     sch.TaskType,
		ProfilerType: sch.ProfilerType,
		TargetIP:     sch.TargetIP,
		TargetPID:    params.TargetPID,
		Duration:     params.Duration,
		Frequency:    params.Frequency,
		Callgraph:    params.Callgraph,
		Event:        params.Event,
		Subprocess:   params.Subprocess,
		PprofURL:     params.PprofURL,
	}

	// A5: 同事务写 Task + Outbox，下发交给后台 dispatchOutboxLoop 异步执行
	if err := s.createTaskWithOutbox(task, req); err != nil {
		s.Logger.Error("定时任务创建失败", zap.String("sid", sch.SID), zap.Error(err))
		nowFailed := time.Now()
		_ = s.DB.Model(&model.ScheduleTrigger{}).
			Where("schedule_id = ? AND scheduled_at = ?", sch.SID, scheduledAt).
			Updates(map[string]interface{}{"status": "failed", "updated_at": nowFailed}).Error
		return
	}
	nowDone := time.Now()
	_ = s.DB.Model(&model.ScheduleTrigger{}).
		Where("schedule_id = ? AND scheduled_at = ?", sch.SID, scheduledAt).
		Updates(map[string]interface{}{"child_tid": tid, "status": "created", "updated_at": nowDone}).Error

	// 更新最后运行时间
	now2 := time.Now()
	s.DB.Model(&sch).Updates(map[string]interface{}{
		"last_run_at": &now2,
	})

	s.Logger.Info("定时任务执行完成", zap.String("sid", sch.SID), zap.String("tid", tid))
}

func (s *APIServer) claimScheduleTrigger(scheduleID string, scheduledAt time.Time) (model.ScheduleTrigger, bool) {
	trigger := model.ScheduleTrigger{
		ScheduleID:  scheduleID,
		ScheduledAt: scheduledAt,
		Status:      "claimed",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			var locked bool
			if err := tx.Raw("SELECT pg_try_advisory_xact_lock(hashtext(?))", scheduleID+"|"+scheduledAt.Format(time.RFC3339)).Scan(&locked).Error; err != nil {
				return err
			}
			if !locked {
				return gorm.ErrDuplicatedKey
			}
		}
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "schedule_id"}, {Name: "scheduled_at"}},
			DoNothing: true,
		}).Create(&trigger)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrDuplicatedKey
		}
		return nil
	})
	if err == nil {
		return trigger, true
	}
	var existing model.ScheduleTrigger
	_ = s.DB.Where("schedule_id = ? AND scheduled_at = ?", scheduleID, scheduledAt).First(&existing).Error
	return existing, false
}

// ----------------------------------------------------------
// CreateSchedule 创建定时任务
// POST /api/v1/schedule/task
// ----------------------------------------------------------
func (s *APIServer) CreateSchedule(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	var req CreateScheduleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}

	// 验证 cron 表达式（标准 5 字段：分 时 日 月 周）
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(req.CronExpr); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "Cron 表达式无效: "+err.Error())
		return
	}

	// 设置默认值
	if req.Duration == 0 {
		req.Duration = 10
	}
	if req.Frequency == 0 {
		req.Frequency = 99
	}
	if req.Callgraph == "" {
		req.Callgraph = "fp"
	}
	collectorReq := CreateTaskReq{
		TaskKind: req.TaskKind, TaskType: req.TaskType, ProfilerType: req.ProfilerType, TargetPID: req.TargetPID,
		Duration: req.Duration, Frequency: req.Frequency, Callgraph: req.Callgraph, Event: req.Event, PprofURL: req.PprofURL,
	}
	if err := normalizeAndValidateCollector(&collectorReq); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, err.Error())
		return
	}
	kind, _ := taskKindByID(collectorReq.TaskKind)
	if !s.agentSupportsTaskKind(req.TargetIP, kind) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeAgentIncompatible, "目标 Agent 不支持该 TaskKind: "+collectorReq.TaskKind)
		return
	}
	req.TaskKind, req.TaskType, req.ProfilerType = collectorReq.TaskKind, collectorReq.TaskType, collectorReq.ProfilerType
	req.Event, req.PprofURL = collectorReq.Event, collectorReq.PprofURL

	// 窗口校验：周期性深度采样的每次采样必须在窗口周期内结束，否则相邻窗口会重叠，
	// "回溯任意窗口"的语义就不成立了。
	if req.WindowSeconds > 0 && req.Duration >= req.WindowSeconds {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, fmt.Sprintf("采样时长(%ds)需小于窗口周期(%ds)，否则相邻窗口会重叠", req.Duration, req.WindowSeconds))
		return
	}

	sid := "sch-" + util.GenTID()[4:]
	uid := auth.UID
	userName := auth.Name

	// 序列化采集参数
	paramsJSON, _ := util.MarshalJSONB(PerfParams{
		TargetPID:  req.TargetPID,
		Duration:   req.Duration,
		Frequency:  req.Frequency,
		Callgraph:  req.Callgraph,
		Event:      req.Event,
		Subprocess: req.Subprocess,
		PprofURL:   req.PprofURL,
	})

	now := time.Now()
	sch := &model.ScheduleTask{
		SID:           sid,
		Name:          req.Name,
		CronExpr:      req.CronExpr,
		TaskKind:      req.TaskKind,
		TaskType:      req.TaskType,
		ProfilerType:  req.ProfilerType,
		TargetIP:      req.TargetIP,
		RequestParams: paramsJSON,
		Enabled:       true,
		UID:           uid,
		UserName:      userName,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := s.DB.Create(sch).Error; err != nil {
		s.Logger.Error("创建定时任务失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建定时任务失败")
		return
	}

	// 添加到 cron 调度器
	s.addCronJob(*sch)

	s.Logger.Info("定时任务已创建", zap.String("sid", sid), zap.String("cron", req.CronExpr))
	sch.CanManage = true
	s.RespondOK(c, sch)
}

// ----------------------------------------------------------
// ListSchedules 获取定时任务列表
// GET /api/v1/schedule/tasks
// ----------------------------------------------------------
func (s *APIServer) ListSchedules(c *gin.Context) {
	auth := s.AuthContext(c)
	var schedules []model.ScheduleTask
	query := s.DB.Order("created_at DESC")
	switch strings.ToLower(strings.TrimSpace(c.DefaultQuery("owner_filter", "all"))) {
	case "", "all":
	case "mine":
		query = query.Where("uid = ?", auth.UID)
	default:
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "owner_filter 仅支持 all/mine")
		return
	}
	if err := query.Find(&schedules).Error; err != nil {
		s.Logger.Error("查询定时任务失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询失败")
		return
	}

	if schedules == nil {
		schedules = []model.ScheduleTask{}
	}
	for index := range schedules {
		schedules[index].CanManage = s.canManageOwner(schedules[index].UID, auth)
	}

	s.RespondOK(c, gin.H{"schedules": schedules, "total": len(schedules)})
}

// ----------------------------------------------------------
// DeleteSchedule 删除定时任务
// DELETE /api/v1/schedule/:sid
// ----------------------------------------------------------
func (s *APIServer) DeleteSchedule(c *gin.Context) {
	sid := c.Param("sid")

	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", sid).First(&sch).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "定时任务不存在: "+sid)
		return
	}
	if !s.canManageOwner(sch.UID, s.AuthContext(c)) {
		s.forbid(c)
		return
	}
	result := s.DB.Where("sid = ?", sid).Delete(&model.ScheduleTask{})
	if result.RowsAffected == 0 {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "定时任务不存在: "+sid)
		return
	}

	// 从 cron 调度器中立即停止
	s.removeCronJob(sid)

	s.Logger.Info("定时任务已删除并停止", zap.String("sid", sid))
	s.RespondOK(c, gin.H{"message": "定时任务已删除并停止"})
}

// ----------------------------------------------------------
// ToggleSchedule 启用/禁用定时任务
// POST /api/v1/schedule/:sid/toggle
// ----------------------------------------------------------
func (s *APIServer) ToggleSchedule(c *gin.Context) {
	sid := c.Param("sid")

	var sch model.ScheduleTask
	if err := s.DB.Unscoped().Where("sid = ?", sid).First(&sch).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "定时任务不存在: "+sid)
		return
	}
	if !s.canManageOwner(sch.UID, s.AuthContext(c)) {
		s.forbid(c)
		return
	}

	newEnabled := !sch.Enabled
	s.DB.Model(&sch).Update("enabled", newEnabled)

	// 动态管理 cron 调度器：真正停止/启动
	if newEnabled {
		sch.Enabled = true
		s.addCronJob(sch)
	} else {
		s.removeCronJob(sid)
	}

	s.Logger.Info("定时任务状态已切换",
		zap.String("sid", sid),
		zap.Bool("enabled", newEnabled),
	)

	s.RespondOK(c, gin.H{"sid": sid, "enabled": newEnabled})
}
