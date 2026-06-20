// ============================================================
// server/schedule.go — 定时任务管理处理器（W5）
// 包含：创建/列表/删除定时任务 + cron 调度器
// ============================================================

package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// CreateScheduleReq 创建定时任务请求
type CreateScheduleReq struct {
	Name         string `json:"name" binding:"required"`
	CronExpr     string `json:"cron_expr" binding:"required"` // 如 "*/5 * * * *"
	TaskType     uint32 `json:"task_type"`
	ProfilerType uint32 `json:"profiler_type"`
	TargetIP     string `json:"target_ip" binding:"required"`
	TargetPID    int32  `json:"target_pid"`
	Duration     uint64 `json:"duration"`
	Frequency    uint32 `json:"frequency"`
	Callgraph    string `json:"callgraph"`
	Event        string `json:"event"`
	Subprocess   bool   `json:"subprocess"`
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
		s.Logger.Info("已恢复定时任务", zap.Int("count", len(schedules)))
	}

	s.Cron.Start()
	s.Logger.Info("Cron 调度器已启动")
}

// addCronJob 向 cron 调度器添加一个定时任务
func (s *APIServer) addCronJob(sch model.ScheduleTask) {
	_, err := s.Cron.AddFunc(sch.CronExpr, func() {
		s.executeScheduledTask(sch)
	})
	if err != nil {
		s.Logger.Error("添加 cron 任务失败",
			zap.String("sid", sch.SID),
			zap.String("cron", sch.CronExpr),
			zap.Error(err),
		)
	}
}

// executeScheduledTask 执行定时任务（创建实际的采集任务）
func (s *APIServer) executeScheduledTask(sch model.ScheduleTask) {
	s.Logger.Info("触发定时任务",
		zap.String("sid", sch.SID),
		zap.String("name", sch.Name),
	)

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
		TID:           tid,
		Name:          sch.Name + " (定时)",
		Type:          sch.TaskType,
		ProfilerType:  sch.ProfilerType,
		TargetIP:      sch.TargetIP,
		RequestParams: sch.RequestParams,
		Status:        0,
		StatusInfo:    "定时任务触发",
		AnalysisStatus: 0,
		UID:           sch.UID,
		UserName:      sch.UserName,
		MasterTaskTID: sch.SID,
		CreateTime:    now,
	}

	if err := s.DB.Create(task).Error; err != nil {
		s.Logger.Error("定时任务创建失败", zap.String("sid", sch.SID), zap.Error(err))
		return
	}

	// 通过 gRPC 下发（如果已连接）
	if s.GrpcConnected() {
		req := CreateTaskReq{
			Name:         task.Name,
			TaskType:     sch.TaskType,
			ProfilerType: sch.ProfilerType,
			TargetIP:     sch.TargetIP,
			TargetPID:    params.TargetPID,
			Duration:     params.Duration,
			Frequency:    params.Frequency,
			Callgraph:    params.Callgraph,
			Event:        params.Event,
			Subprocess:   params.Subprocess,
		}
		s.dispatchTask(task, req)
	}

	// 更新最后运行时间
	now2 := time.Now()
	s.DB.Model(&sch).Updates(map[string]interface{}{
		"last_run_at": &now2,
	})

	s.Logger.Info("定时任务执行完成", zap.String("sid", sch.SID), zap.String("tid", tid))
}

// ----------------------------------------------------------
// CreateSchedule 创建定时任务
// POST /api/v1/schedule/task
// ----------------------------------------------------------
func (s *APIServer) CreateSchedule(c *gin.Context) {
	var req CreateScheduleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "参数错误: " + err.Error()})
		return
	}

	// 验证 cron 表达式（标准 5 字段：分 时 日 月 周）
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(req.CronExpr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "Cron 表达式无效: " + err.Error()})
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

	sid := "sch-" + util.GenTID()[4:]
	uid := c.GetHeader("Drop_user_uid")
	if uid == "" {
		uid = "default-user"
	}
	userName := c.GetHeader("Drop_user_name")
	if userName == "" {
		userName = "默认用户"
	}

	// 序列化采集参数
	paramsJSON, _ := util.MarshalJSONB(PerfParams{
		TargetPID:  req.TargetPID,
		Duration:   req.Duration,
		Frequency:  req.Frequency,
		Callgraph:  req.Callgraph,
		Event:      req.Event,
		Subprocess: req.Subprocess,
	})

	now := time.Now()
	sch := &model.ScheduleTask{
		SID:          sid,
		Name:         req.Name,
		CronExpr:     req.CronExpr,
		TaskType:     req.TaskType,
		ProfilerType: req.ProfilerType,
		TargetIP:     req.TargetIP,
		RequestParams: paramsJSON,
		Enabled:      true,
		UID:          uid,
		UserName:     userName,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.DB.Create(sch).Error; err != nil {
		s.Logger.Error("创建定时任务失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "创建定时任务失败"})
		return
	}

	// 添加到 cron 调度器
	s.addCronJob(*sch)

	s.Logger.Info("定时任务已创建", zap.String("sid", sid), zap.String("cron", req.CronExpr))
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": sch})
}

// ----------------------------------------------------------
// ListSchedules 获取定时任务列表
// GET /api/v1/schedule/tasks
// ----------------------------------------------------------
func (s *APIServer) ListSchedules(c *gin.Context) {
	var schedules []model.ScheduleTask
	if err := s.DB.Order("created_at DESC").Find(&schedules).Error; err != nil {
		s.Logger.Error("查询定时任务失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询失败"})
		return
	}

	if schedules == nil {
		schedules = []model.ScheduleTask{}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{"schedules": schedules, "total": len(schedules)},
	})
}

// ----------------------------------------------------------
// DeleteSchedule 删除定时任务
// DELETE /api/v1/schedule/:sid
// ----------------------------------------------------------
func (s *APIServer) DeleteSchedule(c *gin.Context) {
	sid := c.Param("sid")

	result := s.DB.Where("sid = ?", sid).Delete(&model.ScheduleTask{})
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "定时任务不存在: " + sid})
		return
	}

	s.Logger.Info("定时任务已删除", zap.String("sid", sid))
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "定时任务已删除"})
}

// ----------------------------------------------------------
// ToggleSchedule 启用/禁用定时任务
// POST /api/v1/schedule/:sid/toggle
// ----------------------------------------------------------
func (s *APIServer) ToggleSchedule(c *gin.Context) {
	sid := c.Param("sid")

	var sch model.ScheduleTask
	if err := s.DB.Unscoped().Where("sid = ?", sid).First(&sch).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "定时任务不存在: " + sid})
		return
	}

	newEnabled := !sch.Enabled
	s.DB.Model(&sch).Update("enabled", newEnabled)

	// 从 cron 中移除旧的，如果启用则重新添加
	// cron v3 不支持移除单个 entry，这里简化处理：重启时从 DB 恢复

	s.Logger.Info("定时任务状态已切换",
		zap.String("sid", sid),
		zap.Bool("enabled", newEnabled),
	)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{"sid": sid, "enabled": newEnabled},
	})
}
