// ============================================================
// server/schedule.go — 周期性深度采样管理处理器
// 包含：创建/列表/删除定时任务 + cron 兼容调度
//
// 两种计划模式：
//   - 间隔型（新建默认）：interval_seconds + start_at，由 DB 周期轮询
//     worker（schedule_worker.go）驱动，可靠性依赖 schedule_triggers 的
//     (schedule_id, scheduled_at) 唯一约束。
//   - 旧 cron 型（兼容）：cron_expr，继续走进程内 cron 调度器；仅用于
//     历史任务兼容读取与运行，新任务不再创建。
// ============================================================

package server

import (
	"fmt"
	"net/http"
	"strconv"
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

// CreateScheduleReq 创建定时任务请求。
// 新任务使用"间隔 + 开始时间"：interval_seconds（秒，预设 1/5/10/30 分钟
// 或自定义）+ start_at（可选，缺省立即开始）。cron_expr 仅旧客户端兼容，
// 新任务不再要求填写。
type CreateScheduleReq struct {
	Name           string     `json:"name" binding:"required"`
	CronExpr       string     `json:"cron_expr"`       // 旧式 cron 表达式（仅兼容旧客户端）
	IntervalSeconds uint64    `json:"interval_seconds"` // 采样间隔（秒），>0 走间隔型
	StartAt        *time.Time `json:"start_at"`        // 首次采样开始时间（可选）
	TaskKind       string     `json:"task_kind"`
	TaskType       uint32     `json:"task_type"`
	ProfilerType   uint32     `json:"profiler_type"`
	TargetIP       string     `json:"target_ip" binding:"required"`
	TargetPID      int32      `json:"target_pid"`
	Duration       uint64     `json:"duration"`
	Frequency      uint32     `json:"frequency"`
	Callgraph      string     `json:"callgraph"`
	Event          string     `json:"event"`
	Subprocess     bool       `json:"subprocess"`
	PprofURL       string     `json:"pprof_url"`
	// WindowSeconds 旧字段：前端按"窗口预设"下发时携带的窗口周期（秒）。
	// 间隔型计划下后端始终以最终的 interval_seconds 校验 duration < interval，
	// 不信任前端传入的旧 window_seconds（防止配置漂移）。
	WindowSeconds uint64 `json:"window_seconds"`
}

// scheduleUsesInterval 判断计划是否为"间隔 + 开始时间"型。
func scheduleUsesInterval(sch model.ScheduleTask) bool {
	return sch.IntervalSeconds > 0
}

// scheduleUsesCron 判断计划是否为旧式 cron 型（仅兼容路径）。
func scheduleUsesCron(sch model.ScheduleTask) bool {
	return sch.IntervalSeconds == 0 && strings.TrimSpace(sch.CronExpr) != ""
}

// intervalNextRun 计算间隔型计划的第一个未来运行槽位：
//   - startAt 在未来 → 直接取 startAt（首个采样点）；
//   - startAt 在过去/缺省 → 从 startAt（缺省 now）按 interval 对齐到
//     严格晚于 now 的最近未来槽位。
func intervalNextRun(startAt *time.Time, intervalSeconds uint64, now time.Time) time.Time {
	anchor := now
	if startAt != nil {
		anchor = *startAt
	}
	if intervalSeconds == 0 {
		return anchor
	}
	interval := time.Duration(intervalSeconds) * time.Second
	if anchor.After(now) {
		return anchor
	}
	// 已过期：从锚点逐槽推进，取第一个严格晚于 now 的槽位。
	next := anchor
	for !next.After(now) {
		next = next.Add(interval)
	}
	return next
}

// ----------------------------------------------------------
// initCron 初始化 cron 调度器并恢复旧式 cron 定时任务（W5 兼容）。
// 间隔型计划由 DB 周期轮询 worker（startScheduleWorker）驱动，不注册 cron。
// ----------------------------------------------------------
func (s *APIServer) initCron() {
	s.Cron = cron.New() // 标准 5 字段 cron（分 时 日 月 周），无需秒级

	// 从数据库恢复所有已启用的旧式 cron 定时任务
	var schedules []model.ScheduleTask
	if err := s.DB.Where("enabled = ?", true).Find(&schedules).Error; err != nil {
		s.Logger.Warn("恢复定时任务失败", zap.Error(err))
	} else {
		for _, sch := range schedules {
			if scheduleUsesCron(sch) {
				s.addCronJob(sch)
			}
		}
		s.Logger.Info("已恢复周期性深度采样计划", zap.Int("count", len(schedules)))
	}

	s.Cron.Start()
	s.Logger.Info("Cron 调度器已启动")
}

// addCronJob 向 cron 调度器添加一个旧式 cron 定时任务，返回 EntryID。
// 间隔型计划不会走到这里（新任务不注册 cron）。
func (s *APIServer) addCronJob(sch model.ScheduleTask) cron.EntryID {
	if !scheduleUsesCron(sch) {
		return 0
	}
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

// removeCronJob 从 cron 调度器中移除定时任务（幂等；间隔型计划无 cron 条目，
// 直接返回）。
func (s *APIServer) removeCronJob(sid string) {
	if entryID, ok := s.CronJobs[sid]; ok {
		s.Cron.Remove(entryID)
		delete(s.CronJobs, sid)
		s.Logger.Info("cron 任务已移除", zap.String("sid", sid), zap.Int("entryID", int(entryID)))
	}
}

// executeScheduledTask 执行定时任务（旧 cron 兼容入口：scheduledAt 取当前
// 分钟截断，保持历史行为）。
func (s *APIServer) executeScheduledTask(sch model.ScheduleTask) {
	s.executeScheduledTaskAt(sch, time.Now().Truncate(time.Minute))
}

// executeScheduledTaskAt 在指定 scheduledAt 执行定时任务（创建实际的采集任务）。
// 间隔型 worker 传入计划的 next_run_at；cron 兼容入口传入当前分钟。
func (s *APIServer) executeScheduledTaskAt(sch model.ScheduleTask, scheduledAt time.Time) {
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
	if ok, message, _ := s.canStartCollection(CollectionSourceScheduled); !ok {
		now := time.Now()
		_ = s.DB.Model(&model.ScheduleTrigger{}).Where("id = ?", trigger.ID).
			Updates(map[string]interface{}{"status": "skipped_low_disk", "updated_at": now}).Error
		s.Logger.Warn("定时采集因低磁盘跳过", zap.String("sid", sch.SID), zap.String("reason", message))
		return
	}
	// Collection single-flight is deliberately independent from analysis: a
	// completed upload may queue analysis while the next capture starts.
	var active int64
	if err := s.DB.Model(&model.HotmethodTask{}).
		Where("target_ip = ? AND task_kind = ? AND status IN ?", sch.TargetIP, sch.TaskKind,
			[]int{TaskStatusCreated, TaskStatusRunning, TaskStatusUploading}).Count(&active).Error; err != nil {
		s.Logger.Warn("检查定时采集重叠失败", zap.String("sid", sch.SID), zap.Error(err))
		return
	}
	if active > 0 {
		now := time.Now()
		_ = s.DB.Model(&model.ScheduleTrigger{}).Where("id = ?", trigger.ID).
			Updates(map[string]interface{}{"status": "skipped_overlap", "updated_at": now}).Error
		s.Logger.Info("上一轮采集尚未结束，本轮跳过", zap.String("sid", sch.SID), zap.Int64("active", active))
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
// 新任务走"间隔 + 开始时间"：interval_seconds + start_at。
// 旧客户端（cron_expr）仍兼容，仅用于历史路径。
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

	intervalMode := req.IntervalSeconds > 0
	legacyCron := !intervalMode && strings.TrimSpace(req.CronExpr) != ""
	if !intervalMode && !legacyCron {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "请填写采样间隔（interval_seconds），或使用旧式 cron 表达式")
		return
	}

	// 间隔型校验：间隔 >= 1 分钟；采样时长必须小于间隔，否则相邻窗口重叠。
	if intervalMode {
		if req.IntervalSeconds < 60 {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, fmt.Sprintf("采样间隔(%ds)不能小于 1 分钟", req.IntervalSeconds))
			return
		}
		if req.Duration == 0 {
			req.Duration = 10
		}
		if req.Duration >= req.IntervalSeconds {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, fmt.Sprintf("采样时长(%ds)需小于采样间隔(%ds)，否则相邻窗口会重叠", req.Duration, req.IntervalSeconds))
			return
		}
	} else {
		// 旧式 cron：验证 cron 表达式（标准 5 字段：分 时 日 月 周）
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		if _, err := parser.Parse(req.CronExpr); err != nil {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "Cron 表达式无效: "+err.Error())
			return
		}
		if req.Duration == 0 {
			req.Duration = 10
		}
		// 窗口校验：周期深度采样的每次采样必须在窗口周期内结束，否则相邻窗口
		// 会重叠，"回溯任意窗口"的语义就不成立了。
		if req.WindowSeconds > 0 && req.Duration >= req.WindowSeconds {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, fmt.Sprintf("采样时长(%ds)需小于窗口周期(%ds)，否则相邻窗口会重叠", req.Duration, req.WindowSeconds))
			return
		}
	}

	// 设置默认值
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

	sid := "sch-" + util.GenTID()[4:]
	uid := auth.UID
	userName := auth.Name

	now := time.Now()

	// 间隔型：计算首次采样时间；过去/缺省 start_at 对齐到最近未来槽位。
	var startAt *time.Time
	var nextRunAt *time.Time
	if intervalMode {
		if req.StartAt != nil {
			startAt = req.StartAt
		} else {
			startAt = &now
		}
		computed := intervalNextRun(startAt, req.IntervalSeconds, now)
		nextRunAt = &computed
	}

	// 序列化采集参数；间隔型把 interval_seconds/window_seconds 固化进
	// request_params，保证创建后配置不可漂移（列表/详情/重放都以此为准）。
	params := PerfParams{
		TargetPID:       req.TargetPID,
		Duration:        req.Duration,
		Frequency:       req.Frequency,
		Callgraph:       req.Callgraph,
		Event:           req.Event,
		Subprocess:      req.Subprocess,
		PprofURL:        req.PprofURL,
		IntervalSeconds: req.IntervalSeconds,
		WindowSeconds:   req.IntervalSeconds,
	}
	paramsJSON, _ := util.MarshalJSONB(params)

	sch := &model.ScheduleTask{
		SID:             sid,
		Name:            req.Name,
		CronExpr:        req.CronExpr,
		IntervalSeconds: req.IntervalSeconds,
		StartAt:         startAt,
		TaskKind:        req.TaskKind,
		TaskType:        req.TaskType,
		ProfilerType:    req.ProfilerType,
		TargetIP:        req.TargetIP,
		RequestParams:   paramsJSON,
		Enabled:         true,
		UID:             uid,
		UserName:        userName,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	sch.NextRunAt = nextRunAt

	if err := s.DB.Create(sch).Error; err != nil {
		s.Logger.Error("创建定时任务失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建定时任务失败")
		return
	}

	// 旧式 cron 计划注册到进程内 cron；间隔型计划由 DB 轮询 worker 驱动。
	if legacyCron {
		s.addCronJob(*sch)
	}

	s.Logger.Info("定时任务已创建",
		zap.String("sid", sid),
		zap.Uint64("interval_seconds", req.IntervalSeconds),
		zap.String("cron", req.CronExpr),
	)
	sch.CanManage = true
	s.RespondOK(c, sch)
}

// ----------------------------------------------------------
// ListSchedules 获取定时任务列表
// GET /api/v1/schedule/tasks
// 支持过滤与分页：target_ip / keyword / enabled / owner_filter / page / page_size
// ----------------------------------------------------------
func (s *APIServer) ListSchedules(c *gin.Context) {
	auth := s.AuthContext(c)
	query := s.DB.Model(&model.ScheduleTask{}).Order("created_at DESC")

	// 目标 IP 过滤（主机周期 Tab 只展示当前主机的周期任务）
	if targetIP := strings.TrimSpace(c.Query("target_ip")); targetIP != "" {
		query = query.Where("target_ip = ?", targetIP)
	}
	// 关键词：名称 / SID / 目标 IP
	if keyword := strings.TrimSpace(c.Query("keyword")); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where("name LIKE ? OR sid LIKE ? OR target_ip LIKE ?", like, like, like)
	}
	// 启停状态过滤
	if enabled := strings.TrimSpace(c.Query("enabled")); enabled != "" {
		switch enabled {
		case "true":
			query = query.Where("enabled = ?", true)
		case "false":
			query = query.Where("enabled = ?", false)
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.DefaultQuery("owner_filter", "all"))) {
	case "", "all":
	case "mine":
		query = query.Where("uid = ?", auth.UID)
	default:
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "owner_filter 仅支持 all/mine")
		return
	}

	// 分页（默认 50 条；保持旧调用方兼容：不传分页参数时返回全部）
	page := 1
	pageSize := 50
	if p, err := strconv.Atoi(c.DefaultQuery("page", "1")); err == nil && p > 0 {
		page = p
	}
	if ps, err := strconv.Atoi(c.DefaultQuery("page_size", "50")); err == nil && ps > 0 && ps <= 200 {
		pageSize = ps
	}

	var total int64
	query.Count(&total)

	var schedules []model.ScheduleTask
	if err := query.Offset((page - 1) * pageSize).Limit(pageSize).Find(&schedules).Error; err != nil {
		s.Logger.Error("查询定时任务失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询失败")
		return
	}

	if schedules == nil {
		schedules = []model.ScheduleTask{}
	}
	for index := range schedules {
		schedules[index].CanManage = s.canManageOwner(schedules[index].UID, auth)
		schedules[index].NextRunAt = s.scheduleNextRun(schedules[index])
	}

	s.RespondOK(c, gin.H{"schedules": schedules, "total": total, "page": page, "pageSize": pageSize})
}

// ----------------------------------------------------------
// GetScheduleDetail 获取单个定时任务详情
// GET /api/v1/schedule/:sid
// ----------------------------------------------------------
func (s *APIServer) GetScheduleDetail(c *gin.Context) {
	sid := c.Param("sid")
	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", sid).First(&sch).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "定时任务不存在: "+sid)
		return
	}
	sch.CanManage = s.canManageOwner(sch.UID, s.AuthContext(c))
	sch.NextRunAt = s.scheduleNextRun(sch)
	s.RespondOK(c, sch)
}

// scheduleNextRun 计算计划的"下次运行"展示值（仅 enabled 的计划）：
//   - 间隔型：返回持久化的 next_run_at（worker 在每次触发后原子推进，重启
//     后从 DB 恢复，不会漂移）；
//   - 旧 cron 型：用 cron 解析器实时计算。
// 计划停用或无法计算时返回 nil。
func (s *APIServer) scheduleNextRun(sch model.ScheduleTask) *time.Time {
	if !sch.Enabled {
		return nil
	}
	if scheduleUsesInterval(sch) {
		return sch.NextRunAt
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(sch.CronExpr)
	if err != nil {
		return nil
	}
	next := sched.Next(time.Now())
	return &next
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

	// 动态管理 cron 调度器：旧式 cron 计划真正停止/启动；
	// 间隔型计划由 DB 轮询 worker 按 enabled 状态自动拾取/跳过。
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
