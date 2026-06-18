// ============================================================
// server/task.go — 任务管理处理器
// 包含：创建/列表/详情/删除/重试任务
// W1 MVP 阶段：所有接口返回合理的 mock/真实数据
// ============================================================

package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	pb_common "github.com/mini-drop/apiserver/proto/common"
	pb_control "github.com/mini-drop/apiserver/proto/control"
	pb_hotmethod "github.com/mini-drop/apiserver/proto/hotmethod"
	"github.com/mini-drop/apiserver/util"
)

// CreateTaskReq 创建任务请求体
type CreateTaskReq struct {
	Name          string `json:"name" binding:"required"`
	TaskType      uint32 `json:"task_type"`     // 0=通用 1=Java 2=Tracing
	ProfilerType  uint32 `json:"profiler_type"` // 0=perf 1=async-profiler 2=pprof
	TargetIP      string `json:"target_ip" binding:"required"`
	TargetPID     int32  `json:"target_pid"`
	Duration      uint64 `json:"duration"`      // 采集秒数
	Frequency     uint32 `json:"frequency"`     // 采样频率 Hz
	Callgraph     string `json:"callgraph"`     // fp / dwarf / lbr
	Event         string `json:"event"`         // cpu-cycles / cache-misses
	Subprocess    bool   `json:"subprocess"`
	ContainerName string `json:"container_name"`
}

// PerfParams 性能采集参数，会被序列化为 JSONB 存入 request_params 字段
type PerfParams struct {
	TargetPID  int32  `json:"target_pid"`
	Duration   uint64 `json:"duration"`
	Frequency  uint32 `json:"frequency"`
	Callgraph  string `json:"callgraph"`
	Event      string `json:"event"`
	Subprocess bool   `json:"subprocess"`
}

// CreateTask 创建性能采集任务
// POST /api/v1/tasks
func (s *APIServer) CreateTask(c *gin.Context) {
	var req CreateTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "请求参数错误: " + err.Error(),
		})
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

	tid := util.GenTID()
	uid := c.GetHeader("Drop_user_uid")
	if uid == "" {
		uid = "default-user"
	}
	userName := c.GetHeader("Drop_user_name")
	if userName == "" {
		userName = "默认用户"
	}

	// 将性能采集参数序列化为 JSONB（只存采参，不存 Name/TargetIP 等已落列的字段）
	paramsJSON, err := util.MarshalJSONB(PerfParams{
		TargetPID:  req.TargetPID,
		Duration:   req.Duration,
		Frequency:  req.Frequency,
		Callgraph:  req.Callgraph,
		Event:      req.Event,
		Subprocess: req.Subprocess,
	})
	if err != nil {
		s.Logger.Error("序列化任务参数失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "服务器内部错误",
		})
		return
	}

	now := time.Now()

	task := &model.HotmethodTask{
		TID:           tid,
		Name:          req.Name,
		Type:          req.TaskType,
		ProfilerType:  req.ProfilerType,
		TargetIP:      req.TargetIP,
		RequestParams: paramsJSON,
		Status:        0,      // 新建
		StatusInfo:    "任务已创建，等待下发",
		AnalysisStatus: 0,     // 待分析
		UID:           uid,
		UserName:      userName,
		CreateTime:    now,
	}

	if err := s.DB.Create(task).Error; err != nil {
		s.Logger.Error("创建任务失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "创建任务失败: " + err.Error(),
		})
		return
	}

	// W3: 通过 gRPC 下发任务到 drop_server
	if s.GrpcConnected() {
		s.dispatchTask(task, req)
	} else {
		s.Logger.Warn("gRPC 未连接，任务仅写库未下发",
			zap.String("tid", tid),
		)
		// 标记为失败，等待后续手动重试或 cron 重发
		s.DB.Model(task).Updates(map[string]interface{}{
			"status":      3,
			"status_info": "gRPC 未连接，任务无法下发到 drop_server",
		})
	}

	s.Logger.Info("任务创建成功",
		zap.String("tid", tid),
		zap.String("target_ip", req.TargetIP),
		zap.String("name", req.Name),
	)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"tid": tid,
		},
	})
}

// dispatchTask 通过 gRPC 将任务下发到 drop_server
// 如果下发失败，更新数据库状态为失败
func (s *APIServer) dispatchTask(task *model.HotmethodTask, req CreateTaskReq) {
	// 构建 CosConfig（使用配置中的 MinIO 凭证）
	cosCfg := &pb_common.CosConfig{
		Endpoint:        s.Config.Storage.Endpoint,
		AccessKeyId:     s.Config.Storage.AccessKey,
		SecretAccessKey: s.Config.Storage.SecretKey,
		Bucket:          s.Config.Storage.Bucket,
		UseSsl:          s.Config.Storage.UseSSL,
	}

	// 构建 RecordArgv（采集参数）
	recordArgv := &pb_hotmethod.RecordArgv{
		Hz:         req.Frequency,
		Duration:   req.Duration,
		Pid:        req.TargetPID,
		Callgraph:  req.Callgraph,
		Subprocess: req.Subprocess,
		Event:      req.Event,
	}

	// 构建 TaskDesc
	taskDesc := &pb_hotmethod.TaskDesc{
		TaskID:        task.TID,
		TaskType:      req.TaskType,
		ProfilerType:  req.ProfilerType,
		SampleArgv:    recordArgv,
		ContainerName: req.ContainerName,
		TimeoutSec:    uint32(req.Duration + 30), // 多给 30s 上传时间
		CosConfig:     cosCfg,
	}

	// 构建 CreateTaskRequest
	pbReq := &pb_control.CreateTaskRequest{
		TargetIP: req.TargetIP,
		Service:  "hotmethod",
		TaskDesc: taskDesc,
	}

	// 调用 gRPC（带超时）
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(s.Config.GRPC.TimeoutSec)*time.Second)
	defer cancel()

	resp, err := s.ControlCli.CreateTask(ctx, pbReq)
	if err != nil {
		errMsg := fmt.Sprintf("gRPC 下发失败: %v", err)
		s.Logger.Error("任务下发到 drop_server 失败",
			zap.String("tid", task.TID),
			zap.String("target_ip", req.TargetIP),
			zap.Error(err),
		)
		s.DB.Model(task).Updates(map[string]interface{}{
			"status":      3,
			"status_info": errMsg,
		})
		return
	}

	// 下发成功，更新状态为"已下发"
	s.Logger.Info("任务已下发到 drop_server",
		zap.String("tid", task.TID),
		zap.String("grpc_resp_code", fmt.Sprintf("%d", resp.Code)),
	)

	now := time.Now()
	s.DB.Model(task).Updates(map[string]interface{}{
		"status":      1, // 执行中
		"status_info": fmt.Sprintf("已下发到 drop_server, code=%d msg=%s", resp.Code, resp.Msg),
		"begin_time":  &now,
	})
}

// ListTasks 获取任务列表
// GET /api/v1/tasks?page=1&pageSize=20&status=0
func (s *APIServer) ListTasks(c *gin.Context) {
	// 分页参数
	page := 1
	pageSize := 20
	// 简化分页获取（后续可用 query 参数）

	var tasks []model.HotmethodTask
	var total int64

	query := s.DB.Model(&model.HotmethodTask{})

	// 按状态筛选
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	// 按用户筛选（权限控制）
	if uid := c.GetHeader("Drop_user_uid"); uid != "" {
		query = query.Where("uid = ?", uid)
	}

	query.Count(&total)

	offset := (page - 1) * pageSize
	if err := query.Order("create_time DESC").
		Offset(offset).Limit(pageSize).
		Find(&tasks).Error; err != nil {
		s.Logger.Error("查询任务列表失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "查询任务列表失败",
		})
		return
	}

	// 确保返回空数组而不是 null
	if tasks == nil {
		tasks = []model.HotmethodTask{}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"tasks":    tasks,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
}

// GetTaskDetail 获取任务详情
// GET /api/v1/tasks/:tid
func (s *APIServer) GetTaskDetail(c *gin.Context) {
	tid := c.Param("tid")

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "任务不存在: " + tid,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": task,
	})
}

// DeleteTask 软删除任务
// DELETE /api/v1/tasks/:tid
func (s *APIServer) DeleteTask(c *gin.Context) {
	tid := c.Param("tid")

	// 使用 GORM 软删除
	result := s.DB.Where("tid = ?", tid).Delete(&model.HotmethodTask{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "删除任务失败: " + result.Error.Error(),
		})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "任务不存在或已删除: " + tid,
		})
		return
	}

	s.Logger.Info("任务已删除", zap.String("tid", tid))

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "任务已删除",
	})
}

// RetryTask 重试失败的任务（用同参数重新创建）
// POST /api/v1/tasks/:tid/retry
func (s *APIServer) RetryTask(c *gin.Context) {
	tid := c.Param("tid")

	// 查找原任务
	var oldTask model.HotmethodTask
	if err := s.DB.Unscoped().Where("tid = ?", tid).First(&oldTask).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "原任务不存在: " + tid,
		})
		return
	}

	// 用相同参数创建新任务
	newTID := util.GenTID()
	now := time.Now()

	newTask := model.HotmethodTask{
		TID:            newTID,
		Name:           oldTask.Name + "(重试)",
		Type:           oldTask.Type,
		ProfilerType:   oldTask.ProfilerType,
		TargetIP:       oldTask.TargetIP,
		RequestParams:  oldTask.RequestParams,
		Status:         0,
		StatusInfo:     "重试任务，等待下发",
		AnalysisStatus: 0,
		UID:            oldTask.UID,
		UserName:       oldTask.UserName,
		CreateTime:     now,
		MasterTaskTID:  tid, // 记录父任务
	}

	if err := s.DB.Create(&newTask).Error; err != nil {
		s.Logger.Error("重试任务创建失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "重试任务创建失败: " + err.Error(),
		})
		return
	}

	// W3: 重试任务也通过 gRPC 下发
	if s.GrpcConnected() {
		// 从原任务的 request_params 重建采集参数
		var oldParams PerfParams
		if err := util.UnmarshalJSONB(oldTask.RequestParams, &oldParams); err != nil {
			s.Logger.Warn("解析原任务参数失败，使用默认值", zap.Error(err))
		}
		req := CreateTaskReq{
			Name:         newTask.Name,
			TaskType:     newTask.Type,
			ProfilerType: newTask.ProfilerType,
			TargetIP:     newTask.TargetIP,
			TargetPID:    oldParams.TargetPID,
			Duration:     oldParams.Duration,
			Frequency:    oldParams.Frequency,
			Callgraph:    oldParams.Callgraph,
			Event:        oldParams.Event,
			Subprocess:   oldParams.Subprocess,
		}
		s.dispatchTask(&newTask, req)
	} else {
		s.DB.Model(&newTask).Updates(map[string]interface{}{
			"status":      3,
			"status_info": "gRPC 未连接，重试任务无法下发",
		})
	}

	s.Logger.Info("任务重试成功",
		zap.String("old_tid", tid),
		zap.String("new_tid", newTID),
	)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"tid": newTID,
		},
	})
}

// ListCOSFiles 列出任务产物文件
// GET /api/v1/cosfiles?tid=xxx
func (s *APIServer) ListCOSFiles(c *gin.Context) {
	tid := c.Query("tid")
	if tid == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "缺少 tid 参数",
		})
		return
	}

	// MVP 阶段：返回空列表，后续对接 MinIO
	s.Logger.Info("查询产物文件", zap.String("tid", tid))

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"files": []interface{}{},
		},
	})
}
