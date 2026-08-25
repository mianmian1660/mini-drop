// ============================================================
// server/stage4.go — 阶段 4：单次采样最终存储模型
// ============================================================
// 多代分析（generation）支持：
//   - Agent v2 对象路径解析与校验（tasks/{tid}/attempts/{aid}/raw/...）
//   - 人工重分析 API（POST /tasks/{tid}/reanalyze）
//   - 分析作业列表 API（GET /tasks/{tid}/analysis-jobs）
//   - 按 Artifact ID 的内容访问（GET /tasks/{tid}/artifacts/{id}/content）
//   - 任务详情按代选择结果（?analysis_job_id=... / active / legacy 回退）
// ============================================================

package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

var errReanalysisInflight = errors.New("analysis job already in flight")

// ------------------------------------------------------------
// Agent v2 对象路径
// ------------------------------------------------------------

// v2AgentRawPrefix 常量片段。
const (
	v2KeyTasks    = "tasks"
	v2KeyAttempts = "attempts"
	v2KeyRaw      = "raw"
)

// supportedRawBasenames Agent v2 布局下允许的 RAW basename（与任务布局一致）。
var supportedRawBasenames = map[string]bool{
	"perf.data":         true,
	"profile.pb.gz":     true,
	"profile.collapsed": true,
	"raw.bpf":           true,
	"memtrace.txt":      true,
}

// parseV2AgentRawKey 解析 tasks/{tid}/attempts/{attempt_id}/raw/{basename}。
// basename 必须是不含分隔符的纯文件名；返回 (tid, attemptID, basename, ok)。
func parseV2AgentRawKey(key string) (string, uint, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 6 || parts[0] != v2KeyTasks || parts[2] != v2KeyAttempts || parts[4] != v2KeyRaw {
		return "", 0, "", false
	}
	if parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return "", 0, "", false
	}
	aid, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil || aid == 0 {
		return "", 0, "", false
	}
	base := parts[5]
	if base == "." || base == ".." || strings.ContainsAny(base, "/\\") || strings.Contains(base, "\x00") {
		return "", 0, "", false
	}
	return parts[1], uint(aid), base, true
}

// parseV2AgentManifestKey 解析 tasks/{tid}/attempts/{attempt_id}/manifest.json。
func parseV2AgentManifestKey(key string) (string, uint, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 5 || parts[0] != v2KeyTasks || parts[2] != v2KeyAttempts || parts[4] != "manifest.json" {
		return "", 0, false
	}
	aid, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil || aid == 0 {
		return "", 0, false
	}
	return parts[1], uint(aid), true
}

// isV2AgentKey 判断 key 是否属于 Agent v2 布局。
func isV2AgentKey(key string) bool {
	return strings.HasPrefix(key, v2KeyTasks+"/")
}

// validateNotifyObjectKeys 校验采集通知中的 cos_key / manifest_key 与 attempt_id：
//   - v2 布局：tid 必须等于通知 task_id，attempt 必须匹配；RAW basename 必须在白名单；
//   - 旧布局：key 必须以 {tid}/ 开头；
//   - manifest 与 RAW 必须属于同一个 attempt（v2 下）。
func (s *APIServer) validateNotifyObjectKeys(req *TaskResultNotifyReq) error {
	if req == nil || strings.TrimSpace(req.TaskID) == "" {
		return fmt.Errorf("task_id 缺失")
	}
	cosKey := strings.TrimSpace(req.CosKey)
	if cosKey == "" {
		return nil
	}
	layoutV2 := s.Config != nil && s.Config.SingleShot.LayoutV2Enabled

	if isV2AgentKey(cosKey) {
		if !layoutV2 {
			return fmt.Errorf("Agent v2 对象布局未启用，拒绝 v2 路径")
		}
		tid, aid, base, ok := parseV2AgentRawKey(cosKey)
		if !ok {
			return fmt.Errorf("非法 v2 RAW 路径: %s", util.RedactObjectKey(cosKey))
		}
		if tid != req.TaskID {
			return fmt.Errorf("cos_key 的 task 与 task_id 不匹配")
		}
		if !supportedRawBasenames[base] {
			return fmt.Errorf("不支持的 RAW basename: %s", base)
		}
		if req.AttemptID != 0 && req.AttemptID != aid {
			return fmt.Errorf("cos_key 的 attempt_id 与通知 attempt_id 不匹配")
		}
		req.AttemptID = aid
		if manifestKey := strings.TrimSpace(req.ManifestKey); manifestKey != "" {
			if !isV2AgentKey(manifestKey) {
				return fmt.Errorf("v2 RAW 必须使用 v2 manifest 路径")
			}
			mtid, maid, mok := parseV2AgentManifestKey(manifestKey)
			if !mok || mtid != req.TaskID || maid != aid {
				return fmt.Errorf("manifest_key 与 cos_key/attempt_id 不匹配")
			}
		}
		return nil
	}

	// 旧布局：{tid}/xxx
	if !strings.HasPrefix(cosKey, req.TaskID+"/") {
		return fmt.Errorf("cos_key 不在任务命名空间内")
	}
	if manifestKey := strings.TrimSpace(req.ManifestKey); manifestKey != "" {
		if isV2AgentKey(manifestKey) {
			return fmt.Errorf("旧布局 RAW 不允许携带 v2 manifest 路径")
		}
		if !strings.HasPrefix(manifestKey, req.TaskID+"/") {
			return fmt.Errorf("manifest_key 不在任务命名空间内")
		}
	}
	return nil
}

// resolveLatestTaskAttemptIDTx 返回任务最新的 TaskAttempt.ID（事务内查询）。
func resolveLatestTaskAttemptIDTx(tx *gorm.DB, tid string) uint {
	if tx == nil || tid == "" {
		return 0
	}
	var attempt model.TaskAttempt
	if err := tx.Where("task_tid = ?", tid).Order("attempt_seq DESC, id DESC").First(&attempt).Error; err != nil {
		return 0
	}
	return attempt.ID
}

// ------------------------------------------------------------
// 重分析 API
// ------------------------------------------------------------

// ReanalyzeReq 人工重分析请求体。
type ReanalyzeReq struct {
	AttemptID uint `json:"attempt_id"` // 可省略：默认 attempt_seq 最大且仍有 ready RAW 的尝试
}

// ReanalyzeTask POST /api/v1/tasks/{tid}/reanalyze
// 仅任务 owner（可管理）或管理员；任务必须为终态；同一任务已有
// pending/running/retry 作业时返回 409。generation 在锁定任务行的事务中分配。
func (s *APIServer) ReanalyzeTask(c *gin.Context) {
	tid := c.Param("tid")
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.RespondHTTPError(c, http.StatusForbidden, ErrCodeAuthForbidden, "Viewer 不能发起重分析")
		return
	}
	if s.Config == nil || !s.Config.SingleShot.GenerationsEnabled || !s.Config.SingleShot.ReanalyzeEnabled {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "重分析功能未开启")
		return
	}
	var req ReanalyzeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "请求参数错误: "+err.Error())
		return
	}

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "任务不存在: "+tid)
		return
	}
	if !s.canManageOwner(task.UID, auth) {
		s.forbid(c)
		return
	}
	if !isTerminalTaskStatus(task.Status) {
		s.RespondHTTPError(c, http.StatusConflict, ErrCodeTaskExecutionFailed, "任务必须为终态才能重分析")
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}
	if ok, message, _ := s.canStartCollection(CollectionSourceRetry); !ok {
		s.RespondHTTPError(c, http.StatusInsufficientStorage, ErrCodeStorageLowDisk, message)
		return
	}

	pipeline := s.pipelineForTask(&task)

	// 已有进行中作业 → 409 返回现有作业信息（先于 attempt 解析：并发检查是
	// 更基础的先决条件，不依赖请求携带的 attempt 是否有效）。
	var inflight model.AnalysisJob
	if err := s.DB.Where("task_tid = ? AND status IN ?", tid,
		[]string{model.AnalysisJobStatusPending, model.AnalysisJobStatusRunning, model.AnalysisJobStatusRetry}).
		Order("id ASC").First(&inflight).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{
			"code":    http.StatusConflict,
			"message": "该任务已有进行中的分析作业",
			"job":     analysisJobPublic(inflight, 0, false, 0),
		})
		return
	}

	// 尝试选择：显式 attempt_id 或默认最新可用。
	attemptID := req.AttemptID
	if attemptID == 0 {
		attemptID = s.latestAttemptWithReadyRaw(tid)
	}
	if attemptID == 0 {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "任务没有任何仍有 RAW 的采集尝试")
		return
	}
	var attempt model.TaskAttempt
	if err := s.DB.Where("id = ? AND task_tid = ?", attemptID, tid).First(&attempt).Error; err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "attempt_id 不属于该任务")
		return
	}
	rawIDs := s.readyRawIDsForAttempt(tid, attemptID)
	if len(rawIDs) == 0 {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "该采集尝试的 RAW 已过期或不存在")
		return
	}
	// RAW 必须符合 pipeline 支持的格式（至少一个）。
	if !s.anyRawMatchesPipeline(rawIDs, pipeline) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "该采集尝试的 RAW 不符合任务分析流水线格式")
		return
	}

	// 事务：锁任务行 → 分配 generation → 创建 manual 作业。
	requestedBy := firstNonEmpty(auth.Name, auth.UID)
	var created model.AnalysisJob
	inputJSON, _ := util.MarshalJSONB(rawIDs)
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		q := tx.Where("tid = ?", tid)
		if tx.Dialector.Name() == "postgres" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var locked model.HotmethodTask
		if err := q.First(&locked).Error; err != nil {
			return err
		}
		// The preflight check provides a fast 409, but only this check is
		// concurrency-safe: all writers allocate generations under the same
		// task-row lock.
		var inflightCount int64
		if err := tx.Model(&model.AnalysisJob{}).
			Where("task_tid = ? AND status IN ?", tid,
				[]string{model.AnalysisJobStatusPending, model.AnalysisJobStatusRunning, model.AnalysisJobStatusRetry}).
			Count(&inflightCount).Error; err != nil {
			return err
		}
		if inflightCount > 0 {
			return errReanalysisInflight
		}
		var maxGen int
		if err := tx.Model(&model.AnalysisJob{}).
			Where("task_tid = ?", tid).
			Select("COALESCE(MAX(generation), 0)").
			Scan(&maxGen).Error; err != nil {
			return err
		}
		now := time.Now()
		created = model.AnalysisJob{
			TaskTID:          tid,
			AttemptID:        attemptID,
			Generation:       maxGen + 1,
			Pipeline:         pipeline,
			Trigger:          model.AnalysisJobTriggerManual,
			RequestedBy:      requestedBy,
			Status:           model.AnalysisJobStatusPending,
			Attempt:          0,
			MaxAttempts:      3,
			InputArtifactIDs: inputJSON,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := tx.Create(&created).Error; err != nil {
			return err
		}
		return recordTaskStatusEventWithPayloadTx(tx, tid, task.Status, task.Status,
			fmt.Sprintf("用户 %s 发起人工重分析（generation %d）", requestedBy, created.Generation),
			"analysis_reanalyze",
			map[string]interface{}{"job_id": created.ID, "generation": created.Generation, "attempt_id": attemptID, "pipeline": pipeline})
	})
	if err != nil {
		if errors.Is(err, errReanalysisInflight) {
			var current model.AnalysisJob
			s.DB.Where("task_tid = ? AND status IN ?", tid,
				[]string{model.AnalysisJobStatusPending, model.AnalysisJobStatusRunning, model.AnalysisJobStatusRetry}).
				Order("id ASC").First(&current)
			c.JSON(http.StatusConflict, gin.H{
				"code": http.StatusConflict, "message": "该任务已有进行中的分析作业",
				"job": analysisJobPublic(current, 0, false, 0),
			})
			return
		}
		s.Logger.Error("创建重分析作业失败", zap.String("tid", tid), zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建重分析作业失败")
		return
	}
	incAnalysisQueued()
	s.Logger.Info("重分析作业已创建",
		zap.String("tid", tid),
		zap.Uint("job_id", created.ID),
		zap.Int("generation", created.Generation),
		zap.Uint("attempt_id", attemptID),
		zap.String("requested_by", requestedBy))
	c.JSON(http.StatusAccepted, gin.H{
		"code":       http.StatusAccepted,
		"job_id":     created.ID,
		"generation": created.Generation,
		"attempt_id": attemptID,
		"pipeline":   pipeline,
	})
}

// pipelineForTask 按任务类型确定分析流水线（与入队逻辑一致）。
func (s *APIServer) pipelineForTask(task *model.HotmethodTask) string {
	pipeline := analysisPipelineForObject("perf.data")
	if task == nil || task.TaskKind == "" {
		return pipeline
	}
	if kind, ok := taskKindByID(task.TaskKind); ok && kind.AnalysisPipeline != "" {
		return kind.AnalysisPipeline
	}
	return pipeline
}

// latestAttemptWithReadyRaw 返回 attempt_seq 最大且仍有 ready RAW 的 TaskAttempt.ID。
func (s *APIServer) latestAttemptWithReadyRaw(tid string) uint {
	var attemptID uint
	err := s.DB.Table("artifacts a").
		Select("a.attempt_id").
		Joins("JOIN task_attempts ta ON ta.id = a.attempt_id").
		Where("a.task_tid = ? AND a.kind = ? AND a.status = ? AND a.deleted_at IS NULL AND a.attempt_id > 0", tid, model.ArtifactKindRaw, model.ArtifactStatusReady).
		Order("ta.attempt_seq DESC, ta.id DESC").
		Limit(1).
		Scan(&attemptID).Error
	if err != nil || attemptID == 0 {
		return 0
	}
	return attemptID
}

// readyRawIDsForAttempt 返回某 attempt 下 ready、未删除的 RAW Artifact ID 列表。
func (s *APIServer) readyRawIDsForAttempt(tid string, attemptID uint) []uint {
	if attemptID == 0 {
		return nil
	}
	var ids []uint
	if err := s.DB.Model(&model.Artifact{}).
		Where("task_tid = ? AND attempt_id = ? AND kind = ? AND status = ? AND deleted_at IS NULL",
			tid, attemptID, model.ArtifactKindRaw, model.ArtifactStatusReady).
		Order("id ASC").
		Pluck("id", &ids).Error; err != nil {
		return nil
	}
	return ids
}

// anyRawMatchesPipeline 检查给定 RAW artifact 中至少一个符合 pipeline 格式。
func (s *APIServer) anyRawMatchesPipeline(rawIDs []uint, pipeline string) bool {
	if len(rawIDs) == 0 {
		return false
	}
	var keys []string
	if err := s.DB.Model(&model.Artifact{}).Where("id IN ?", rawIDs).Pluck("object_key", &keys).Error; err != nil {
		return false
	}
	for _, key := range keys {
		if pipelineAcceptsRaw(pipeline, key) {
			return true
		}
	}
	return false
}

// pipelineAcceptsRaw pipeline 是否接受该 RAW 对象。
func pipelineAcceptsRaw(pipeline, objectKey string) bool {
	lower := strings.ToLower(objectKey)
	switch pipeline {
	case "pprof", "pprof_heap":
		return strings.HasSuffix(lower, ".pb.gz") || strings.HasSuffix(lower, "perf.data")
	case "bpf_histogram":
		return strings.HasSuffix(lower, ".bpf") || strings.HasSuffix(lower, "perf.data") || strings.HasSuffix(lower, ".txt")
	case "memleak":
		return strings.HasSuffix(lower, "memtrace.txt") || strings.HasSuffix(lower, ".txt")
	case "java_async_profiler", "java_heap":
		return strings.HasSuffix(lower, "perf.data") || strings.HasSuffix(lower, ".collapsed") || strings.HasSuffix(lower, ".jfr")
	default: // perf_flamegraph
		return strings.HasSuffix(lower, "perf.data")
	}
}

// ------------------------------------------------------------
// 分析作业列表 API
// ------------------------------------------------------------

// analysisJobPublic 单条作业的公共视图。
// attemptSeq 为 0 时省略；artifactsAvailable 标记产物是否仍可访问。
func analysisJobPublic(job model.AnalysisJob, attemptSeq int, artifactsAvailable bool, liveArtifactCount int) gin.H {
	out := gin.H{
		"id":                  job.ID,
		"generation":          job.Generation,
		"attempt_id":          job.AttemptID,
		"pipeline":            job.Pipeline,
		"analyzer_version":    job.AnalyzerVersion,
		"status":              job.Status,
		"trigger":             job.Trigger,
		"requested_by":        job.RequestedBy,
		"last_error":          job.LastError,
		"superseded_at":       job.SupersededAt,
		"created_at":          job.CreatedAt,
		"updated_at":          job.UpdatedAt,
		"artifacts_available": artifactsAvailable,
		"live_artifact_count": liveArtifactCount,
	}
	if attemptSeq > 0 {
		out["attempt_seq"] = attemptSeq
	}
	if job.Status == model.AnalysisJobStatusSuccess && job.UpdatedAt.After(job.CreatedAt) {
		out["completed_at"] = job.UpdatedAt
	}
	return out
}

// ListAnalysisJobs GET /api/v1/tasks/{tid}/analysis-jobs
// 返回 active job、各 generation 的作业记录与可用于重分析的 attempts。
func (s *APIServer) ListAnalysisJobs(c *gin.Context) {
	tid := c.Param("tid")
	if _, serr := s.taskService().requireReadableTask(tid, s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	s.RespondOK(c, s.analysisJobsPayload(tid))
}

// analysisJobsPayload 组装分析作业列表载荷（任务详情与独立 API 共用）。
func (s *APIServer) analysisJobsPayload(tid string) gin.H {
	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		return gin.H{"active_analysis_job_id": nil, "analysis_jobs": []gin.H{}, "reanalysis_candidates": []gin.H{}}
	}
	var jobs []model.AnalysisJob
	if err := s.DB.Where("task_tid = ?", tid).Order("generation ASC, id ASC").Find(&jobs).Error; err != nil {
		jobs = nil
	}
	// attempt_seq 映射
	attemptSeqByID := map[uint]int{}
	{
		var attempts []model.TaskAttempt
		if err := s.DB.Where("task_tid = ?", tid).Find(&attempts).Error; err == nil {
			for _, a := range attempts {
				attemptSeqByID[a.ID] = a.AttemptSeq
			}
		}
	}
	// 每个 job 的存活产物数与可用性
	liveByJob := map[uint]int{}
	{
		var rows []struct {
			AnalysisJobID uint
			Cnt           int
		}
		if err := s.DB.Model(&model.Artifact{}).
			Select("analysis_job_id, count(*) as cnt").
			Where("task_tid = ? AND analysis_job_id IS NOT NULL AND deleted_at IS NULL AND status = ?", tid, model.ArtifactStatusReady).
			Group("analysis_job_id").Scan(&rows).Error; err == nil {
			for _, r := range rows {
				liveByJob[r.AnalysisJobID] = r.Cnt
			}
		}
	}
	out := make([]gin.H, 0, len(jobs))
	for _, job := range jobs {
		cnt := liveByJob[job.ID]
		if cnt == 0 && task.ActiveAnalysisJobID != nil && *task.ActiveAnalysisJobID == job.ID &&
			job.Generation == 1 && job.Trigger == model.AnalysisJobTriggerInitial && job.Status == model.AnalysisJobStatusSuccess {
			// 旧任务回填的 active job：产物没有 analysis_job_id，按任务的
			// legacy（analysis_job_id IS NULL）ready 产物计数，避免误报"已清理"。
			var legacy int64
			if err := s.DB.Model(&model.Artifact{}).
				Where("task_tid = ? AND analysis_job_id IS NULL AND kind IN ? AND deleted_at IS NULL AND status = ?", tid,
					[]string{model.ArtifactKindResult, model.ArtifactKindIntermediate, model.ArtifactKindManifest}, model.ArtifactStatusReady).
				Count(&legacy).Error; err == nil {
				cnt = int(legacy)
			}
		}
		out = append(out, analysisJobPublic(job, attemptSeqByID[job.AttemptID], cnt > 0, cnt))
	}
	activeID := task.ActiveAnalysisJobID
	// 候选：仍有 ready RAW 的 attempts（含 attempt 信息）。
	var candidates []gin.H
	{
		var rows []struct {
			AttemptID uint
			Seq       int
			Trigger   string
			CreatedAt time.Time
			Cnt       int
		}
		if err := s.DB.Table("artifacts a").
			Select("a.attempt_id, ta.attempt_seq as seq, ta.trigger, ta.created_at, COUNT(*) as cnt").
			Joins("JOIN task_attempts ta ON ta.id = a.attempt_id").
			Where("a.task_tid = ? AND a.kind = ? AND a.status = ? AND a.deleted_at IS NULL AND a.attempt_id > 0", tid, model.ArtifactKindRaw, model.ArtifactStatusReady).
			Group("a.attempt_id, ta.attempt_seq, ta.trigger, ta.created_at").
			Order("ta.attempt_seq DESC").
			Scan(&rows).Error; err == nil {
			for _, r := range rows {
				candidates = append(candidates, gin.H{
					"attempt_id":         r.AttemptID,
					"attempt_seq":        r.Seq,
					"trigger":            r.Trigger,
					"created_at":         r.CreatedAt,
					"raw_available":      r.Cnt > 0,
					"raw_artifact_count": r.Cnt,
				})
			}
		}
	}
	return gin.H{
		"active_analysis_job_id": activeID,
		"analysis_jobs":          out,
		"reanalysis_candidates":  candidates,
	}
}

// ------------------------------------------------------------
// Artifact 内容访问
// ------------------------------------------------------------

// ServeTaskArtifactContent GET /api/v1/tasks/{tid}/artifacts/{artifact_id}/content[?download=1]
// 按 Artifact ID 读取内容（经 Blob resolver 解析物理 key），不再从 object key 推断任务。
func (s *APIServer) ServeTaskArtifactContent(c *gin.Context) {
	tid := c.Param("tid")
	if _, serr := s.taskService().requireReadableTask(tid, s.AuthContext(c)); serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	artifactID, err := strconv.ParseUint(c.Param("artifact_id"), 10, 64)
	if err != nil || artifactID == 0 {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法 artifact_id")
		return
	}
	var artifact model.Artifact
	if err := s.DB.Where("id = ? AND task_tid = ? AND status = ? AND deleted_at IS NULL", artifactID, tid, model.ArtifactStatusReady).First(&artifact).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "产物不存在")
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resolved := s.resolveBlobForKey(ctx, artifact.ObjectKey)
	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, resolved.PhysicalKey)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
		return
	}
	defer reader.Close()

	name := artifact.LogicalName
	if name == "" {
		name = filepath.Base(artifact.ObjectKey)
	}
	contentType := artifact.ContentType
	if contentType == "" {
		contentType = mimeType(artifact.ObjectKey)
	}
	c.Header("Content-Type", contentType)
	if resolved.Blob != nil && resolved.Blob.ContentEncoding != "" {
		c.Header("Content-Encoding", resolved.Blob.ContentEncoding)
	}
	disposition := "inline"
	if c.Query("download") == "1" {
		disposition = "attachment"
	}
	c.Header("Content-Disposition", contentDisposition(disposition, name))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理输出产物内容失败", zap.Uint("artifact_id", artifact.ID), zap.String("object_key", util.RedactObjectKey(artifact.ObjectKey)), zap.Error(err))
	}
}

// ------------------------------------------------------------
// 任务详情按代选择
// ------------------------------------------------------------

// resolveSelectedAnalysisJob 按优先级确定详情展示用的 AnalysisJob：
//  1. 请求显式 analysis_job_id（须属于该任务）；
//  2. task.ActiveAnalysisJobID；
//  3. nil（回退旧 {tid}/... 路径）。
func (s *APIServer) resolveSelectedAnalysisJob(task *model.HotmethodTask, explicitJobID string) (*model.AnalysisJob, error) {
	if task == nil {
		return nil, nil
	}
	jobID := uint(0)
	if raw := strings.TrimSpace(explicitJobID); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("非法 analysis_job_id")
		}
		jobID = uint(n)
	} else if task.ActiveAnalysisJobID != nil && *task.ActiveAnalysisJobID > 0 {
		jobID = *task.ActiveAnalysisJobID
	} else {
		return nil, nil
	}
	var job model.AnalysisJob
	if err := s.DB.Where("id = ? AND task_tid = ?", jobID, task.TID).First(&job).Error; err != nil {
		if jobID != 0 && strings.TrimSpace(explicitJobID) != "" {
			return nil, fmt.Errorf("analysis_job_id 不属于该任务")
		}
		return nil, nil
	}
	return &job, nil
}

// jobArtifactsByLogicalName 返回某 job 下指定逻辑名的 ready Artifact 列表。
func (s *APIServer) jobArtifactsByLogicalName(tid string, jobID uint, logicalNames ...string) []model.Artifact {
	var artifacts []model.Artifact
	query := s.DB.Where("task_tid = ? AND analysis_job_id = ? AND status = ? AND deleted_at IS NULL",
		tid, jobID, model.ArtifactStatusReady)
	if len(logicalNames) > 0 {
		query = query.Where("logical_name IN ? OR (logical_name IS NULL OR logical_name = '') AND ("+jobLogicalNameFallbackSQL(logicalNames)+")", logicalNames)
	}
	if err := query.Order("id ASC").Find(&artifacts).Error; err != nil {
		return nil
	}
	return artifacts
}

// readArtifactLogicalContent 读取 Artifact 的逻辑内容。Artifact/Blob 的对象
// 可能为 gzip 物理存储；HTTP 展示依赖 Content-Encoding 由浏览器解压，而
// 服务端内部消费者（JSON 解析、Diff 建树）必须在这里显式解压。
func (s *APIServer) readArtifactLogicalContent(ctx context.Context, artifact model.Artifact, maxBytes int64) ([]byte, error) {
	resolved := s.resolveBlobForKey(ctx, artifact.ObjectKey)
	raw, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, resolved.PhysicalKey)
	if err != nil {
		return nil, err
	}
	defer raw.Close()

	compression := artifact.Compression
	if resolved.Blob != nil && resolved.Blob.Compression != "" {
		compression = resolved.Blob.Compression
	}
	var reader io.Reader = raw
	if compression == model.CompressionGzip {
		zr, err := gzip.NewReader(raw)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("artifact logical content exceeds %d bytes", maxBytes)
	}
	return body, nil
}

// jobLogicalNameFallbackSQL 生成 object_key 后缀回退条件（旧数据 logical_name 为空时）。
func jobLogicalNameFallbackSQL(logicalNames []string) string {
	parts := make([]string, 0, len(logicalNames))
	for _, name := range logicalNames {
		parts = append(parts, fmt.Sprintf("LOWER(object_key) LIKE %s", "'%/"+strings.ToLower(name)+"'"))
	}
	return strings.Join(parts, " OR ")
}

// fetchJSONArtifactForJob 读取 job 指定逻辑名 JSON 产物并解析为 map。
func (s *APIServer) fetchJSONArtifactForJob(tid string, job *model.AnalysisJob, logicalName string) map[string]interface{} {
	if job == nil || !s.StorageConnected() {
		return nil
	}
	artifacts := s.jobArtifactsByLogicalName(tid, job.ID, logicalName)
	if len(artifacts) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range artifacts {
		body, err := s.readArtifactLogicalContent(ctx, artifacts[i], 16<<20)
		if err != nil {
			continue
		}
		var data map[string]interface{}
		if json.Unmarshal(body, &data) == nil {
			return data
		}
	}
	return nil
}

// fetchTopFunctionsForJob 按代读取 top.json（TopN）。
func (s *APIServer) fetchTopFunctionsForJob(tid string, job *model.AnalysisJob) []map[string]interface{} {
	data := s.fetchJSONArtifactForJob(tid, job, "top.json")
	if data == nil {
		return nil
	}
	return normalizeTopFunctions(data)
}

// fetchBPFDataForJob 按代读取 bpf_data.json。
func (s *APIServer) fetchBPFDataForJob(tid string, job *model.AnalysisJob) map[string]interface{} {
	return s.fetchJSONArtifactForJob(tid, job, "bpf_data.json")
}

// fetchSuggestionsForJob 按代读取建议：优先 suggestions.json 产物，其次 DB 建议。
func (s *APIServer) fetchSuggestionsForJob(tid string, job *model.AnalysisJob) []map[string]interface{} {
	if job == nil {
		return nil
	}
	if data := s.fetchJSONArtifactForJob(tid, job, "suggestions.json"); data != nil {
		if items := normalizeSuggestions(data); len(items) > 0 {
			return items
		}
	}
	return s.fetchDBSuggestionsForJob(tid, job.ID)
}

// fetchDBSuggestionsForJob 按 analysis_job_id 过滤的建议。
func (s *APIServer) fetchDBSuggestionsForJob(tid string, jobID uint) []map[string]interface{} {
	var rows []model.AnalysisSuggestion
	if err := s.DB.Where("tid = ? AND analysis_job_id = ?", tid, jobID).Order("id ASC").Find(&rows).Error; err != nil {
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

// fetchJobFlamegraphArtifact 返回 job 的火焰图 Artifact（供前端 view_url 使用）。
func (s *APIServer) fetchJobFlamegraphArtifact(tid string, job *model.AnalysisJob) *model.Artifact {
	if job == nil {
		return nil
	}
	artifacts := s.jobArtifactsByLogicalName(tid, job.ID, "flamegraph.svg", "java_flamegraph.svg", "bpf_histogram.svg", "memleak_report.svg")
	if len(artifacts) == 0 {
		return nil
	}
	return &artifacts[0]
}

// jobHasAnyArtifacts 该 job 是否有任何关联产物（含已删除）。旧任务回填的
// active job 其产物没有 analysis_job_id，需要回退旧 {tid}/... 路径。
func (s *APIServer) jobHasAnyArtifacts(tid string, jobID uint) bool {
	var count int64
	if err := s.DB.Model(&model.Artifact{}).
		Where("task_tid = ? AND analysis_job_id = ?", tid, jobID).
		Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}

// jobGenerationMap 返回任务 jobID→generation 映射。
func (s *APIServer) jobGenerationMap(tid string) map[uint]int {
	out := map[uint]int{}
	var rows []model.AnalysisJob
	if err := s.DB.Select("id, generation").Where("task_tid = ?", tid).Find(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Generation
	}
	return out
}
