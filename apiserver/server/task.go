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
	Name           string          `json:"name" binding:"required"`
	TaskKind       string          `json:"task_kind"`
	RequestID      string          `json:"request_id"`
	TaskType       uint32          `json:"task_type"`     // 0=通用 1=Java 2=Tracing
	ProfilerType   uint32          `json:"profiler_type"` // 0=perf 1=async-profiler 2=pprof
	TargetIP       string          `json:"target_ip" binding:"required"`
	TargetPID      int32           `json:"target_pid"`
	Duration       uint64          `json:"duration"`  // 采集秒数
	Frequency      uint32          `json:"frequency"` // 采样频率 Hz
	Callgraph      string          `json:"callgraph"` // fp / dwarf / lbr
	Event          string          `json:"event"`     // cpu-cycles / cache-misses
	Subprocess     bool            `json:"subprocess"`
	ContainerName  string          `json:"container_name"`
	PprofURL       string          `json:"pprof_url"`
	ResourceBudget json.RawMessage `json:"resource_budget,omitempty"`
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

	TaskTypeGeneric  uint32 = 0
	TaskTypeJava     uint32 = 1
	TaskTypePprof    uint32 = 2
	TaskTypeMemcheck uint32 = 4
	TaskTypeBPF      uint32 = 5
	TaskTypeJavaHeap uint32 = 6
)

// TaskResultNotifyReq 是 drop_server 完成采集后回调 apiserver 的内部请求体。
type TaskResultNotifyReq struct {
	TaskID         string `json:"task_id" binding:"required"`
	ErrorMessage   string `json:"error_message"`
	CosKey         string `json:"cos_key"`
	AttemptID      uint   `json:"attempt_id"`
	ArtifactSize   int64  `json:"artifact_size"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	ManifestKey    string `json:"manifest_key"`
	ErrorCode      string `json:"error_code"`
	Partial        bool   `json:"partial"`
}

// CreateTask 创建性能采集任务
// POST /api/v1/tasks
func (s *APIServer) CreateTask(c *gin.Context) {
	var req CreateTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "请求参数错误: "+err.Error())
		return
	}
	data, serr := s.taskService().CreateTask(req, s.AuthContext(c), c.GetHeader("Idempotency-Key"), requestIDFromGin(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	incTasksCreated()
	s.Logger.Info("任务创建成功",
		zap.String("request_id", requestIDFromGin(c)),
		zap.String("tid", data["tid"].(string)),
		zap.String("task_id", data["tid"].(string)),
		zap.String("target_ip", req.TargetIP),
		zap.String("task_kind", req.TaskKind),
		zap.String("name", req.Name),
	)
	s.RespondOK(c, data)
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
		Status:      model.OutboxStatusPending,
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
		TaskID:         task.TID,
		RequestId:      req.RequestID,
		AttemptId:      uint64(attempt.ID),
		DeadlineUnixMs: task.DeadlineUnixMS,
		TaskKind:       req.TaskKind,
		TaskType:       req.TaskType,
		ProfilerType:   req.ProfilerType,
		SampleArgv:     recordArgv,
		ContainerName:  req.ContainerName,
		PprofUrl:       req.PprofURL,
		TimeoutSec:     uint32(req.Duration + 30), // 多给 30s 上传时间
		CosConfig:      cosCfg,
	}
	if len(req.ResourceBudget) > 0 {
		taskDesc.ResourceBudget = string(req.ResourceBudget)
	}
	switch req.ProfilerType {
	case ProfilerPprof:
		taskDesc.Payload = &pb_hotmethod.TaskDesc_PprofPayload{PprofPayload: &pb_hotmethod.PprofPayload{Url: req.PprofURL, Duration: req.Duration}}
	case ProfilerBPF:
		taskDesc.Payload = &pb_hotmethod.TaskDesc_EbpfPayload{EbpfPayload: &pb_hotmethod.EBpfPayload{Mode: req.Event, Duration: req.Duration, Pid: req.TargetPID}}
	default:
		taskDesc.Payload = &pb_hotmethod.TaskDesc_PerfPayload{PerfPayload: &pb_hotmethod.PerfPayload{Argv: recordArgv}}
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
			zap.String("task_id", task.TID),
			zap.Uint("attempt_id", attempt.ID),
			zap.String("target_ip", req.TargetIP),
			zap.String("error_code", ErrCodeDependencyUnavailable),
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
		zap.String("task_id", task.TID),
		zap.Uint("attempt_id", attempt.ID),
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
		incTaskNotifyFailed()
		endTime := time.Now()
		errorCode := req.ErrorCode
		if errorCode == "" {
			errorCode = ErrCodeTaskExecutionFailed
		}
		s.finishTaskAttemptForNotify(task.TID, req.AttemptID, errorCode, req.ErrorMessage, nil, 0)
		if task.Status != TaskStatusFailed {
			_ = s.transitionTaskStatus(
				&task,
				TaskStatusFailed,
				formatErrorReason(errorCode, req.ErrorMessage),
				"drop_server_notify",
				map[string]interface{}{"end_time": &endTime, "analysis_status": 3},
			)
		} else {
			_ = s.DB.Model(&model.HotmethodTask{}).
				Where("tid = ?", task.TID).
				Update("analysis_status", 3).Error
		}
		s.refreshCompositeParent(task)
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"tid": task.TID}})
		return
	}

	if strings.TrimSpace(req.CosKey) == "" {
		incTaskNotifyFailed()
		incArtifactUploadFailed()
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "采集成功通知缺少 cos_key",
		})
		return
	}

	endTime := time.Now()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if task.Status != TaskStatusDone {
			currentStatus := task.Status
			if currentStatus == TaskStatusRunning {
				if err := tx.Model(&model.HotmethodTask{}).
					Where("tid = ?", task.TID).
					Updates(map[string]interface{}{
						"status":      TaskStatusUploading,
						"status_info": "采集产物已上传，等待登记完成",
					}).Error; err != nil {
					return err
				}
				if err := tx.Create(&model.TaskStatusEvent{
					TID:          task.TID,
					FromStatus:   TaskStatusRunning,
					ToStatus:     TaskStatusUploading,
					Reason:       "采集产物已上传，等待登记完成",
					Source:       "drop_server_notify",
					Sequence:     nextTaskEventSequenceTx(tx, task.TID),
					SourceModule: "drop_server_notify",
					Payload:      []byte(fmt.Sprintf(`{"cos_key":%q}`, req.CosKey)),
					CreatedAt:    endTime,
				}).Error; err != nil {
					return err
				}
				currentStatus = TaskStatusUploading
			}
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
				TID:          task.TID,
				FromStatus:   currentStatus,
				ToStatus:     TaskStatusDone,
				Reason:       "采集产物已上传，任务完成",
				Source:       "drop_server_notify",
				Sequence:     nextTaskEventSequenceTx(tx, task.TID),
				SourceModule: "drop_server_notify",
				Payload:      []byte(fmt.Sprintf(`{"cos_key":%q}`, req.CosKey)),
				CreatedAt:    endTime,
			}).Error; err != nil {
				return err
			}
		}
		if err := s.ensureAnalysisQueuedTx(tx, task.TID, req.CosKey, req.ArtifactSize); err != nil {
			return err
		}
		var artifact model.Artifact
		if err := tx.Where("task_tid = ? AND kind = ? AND object_key = ?", task.TID, model.ArtifactKindRaw, req.CosKey).First(&artifact).Error; err == nil {
			updates := map[string]interface{}{}
			if req.ArtifactSize > 0 {
				updates["size"] = req.ArtifactSize
			}
			if req.ArtifactSHA256 != "" {
				updates["sha256"] = req.ArtifactSHA256
				updates["hash"] = "sha256:" + req.ArtifactSHA256
			}
			if req.ManifestKey != "" {
				updates["manifest_key"] = req.ManifestKey
			}
			if req.Partial {
				updates["status"] = model.ArtifactStatusUploading
			}
			if len(updates) > 0 {
				if err := tx.Model(&artifact).Updates(updates).Error; err != nil {
					return err
				}
			}
			return s.finishTaskAttemptForNotifyTx(tx, task.TID, req.AttemptID, "", "", []string{req.CosKey}, artifact.ID)
		}
		return s.finishTaskAttemptForNotifyTx(tx, task.TID, req.AttemptID, "", "", []string{req.CosKey}, 0)
	})
	if err != nil {
		incTaskNotifyFailed()
		incArtifactUploadFailed()
		s.Logger.Error("处理采集结果通知失败",
			zap.String("tid", task.TID),
			zap.String("task_id", task.TID),
			zap.Uint("attempt_id", req.AttemptID),
			zap.String("cos_key", util.RedactObjectKey(req.CosKey)),
			zap.String("error_code", ErrCodeArtifactUploadFailed),
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
		zap.String("task_id", task.TID),
		zap.Uint("attempt_id", req.AttemptID),
		zap.String("cos_key", util.RedactObjectKey(req.CosKey)),
		zap.String("source", "drop_server_notify"),
	)
	s.refreshCompositeParent(task)
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

	pipeline := analysisPipelineForObject(objectKey)
	var task model.HotmethodTask
	if err := tx.Where("tid = ?", tid).First(&task).Error; err == nil && task.TaskKind != "" {
		if kind, ok := taskKindByID(task.TaskKind); ok && kind.AnalysisPipeline != "" {
			pipeline = kind.AnalysisPipeline
		}
	}
	inputArtifactIDs := []uint{}
	var savedArtifact model.Artifact
	if err := tx.Where("task_tid = ? AND kind = ? AND object_key = ?", tid, model.ArtifactKindRaw, objectKey).First(&savedArtifact).Error; err == nil {
		inputArtifactIDs = append(inputArtifactIDs, savedArtifact.ID)
	}
	inputArtifactJSON, _ := util.MarshalJSONB(inputArtifactIDs)
	job := model.AnalysisJob{
		TaskTID:          tid,
		Pipeline:         pipeline,
		Status:           model.AnalysisJobStatusPending,
		Attempt:          0,
		MaxAttempts:      3,
		InputArtifactIDs: inputArtifactJSON,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	result := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "task_tid"}},
		DoNothing: true,
	}).Create(&job)
	if err := result.Error; err != nil {
		return err
	}
	if result.RowsAffected > 0 {
		incAnalysisQueued()
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

	auth := s.AuthContext(c)
	if !auth.IsPlatformAdmin() {
		visibleUIDs := s.visibleOwnerUIDs(auth)
		if len(visibleUIDs) == 0 {
			visibleUIDs = []string{auth.UID}
		}
		query = query.Where("uid IN ?", visibleUIDs)
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

	s.RespondOK(c, gin.H{
		"tasks":    tasks,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
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

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "任务不存在: "+tid)
		return
	}
	if !s.canReadOwner(task.UID, s.AuthContext(c)) {
		s.forbid(c)
		return
	}

	result := s.taskDetailPayload(task)

	s.RespondOK(c, result)
}

func (s *APIServer) taskDetailPayload(task model.HotmethodTask) gin.H {
	result := gin.H{"task": taskDetailResponse(task)}
	files := []map[string]interface{}{}
	var topFuncs []map[string]interface{}
	var bpfData map[string]interface{}
	var suggestions []map[string]interface{}
	statusEvents := s.fetchTaskStatusEvents(task.TID)
	attempts := s.fetchTaskAttempts(task.TID)
	artifacts := s.fetchArtifacts(task.TID)
	if childPayload, err := s.compositeChildrenPayload(task.TID); err == nil {
		result["children"] = childPayload
	}

	// W4: 优先从对象存储列出产物，存储不可用或无产物时回退本地目录。
	if s.StorageConnected() {
		storageFiles, err := s.listTaskFiles(task.TID)
		if err != nil {
			s.Logger.Warn("列出任务文件失败", zap.String("tid", task.TID), zap.Error(err))
		} else {
			files = storageFiles

			// 尝试从 MinIO 读取 top.json → TopN 热点数据
			topFuncs = s.fetchTopFunctions(task.TID)
			bpfData = s.fetchBPFData(task.TID)
			suggestions = s.fetchSuggestions(task.TID)
		}
	}
	if len(files) == 0 {
		files = s.listLocalFiles(task.TID)
		if len(topFuncs) == 0 {
			topFuncs = s.fetchLocalTopFunctions(task.TID)
		}
		if bpfData == nil {
			bpfData = s.fetchLocalBPFData(task.TID)
		}
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchLocalSuggestions(task.TID)
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchDBSuggestions(task.TID)
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

	return result
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
		"task_kind":       task.TaskKind,
		"request_id":      task.RequestID,
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

func (s *APIServer) StreamTaskEvents(c *gin.Context) {
	task, serr := s.taskService().requireReadableTask(c.Param("tid"), s.AuthContext(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	s.prepareSSE(c)
	incSSEActive()
	defer decSSEActive()

	lastSequence := parseLastEventID(c.GetHeader("Last-Event-ID"))
	send := func(eventName string, eventID int64, payload gin.H) bool {
		return writeSSE(c, eventName, eventID, payload)
	}

	if latest, ok := s.latestTaskEventSequence(task.TID); ok && latest > lastSequence {
		lastSequence = latest
	}
	if !send("snapshot", lastSequence, s.taskEventStreamPayload(task)) {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if !send("heartbeat", lastSequence, gin.H{"request_id": requestIDFromGin(c), "tid": task.TID, "ts": time.Now()}) {
				return
			}
		case <-ticker.C:
			var fresh model.HotmethodTask
			if err := s.DB.Where("tid = ?", task.TID).First(&fresh).Error; err != nil {
				return
			}
			events := s.fetchTaskStatusEventsAfter(task.TID, lastSequence)
			if len(events) == 0 {
				if isTerminalTaskStatus(fresh.Status) && fresh.AnalysisStatus >= 2 {
					_ = send("complete", lastSequence, s.taskEventStreamPayload(fresh))
					return
				}
				continue
			}
			for _, event := range events {
				if event.Sequence > lastSequence {
					lastSequence = event.Sequence
				}
			}
			if !send("task-events", lastSequence, s.taskEventStreamPayload(fresh)) {
				return
			}
			if isTerminalTaskStatus(fresh.Status) && fresh.AnalysisStatus >= 2 {
				_ = send("complete", lastSequence, s.taskEventStreamPayload(fresh))
				return
			}
		}
	}
}

func (s *APIServer) StreamTaskSuggestions(c *gin.Context) {
	task, serr := s.taskService().requireReadableTask(c.Param("tid"), s.AuthContext(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	s.prepareSSE(c)
	incSSEActive()
	defer decSSEActive()

	lastHash := ""
	sendSuggestions := func(eventName string) bool {
		payload := s.taskSuggestionStreamPayload(task.TID, requestIDFromGin(c))
		raw, _ := json.Marshal(payload["suggestions"])
		lastHash = string(raw)
		return writeSSE(c, eventName, 0, payload)
	}
	if !sendSuggestions("suggestions") {
		return
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			_ = writeSSE(c, "heartbeat", 0, gin.H{"request_id": requestIDFromGin(c), "tid": task.TID, "ts": time.Now()})
		case <-ticker.C:
			payload := s.taskSuggestionStreamPayload(task.TID, requestIDFromGin(c))
			raw, _ := json.Marshal(payload["suggestions"])
			if string(raw) != lastHash {
				lastHash = string(raw)
				_ = writeSSE(c, "suggestions", 0, payload)
			}
			var fresh model.HotmethodTask
			if err := s.DB.Where("tid = ?", task.TID).First(&fresh).Error; err == nil && isTerminalTaskStatus(fresh.Status) && fresh.AnalysisStatus >= 2 {
				_ = writeSSE(c, "complete", 0, payload)
				return
			}
		}
	}
}

func (s *APIServer) prepareSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

func writeSSE(c *gin.Context, eventName string, eventID int64, payload interface{}) bool {
	if eventID > 0 {
		_, _ = fmt.Fprintf(c.Writer, "id: %d\n", eventID)
	}
	if eventName != "" {
		_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"error":"marshal_failed"}`)
	}
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return c.Request.Context().Err() == nil
}

func parseLastEventID(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *APIServer) taskEventStreamPayload(task model.HotmethodTask) gin.H {
	events := s.fetchTaskStatusEvents(task.TID)
	latest := latestSequenceFromEvents(events)
	return gin.H{
		"id":              latest,
		"sequence":        latest,
		"request_id":      task.RequestID,
		"task":            taskDetailResponse(task),
		"task_snapshot":   s.taskDetailPayload(task),
		"status_events":   events,
		"latest_event_id": latest,
	}
}

func (s *APIServer) taskSuggestionStreamPayload(tid string, requestID string) gin.H {
	var suggestions []map[string]interface{}
	if s.StorageConnected() {
		suggestions = s.fetchSuggestions(tid)
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchLocalSuggestions(tid)
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchDBSuggestions(tid)
	}
	return gin.H{"request_id": requestID, "tid": tid, "suggestions": suggestions}
}

func (s *APIServer) latestTaskEventSequence(tid string) (int64, bool) {
	var seq int64
	err := s.DB.Model(&model.TaskStatusEvent{}).
		Where("tid = ?", tid).
		Select("COALESCE(MAX(sequence), 0)").
		Scan(&seq).Error
	return seq, err == nil
}

func latestSequenceFromEvents(events []model.TaskStatusEvent) int64 {
	var latest int64
	for _, event := range events {
		if event.Sequence > latest {
			latest = event.Sequence
		}
	}
	return latest
}

func (s *APIServer) fetchTaskStatusEventsAfter(tid string, sequence int64) []model.TaskStatusEvent {
	var events []model.TaskStatusEvent
	if err := s.DB.Where("tid = ? AND sequence > ?", tid, sequence).
		Order("sequence ASC, created_at ASC, id ASC").
		Find(&events).Error; err != nil {
		return []model.TaskStatusEvent{}
	}
	if events == nil {
		return []model.TaskStatusEvent{}
	}
	return events
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
	auth := s.AuthContext(c)

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "任务不存在或已删除: "+tid)
		return
	}
	if !s.canManageOwner(task.UID, auth) {
		s.forbid(c)
		return
	}

	// 使用 GORM 软删除
	result := s.DB.Where("tid = ?", tid).Delete(&model.HotmethodTask{})
	if result.Error != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "删除任务失败")
		return
	}
	if result.RowsAffected == 0 {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "任务不存在或已删除: "+tid)
		return
	}

	s.Logger.Info("任务已删除", zap.String("tid", tid))

	s.RespondOK(c, gin.H{"message": "任务已删除"})
}

// RetryTask 重试失败的任务（用同参数重新创建）
// POST /api/v1/tasks/:tid/retry
// A4: 加 uid 过滤，防止用他人任务的 tid 发起重试（越权发起新的采集）。
// 这个函数没有在 A4 原定文件清单里被单独点名，但和 GetTaskDetail/DeleteTask
// 是同一类"按 tid 直查，不校验 uid"的漏洞，放着不管等于没修完，顺手一起补上。
func (s *APIServer) RetryTask(c *gin.Context) {
	tid := c.Param("tid")
	data, serr := s.taskService().RetryTask(tid, s.AuthContext(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}

	s.Logger.Info("任务重试成功",
		zap.String("old_tid", tid),
		zap.String("new_tid", data["tid"].(string)),
	)

	s.RespondOK(c, data)
}

func (s *APIServer) CancelTask(c *gin.Context) {
	data, serr := s.taskService().CancelTask(c.Param("tid"), s.AuthContext(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) ListTaskArtifacts(c *gin.Context) {
	data, serr := s.taskService().ListArtifacts(c.Param("tid"), s.AuthContext(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) DownloadTaskArtifact(c *gin.Context) {
	data, serr := s.taskService().DownloadArtifact(c.Param("tid"), c.Param("artifact_id"), s.AuthContext(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	s.RespondOK(c, data)
}

// ListCOSFiles 列出任务产物文件并提供签名下载链接
// GET /api/v1/cosfiles?tid=xxx
// W4: MinIO 不可用时回退到本地文件系统
func (s *APIServer) ListCOSFiles(c *gin.Context) {
	tid := c.Query("tid")
	if tid == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "缺少 tid 参数")
		return
	}
	if _, serr := s.taskService().requireReadableTask(tid, s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
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
			s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "列出文件失败")
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

	s.RespondOK(c, response)
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
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法路径")
		return
	}
	if tid := strings.SplitN(filename, "_", 2)[0]; tid == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法文件名")
		return
	} else if _, serr := s.taskService().requireReadableTask(tid, s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}

	localPath := filepath.Join("/tmp/drop-output", filename)

	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在: "+filename)
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
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "缺少 key 参数")
		return
	}
	if strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法对象路径")
		return
	}
	if filepath.Ext(key) != ".svg" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "仅支持查看 SVG 可视化产物")
		return
	}
	if _, serr := s.taskService().requireReadableTask(objectKeyTID(key), s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
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
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "缺少 key 参数")
		return
	}
	if strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法对象路径")
		return
	}
	if _, serr := s.taskService().requireReadableTask(objectKeyTID(key), s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
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

func objectKeyTID(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if idx := strings.Index(key, "/"); idx > 0 {
		return key[:idx]
	}
	if idx := strings.Index(key, "_"); idx > 0 {
		return key[:idx]
	}
	return ""
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
	statusRaw := c.Query("status")
	taskKindRaw := strings.TrimSpace(c.Query("task_kind"))
	hasResultRaw := strings.TrimSpace(c.Query("has_result"))

	if statusRaw != "" {
		statusValue, err := strconv.Atoi(statusRaw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "status 参数必须是数字"})
			return
		}
		query = query.Where("status = ?", statusValue)
	}
	if taskKindRaw != "" {
		query = query.Where("task_kind = ?", taskKindRaw)
	}
	if hasResultRaw != "" {
		hasResult, err := strconv.ParseBool(hasResultRaw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "has_result 参数必须是 true/false"})
			return
		}
		if hasResult {
			query = query.Where("status = ? AND analysis_status >= ?", TaskStatusDone, 2)
		} else {
			query = query.Where("NOT (status = ? AND analysis_status >= ?)", TaskStatusDone, 2)
		}
	}

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
		TaskKind    string     `json:"task_kind"`
		ScheduledAt time.Time  `json:"scheduled_at"`
		DurationSec uint64     `json:"duration_seconds"`
		ResultURL   string     `json:"result_url,omitempty"`
	}

	timeline := make([]TimelinePoint, 0, len(tasks))
	trends := gin.H{
		"total":        len(tasks),
		"success":      0,
		"failed":       0,
		"running":      0,
		"has_result":   0,
		"by_task_kind": gin.H{},
	}
	byKind := map[string]int{}
	for _, t := range tasks {
		scheduledAt := t.CreateTime
		var trigger model.ScheduleTrigger
		if err := s.DB.Where("child_tid = ?", t.TID).First(&trigger).Error; err == nil {
			scheduledAt = trigger.ScheduledAt
		}
		tp := TimelinePoint{
			TID:            t.TID,
			Name:           t.Name,
			TaskKind:       t.TaskKind,
			Status:         t.Status,
			CreateTime:     t.CreateTime,
			AnalysisStatus: t.AnalysisStatus,
			WindowStart:    t.CreateTime,
			ScheduledAt:    scheduledAt,
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
		if tp.HasResult {
			tp.ResultURL = "/task/result?tid=" + t.TID
			trends["has_result"] = trends["has_result"].(int) + 1
		}
		switch t.Status {
		case TaskStatusDone:
			trends["success"] = trends["success"].(int) + 1
		case TaskStatusFailed:
			trends["failed"] = trends["failed"].(int) + 1
		case TaskStatusRunning, TaskStatusUploading, TaskStatusCreated:
			trends["running"] = trends["running"].(int) + 1
		}
		if t.TaskKind != "" {
			byKind[t.TaskKind]++
		}

		// 窗口结束时间 = 触发时刻 + 该任务自身的采集时长（每条任务参数独立，不依赖 schedule 当前配置）
		// 解析失败或 duration 缺失时保持 WindowEnd 为 nil，让它从 JSON 里消失
		var params PerfParams
		if err := util.UnmarshalJSONB(t.RequestParams, &params); err == nil && params.Duration > 0 {
			windowEnd := t.CreateTime.Add(time.Duration(params.Duration) * time.Second)
			tp.WindowEnd = &windowEnd
			tp.DurationSec = params.Duration
			tp.FrequencyHz = params.Frequency
		}

		timeline = append(timeline, tp)
	}
	trends["by_task_kind"] = byKind

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"master_tid": masterTID,
			"total":      len(timeline),
			"points":     timeline,
			"trends":     trends,
		},
	})
}
