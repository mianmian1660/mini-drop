package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
	pb_control "github.com/mini-drop/apiserver/proto/control"
	"github.com/mini-drop/apiserver/util"
)

type ServiceError struct {
	HTTPStatus int
	Code       string
	Message    string
}

type TaskService struct {
	server *APIServer
}

func NewTaskService(s *APIServer) *TaskService {
	return &TaskService{server: s}
}

func serviceError(status int, code string, msg string) *ServiceError {
	return &ServiceError{HTTPStatus: status, Code: code, Message: msg}
}

func (svc *TaskService) CreateTask(req CreateTaskReq, auth AuthContext, idempotencyKey string, requestID string) (gin.H, *ServiceError) {
	s := svc.server
	if !auth.CanWrite() {
		return nil, serviceError(http.StatusForbidden, ErrCodeAuthForbidden, "Viewer 只能查看资源，不能创建任务")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.TargetIP) == "" {
		return nil, serviceError(http.StatusBadRequest, ErrCodeTaskInvalidArgument, "缺少任务名称或目标 Agent")
	}
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
		return nil, serviceError(http.StatusBadRequest, ErrCodeTaskInvalidArgument, err.Error())
	}
	kind, _ := taskKindByID(req.TaskKind)
	if !s.agentSupportsTaskKind(req.TargetIP, kind) {
		return nil, serviceError(http.StatusBadRequest, ErrCodeAgentIncompatible, "目标 Agent 不支持该 TaskKind: "+req.TaskKind)
	}
	if req.RequestID == "" {
		req.RequestID = requestID
	}

	var idemPtr *string
	if raw := strings.TrimSpace(idempotencyKey); raw != "" {
		idemPtr = &raw
		var existing model.HotmethodTask
		err := s.DB.Where("uid = ? AND idempotency_key = ?", auth.UID, raw).First(&existing).Error
		if err == nil {
			return gin.H{"tid": existing.TID, "replayed": true}, nil
		}
		if err != gorm.ErrRecordNotFound {
			s.Logger.Error("查询幂等键失败", zap.String("idempotency_key", raw), zap.Error(err))
			return nil, serviceError(http.StatusInternalServerError, ErrCodeDependencyUnavailable, "服务器内部错误")
		}
	}

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
		return nil, serviceError(http.StatusInternalServerError, ErrCodeTaskInvalidArgument, "序列化任务参数失败")
	}

	now := time.Now()
	task := &model.HotmethodTask{
		TID:            util.GenTID(),
		Name:           req.Name,
		TaskKind:       req.TaskKind,
		RequestID:      req.RequestID,
		Type:           req.TaskType,
		ProfilerType:   req.ProfilerType,
		TargetIP:       req.TargetIP,
		RequestParams:  paramsJSON,
		Status:         TaskStatusCreated,
		StatusInfo:     "任务已创建，等待下发",
		AnalysisStatus: 0,
		UID:            auth.UID,
		UserName:       auth.Name,
		IdempotencyKey: idemPtr,
		CreateTime:     now,
		DeadlineUnixMS: int64(now.Add(time.Duration(req.Duration+30) * time.Second).UnixMilli()),
		ResourceBudget: []byte(req.ResourceBudget),
	}
	if err := s.createTaskWithOutbox(task, req); err != nil {
		if idemPtr != nil && isUniqueViolation(err) {
			var existing model.HotmethodTask
			if lookupErr := s.DB.Where("uid = ? AND idempotency_key = ?", auth.UID, *idemPtr).First(&existing).Error; lookupErr == nil {
				return gin.H{"tid": existing.TID, "replayed": true}, nil
			}
		}
		s.Logger.Error("创建任务失败", zap.Error(err))
		return nil, serviceError(http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建任务失败")
	}
	s.recordTaskStatusEvent(task.TID, -1, TaskStatusCreated, "任务已创建，等待下发", "apiserver")
	return gin.H{"tid": task.TID}, nil
}

func (svc *TaskService) RetryTask(tid string, auth AuthContext) (gin.H, *ServiceError) {
	s := svc.server
	if !auth.CanWrite() {
		return nil, serviceError(http.StatusForbidden, ErrCodeAuthForbidden, "Viewer 只能查看资源，不能重试任务")
	}
	var oldTask model.HotmethodTask
	if err := s.DB.Unscoped().Where("tid = ?", tid).First(&oldTask).Error; err != nil {
		return nil, serviceError(http.StatusNotFound, ErrCodeTargetNotFound, "原任务不存在: "+tid)
	}
	if !s.canManageOwner(oldTask.UID, auth) {
		return nil, serviceError(http.StatusForbidden, ErrCodeAuthForbidden, "没有权限重试该任务")
	}

	var oldParams PerfParams
	if err := util.UnmarshalJSONB(oldTask.RequestParams, &oldParams); err != nil {
		s.Logger.Warn("解析原任务参数失败，使用默认值", zap.String("tid", tid), zap.Error(err))
	}
	now := time.Now()
	req := CreateTaskReq{
		Name:           oldTask.Name + "(重试)",
		TaskKind:       oldTask.TaskKind,
		RequestID:      oldTask.RequestID,
		TaskType:       oldTask.Type,
		ProfilerType:   oldTask.ProfilerType,
		TargetIP:       oldTask.TargetIP,
		TargetPID:      oldParams.TargetPID,
		Duration:       oldParams.Duration,
		Frequency:      oldParams.Frequency,
		Callgraph:      oldParams.Callgraph,
		Event:          oldParams.Event,
		Subprocess:     oldParams.Subprocess,
		PprofURL:       oldParams.PprofURL,
		ResourceBudget: json.RawMessage(oldTask.ResourceBudget),
	}
	if err := normalizeAndValidateCollector(&req); err != nil {
		return nil, serviceError(http.StatusBadRequest, ErrCodeTaskInvalidArgument, err.Error())
	}
	newTask := model.HotmethodTask{
		TID:            util.GenTID(),
		Name:           req.Name,
		TaskKind:       req.TaskKind,
		RequestID:      req.RequestID,
		Type:           req.TaskType,
		ProfilerType:   req.ProfilerType,
		TargetIP:       req.TargetIP,
		RequestParams:  oldTask.RequestParams,
		Status:         TaskStatusCreated,
		StatusInfo:     "重试任务，等待下发",
		AnalysisStatus: 0,
		UID:            oldTask.UID,
		UserName:       oldTask.UserName,
		CreateTime:     now,
		MasterTaskTID:  tid,
		DeadlineUnixMS: int64(now.Add(time.Duration(req.Duration+30) * time.Second).UnixMilli()),
		ResourceBudget: oldTask.ResourceBudget,
	}
	if err := s.createTaskWithOutbox(&newTask, req); err != nil {
		s.Logger.Error("重试任务创建失败", zap.Error(err))
		return nil, serviceError(http.StatusInternalServerError, ErrCodeDependencyUnavailable, "重试任务创建失败")
	}
	s.recordTaskStatusEvent(newTask.TID, -1, TaskStatusCreated, "重试任务已创建，等待下发", "apiserver")
	return gin.H{"tid": newTask.TID}, nil
}

func (svc *TaskService) CancelTask(tid string, auth AuthContext) (gin.H, *ServiceError) {
	s := svc.server
	if !auth.CanWrite() {
		return nil, serviceError(http.StatusForbidden, ErrCodeAuthForbidden, "Viewer 只能查看资源，不能取消任务")
	}
	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		return nil, serviceError(http.StatusNotFound, ErrCodeTargetNotFound, "任务不存在: "+tid)
	}
	if !s.canManageOwner(task.UID, auth) {
		return nil, serviceError(http.StatusForbidden, ErrCodeAuthForbidden, "没有权限取消该任务")
	}
	if isTerminalTaskStatus(task.Status) {
		return gin.H{"tid": task.TID, "status": task.Status, "already_terminal": true}, nil
	}
	now := time.Now()
	extra := map[string]interface{}{
		"cancel_requested": true,
		"canceled_at":      &now,
		"end_time":         &now,
	}
	if err := s.transitionTaskStatus(&task, TaskStatusCanceled, "用户请求取消任务", "apiserver_cancel", extra); err != nil {
		if errors.Is(err, ErrTaskStatusStale) {
			// 竞态丢失：在我们判断"未终态"和真正写入之间，任务已经被
			// notify 回调/outbox/巡检器之一推进到了终态，语义上等同于
			// 上面 isTerminalTaskStatus 分支，不是内部错误。
			var latest model.HotmethodTask
			if err := s.DB.Where("tid = ?", task.TID).First(&latest).Error; err == nil {
				return gin.H{"tid": latest.TID, "status": latest.Status, "already_terminal": true}, nil
			}
			return gin.H{"tid": task.TID, "status": task.Status, "already_terminal": true}, nil
		}
		return nil, serviceError(http.StatusInternalServerError, ErrCodeDependencyUnavailable, "取消任务失败")
	}
	s.finishLatestTaskAttempt(task.TID, ErrCodeTaskExecutionFailed, "用户请求取消任务", nil)
	_ = s.DB.Model(&model.Outbox{}).
		Where("aggregate = ? AND aggregate_id = ? AND event = ? AND published_at IS NULL", model.OutboxAggregateTask, task.TID, model.OutboxEventDispatchTask).
		Updates(map[string]interface{}{"published_at": &now, "status": model.OutboxStatusPending, "last_error": "任务已取消，跳过下发"}).Error
	if s.GrpcConnected() {
		var attempt model.TaskAttempt
		_ = s.DB.Where("task_tid = ?", task.TID).Order("attempt_seq DESC").First(&attempt).Error
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := s.ControlCli.CancelTask(ctx, &pb_control.CancelTaskRequest{
			TargetIP:  task.TargetIP,
			TaskID:    task.TID,
			AttemptId: uint64(attempt.ID),
		})
		cancel()
		if err != nil {
			s.Logger.Warn("任务取消：通知 drop_server 失败，已保留 DB 取消意图", zap.String("tid", task.TID), zap.Error(err))
		} else {
			s.Logger.Info("任务取消：drop_server 已接收取消意图",
				zap.String("tid", task.TID),
				zap.Bool("queued_canceled", resp.GetCanceled()),
				zap.String("msg", resp.GetMsg()))
		}
	}
	return gin.H{"tid": task.TID, "status": TaskStatusCanceled, "cancel_requested": true}, nil
}

func isTerminalTaskStatus(status int) bool {
	return status == TaskStatusDone || status == TaskStatusFailed || status == TaskStatusCanceled
}

func (svc *TaskService) ListArtifacts(tid string, auth AuthContext) (gin.H, *ServiceError) {
	task, serr := svc.requireReadableTask(tid, auth)
	if serr != nil {
		return nil, serr
	}
	artifacts := svc.server.fetchArtifacts(task.TID)
	out := make([]gin.H, 0, len(artifacts))
	for _, artifact := range artifacts {
		out = append(out, publicArtifact(artifact))
	}
	return gin.H{"artifacts": out, "total": len(out)}, nil
}

func (svc *TaskService) DownloadArtifact(tid string, artifactIDRaw string, auth AuthContext) (gin.H, *ServiceError) {
	task, serr := svc.requireReadableTask(tid, auth)
	if serr != nil {
		return nil, serr
	}
	artifactID, err := strconv.ParseUint(artifactIDRaw, 10, 64)
	if err != nil || artifactID == 0 {
		return nil, serviceError(http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法 artifact_id")
	}
	var artifact model.Artifact
	if err := svc.server.DB.Where("id = ? AND task_tid = ? AND status = ?", artifactID, task.TID, model.ArtifactStatusReady).First(&artifact).Error; err != nil {
		return nil, serviceError(http.StatusNotFound, ErrCodeTargetNotFound, "产物不存在")
	}
	if !svc.server.StorageConnected() {
		return nil, serviceError(http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
	}
	downloadURL := "/api/v1/cosfiles/download?key=" + url.QueryEscape(artifact.ObjectKey)
	expires := 5 * time.Minute
	svc.server.Logger.Info("生成产物下载链接",
		zap.String("task_id", task.TID),
		zap.Uint("attempt_id", artifact.AttemptID),
		zap.Uint("artifact_id", artifact.ID),
		zap.String("object_key", util.RedactObjectKey(artifact.ObjectKey)),
		zap.String("user_id", auth.UID),
	)
	svc.server.recordTaskStatusEvent(task.TID, task.Status, task.Status, fmt.Sprintf("用户 %s 生成产物下载链接", auth.UID), "artifact_download")
	return gin.H{
		"artifact":   publicArtifact(artifact),
		"url":        downloadURL,
		"expires_at": time.Now().Add(expires),
	}, nil
}

func (svc *TaskService) requireReadableTask(tid string, auth AuthContext) (model.HotmethodTask, *ServiceError) {
	var task model.HotmethodTask
	if err := svc.server.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		return task, serviceError(http.StatusNotFound, ErrCodeTargetNotFound, "任务不存在: "+tid)
	}
	if !svc.server.canReadOwner(task.UID, auth) {
		return task, serviceError(http.StatusForbidden, ErrCodeAuthForbidden, "没有权限访问该任务")
	}
	return task, nil
}

func publicArtifact(artifact model.Artifact) gin.H {
	return gin.H{
		"id":           artifact.ID,
		"task_tid":     artifact.TaskTID,
		"attempt_id":   artifact.AttemptID,
		"kind":         artifact.Kind,
		"name":         filepath.Base(artifact.ObjectKey),
		"size":         artifact.Size,
		"sha256":       artifact.SHA256,
		"etag":         artifact.ETag,
		"content_type": artifact.ContentType,
		"status":       artifact.Status,
		"created_at":   artifact.CreatedAt,
	}
}
