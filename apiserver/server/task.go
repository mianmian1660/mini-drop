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
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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
	// IntervalSeconds / WindowSeconds 仅周期深度采样计划写入：固化"采样间隔"
	// 与"窗口时长"到每个采集窗口的 request_params，保证创建后配置不可漂移。
	IntervalSeconds uint64 `json:"interval_seconds,omitempty"`
	WindowSeconds   uint64 `json:"window_seconds,omitempty"`
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

var errArtifactTombstoned = errors.New("artifact key belongs to a deleted tombstone")

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

	// 阶段 4：Agent v2 路径（tasks/{tid}/attempts/{attempt_id}/raw/...）严格校验。
	// 通知中的 cos_key / manifest_key / attempt_id 必须相互匹配；v2 布局未开启时
	// 拒绝 v2 路径（Release C 前 Agent 不会发送，避免误解析历史 key）。
	if verr := s.validateNotifyObjectKeys(&req); verr != nil {
		incTaskNotifyFailed()
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": verr.Error(),
		})
		return
	}
	// A v2 key carries a database attempt ID. String-level path validation is
	// not enough: reject stale/fabricated IDs instead of finishing a different
	// latest attempt through the legacy fallback.
	if isV2AgentKey(strings.TrimSpace(req.CosKey)) {
		var attemptCount int64
		if err := s.DB.Model(&model.TaskAttempt{}).
			Where("id = ? AND task_tid = ?", req.AttemptID, task.TID).
			Count(&attemptCount).Error; err != nil || attemptCount != 1 {
			incTaskNotifyFailed()
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "attempt_id 不属于该任务"})
			return
		}
	}

	if strings.TrimSpace(req.ErrorMessage) != "" {
		incTaskNotifyFailed()
		endTime := time.Now()
		errorCode := req.ErrorCode
		if errorCode == "" {
			errorCode = ErrCodeTaskExecutionFailed
		}
		s.finishTaskAttemptForNotify(task.TID, req.AttemptID, errorCode, req.ErrorMessage, nil, 0)
		if task.Status == TaskStatusCanceled {
			// Cancellation is terminal. The Agent still reports TASK_CANCELED so
			// the attempt can record its real outcome, but that late notification
			// must not overwrite CANCELED with FAILED.
			s.Logger.Info("保留已取消任务状态，记录迟到的 Agent 结果",
				zap.String("tid", task.TID),
				zap.String("error_code", errorCode))
		} else if task.Status != TaskStatusFailed {
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
		keys := []string{req.CosKey}
		if strings.TrimSpace(req.ManifestKey) != "" {
			keys = append(keys, req.ManifestKey)
		}
		var tombstones int64
		if err := tx.Model(&model.Artifact{}).
			Where("task_tid = ? AND object_key IN ? AND deleted_at IS NOT NULL", task.TID, keys).
			Count(&tombstones).Error; err != nil {
			return err
		}
		if tombstones > 0 {
			return errArtifactTombstoned
		}
		if task.Status != TaskStatusDone {
			currentStatus := task.Status
			shouldAdvanceToDone := true

			if currentStatus == TaskStatusRunning {
				// 乐观锁：只有数据库里当前状态确实还是 RUNNING 才允许写
				// UPLOADING，防止和巡检器/取消等并发路径互相覆盖。
				result := tx.Model(&model.HotmethodTask{}).
					Where("tid = ? AND status = ?", task.TID, TaskStatusRunning).
					Updates(map[string]interface{}{
						"status":      TaskStatusUploading,
						"status_info": "采集产物已上传，等待登记完成",
					})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected > 0 {
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
				} else {
					// CAS 没命中：任务状态已被别的路径并发改过，重新读一次
					// 真实值——只用来判断能不能继续推进到 DONE，不作为下面
					// 更新语句的精确匹配条件。
					var actual model.HotmethodTask
					if err := tx.Select("status").Where("tid = ?", task.TID).First(&actual).Error; err != nil {
						return err
					}
					currentStatus = actual.Status
					if currentStatus != TaskStatusUploading {
						shouldAdvanceToDone = false
					}
				}
			}

			if shouldAdvanceToDone {
				// 同样加乐观锁：状态必须仍是 RUNNING 或 UPLOADING 之一才允许
				// 写 DONE，避免把并发路径已经置为 FAILED/CANCELED 的任务强行
				// 改回 DONE。
				result := tx.Model(&model.HotmethodTask{}).
					Where("tid = ? AND status IN (?, ?)", task.TID, TaskStatusRunning, TaskStatusUploading).
					Updates(map[string]interface{}{
						"status":      TaskStatusDone,
						"status_info": "采集产物已上传，任务完成",
						"end_time":    &endTime,
					})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected > 0 {
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
				} else {
					s.Logger.Warn("跳过采集完成状态推进：写入 DONE 时状态已被并发修改",
						zap.String("tid", task.TID))
				}
			} else {
				s.Logger.Warn("跳过采集完成状态推进：任务已处于并发终态",
					zap.String("tid", task.TID),
					zap.Int("actual_status", currentStatus))
			}
		}
		if err := s.ensureAnalysisQueuedTx(tx, task.TID, req.AttemptID, req.CosKey, req.ArtifactSize, model.AnalysisJobTriggerInitial, ""); err != nil {
			return err
		}
		if err := ensureManifestArtifactTx(tx, task.TID, req.AttemptID, req.ManifestKey); err != nil {
			return err
		}
		var artifact model.Artifact
		// 只允许更新非墓碑行：同 key 的迟到通知不允许复活已删除的 tombstone。
		if err := tx.Where("task_tid = ? AND kind = ? AND object_key = ? AND deleted_at IS NULL", task.TID, model.ArtifactKindRaw, req.CosKey).First(&artifact).Error; err == nil {
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
			} else {
				// 完整通知到达：uploading 必须切回 ready（partial 上传的最终确认）。
				updates["status"] = model.ArtifactStatusReady
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
		if errors.Is(err, errArtifactTombstoned) {
			// 上传发生在通知之前；拒绝墓碑复活时同步删除迟到对象，避免留下
			// 数据库不可见的同 key 孤儿。删除失败仍会被历史孤儿扫尾兜底。
			if s.StorageConnected() {
				for _, key := range []string{req.CosKey, req.ManifestKey} {
					if strings.TrimSpace(key) == "" {
						continue
					}
					if deleteErr := s.Storage.DeleteObject(c.Request.Context(), s.Config.Storage.Bucket, key); deleteErr != nil {
						s.Logger.Warn("拒绝墓碑复活后删除迟到对象失败", zap.String("object_key", util.RedactObjectKey(key)), zap.Error(deleteErr))
					}
				}
			}
			c.JSON(http.StatusConflict, gin.H{"code": http.StatusConflict, "message": "产物已进入删除墓碑，拒绝同 key 迟到上传"})
			return
		}
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

func ensureManifestArtifactTx(tx *gorm.DB, tid string, attemptID uint, manifestKey string) error {
	manifestKey = strings.TrimSpace(manifestKey)
	if tx == nil || tid == "" || manifestKey == "" {
		return nil
	}
	manifest := model.Artifact{
		TaskTID:     tid,
		AttemptID:   attemptID,
		Kind:        model.ArtifactKindManifest,
		ObjectKey:   manifestKey,
		ContentType: "application/json",
		Status:      model.ArtifactStatusReady,
		CreatedAt:   time.Now(),
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "task_tid"}, {Name: "kind"}, {Name: "object_key"}},
		DoNothing: true,
	}).Create(&manifest).Error
}

func (s *APIServer) ensureAnalysisQueued(tid string, objectKey string, size int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		return s.ensureAnalysisQueuedTx(tx, tid, 0, objectKey, size, model.AnalysisJobTriggerInitial, "")
	})
}

// ensureAnalysisQueuedTx 为采集 RAW 创建/复用 AnalysisJob（阶段 4：多代）。
//
// 语义：
//   - 幂等：同一 (task, attempt, pipeline, trigger='initial') 只创建一条初始作业。
//     事务内先锁任务行（generation 分配与并发去重都依赖这把锁），再做存在性
//     检查；PostgreSQL 侧的 partial unique index（uidx_analysis_jobs_initial_once）
//     兜底并发竞态。
//   - generation：在锁内按 MAX(generation)+1 分配，从 1 单调递增。
//   - input_artifact_ids 只包含该 attempt 的 RAW。
//   - attemptID == 0（旧链路/轮询补建）时解析为该任务最新的 TaskAttempt。
func (s *APIServer) ensureAnalysisQueuedTx(tx *gorm.DB, tid string, attemptID uint, objectKey string, size int64, trigger string, requestedBy string) error {
	objectKey = strings.TrimSpace(objectKey)
	if tid == "" || objectKey == "" {
		return nil
	}
	if trigger == "" {
		trigger = model.AnalysisJobTriggerInitial
	}
	ownsTx := tx == nil
	if ownsTx {
		tx = s.DB
	}

	artifact := model.Artifact{
		TaskTID:     tid,
		AttemptID:   attemptID,
		Kind:        model.ArtifactKindRaw,
		ObjectKey:   objectKey,
		Size:        size,
		ContentType: mimeType(objectKey),
		Status:      model.ArtifactStatusReady,
		CreatedAt:   time.Now(),
		LogicalName: filepath.Base(objectKey),
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
	// 排除墓碑：tombstone 不允许被重新作为分析输入（同 key 迟到通知保持墓碑）。
	if err := tx.Where("task_tid = ? AND kind = ? AND object_key = ? AND deleted_at IS NULL", tid, model.ArtifactKindRaw, objectKey).First(&savedArtifact).Error; err == nil {
		inputArtifactIDs = append(inputArtifactIDs, savedArtifact.ID)
	}
	if attemptID == 0 {
		attemptID = resolveLatestTaskAttemptIDTx(tx, tid)
	}

	// 锁任务行：generation 分配与"同一 attempt 初始作业只建一次"的并发安全
	// 都依赖这把锁（reanalyze 入口用同一把锁，互斥）。
	q := tx.Where("tid = ?", tid)
	if tx.Dialector.Name() == "postgres" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var locked model.HotmethodTask
	if err := q.First(&locked).Error; err != nil {
		return err
	}

	if trigger == model.AnalysisJobTriggerInitial {
		var existing int64
		existingQuery := tx.Model(&model.AnalysisJob{}).
			Where("task_tid = ? AND pipeline = ? AND trigger = ?", tid, pipeline, model.AnalysisJobTriggerInitial)
		generationsEnabled := s.Config != nil && s.Config.SingleShot.GenerationsEnabled
		if generationsEnabled {
			existingQuery = existingQuery.Where("attempt_id = ?", attemptID)
		}
		if err := existingQuery.Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return nil // 幂等：同一采集通知不重复建作业
		}
	}
	var maxGen int
	if err := tx.Model(&model.AnalysisJob{}).
		Where("task_tid = ?", tid).
		Select("COALESCE(MAX(generation), 0)").
		Scan(&maxGen).Error; err != nil {
		return err
	}
	generation := maxGen + 1
	if s.Config == nil || !s.Config.SingleShot.GenerationsEnabled {
		generation = 1
	}

	inputArtifactJSON, _ := util.MarshalJSONB(inputArtifactIDs)
	job := model.AnalysisJob{
		TaskTID:          tid,
		AttemptID:        attemptID,
		Generation:       generation,
		Pipeline:         pipeline,
		Trigger:          trigger,
		RequestedBy:      requestedBy,
		Status:           model.AnalysisJobStatusPending,
		Attempt:          0,
		MaxAttempts:      3,
		InputArtifactIDs: inputArtifactJSON,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	result := tx.Create(&job)
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
	for _, wanted := range []string{"raw.bpf", "profile.collapsed", "profile.pb.gz", "perf.data"} {
		for _, file := range files {
			if filepath.Base(file.Name) == wanted {
				return file.Name, file.Size, true
			}
		}
	}
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
	for _, wanted := range []string{"raw.bpf", "profile.collapsed", "profile.pb.gz", "perf.data"} {
		for _, file := range files {
			name, _ := file["name"].(string)
			if name == "" || filepath.Base(name) != wanted {
				continue
			}
			size := rawLocalFileSize(file)
			return name, size, true
		}
	}
	for _, file := range files {
		name, _ := file["name"].(string)
		if name == "" {
			continue
		}
		size := rawLocalFileSize(file)
		if strings.HasSuffix(name, "_perf.data") || filepath.Base(name) == "perf.data" {
			return name, size, true
		}
	}
	for _, file := range files {
		name, _ := file["name"].(string)
		if name == "" || !isRawCollectionName(name) {
			continue
		}
		size := rawLocalFileSize(file)
		return name, size, true
	}
	return "", 0, false
}

func rawLocalFileSize(file map[string]interface{}) int64 {
	switch v := file["size"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
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
		strings.HasSuffix(base, ".collapsed") ||
		strings.HasSuffix(base, ".pb.gz")
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
	if limit, err := strconv.Atoi(c.Query("limit")); err == nil && limit > 0 && limit <= 100 {
		pageSize = limit
	}

	var tasks []model.HotmethodTask
	var total int64

	query := s.DB.Model(&model.HotmethodTask{})

	// 按关键词搜索（任务名称 / 任务 ID / 目标 IP）
	if keyword := c.Query("keyword"); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where("name LIKE ? OR tid LIKE ? OR target_ip LIKE ?", like, like, like)
	}

	if taskKind := strings.TrimSpace(c.Query("task_kind")); taskKind != "" {
		query = query.Where("task_kind = ?", taskKind)
	}
	if host := strings.TrimSpace(firstNonEmpty(c.Query("host"), c.Query("target_ip"))); host != "" {
		query = query.Where("target_ip = ?", host)
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			query = query.Where("create_time >= ?", parsed)
		}
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			query = query.Where("create_time <= ?", parsed)
		}
	}

	// 按状态筛选
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	auth := s.AuthContext(c)
	switch strings.ToLower(strings.TrimSpace(c.DefaultQuery("owner_filter", "all"))) {
	case "", "all":
	case "mine":
		query = query.Where("uid = ?", auth.UID)
	default:
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "owner_filter 仅支持 all/mine")
		return
	}

	// task_scope 过滤（周期性采样与单次任务列表重构）：
	//   - all（默认）：不过滤，保持接口兼容；
	//   - single：排除由周期计划直接生成的采集窗口（master_task_tid 以 "sch-" 开头），
	//     但保留普通任务、复合任务及人工重试任务（它们的 master_task_tid 是任务 tid 或空）；
	//   - periodic：只返回周期计划生成的采集窗口。
	// 周期计划删除后其历史窗口 master_task_tid 仍是 "sch-*"，因此仍归为周期窗口，
	// 不会重新出现在单次任务列表。
	switch strings.ToLower(strings.TrimSpace(c.DefaultQuery("task_scope", "all"))) {
	case "", "all":
	case "single":
		query = query.Where("master_task_tid = '' OR master_task_tid NOT LIKE 'sch-%'")
	case "periodic":
		query = query.Where("master_task_tid LIKE 'sch-%'")
	default:
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "task_scope 仅支持 all/single/periodic")
		return
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
	for index := range tasks {
		tasks[index].CanManage = s.canManageOwner(tasks[index].UID, auth)
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
	auth := s.AuthContext(c)
	if !s.canReadOwner(task.UID, auth) {
		s.forbid(c)
		return
	}
	task.CanManage = s.canManageOwner(task.UID, auth)

	result, serr := s.taskDetailPayload(task, c.Query("analysis_job_id"))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}

	s.RespondOK(c, result)
}

func (s *APIServer) taskDetailPayload(task model.HotmethodTask, explicitJobID string) (gin.H, *ServiceError) {
	result := gin.H{"task": taskDetailResponse(task)}
	files := []map[string]interface{}{}
	var topFuncs []map[string]interface{}
	var bpfData map[string]interface{}
	var suggestions []map[string]interface{}
	statusEvents := s.fetchTaskStatusEvents(task.TID)
	attempts := s.fetchTaskAttempts(task.TID)
	artifacts := s.fetchArtifacts(task.TID)
	deletedArtifacts := s.fetchDeletedArtifacts(task.TID)
	if childPayload, err := s.compositeChildrenPayload(task.TID); err == nil {
		result["children"] = childPayload
	}

	// 阶段 4：按代选择结果。优先级：显式 analysis_job_id → active job → legacy。
	selectedJob, selectErr := s.resolveSelectedAnalysisJob(&task, explicitJobID)
	if selectErr != nil {
		return nil, serviceError(http.StatusBadRequest, ErrCodeTaskInvalidArgument, selectErr.Error())
	}
	// legacyFallback：旧任务回填的 active job 没有关联产物（产物 analysis_job_id
	// 为 NULL），此时回退旧 {tid}/... 路径，保持历史任务正常展示。
	legacyFallback := false
	if selectedJob != nil {
		topFuncs = s.fetchTopFunctionsForJob(task.TID, selectedJob)
		bpfData = s.fetchBPFDataForJob(task.TID, selectedJob)
		suggestions = s.fetchSuggestionsForJob(task.TID, selectedJob)
		if flame := s.fetchJobFlamegraphArtifact(task.TID, selectedJob); flame != nil {
			result["flamegraph_artifact_id"] = flame.ID
			result["flamegraph_artifact_url"] = fmt.Sprintf("/api/v1/tasks/%s/artifacts/%d/content", task.TID, flame.ID)
		}
		if !s.jobHasAnyArtifacts(task.TID, selectedJob.ID) &&
			selectedJob.Generation == 1 && selectedJob.Trigger == model.AnalysisJobTriggerInitial &&
			selectedJob.Status == model.AnalysisJobStatusSuccess && task.ActiveAnalysisJobID != nil &&
			*task.ActiveAnalysisJobID == selectedJob.ID {
			legacyFallback = true
		}
	}
	generationScoped := selectedJob != nil && !legacyFallback

	// W4: 优先从对象存储列出产物，存储不可用或无产物时回退本地目录。
	if s.StorageConnected() {
		var selectedJobID *uint
		if selectedJob != nil && !legacyFallback {
			selectedJobID = &selectedJob.ID
		}
		storageFiles, err := s.listTaskFilesForAnalysisJob(task.TID, selectedJobID)
		if err != nil {
			s.Logger.Warn("列出任务文件失败", zap.String("tid", task.TID), zap.Error(err))
		} else {
			files = storageFiles

			if selectedJob == nil || legacyFallback {
				// 旧链路（无 active job 或旧任务回填的 job 无关联产物）：
				// 从 MinIO 读取 {tid}/top.json 等
				topFuncs = s.fetchTopFunctions(task.TID)
				bpfData = s.fetchBPFData(task.TID)
				suggestions = s.fetchSuggestions(task.TID)
			}
		}
	}
	if len(files) == 0 && !generationScoped {
		files = s.listLocalFiles(task.TID)
		if len(topFuncs) == 0 {
			topFuncs = s.fetchLocalTopFunctions(task.TID)
		}
		if bpfData == nil {
			bpfData = s.fetchLocalBPFData(task.TID)
		}
	}
	if len(suggestions) == 0 && !generationScoped {
		suggestions = s.fetchLocalSuggestions(task.TID)
	}
	if len(suggestions) == 0 && selectedJob == nil {
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
	genMap := s.jobGenerationMap(task.TID)
	activeGen := 0
	if task.ActiveAnalysisJobID != nil {
		activeGen = genMap[*task.ActiveAnalysisJobID]
	}
	publicArtifacts := make([]gin.H, 0, len(artifacts))
	for _, artifact := range artifacts {
		gen := 0
		if artifact.AnalysisJobID != nil {
			gen = genMap[*artifact.AnalysisJobID]
		}
		publicArtifacts = append(publicArtifacts, publicArtifactFull(artifact, task.ArtifactsPinned, gen, activeGen))
	}
	cleanedArtifacts := make([]gin.H, 0, len(deletedArtifacts))
	for _, artifact := range deletedArtifacts {
		gen := 0
		if artifact.AnalysisJobID != nil {
			gen = genMap[*artifact.AnalysisJobID]
		}
		cleanedArtifacts = append(cleanedArtifacts, publicArtifactFull(artifact, task.ArtifactsPinned, gen, activeGen))
	}

	result["status_events"] = statusEvents
	result["attempts"] = attempts
	result["artifacts"] = publicArtifacts
	result["cleaned_artifacts"] = cleanedArtifacts
	result["files"] = files
	result["active_analysis_job_id"] = task.ActiveAnalysisJobID
	if selectedJob != nil {
		result["selected_analysis_job_id"] = selectedJob.ID
		result["selected_generation"] = selectedJob.Generation
	}
	result["analysis_jobs"] = s.analysisJobsPayload(task.TID)

	return result, nil
}

func taskDetailResponse(task model.HotmethodTask) gin.H {
	var params map[string]interface{}
	if len(task.RequestParams) > 0 {
		_ = json.Unmarshal(task.RequestParams, &params)
	}

	return gin.H{
		"id":                   task.ID,
		"tid":                  task.TID,
		"name":                 task.Name,
		"task_kind":            task.TaskKind,
		"request_id":           task.RequestID,
		"type":                 task.Type,
		"profiler_type":        task.ProfilerType,
		"target_ip":            task.TargetIP,
		"request_params":       params,
		"status":               task.Status,
		"status_info":          task.StatusInfo,
		"analysis_status":      task.AnalysisStatus,
		"uid":                  task.UID,
		"user_name":            task.UserName,
		"create_time":          task.CreateTime,
		"begin_time":           task.BeginTime,
		"end_time":             task.EndTime,
		"master_task_tid":      task.MasterTaskTID,
		"can_manage":           task.CanManage,
		"artifacts_pinned":     task.ArtifactsPinned,
		"artifacts_pinned_at":  task.ArtifactsPinnedAt,
		"artifacts_pinned_by":  task.ArtifactsPinnedBy,
		"artifacts_pin_reason": task.ArtifactsPinReason,
	}
}

func (s *APIServer) StreamTaskEvents(c *gin.Context) {
	auth := s.AuthContext(c)
	task, serr := s.taskService().requireReadableTask(c.Param("tid"), auth)
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	task.CanManage = s.canManageOwner(task.UID, auth)
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
			fresh.CanManage = task.CanManage
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
		"task_snapshot":   s.taskDetailPayloadWithFallback(task),
		"status_events":   events,
		"latest_event_id": latest,
	}
}

// taskDetailPayloadWithFallback SSE 场景没有 analysis_job_id 查询参数，
// 使用默认（active/legacy）选择；载荷构造失败时退回最小快照。
func (s *APIServer) taskDetailPayloadWithFallback(task model.HotmethodTask) gin.H {
	payload, _ := s.taskDetailPayload(task, "")
	if payload != nil {
		return payload
	}
	return gin.H{"task": taskDetailResponse(task)}
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

// fetchTopFunctionsForTask 和任务详情页使用同一套 generation 选择规则：优先
// active analysis job，仅对没有分代产物的历史任务回退 {tid}/top.json。
func (s *APIServer) fetchTopFunctionsForTask(task *model.HotmethodTask) []map[string]interface{} {
	if task == nil {
		return nil
	}
	job, _ := s.resolveSelectedAnalysisJob(task, "")
	if job != nil {
		if top := s.fetchTopFunctionsForJob(task.TID, job); len(top) > 0 {
			return top
		}
		if s.jobHasAnyArtifacts(task.TID, job.ID) {
			return nil
		}
	}
	return s.fetchTopFunctions(task.TID)
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
	// 默认只展示可用产物（排除 deleted 墓碑；deleting 属于清理中，同样不展示为可用）。
	if err := s.DB.Where("task_tid = ? AND deleted_at IS NULL AND status = ?", tid, model.ArtifactStatusReady).
		Order("created_at ASC, id ASC").Find(&artifacts).Error; err != nil || artifacts == nil {
		return []model.Artifact{}
	}
	return artifacts
}

// fetchDeletedArtifacts 返回任务的 deleted 墓碑（已清理产物折叠区）。
func (s *APIServer) fetchDeletedArtifacts(tid string) []model.Artifact {
	var artifacts []model.Artifact
	if err := s.DB.Where("task_tid = ? AND deleted_at IS NOT NULL", tid).
		Order("deleted_at DESC, id DESC").Find(&artifacts).Error; err != nil || artifacts == nil {
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

	// 即使对象存储当前不可用也必须登记 deleting 并进入重试状态；否则任务
	// 已软删除后将失去后续回收入口。未登记对象的前缀扫描仍只在存储可用时执行。
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	s.cleanupTaskArtifacts(ctx, tid, true)

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
// 阶段二：key 可以是逻辑名（Artifact.object_key）或物理 key；内部经
// Blob resolver 解析；SVG/folded 等浏览器资源透明 gzip 解码。
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
	tid := strings.TrimSpace(c.Query("tid"))
	if tid == "" {
		tid = objectKeyTID(key)
	}
	if _, serr := s.taskService().requireReadableTask(tid, s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	if !s.cosKeyBelongsToTask(tid, key) {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resolved := s.resolveBlobForKey(ctx, key)
	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, resolved.PhysicalKey)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
		return
	}
	defer reader.Close()

	c.Header("Content-Type", mimeType(key))
	// 透明 gzip：浏览器按 Content-Encoding 自动解码，无需前端感知物理压缩。
	if resolved.Blob != nil && resolved.Blob.ContentEncoding != "" {
		c.Header("Content-Encoding", resolved.Blob.ContentEncoding)
	}
	c.Header("Content-Disposition", contentDisposition("inline", filepath.Base(key)))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理输出对象存储文件失败", zap.String("key", util.RedactObjectKey(key)), zap.Error(err))
	}
}

// DownloadCOSFile 通过 apiserver 代理下载对象存储产物，强制 attachment。
// GET /api/v1/cosfiles/download?key=tid/top.json
// key 一般以 tid/ 为前缀，可以直接猜出所属任务；但像 kallsyms 这类跨任务
// 去重共享对象，key 是 kernel-symbols/<sha256>/kallsyms，猜不出真实 tid。
// 调用方（DownloadArtifact）已经用真实 tid 做过鉴权，这里优先信任显式传入
// 的 tid 参数，没传时才回退到从 key 里猜（兼容旧链接/ListCOSFiles 场景）。
// 阶段二：key 可以是逻辑名或物理 key，内部经 Blob resolver 解析。
// SVG/folded 透明 gzip；pprof 作为文件格式本身保持 .gz 原始字节。
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
	tid := strings.TrimSpace(c.Query("tid"))
	if tid == "" {
		tid = objectKeyTID(key)
	}
	if _, serr := s.taskService().requireReadableTask(tid, s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	if !s.cosKeyBelongsToTask(tid, key) {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resolved := s.resolveBlobForKey(ctx, key)
	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, resolved.PhysicalKey)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
		return
	}
	defer reader.Close()

	filename := filepath.Base(key)
	c.Header("Content-Type", mimeType(key))
	// 透明 gzip 只用于浏览器展示类资源；pprof 下载保持 .gz 原样。
	if resolved.Blob != nil && resolved.Blob.ContentEncoding != "" {
		c.Header("Content-Encoding", resolved.Blob.ContentEncoding)
	}
	c.Header("Content-Disposition", contentDisposition("attachment", filename))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理下载对象存储文件失败", zap.String("key", util.RedactObjectKey(key)), zap.Error(err))
	}
}

// cosKeyBelongsToTask keeps the legacy key-based endpoints safe while old
// clients migrate to Artifact-ID content URLs. Shared/CAS keys are accepted
// only when an Artifact ledger row explicitly links them to the authorized
// task; unregistered legacy objects must remain inside that task namespace.
func (s *APIServer) cosKeyBelongsToTask(tid, key string) bool {
	tid = strings.TrimSpace(tid)
	key = strings.TrimSpace(key)
	if tid == "" || key == "" {
		return false
	}
	var count int64
	if err := s.DB.Model(&model.Artifact{}).
		Where("task_tid = ? AND object_key = ? AND deleted_at IS NULL AND status NOT IN ?", tid, key,
			[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Count(&count).Error; err == nil && count > 0 {
		return true
	}
	return strings.HasPrefix(key, tid+"/")
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
	case ".bpf", ".collapsed":
		return "text/plain; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".gz":
		return "application/gzip"
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

// listTaskFiles 列出指定 tid 下的所有产物文件，并生成签名下载 URL。
// 阶段二：产物以 Artifact 账本为准（逻辑名 + blob 物理 key），
// MinIO 列表只用于补充账本外的历史/孤儿对象（key 直接当物理 key 用）。
func (s *APIServer) listTaskFiles(tid string) ([]map[string]interface{}, error) {
	return s.listTaskFilesForAnalysisJob(tid, nil)
}

// listTaskFilesForAnalysisJob returns only the selected generation when jobID
// is non-nil. The complete Artifact ledger is returned separately by the task
// detail API for the grouped Artifact panel.
func (s *APIServer) listTaskFilesForAnalysisJob(tid string, jobID *uint) ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bucket := s.Config.Storage.Bucket
	var files []map[string]interface{}
	seen := map[string]bool{}

	// 1) Artifact 账本（含 blob 引用）为准。
	var artifacts []model.Artifact
	query := s.DB.WithContext(ctx).
		Where("task_tid = ? AND deleted_at IS NULL AND status NOT IN ?", tid,
			[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted})
	if jobID != nil {
		query = query.Where("analysis_job_id = ?", *jobID)
	}
	if err := query.Order("id ASC").Find(&artifacts).Error; err != nil {
		return nil, err
	}
	blobCache := map[uint]*model.StorageBlob{}
	for i := range artifacts {
		a := &artifacts[i]
		if a.ObjectKey == "" || seen[a.ObjectKey] {
			continue
		}
		seen[a.ObjectKey] = true
		name := a.ObjectKey
		size := a.Size
		contentType := firstNonEmpty(a.ContentType, mimeType(name))
		if a.BlobID != nil && *a.BlobID > 0 {
			blob, ok := blobCache[*a.BlobID]
			if !ok {
				blob = &model.StorageBlob{}
				if err := s.DB.WithContext(ctx).Where("id = ?", *a.BlobID).First(blob).Error; err != nil {
					blob = nil
				}
				blobCache[*a.BlobID] = blob
			}
			if blob != nil && blob.ObjectKey != "" {
				if blob.StoredSize > 0 {
					size = blob.StoredSize
				}
				if blob.ContentType != "" {
					contentType = blob.ContentType
				}
			}
		}
		fileInfo := map[string]interface{}{
			"name":          name,
			"size":          size,
			"content_type":  contentType,
			"artifact_id":   a.ID,
			"kind":          a.Kind,
			"retention":     a.Retention,
			"last_modified": a.CreatedAt,
		}
		contentURL := fmt.Sprintf("/api/v1/tasks/%s/artifacts/%d/content", url.PathEscape(tid), a.ID)
		fileInfo["download_url"] = contentURL + "?download=1"
		if filepath.Ext(name) == ".svg" {
			fileInfo["view_url"] = contentURL
		}
		files = append(files, fileInfo)
	}

	// 2) MinIO 列表补充账本外对象（历史遗留/无元数据文件）。
	if jobID != nil {
		return files, nil
	}
	prefix := tid + "/"
	objects, err := s.Storage.ListObjects(ctx, bucket, prefix)
	if err != nil {
		// 账本数据仍然有效；MinIO 列表失败不阻塞。
		s.Logger.Warn("列出任务文件失败（MinIO）", zap.String("tid", tid), zap.Error(err))
	} else {
		for _, obj := range objects {
			if seen[obj.Name] {
				continue
			}
			seen[obj.Name] = true
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
			fileInfo["download_url"] = "/api/v1/cosfiles/download?key=" + url.QueryEscape(obj.Name) + "&tid=" + url.QueryEscape(tid)
			if filepath.Ext(obj.Name) == ".svg" {
				fileInfo["view_url"] = "/api/v1/cosfiles/view?key=" + url.QueryEscape(obj.Name) + "&tid=" + url.QueryEscape(tid)
			}
			files = append(files, fileInfo)
		}
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
// GetTimeline — Periodic Deep Sampling 时间轴（旧 schedule master + child tasks）
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
	auth := s.AuthContext(c)

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
			query = query.Where("status = ? AND analysis_status = ?", TaskStatusDone, 2)
		} else {
			query = query.Where("NOT (status = ? AND analysis_status = ?)", TaskStatusDone, 2)
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
		CanManage   bool       `json:"can_manage"`
	}

	timeline := make([]TimelinePoint, 0, len(tasks))
	trends := gin.H{
		"total":           len(tasks),
		"success":         0,
		"failed":          0,
		"analysis_failed": 0,
		"canceled":        0,
		"running":         0,
		"has_result":      0,
		"by_task_kind":    gin.H{},
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
			CanManage:      s.canManageOwner(t.UID, auth),
		}
		if t.BeginTime != nil {
			tp.BeginTime = t.BeginTime
		}
		if t.EndTime != nil {
			tp.EndTime = t.EndTime
		}
		// DONE 且 analysis_status=2（分析成功）才视为有结果；3 是分析失败，不能提供火焰图。
		tp.HasResult = t.Status == TaskStatusDone && t.AnalysisStatus == 2
		if tp.HasResult {
			tp.ResultURL = "/task/result?tid=" + t.TID
			trends["has_result"] = trends["has_result"].(int) + 1
		}
		switch t.Status {
		case TaskStatusDone:
			if t.AnalysisStatus == 3 {
				trends["analysis_failed"] = trends["analysis_failed"].(int) + 1
			} else {
				trends["success"] = trends["success"].(int) + 1
			}
		case TaskStatusFailed:
			trends["failed"] = trends["failed"].(int) + 1
		case TaskStatusCanceled:
			trends["canceled"] = trends["canceled"].(int) + 1
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

// ============================================================
// GetTaskDiff — 两个任务的热点函数对比（基线 vs 对比）
// GET /api/v1/tasks/diff?baseline_tid=X&compare_tid=Y&threshold=1
//
// 数据来自两侧当前 active analysis generation 的 top.json，旧任务回退
// {tid}/top.json。perf、pprof、async-profiler、eBPF-CPU
// 四种采集器都会产出该文件，所以对比不挑采集器；eBPF 直方图任务只有
// bpf_data.json，没有可比的函数列表，会被明确拒绝而不是返回一张空表。
//
// 口径：按 percentage 比较而不是原始 samples——两次采集的时长和频率可能不同，
// 绝对采样数不可比，百分比才是归一化的。两侧原始值一并返回，方便前端同时
// 展示"基线值 / 对比值 / 差值"三个数。
//
// 已知限制：top.json 只保留各自 Top20，因此 direction 为 baseline_only /
// compare_only 的准确含义是"没进入对方的 Top20"，不代表该函数在对方那次采集
// 里不存在。
// ============================================================

// DiffEntry 描述一个函数在两次采集之间的占比变化。
type DiffEntry struct {
	Function           string  `json:"function"`
	BaselinePercentage float64 `json:"baseline_percentage"`
	ComparePercentage  float64 `json:"compare_percentage"`
	DeltaPercentage    float64 `json:"delta_percentage"`
	BaselineSamples    float64 `json:"baseline_samples"`
	CompareSamples     float64 `json:"compare_samples"`
	// up=对比侧更热 down=对比侧更冷 compare_only/baseline_only=只进了一侧的 Top20
	Direction string `json:"direction"`
}

// topEntryFloat 取 top.json 一条记录里的数值字段；JSON 数字统一解码为 float64。
func topEntryFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

// diffTopFunctions 把两侧 TopN 按函数名对齐并算差值。
// 纯函数，不碰 DB/存储，便于单测覆盖各种边界。
// threshold 单位是百分点，|delta| 小于它的条目视为噪声滤掉。
func diffTopFunctions(baseline, compare []map[string]interface{}, threshold float64) []DiffEntry {
	type side struct{ percentage, samples float64 }

	index := func(items []map[string]interface{}) map[string]side {
		out := make(map[string]side, len(items))
		for _, m := range items {
			name, _ := m["function"].(string)
			if name == "" {
				continue
			}
			// 同名函数重复出现时保留占比更高的一条
			s := side{topEntryFloat(m, "percentage"), topEntryFloat(m, "samples")}
			if prev, exists := out[name]; !exists || s.percentage > prev.percentage {
				out[name] = s
			}
		}
		return out
	}

	baseIdx, cmpIdx := index(baseline), index(compare)

	entries := make([]DiffEntry, 0, len(baseIdx)+len(cmpIdx))
	seen := make(map[string]struct{}, len(baseIdx)+len(cmpIdx))
	appendEntry := func(name string) {
		if _, done := seen[name]; done {
			return
		}
		seen[name] = struct{}{}

		b, inBase := baseIdx[name]
		cmp, inCmp := cmpIdx[name]
		delta := cmp.percentage - b.percentage
		if math.Abs(delta) < threshold {
			return
		}

		direction := "up"
		switch {
		case !inBase:
			direction = "compare_only"
		case !inCmp:
			direction = "baseline_only"
		case delta < 0:
			direction = "down"
		}

		entries = append(entries, DiffEntry{
			Function:           name,
			BaselinePercentage: b.percentage,
			ComparePercentage:  cmp.percentage,
			DeltaPercentage:    math.Round(delta*100) / 100,
			BaselineSamples:    b.samples,
			CompareSamples:     cmp.samples,
			Direction:          direction,
		})
	}

	for name := range baseIdx {
		appendEntry(name)
	}
	for name := range cmpIdx {
		appendEntry(name)
	}

	// 变化最大的排最前；差值相同时按函数名排序，保证输出稳定、可断言
	sort.Slice(entries, func(i, j int) bool {
		di, dj := math.Abs(entries[i].DeltaPercentage), math.Abs(entries[j].DeltaPercentage)
		if di != dj {
			return di > dj
		}
		return entries[i].Function < entries[j].Function
	})
	return entries
}

func (s *APIServer) GetTaskDiff(c *gin.Context) {
	baselineTID := strings.TrimSpace(c.Query("baseline_tid"))
	compareTID := strings.TrimSpace(c.Query("compare_tid"))
	if baselineTID == "" || compareTID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 baseline_tid 或 compare_tid 参数"})
		return
	}

	threshold := 1.0
	if raw := strings.TrimSpace(c.Query("threshold")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "threshold 需为非负数（单位：百分点）"})
			return
		}
		threshold = v
	}

	loadTask := func(tid, field string) (*model.HotmethodTask, bool) {
		var t model.HotmethodTask
		err := s.DB.Where("tid = ? AND deleted_at IS NULL", tid).First(&t).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": field + " 对应的任务不存在: " + tid})
			return nil, false
		}
		if err != nil {
			s.Logger.Error("查询对比任务失败", zap.String("tid", tid), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询失败"})
			return nil, false
		}
		return &t, true
	}

	baselineTask, ok := loadTask(baselineTID, "baseline_tid")
	if !ok {
		return
	}
	compareTask, ok := loadTask(compareTID, "compare_tid")
	if !ok {
		return
	}

	// format=flamegraph：走差分火焰图路径（folded.txt 建树 + 归一化 + 复用
	// 持续采集已有的 diffContinuousTreeNode/truncateDiffTree），不传或传
	// table 时维持原有的 top.json 扁平表格对比，行为不变。
	if strings.EqualFold(strings.TrimSpace(c.Query("format")), "flamegraph") {
		maxNodes := 0
		if raw := strings.TrimSpace(c.Query("max_nodes")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil && v > 0 {
				maxNodes = v
			}
		}
		result, reason := s.buildTaskDiffFlamegraph(baselineTask, compareTask, maxNodes)
		if reason != "" {
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": reason})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": result})
		return
	}

	// 缺产物时说明是哪一侧、为什么缺，而不是回空表让用户自己猜
	fetchSide := func(t *model.HotmethodTask, field string) ([]map[string]interface{}, bool) {
		top := s.fetchTopFunctionsForTask(t)
		if len(top) > 0 {
			return top, true
		}
		reason := "没有可对比的热点函数产物（top.json）"
		if t.AnalysisStatus == 3 {
			reason = "分析失败，无法生成热点函数对比"
		} else if t.AnalysisStatus < 2 {
			reason = "分析尚未完成（analysis_status=" + strconv.Itoa(t.AnalysisStatus) + "）"
		} else if t.ProfilerType == ProfilerBPF {
			reason = "eBPF 直方图任务产出的是延迟分布而非函数列表，无法做热点对比"
		}
		c.JSON(http.StatusConflict, gin.H{
			"code":    409,
			"message": field + "（" + t.TID + "）" + reason,
		})
		return nil, false
	}

	baselineTop, ok := fetchSide(baselineTask, "baseline_tid")
	if !ok {
		return
	}
	compareTop, ok := fetchSide(compareTask, "compare_tid")
	if !ok {
		return
	}

	entries := diffTopFunctions(baselineTop, compareTop, threshold)

	brief := func(t *model.HotmethodTask) gin.H {
		return gin.H{
			"tid":           t.TID,
			"name":          t.Name,
			"profiler_type": t.ProfilerType,
			"create_time":   t.CreateTime,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"baseline":  brief(baselineTask),
			"compare":   brief(compareTask),
			"threshold": threshold,
			"total":     len(entries),
			"functions": entries,
		},
	})
}
