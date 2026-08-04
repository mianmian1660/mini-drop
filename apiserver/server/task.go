// ============================================================
// server/task.go — 任务管理处理器
// 包含：创建/列表/详情/删除/重试任务
// W1 MVP 阶段：所有接口返回合理的 mock/真实数据
// ============================================================

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/pkg/storage"
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
	Duration      uint64 `json:"duration"`  // 采集秒数
	Frequency     uint32 `json:"frequency"` // 采样频率 Hz
	Callgraph     string `json:"callgraph"` // fp / dwarf / lbr
	Event         string `json:"event"`     // cpu-cycles / cache-misses
	Subprocess    bool   `json:"subprocess"`
	ContainerName string `json:"container_name"`
	PprofURL      string `json:"pprof_url"`
}

// PerfParams 性能采集参数，会被序列化为 JSONB 存入 request_params 字段
type PerfParams struct {
	TargetPID  int32  `json:"target_pid"`
	Duration   uint64 `json:"duration"`
	Frequency  uint32 `json:"frequency"`
	Callgraph  string `json:"callgraph"`
	Event      string `json:"event"`
	Subprocess bool   `json:"subprocess"`
	PprofURL   string `json:"pprof_url"`
}

const (
	ProfilerPerf  uint32 = 0
	ProfilerAsync uint32 = 1
	ProfilerPprof uint32 = 2
	ProfilerBPF   uint32 = 3

	TaskTypeGeneric uint32 = 0
	TaskTypeJava    uint32 = 1
	TaskTypePprof   uint32 = 2
	TaskTypeBPF     uint32 = 5
)

// normalizeAndValidateCollector keeps the public REST contract and the agent
// contract aligned.  In particular it prevents pprof/Java jobs from silently
// being persisted as generic perf jobs and analysed by the wrong pipeline.
func normalizeAndValidateCollector(req *CreateTaskReq) error {
	switch req.ProfilerType {
	case ProfilerPerf:
		req.TaskType = TaskTypeGeneric
		if req.Event == "" {
			req.Event = "cpu-clock"
		}
	case ProfilerAsync:
		req.TaskType = TaskTypeJava
		if req.TargetPID <= 0 {
			return fmt.Errorf("async-profiler 必须指定 Java 目标 PID")
		}
		if req.Event == "" {
			req.Event = "cpu"
		}
	case ProfilerPprof:
		req.TaskType = TaskTypePprof
		// Compatibility: old jobs stored a full endpoint in event.
		if req.PprofURL == "" && (strings.HasPrefix(req.Event, "http://") || strings.HasPrefix(req.Event, "https://")) {
			req.PprofURL = req.Event
		}
		parsed, err := url.ParseRequestURI(req.PprofURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("pprof_url 必须是可访问的 http/https 完整 URL")
		}
		req.Event = ""
	case ProfilerBPF:
		req.TaskType = TaskTypeBPF
		if req.Event == "" {
			req.Event = "cpu"
		}
		if req.Event != "cpu" && req.Event != "io" && req.Event != "sched" {
			return fmt.Errorf("eBPF event 仅支持 cpu、io 或 sched")
		}
	default:
		return fmt.Errorf("不支持的 profiler_type=%d", req.ProfilerType)
	}
	if req.Duration == 0 || req.Duration > 3600 {
		return fmt.Errorf("采样时长需为 1-3600 秒")
	}
	if req.Frequency == 0 || req.Frequency > 10000 {
		return fmt.Errorf("采样频率需为 1-10000 Hz")
	}
	return nil
}

// TaskResultNotifyReq 是 drop_server 完成采集后回调 apiserver 的内部请求体。
type TaskResultNotifyReq struct {
	TaskID       string `json:"task_id" binding:"required"`
	ErrorMessage string `json:"error_message"`
	CosKey       string `json:"cos_key"`
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
	if err := normalizeAndValidateCollector(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}

	uid := getRequestUIDOrDefault(c)
	userName := getRequestUserName(c)

	// A2: Idempotency-Key 去重（4.2 节）。同一用户携带相同幂等键重复提交，
	// 直接返回已存在的任务，不重复创建、不重复下发。
	var idempotencyKey *string
	if raw := strings.TrimSpace(c.GetHeader("Idempotency-Key")); raw != "" {
		idempotencyKey = &raw

		var existing model.HotmethodTask
		err := s.DB.Where("uid = ? AND idempotency_key = ?", uid, raw).First(&existing).Error
		if err == nil {
			c.JSON(http.StatusOK, gin.H{
				"code": 0,
				"data": gin.H{
					"tid":      existing.TID,
					"replayed": true,
				},
			})
			return
		}
		if err != gorm.ErrRecordNotFound {
			s.Logger.Error("查询幂等键失败", zap.String("idempotency_key", raw), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{
				"code":    500,
				"message": "服务器内部错误",
			})
			return
		}
	}

	tid := util.GenTID()

	// 将性能采集参数序列化为 JSONB（只存采参，不存 Name/TargetIP 等已落列的字段）
	paramsJSON, err := util.MarshalJSONB(PerfParams{
		TargetPID:  req.TargetPID,
		Duration:   req.Duration,
		Frequency:  req.Frequency,
		Callgraph:  req.Callgraph,
		Event:      req.Event,
		Subprocess: req.Subprocess,
		PprofURL:   req.PprofURL,
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
		TID:            tid,
		Name:           req.Name,
		Type:           req.TaskType,
		ProfilerType:   req.ProfilerType,
		TargetIP:       req.TargetIP,
		RequestParams:  paramsJSON,
		Status:         0, // 新建
		StatusInfo:     "任务已创建，等待下发",
		AnalysisStatus: 0, // 待分析
		UID:            uid,
		UserName:       userName,
		IdempotencyKey: idempotencyKey,
		CreateTime:     now,
	}

	// A5: Task 和"需要下发"这件事在同一事务里落地（新复刻指南 9.6 节 Transactional
	// Outbox），替代之前"写库后立刻在本次请求里同步调 gRPC"的非事务模式。
	// 真正的 gRPC 下发交给后台 dispatchOutboxLoop 异步执行，HTTP 响应不用等 gRPC 往返。
	if err := s.createTaskWithOutbox(task, req); err != nil {
		// A2: 并发下两个携带相同幂等键的请求可能都通过了前面的查重，
		// 只有一个能真正插入成功；另一个会撞在 (uid, idempotency_key) 唯一索引上。
		// 这种情况下不报错，改为查出已创建成功的那条任务返回，保证"只有一个 Task"。
		if idempotencyKey != nil && isUniqueViolation(err) {
			var existing model.HotmethodTask
			if lookupErr := s.DB.Where("uid = ? AND idempotency_key = ?", uid, *idempotencyKey).First(&existing).Error; lookupErr == nil {
				c.JSON(http.StatusOK, gin.H{
					"code": 0,
					"data": gin.H{
						"tid":      existing.TID,
						"replayed": true,
					},
				})
				return
			}
		}
		s.Logger.Error("创建任务失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "创建任务失败: " + err.Error(),
		})
		return
	}
	s.recordTaskStatusEvent(task.TID, -1, TaskStatusCreated, "任务已创建，等待下发", "apiserver")

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

// createTaskWithOutbox 在同一事务内写入 HotmethodTask 和对应的 Outbox 下发记录（A5，
// 新复刻指南 9.6 节 Transactional Outbox）。task/req 必须已经填好全部字段。
// 真正的 gRPC 下发不在这里做，交给后台 dispatchOutboxLoop 异步领取执行。
func (s *APIServer) createTaskWithOutbox(task *model.HotmethodTask, req CreateTaskReq) error {
	payload, err := util.MarshalJSONB(req)
	if err != nil {
		return err
	}
	outboxEntry := &model.Outbox{
		Aggregate:   model.OutboxAggregateTask,
		AggregateID: task.TID,
		Event:       model.OutboxEventDispatchTask,
		Payload:     payload,
		CreatedAt:   time.Now(),
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(task).Error; err != nil {
			return err
		}
		return tx.Create(outboxEntry).Error
	})
}

// dispatchTask 通过 gRPC 将任务下发到 drop_server
// 如果下发失败，更新数据库状态为失败
func (s *APIServer) dispatchTask(task *model.HotmethodTask, req CreateTaskReq, trigger string) error {
	attempt, err := s.startTaskAttempt(task, trigger)
	if err != nil {
		return fmt.Errorf("创建任务尝试记录: %w", err)
	}
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
		PprofUrl:      req.PprofURL,
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
		s.finishTaskAttempt(attempt.ID, ErrCodeDependencyUnavailable, errMsg)
		return fmt.Errorf("%s", errMsg)
	}
	if resp.GetCode() != 0 {
		errMsg := fmt.Sprintf("drop_server 拒绝任务: code=%d msg=%s", resp.GetCode(), resp.GetMsg())
		s.finishTaskAttempt(attempt.ID, ErrCodeTaskExecutionFailed, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	// 下发成功，更新状态为"已下发"
	s.Logger.Info("任务已下发到 drop_server",
		zap.String("tid", task.TID),
		zap.String("grpc_resp_code", fmt.Sprintf("%d", resp.Code)),
	)

	now := time.Now()
	_ = s.transitionTaskStatus(task, TaskStatusRunning,
		fmt.Sprintf("已下发到 drop_server, code=%d msg=%s", resp.Code, resp.Msg),
		"apiserver",
		map[string]interface{}{"begin_time": &now},
	)
	return nil
}

// NotifyTaskResult 接收 drop_server 的采集完成通知。
// POST /api/v1/internal/task-notify
func (s *APIServer) NotifyTaskResult(c *gin.Context) {
	var req TaskResultNotifyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "请求参数错误: " + err.Error(),
		})
		return
	}

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", req.TaskID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "任务不存在: " + req.TaskID,
		})
		return
	}

	if strings.TrimSpace(req.ErrorMessage) != "" {
		endTime := time.Now()
		s.finishLatestTaskAttempt(task.TID, ErrCodeTaskExecutionFailed, req.ErrorMessage, nil)
		if task.Status != TaskStatusFailed {
			_ = s.transitionTaskStatus(
				&task,
				TaskStatusFailed,
				formatErrorReason(ErrCodeTaskExecutionFailed, req.ErrorMessage),
				"drop_server_notify",
				map[string]interface{}{"end_time": &endTime, "analysis_status": 3},
			)
		} else {
			_ = s.DB.Model(&model.HotmethodTask{}).
				Where("tid = ?", task.TID).
				Update("analysis_status", 3).Error
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"tid": task.TID}})
		return
	}

	if strings.TrimSpace(req.CosKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "采集成功通知缺少 cos_key",
		})
		return
	}

	endTime := time.Now()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if task.Status != TaskStatusDone {
			fromStatus := task.Status
			if err := tx.Model(&model.HotmethodTask{}).
				Where("tid = ?", task.TID).
				Updates(map[string]interface{}{
					"status":      TaskStatusDone,
					"status_info": "采集产物已上传，任务完成",
					"end_time":    &endTime,
				}).Error; err != nil {
				return err
			}
			if err := tx.Create(&model.TaskStatusEvent{
				TID:        task.TID,
				FromStatus: fromStatus,
				ToStatus:   TaskStatusDone,
				Reason:     "采集产物已上传，任务完成",
				Source:     "drop_server_notify",
				CreatedAt:  endTime,
			}).Error; err != nil {
				return err
			}
		}
		if err := s.ensureAnalysisQueuedTx(tx, task.TID, req.CosKey, 0); err != nil {
			return err
		}
		var artifact model.Artifact
		if err := tx.Where("task_tid = ? AND kind = ? AND object_key = ?", task.TID, model.ArtifactKindRaw, req.CosKey).First(&artifact).Error; err == nil {
			return s.finishLatestTaskAttemptTx(tx, task.TID, "", "", []string{req.CosKey}, artifact.ID)
		}
		return s.finishLatestTaskAttemptTx(tx, task.TID, "", "", []string{req.CosKey}, 0)
	})
	if err != nil {
		s.Logger.Error("处理采集结果通知失败",
			zap.String("tid", task.TID),
			zap.String("cos_key", req.CosKey),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "处理采集结果通知失败: " + err.Error(),
		})
		return
	}

	s.Logger.Info("采集结果已登记并进入分析队列",
		zap.String("tid", task.TID),
		zap.String("cos_key", req.CosKey),
		zap.String("source", "drop_server_notify"),
	)
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"tid": task.TID}})
}

func (s *APIServer) ensureAnalysisQueued(tid string, objectKey string, size int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		return s.ensureAnalysisQueuedTx(tx, tid, objectKey, size)
	})
}

func (s *APIServer) ensureAnalysisQueuedTx(tx *gorm.DB, tid string, objectKey string, size int64) error {
	objectKey = strings.TrimSpace(objectKey)
	if tid == "" || objectKey == "" {
		return nil
	}

	artifact := model.Artifact{
		TaskTID:     tid,
		Kind:        model.ArtifactKindRaw,
		ObjectKey:   objectKey,
		Size:        size,
		ContentType: mimeType(objectKey),
		Status:      model.ArtifactStatusReady,
		CreatedAt:   time.Now(),
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "task_tid"},
			{Name: "kind"},
			{Name: "object_key"},
		},
		DoNothing: true,
	}).Create(&artifact).Error; err != nil {
		return err
	}

	job := model.AnalysisJob{
		TaskTID:   tid,
		Pipeline:  analysisPipelineForObject(objectKey),
		Status:    model.AnalysisJobStatusPending,
		Attempt:   0,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "task_tid"}},
		DoNothing: true,
	}).Create(&job).Error; err != nil {
		return err
	}
	return nil
}

func analysisPipelineForObject(objectKey string) string {
	switch {
	case strings.HasSuffix(objectKey, ".bpf"):
		return "bpf_histogram"
	default:
		return "perf_flamegraph"
	}
}

func (s *APIServer) findRawCollectionArtifact(tid string) (string, int64, bool) {
	if tid == "" {
		return "", 0, false
	}
	if s.StorageConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		objects, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, tid+"/")
		if err != nil {
			s.Logger.Warn("检查任务产物失败", zap.String("tid", tid), zap.Error(err))
		} else if key, size, ok := pickRawCollectionObject(objects); ok {
			return key, size, true
		}
	}
	return pickRawCollectionLocalFile(s.listLocalFiles(tid))
}

func pickRawCollectionObject(files []storage.FileInfo) (string, int64, bool) {
	for _, file := range files {
		if filepath.Base(file.Name) == "perf.data" {
			return file.Name, file.Size, true
		}
	}
	for _, file := range files {
		if isRawCollectionName(file.Name) {
			return file.Name, file.Size, true
		}
	}
	return "", 0, false
}

func pickRawCollectionLocalFile(files []map[string]interface{}) (string, int64, bool) {
	for _, file := range files {
		name, _ := file["name"].(string)
		if name == "" {
			continue
		}
		size := int64(0)
		switch v := file["size"].(type) {
		case int64:
			size = v
		case int:
			size = int64(v)
		case float64:
			size = int64(v)
		}
		if strings.HasSuffix(name, "_perf.data") || filepath.Base(name) == "perf.data" {
			return name, size, true
		}
	}
	for _, file := range files {
		name, _ := file["name"].(string)
		if name == "" || !isRawCollectionName(name) {
			continue
		}
		size := int64(0)
		switch v := file["size"].(type) {
		case int64:
			size = v
		case int:
			size = int64(v)
		case float64:
			size = int64(v)
		}
		return name, size, true
	}
	return "", 0, false
}

func isRawCollectionName(name string) bool {
	base := filepath.Base(strings.ToLower(name))
	switch filepath.Ext(base) {
	case ".svg", ".json", ".md", ".txt", ".html":
		return false
	}
	return strings.Contains(base, "perf") ||
		strings.HasSuffix(base, ".data") ||
		strings.HasSuffix(base, ".bpf") ||
		strings.HasSuffix(base, ".collapsed")
}

// ListTasks 获取任务列表（支持分页、搜索、状态筛选）
// GET /api/v1/tasks?page=1&pageSize=20&status=0&keyword=xxx
func (s *APIServer) ListTasks(c *gin.Context) {
	// 分页参数（从 query string 解析，带默认值）
	page := 1
	pageSize := 20
	if p, err := strconv.Atoi(c.DefaultQuery("page", "1")); err == nil && p > 0 {
		page = p
	}
	if ps, err := strconv.Atoi(c.DefaultQuery("pageSize", "20")); err == nil && ps > 0 && ps <= 100 {
		pageSize = ps
	}

	var tasks []model.HotmethodTask
	var total int64

	query := s.DB.Model(&model.HotmethodTask{})

	// 按关键词搜索（任务名称 / 任务 ID）
	if keyword := c.Query("keyword"); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where("name LIKE ? OR tid LIKE ? OR target_ip LIKE ?", like, like, like)
	}

	// 按状态筛选
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	// 按用户筛选（权限控制）
	if uid := getRequestUID(c); uid != "" {
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

// GetTaskDetail 获取任务详情（含产物下载链接 + TopN 热点数据）
// GET /api/v1/tasks/:tid
//
// A4: 加 uid 过滤，防止越权访问他人任务。CheckLogin 中间件保证 uid 非空。
// 查不到时统一返回"任务不存在"（不区分"确实不存在"和"存在但不是你的"），
// 避免向未授权用户泄露 tid 是否存在。
func (s *APIServer) GetTaskDetail(c *gin.Context) {
	tid := c.Param("tid")
	uid := getRequestUID(c)

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ? AND uid = ?", tid, uid).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "任务不存在: " + tid,
		})
		return
	}

	result := gin.H{"task": taskDetailResponse(task)}
	files := []map[string]interface{}{}
	var topFuncs []map[string]interface{}
	var bpfData map[string]interface{}
	var suggestions []map[string]interface{}
	statusEvents := s.fetchTaskStatusEvents(tid)
	attempts := s.fetchTaskAttempts(tid)
	artifacts := s.fetchArtifacts(tid)

	// W4: 优先从对象存储列出产物，存储不可用或无产物时回退本地目录。
	if s.StorageConnected() {
		storageFiles, err := s.listTaskFiles(tid)
		if err != nil {
			s.Logger.Warn("列出任务文件失败", zap.String("tid", tid), zap.Error(err))
		} else {
			files = storageFiles

			// 尝试从 MinIO 读取 top.json → TopN 热点数据
			topFuncs = s.fetchTopFunctions(tid)
			bpfData = s.fetchBPFData(tid)
			suggestions = s.fetchSuggestions(tid)
		}
	}
	if len(files) == 0 {
		files = s.listLocalFiles(tid)
		if len(topFuncs) == 0 {
			topFuncs = s.fetchLocalTopFunctions(tid)
		}
		if bpfData == nil {
			bpfData = s.fetchLocalBPFData(tid)
		}
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchLocalSuggestions(tid)
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchDBSuggestions(tid)
	}
	if len(topFuncs) > 0 {
		result["top_functions"] = topFuncs
	}
	if bpfData != nil {
		result["bpf_histogram"] = bpfData
	}
	if len(suggestions) > 0 {
		result["suggestions"] = suggestions
	}
	result["status_events"] = statusEvents
	result["attempts"] = attempts
	result["artifacts"] = artifacts
	result["files"] = files

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": result,
	})
}

func taskDetailResponse(task model.HotmethodTask) gin.H {
	var params map[string]interface{}
	if len(task.RequestParams) > 0 {
		_ = json.Unmarshal(task.RequestParams, &params)
	}

	return gin.H{
		"id":              task.ID,
		"tid":             task.TID,
		"name":            task.Name,
		"type":            task.Type,
		"profiler_type":   task.ProfilerType,
		"target_ip":       task.TargetIP,
		"request_params":  params,
		"status":          task.Status,
		"status_info":     task.StatusInfo,
		"analysis_status": task.AnalysisStatus,
		"uid":             task.UID,
		"user_name":       task.UserName,
		"create_time":     task.CreateTime,
		"begin_time":      task.BeginTime,
		"end_time":        task.EndTime,
		"master_task_tid": task.MasterTaskTID,
	}
}

// fetchLocalTopFunctions 从 /tmp/drop-output/{tid}_top.json 读取 TopN
func (s *APIServer) fetchLocalTopFunctions(tid string) []map[string]interface{} {
	localPath := filepath.Join("/tmp/drop-output", tid+"_top.json")
	f, err := os.Open(localPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var topData map[string]interface{}
	if err := json.NewDecoder(f).Decode(&topData); err != nil {
		return nil
	}

	return normalizeTopFunctions(topData)
}

// fetchTopFunctions 从 MinIO 读取 {tid}/top.json 并解析 TopN
func (s *APIServer) fetchTopFunctions(tid string) []map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bucket := s.Config.Storage.Bucket
	key := tid + "/top.json"

	// 尝试读取文件内容
	reader, err := s.Storage.GetObject(ctx, bucket, key)
	if err != nil {
		return nil
	}
	defer reader.Close()

	var topData map[string]interface{}
	if err := json.NewDecoder(reader).Decode(&topData); err != nil {
		return nil
	}

	return normalizeTopFunctions(topData)
}

func normalizeTopFunctions(topData map[string]interface{}) []map[string]interface{} {
	sampleUnit, _ := topData["sample_unit"].(string)
	sampleKind, _ := topData["sample_kind"].(string)
	sourceFormat, _ := topData["source_format"].(string)
	collector, _ := topData["collector"].(string)
	for _, key := range []string{"self_time_top", "top_functions", "inclusive_time_top"} {
		items, ok := topData[key].([]interface{})
		if !ok {
			continue
		}
		funcs := make([]map[string]interface{}, 0, len(items))
		for _, item := range items {
			if m, ok := item.(map[string]interface{}); ok {
				if sampleUnit != "" {
					m["sample_unit"] = sampleUnit
				}
				if sampleKind != "" {
					m["sample_kind"] = sampleKind
				}
				if sourceFormat != "" {
					m["source_format"] = sourceFormat
				}
				if collector != "" {
					m["collector"] = collector
				}
				funcs = append(funcs, m)
			}
		}
		if len(funcs) > 0 {
			return funcs
		}
	}
	return nil
}

// fetchBPFData 从 MinIO 读取 {tid}/bpf_data.json，给前端展示直方图摘要和桶表。
func (s *APIServer) fetchBPFData(tid string) map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, tid+"/bpf_data.json")
	if err != nil {
		return nil
	}
	defer reader.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(reader).Decode(&data); err != nil {
		return nil
	}
	return data
}

func (s *APIServer) fetchLocalBPFData(tid string) map[string]interface{} {
	localPath := filepath.Join("/tmp/drop-output", tid+"_bpf_data.json")
	f, err := os.Open(localPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil
	}
	return data
}

// fetchSuggestions 从 MinIO 读取 {tid}/suggestions.json 并返回规则建议列表。
func (s *APIServer) fetchSuggestions(tid string) []map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, tid+"/suggestions.json")
	if err != nil {
		return nil
	}
	defer reader.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(reader).Decode(&data); err != nil {
		return nil
	}
	return normalizeSuggestions(data)
}

func (s *APIServer) fetchLocalSuggestions(tid string) []map[string]interface{} {
	localPath := filepath.Join("/tmp/drop-output", tid+"_suggestions.json")
	f, err := os.Open(localPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil
	}
	return normalizeSuggestions(data)
}

func (s *APIServer) fetchDBSuggestions(tid string) []map[string]interface{} {
	var rows []model.AnalysisSuggestion
	if err := s.DB.Where("tid = ?", tid).Order("id ASC").Find(&rows).Error; err != nil {
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	suggestions := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		suggestions = append(suggestions, map[string]interface{}{
			"function":      row.Func,
			"advice":        row.Suggestion,
			"ai_suggestion": row.AISuggestion,
			"status":        row.Status,
		})
	}
	return suggestions
}

func normalizeSuggestions(data map[string]interface{}) []map[string]interface{} {
	items, ok := data["suggestions"].([]interface{})
	if !ok {
		return nil
	}

	suggestions := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok {
			suggestions = append(suggestions, m)
		}
	}
	return suggestions
}

func (s *APIServer) fetchTaskStatusEvents(tid string) []model.TaskStatusEvent {
	var events []model.TaskStatusEvent
	if err := s.DB.Where("tid = ?", tid).Order("created_at ASC, id ASC").Find(&events).Error; err != nil {
		return []model.TaskStatusEvent{}
	}
	if events == nil {
		return []model.TaskStatusEvent{}
	}
	return events
}

func (s *APIServer) fetchTaskAttempts(tid string) []model.TaskAttempt {
	var attempts []model.TaskAttempt
	if err := s.DB.Where("task_tid = ?", tid).Order("attempt_seq ASC").Find(&attempts).Error; err != nil || attempts == nil {
		return []model.TaskAttempt{}
	}
	return attempts
}

func (s *APIServer) fetchArtifacts(tid string) []model.Artifact {
	var artifacts []model.Artifact
	if err := s.DB.Where("task_tid = ?", tid).Order("created_at ASC, id ASC").Find(&artifacts).Error; err != nil || artifacts == nil {
		return []model.Artifact{}
	}
	return artifacts
}

// DeleteTask 软删除任务
// DELETE /api/v1/tasks/:tid
// A4: 加 uid 过滤，防止越权删除他人任务。
func (s *APIServer) DeleteTask(c *gin.Context) {
	tid := c.Param("tid")
	uid := getRequestUID(c)

	// 使用 GORM 软删除
	result := s.DB.Where("tid = ? AND uid = ?", tid, uid).Delete(&model.HotmethodTask{})
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
// A4: 加 uid 过滤，防止用他人任务的 tid 发起重试（越权发起新的采集）。
// 这个函数没有在 A4 原定文件清单里被单独点名，但和 GetTaskDetail/DeleteTask
// 是同一类"按 tid 直查，不校验 uid"的漏洞，放着不管等于没修完，顺手一起补上。
func (s *APIServer) RetryTask(c *gin.Context) {
	tid := c.Param("tid")
	uid := getRequestUID(c)

	// 查找原任务
	var oldTask model.HotmethodTask
	if err := s.DB.Unscoped().Where("tid = ? AND uid = ?", tid, uid).First(&oldTask).Error; err != nil {
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

	// 从原任务的 request_params 重建采集参数，构造下发用的请求体
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
		PprofURL:     oldParams.PprofURL,
	}

	// A5: 同事务写 Task + Outbox，下发交给后台 dispatchOutboxLoop 异步执行
	if err := s.createTaskWithOutbox(&newTask, req); err != nil {
		s.Logger.Error("重试任务创建失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "重试任务创建失败: " + err.Error(),
		})
		return
	}
	s.recordTaskStatusEvent(newTask.TID, -1, TaskStatusCreated, "重试任务已创建，等待下发", "apiserver")

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

// ListCOSFiles 列出任务产物文件并提供签名下载链接
// GET /api/v1/cosfiles?tid=xxx
// W4: MinIO 不可用时回退到本地文件系统
func (s *APIServer) ListCOSFiles(c *gin.Context) {
	tid := c.Query("tid")
	if tid == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "缺少 tid 参数",
		})
		return
	}

	var files []map[string]interface{}
	var notice string

	// 优先 MinIO，不可用时回退到本地文件
	if s.StorageConnected() {
		var err error
		files, err = s.listTaskFiles(tid)
		if err != nil {
			s.Logger.Error("列出任务文件失败", zap.String("tid", tid), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{
				"code":    500,
				"message": "列出文件失败: " + err.Error(),
			})
			return
		}
	} else {
		// W4: MinIO 不可用 → 扫描本地输出目录
		files = s.listLocalFiles(tid)
		if len(files) > 0 {
			notice = "使用本地文件（MinIO 未连接）"
		} else {
			notice = "对象存储未连接，本地也无产物文件"
		}
	}

	response := gin.H{
		"files": files,
		"total": len(files),
	}
	if notice != "" {
		response["notice"] = notice
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": response,
	})
}

// listLocalFiles 列出本地输出目录中的产物文件（MinIO 降级方案）
// 本地目录: /tmp/drop-output/
// 文件命名: <tid>_flamegraph.svg, <tid>_top.json 等
func (s *APIServer) listLocalFiles(tid string) []map[string]interface{} {
	localDir := "/tmp/drop-output"
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return []map[string]interface{}{}
	}

	prefix := tid + "_"
	var files []map[string]interface{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		if !strings.HasPrefix(filename, prefix) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// 下载走 attachment，避免浏览器直接打开文本/JSON/SVG。
		downloadURL := fmt.Sprintf("/api/v1/files/%s?download=1", url.PathEscape(filename))
		viewURL := fmt.Sprintf("/api/v1/files/%s", url.PathEscape(filename))

		fileInfo := map[string]interface{}{
			"name":          filename,
			"size":          info.Size(),
			"last_modified": info.ModTime(),
			"content_type":  mimeType(filename),
			"download_url":  downloadURL,
			"source":        "local",
		}
		if filepath.Ext(filename) == ".svg" {
			fileInfo["view_url"] = viewURL
		}
		files = append(files, fileInfo)
	}

	return files
}

// ServeLocalFile 提供本地文件下载（MinIO 降级方案）
// GET /api/v1/files/:filename
func (s *APIServer) ServeLocalFile(c *gin.Context) {
	filename := c.Param("filename")

	// 安全检查：防止目录穿越
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "非法路径"})
		return
	}

	localPath := filepath.Join("/tmp/drop-output", filename)

	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "文件不存在: " + filename})
		return
	}

	ext := filepath.Ext(filename)
	switch ext {
	case ".svg":
		c.Header("Content-Type", "image/svg+xml")
	case ".json":
		c.Header("Content-Type", "application/json; charset=utf-8")
	case ".md":
		c.Header("Content-Type", "text/markdown; charset=utf-8")
	case ".txt":
		c.Header("Content-Type", "text/plain; charset=utf-8")
	default:
		c.Header("Content-Type", "application/octet-stream")
	}
	disposition := "inline"
	if c.Query("download") == "1" {
		disposition = "attachment"
	}
	c.Header("Content-Disposition", contentDisposition(disposition, filename))
	c.File(localPath)
}

// ViewCOSFile 通过 apiserver 代理查看对象存储中的小型可视化产物。
// 主要用于修正历史 SVG 对象的 Content-Type，避免浏览器因 nosniff 拒绝渲染。
// GET /api/v1/cosfiles/view?key=tid/flamegraph.svg
func (s *APIServer) ViewCOSFile(c *gin.Context) {
	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 key 参数"})
		return
	}
	if strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "非法对象路径"})
		return
	}
	if filepath.Ext(key) != ".svg" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "仅支持查看 SVG 可视化产物"})
		return
	}
	if !s.StorageConnected() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "对象存储未连接"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "文件不存在: " + key})
		return
	}
	defer reader.Close()

	c.Header("Content-Type", mimeType(key))
	c.Header("Content-Disposition", contentDisposition("inline", filepath.Base(key)))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理输出对象存储文件失败", zap.String("key", key), zap.Error(err))
	}
}

// DownloadCOSFile 通过 apiserver 代理下载对象存储产物，强制 attachment。
// GET /api/v1/cosfiles/download?key=tid/top.json
func (s *APIServer) DownloadCOSFile(c *gin.Context) {
	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 key 参数"})
		return
	}
	if strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "非法对象路径"})
		return
	}
	if !s.StorageConnected() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "对象存储未连接"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "文件不存在: " + key})
		return
	}
	defer reader.Close()

	filename := filepath.Base(key)
	c.Header("Content-Type", mimeType(key))
	c.Header("Content-Disposition", contentDisposition("attachment", filename))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理下载对象存储文件失败", zap.String("key", key), zap.Error(err))
	}
}

// mimeType 根据文件扩展名返回 MIME 类型
func mimeType(filename string) string {
	ext := filepath.Ext(filename)
	switch ext {
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

func contentDisposition(disposition string, filename string) string {
	asciiName := strings.NewReplacer("\\", "_", "\"", "_", "\r", "_", "\n", "_").Replace(filename)
	escapedName := url.PathEscape(filename)
	return fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s", disposition, asciiName, escapedName)
}

// listTaskFiles 列出指定 tid 下的所有产物文件，并生成签名下载 URL
func (s *APIServer) listTaskFiles(tid string) ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bucket := s.Config.Storage.Bucket
	prefix := tid + "/" // MinIO 中以 tid/ 为前缀存放该任务的所有产物

	objects, err := s.Storage.ListObjects(ctx, bucket, prefix)
	if err != nil {
		return nil, err
	}

	var files []map[string]interface{}
	for _, obj := range objects {
		contentType := obj.ContentType
		if contentType == "" || contentType == "application/octet-stream" {
			contentType = mimeType(obj.Name)
		}
		fileInfo := map[string]interface{}{
			"name":          obj.Name,
			"size":          obj.Size,
			"last_modified": obj.LastModified,
			"content_type":  contentType,
		}

		fileInfo["download_url"] = "/api/v1/cosfiles/download?key=" + url.QueryEscape(obj.Name)
		if filepath.Ext(obj.Name) == ".svg" {
			fileInfo["view_url"] = "/api/v1/cosfiles/view?key=" + url.QueryEscape(obj.Name)
		}
		files = append(files, fileInfo)
	}

	if files == nil {
		files = []map[string]interface{}{}
	}

	return files, nil
}

// UploadTestFile 测试文件上传（W4 验收用）
// POST /api/v1/cosfiles/upload
// 上传一个文件到指定 tid 目录下，验证 MinIO 存储链路
func (s *APIServer) UploadTestFile(c *gin.Context) {
	tid := c.PostForm("tid")
	if tid == "" {
		tid = c.Query("tid")
	}
	if tid == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "缺少 tid 参数",
		})
		return
	}

	if !s.StorageConnected() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    503,
			"message": "对象存储未连接",
		})
		return
	}

	// 获取上传文件
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "读取上传文件失败: " + err.Error(),
		})
		return
	}
	defer file.Close()

	// 构建对象路径：tid/filename
	objectKey := fmt.Sprintf("%s/%s", tid, header.Filename)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if err := s.Storage.PutObject(ctx, s.Config.Storage.Bucket, objectKey, file, header.Size, contentType); err != nil {
		s.Logger.Error("文件上传失败",
			zap.String("tid", tid),
			zap.String("key", objectKey),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "文件上传失败: " + err.Error(),
		})
		return
	}

	// 生成下载签名 URL
	expireDuration := time.Duration(s.Config.Storage.PresignExpireSec) * time.Second
	downloadURL, _ := s.Storage.PresignedGetURL(ctx, s.Config.Storage.Bucket, objectKey, expireDuration)

	s.Logger.Info("文件上传成功",
		zap.String("tid", tid),
		zap.String("key", objectKey),
		zap.Int64("size", header.Size),
	)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"key":          objectKey,
			"size":         header.Size,
			"download_url": downloadURL,
		},
	})
}

// at 回溯默认取前后各 30 分钟，可用 span 参数覆盖
const defaultTimelineSpan = 30 * time.Minute

// ============================================================
// GetTimeline — Continuous Profiling 时间轴
// GET /api/v1/tasks/timeline?master_tid=xxx
// GET /api/v1/tasks/timeline?master_tid=xxx&from=<RFC3339>&to=<RFC3339>  区间筛选
// GET /api/v1/tasks/timeline?master_tid=xxx&at=<RFC3339>&span=30m        回溯某一时刻前后的窗口
//
// 窗口语义：一次 cron 触发 = 一个采集窗口 [create_time, create_time+duration)。
// at 语义：返回 [at-span, at+span] 内触发的全部窗口，span 缺省 30m。
//
//	其中 create_time <= at 的最近一个窗口标记 is_effective=true，即 at 时刻正在生效
//	（或最近一次生效）的窗口；该窗口若早于 at-span 也会被补入结果，
//	保证回溯任意时刻都能知道当时在跑什么。
//
// ============================================================
func (s *APIServer) GetTimeline(c *gin.Context) {
	masterTID := c.Query("master_tid")
	if masterTID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 master_tid 参数"})
		return
	}

	// 生效窗口需要独立查询，不能共用已加过滤条件的 query，因此把基础条件封成构造函数
	baseQuery := func() *gorm.DB {
		return s.DB.Where("master_task_tid = ? AND deleted_at IS NULL", masterTID)
	}
	query := baseQuery()

	atRaw := c.Query("at")
	fromRaw := c.Query("from")
	toRaw := c.Query("to")

	// effective 仅在 at 模式下非空：at 时刻正在生效（最近一次触发）的窗口
	var effective *model.HotmethodTask

	switch {
	case atRaw != "":
		at, err := time.Parse(time.RFC3339, atRaw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "at 参数格式错误，需为 RFC3339: " + err.Error()})
			return
		}

		span := defaultTimelineSpan
		if spanRaw := c.Query("span"); spanRaw != "" {
			span, err = time.ParseDuration(spanRaw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "span 参数格式错误，需为时长如 30m/2h: " + err.Error()})
				return
			}
			if span <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "span 必须为正时长"})
				return
			}
		}

		// 单独查一次生效窗口：它可能早于 at-span，落在区间外
		var eff model.HotmethodTask
		errEff := baseQuery().Where("create_time <= ?", at).Order("create_time DESC").First(&eff).Error
		if errEff == nil {
			effective = &eff
		} else if !errors.Is(errEff, gorm.ErrRecordNotFound) {
			s.Logger.Error("查询生效窗口失败", zap.String("master_tid", masterTID), zap.Error(errEff))
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询失败"})
			return
		}

		query = query.Where("create_time >= ? AND create_time <= ?", at.Add(-span), at.Add(span)).
			Order("create_time ASC")
	case fromRaw != "" || toRaw != "":
		if fromRaw != "" {
			from, err := time.Parse(time.RFC3339, fromRaw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "from 参数格式错误，需为 RFC3339: " + err.Error()})
				return
			}
			query = query.Where("create_time >= ?", from)
		}
		if toRaw != "" {
			to, err := time.Parse(time.RFC3339, toRaw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "to 参数格式错误，需为 RFC3339: " + err.Error()})
				return
			}
			query = query.Where("create_time <= ?", to)
		}
		query = query.Order("create_time ASC")
	default:
		query = query.Order("create_time ASC")
	}

	var tasks []model.HotmethodTask
	if err := query.Find(&tasks).Error; err != nil {
		s.Logger.Error("查询时间轴失败", zap.String("master_tid", masterTID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询失败"})
		return
	}

	// 生效窗口若早于 at-span 就不在区间结果里，补到最前面（其 create_time 必然最小，升序不变）
	if effective != nil {
		inRange := false
		for _, t := range tasks {
			if t.TID == effective.TID {
				inRange = true
				break
			}
		}
		if !inRange {
			tasks = append([]model.HotmethodTask{*effective}, tasks...)
		}
	}

	// 构建时间轴数据
	type TimelinePoint struct {
		TID            string     `json:"tid"`
		Name           string     `json:"name"`
		Status         int        `json:"status"`
		CreateTime     time.Time  `json:"create_time"`
		BeginTime      *time.Time `json:"begin_time,omitempty"`
		EndTime        *time.Time `json:"end_time,omitempty"`
		HasResult      bool       `json:"has_result"` // 是否有火焰图/SVG产物
		AnalysisStatus int        `json:"analysis_status"`
		WindowStart    time.Time  `json:"window_start"` // = CreateTime，窗口生效起点
		// = CreateTime + duration，从任务自身采集参数推导。
		// 用指针 + omitempty：采集参数缺 duration 时该字段直接不出现在 JSON 里，
		// 而不是回一个零值时间（0001-01-01）——零值时间在前端是真值，会骗过
		// `window_end ? ... : ...` 这类判断，把公元 1 年的日期当成合法窗口终点。
		WindowEnd   *time.Time `json:"window_end,omitempty"`
		FrequencyHz uint32     `json:"frequency_hz"` // 该窗口的采样频率
		IsEffective bool       `json:"is_effective"` // at 模式下：该窗口是 at 时刻正在生效的那一个
	}

	timeline := make([]TimelinePoint, 0, len(tasks))
	for _, t := range tasks {
		tp := TimelinePoint{
			TID:            t.TID,
			Name:           t.Name,
			Status:         t.Status,
			CreateTime:     t.CreateTime,
			AnalysisStatus: t.AnalysisStatus,
			WindowStart:    t.CreateTime,
			IsEffective:    effective != nil && t.TID == effective.TID,
		}
		if t.BeginTime != nil {
			tp.BeginTime = t.BeginTime
		}
		if t.EndTime != nil {
			tp.EndTime = t.EndTime
		}
		// DONE 且 analysis_status >= 2 (分析完成) 视为有结果，UPLOADING 仍需继续轮询。
		tp.HasResult = t.Status == TaskStatusDone && t.AnalysisStatus >= 2

		// 窗口结束时间 = 触发时刻 + 该任务自身的采集时长（每条任务参数独立，不依赖 schedule 当前配置）
		// 解析失败或 duration 缺失时保持 WindowEnd 为 nil，让它从 JSON 里消失
		var params PerfParams
		if err := util.UnmarshalJSONB(t.RequestParams, &params); err == nil && params.Duration > 0 {
			windowEnd := t.CreateTime.Add(time.Duration(params.Duration) * time.Second)
			tp.WindowEnd = &windowEnd
			tp.FrequencyHz = params.Frequency
		}

		timeline = append(timeline, tp)
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"master_tid": masterTID,
			"total":      len(timeline),
			"points":     timeline,
		},
	})
}
