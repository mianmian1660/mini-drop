// ============================================================
// server/artifact_lifecycle.go — 存储阶段一：Artifact 生命周期闭环
// ============================================================
// 实现：
//   - retention_class 分类（复用 retention 列）与各类别期限
//   - policy version 生成 + reconciler 分批重算（首次回填/策略缩短 24h 保护）
//   - observe / enforce 运行模式
//   - 清理状态机：ready/failed → deleting → deleted（tombstone）
//     失败按 1m→5m→30m→2h→6h 退避重试；对象不存在视为幂等成功
//   - 任务级 pin 保护、活跃 AnalysisJob 输入保护、共享 kallsyms 引用保护
//   - 生命周期统计（供 /api/v1/storage/status 与 Prometheus 指标）
// ============================================================

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

const (
	// lifecycleRulesVersion 分类规则版本：修改分类/期限语义时递增，参与 policy version。
	// v2：阶段 4 引入 result_superseded（被新代次替换的旧代 RESULT/INTERMEDIATE，72h）。
	lifecycleRulesVersion = "v2"

	// deleteBackoffStages 删除失败退避：第 n 次失败（1 起）对应间隔。
	deleteBackoffMin           = 1 * time.Minute
	deleteBackoff5m            = 5 * time.Minute
	deleteBackoff30m           = 30 * time.Minute
	deleteBackoff2h            = 2 * time.Hour
	deleteBackoff6h            = 6 * time.Hour
	lifecycleMaxDeleteAttempts = 10 // 超过后仍持续退避重试，不放弃（墓碑语义）

	retentionTaskStateActive     = "active"
	retentionTaskStateDone       = "done"
	retentionTaskStateDiagnostic = "diagnostic"
	retentionTaskStateOrphan     = "orphan"
)

// PinArtifactsReq 任务级固定请求体。
type PinArtifactsReq struct {
	Pinned bool   `json:"pinned"`
	Reason string `json:"reason"`
}

var (
	errArtifactClaimProtected = errors.New("artifact deletion is protected")
	errArtifactDeleting       = errors.New("artifact deletion already started")
)

// ------------------------------------------------------------
// 策略版本
// ------------------------------------------------------------

// lifecyclePolicyVersion 由分类规则版本 + 各类别时长 + manifest 永久标记共同生成。
// 策略配置变化 → 版本变化 → reconciler 重算历史 Artifact。
func (s *APIServer) lifecyclePolicyVersion() string {
	r := s.Config.Retention
	raw := fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d|%v",
		lifecycleRulesVersion,
		r.RawLargeHours, r.RawPortableHours, r.IntermediateHours,
		r.DiagnosticHours, r.ResultRetentionHours, r.SupersededResultHours, r.ManifestPermanent)
	sum := sha256.Sum256([]byte(raw))
	return lifecycleRulesVersion + "-" + hex.EncodeToString(sum[:])[:12]
}

func lifecycleTaskState(task *model.HotmethodTask) string {
	if task == nil {
		return retentionTaskStateOrphan
	}
	switch task.Status {
	case TaskStatusDone:
		return retentionTaskStateDone
	case TaskStatusFailed, TaskStatusCanceled:
		return retentionTaskStateDiagnostic
	default:
		return retentionTaskStateActive
	}
}

// lifecycleStaleQuery 同时比较配置版本和任务状态类别。只比较配置版本会导致
// 任务运行期间已回填（expires_at=NULL）的 Artifact 在任务终态后永不重算。
func (s *APIServer) lifecycleStaleQuery(ctx context.Context, version string) *gorm.DB {
	expectedTaskState := fmt.Sprintf(
		"CASE WHEN t.tid IS NULL THEN '%s' WHEN t.status IN (%d,%d) THEN '%s' WHEN t.status = %d THEN '%s' ELSE '%s' END",
		retentionTaskStateOrphan, TaskStatusFailed, TaskStatusCanceled, retentionTaskStateDiagnostic,
		TaskStatusDone, retentionTaskStateDone, retentionTaskStateActive,
	)
	return s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Joins("LEFT JOIN hotmethod_tasks t ON t.tid = artifacts.task_tid").
		Where("artifacts.deleted_at IS NULL AND artifacts.status NOT IN ?", []string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Where("artifacts.retention_policy_version IS NULL OR artifacts.retention_policy_version = '' OR artifacts.retention_policy_version <> ? OR COALESCE(artifacts.retention_task_state, '') <> ("+expectedTaskState+")", version)
}

// ------------------------------------------------------------
// 分类与期限
// ------------------------------------------------------------

// classifyArtifactRetentionFull 完整分类：
//   - superseded（阶段 4）：所属 AnalysisJob 已被新代次替换的 RESULT/INTERMEDIATE
//     → result_superseded（72h）；MANIFEST 仍为 manifest（审计清单永久保留）。
//   - MANIFEST → manifest（永不过期）
//   - RESULT → result
//   - LOG → diagnostic
//   - 失败/取消任务的 RAW/INTERMEDIATE → diagnostic
//   - RAW：.pb.gz / .collapsed → raw_portable，其余 → raw_large
//   - INTERMEDIATE → intermediate
func classifyArtifactRetentionFull(kind, objectKey string, taskStatus int, superseded bool) string {
	if kind == model.ArtifactKindManifest {
		return model.RetentionClassManifest
	}
	if superseded {
		if kind == model.ArtifactKindResult || kind == model.ArtifactKindIntermediate {
			return model.RetentionClassResultSuperseded
		}
	}
	if kind == model.ArtifactKindResult {
		return model.RetentionClassResult
	}
	if taskStatus == TaskStatusFailed || taskStatus == TaskStatusCanceled {
		if kind == model.ArtifactKindRaw || kind == model.ArtifactKindIntermediate || kind == model.ArtifactKindLog {
			return model.RetentionClassDiagnostic
		}
	}
	if kind == model.ArtifactKindLog {
		return model.RetentionClassDiagnostic
	}
	if kind == model.ArtifactKindRaw {
		lower := strings.ToLower(objectKey)
		if strings.HasSuffix(lower, ".pb.gz") || strings.HasSuffix(lower, ".collapsed") {
			return model.RetentionClassRawPortable
		}
		return model.RetentionClassRawLarge
	}
	if kind == model.ArtifactKindIntermediate {
		return model.RetentionClassIntermediate
	}
	return model.RetentionClassRawLarge
}

// retentionDurationForClass 返回类别保留时长；manifest 返回 0（永不过期）。
func (s *APIServer) retentionDurationForClass(class string) time.Duration {
	r := s.Config.Retention
	hours := func(v int) time.Duration {
		if v <= 0 {
			return 24 * time.Hour
		}
		return time.Duration(v) * time.Hour
	}
	switch class {
	case model.RetentionClassRawPortable:
		return hours(r.RawPortableHours)
	case model.RetentionClassIntermediate:
		return hours(r.IntermediateHours)
	case model.RetentionClassDiagnostic:
		return hours(r.DiagnosticHours)
	case model.RetentionClassResult:
		return hours(r.ResultRetentionHours)
	case model.RetentionClassResultSuperseded:
		return hours(r.SupersededResultHours)
	case model.RetentionClassManifest:
		return 0
	default: // raw_large
		return hours(r.RawLargeHours)
	}
}

func (s *APIServer) notBeforeProtection() time.Duration {
	h := s.Config.Retention.NotBeforeProtectionHours
	if h <= 0 {
		h = 24
	}
	return time.Duration(h) * time.Hour
}

// lifecycleComputeExpiry 计算（分类, 到期时间, 最早可清理时间）。
//
// 规则：
//   - 非终态任务：不设置有效到期时间（expires_at=nil），保留既有 not_before。
//   - 终态任务：起点 = max(artifact.created_at, task.end_time)，加类别时长。
//   - 首次回填（policy version 为空）：retention_not_before = now + 24h。
//   - 策略缩短（新到期早于旧到期且早于保护点）：expires_at 提到 now+24h，
//     not_before 提升到 now+24h。
//   - 策略延长：立即按新时长计算，不动 not_before。
//   - manifest 永不过期：expires_at=nil。
//   - superseded（阶段 4）：被替换旧代按 created_at 起算 72h，不做 24h 保护
//     （切换事务已写入精确到期，重算只求一致，不延长清理）。
func (s *APIServer) lifecycleComputeExpiry(a *model.Artifact, task *model.HotmethodTask, superseded bool) (class string, expiresAt *time.Time, notBefore *time.Time) {
	now := time.Now()
	guard := now.Add(s.notBeforeProtection())

	taskStatus := -1
	// 找不到所属任务的历史 Artifact 视作 orphan，以自身 created_at 为起点
	// 计算期限；否则它们会因 expires_at 永远为空而永久占用对象存储。
	terminal := task == nil
	if task != nil {
		taskStatus = task.Status
		terminal = isTerminalTaskStatus(task.Status)
	}
	class = classifyArtifactRetentionFull(a.Kind, a.ObjectKey, taskStatus, superseded)

	// manifest 永不过期
	if class == model.RetentionClassManifest && s.Config.Retention.ManifestPermanent {
		return class, nil, a.RetentionNotBefore
	}

	if !terminal {
		return class, nil, a.RetentionNotBefore
	}

	start := a.CreatedAt
	if task != nil && task.EndTime != nil && task.EndTime.After(start) {
		start = *task.EndTime
	}
	base := start.Add(s.retentionDurationForClass(class))
	exp := base
	nb := a.RetentionNotBefore

	// 被替换旧代：直接按自身时间起算，不参与"策略缩短 24h 保护"（避免延长清理）。
	if class == model.RetentionClassResultSuperseded {
		return class, &exp, nb
	}

	// 首次回填：not_before = now + 24h
	if a.RetentionPolicyVersion == "" && a.RetentionNotBefore == nil {
		t := guard
		nb = &t
	} else if a.ExpiresAt != nil && base.Before(*a.ExpiresAt) && base.Before(guard) {
		// 策略缩短：给予 24h 保护
		exp = guard
		if nb == nil || nb.Before(guard) {
			t := guard
			nb = &t
		}
	}
	return class, &exp, nb
}

// ------------------------------------------------------------
// Reconciler：分批重算
// ------------------------------------------------------------

// reconcileLifecycle 分批重算 policy version 不一致的非 deleted Artifact，
// 单轮内循环收敛（每批 ReconcileBatch 个，最多 20 轮），返回剩余 backlog。
func (s *APIServer) reconcileLifecycle(ctx context.Context) int64 {
	if s.DB == nil {
		return 0
	}
	version := s.lifecyclePolicyVersion()
	limit := s.Config.Retention.ReconcileBatch
	if limit <= 0 {
		limit = 2000
	}
	now := time.Now()
	totalProcessed := 0

	for round := 0; round < 20; round++ {
		if ctx.Err() != nil {
			break
		}
		var stale []model.Artifact
		err := s.lifecycleStaleQuery(ctx, version).
			Select("artifacts.*").
			Order("artifacts.id ASC").
			Limit(limit).
			Find(&stale).Error
		if err != nil {
			s.Logger.Warn("生命周期重算：查询过期策略行失败", zap.Error(err))
			return -1
		}
		if len(stale) == 0 {
			break
		}

		// 批量加载涉及的 task（减少查询）
		tids := make([]string, 0, len(stale))
		seenTID := map[string]bool{}
		for i := range stale {
			if !seenTID[stale[i].TaskTID] {
				seenTID[stale[i].TaskTID] = true
				tids = append(tids, stale[i].TaskTID)
			}
		}
		taskByTID := map[string]*model.HotmethodTask{}
		if len(tids) > 0 {
			var tasks []model.HotmethodTask
			if err := s.DB.WithContext(ctx).Unscoped().Where("tid IN ?", tids).Find(&tasks).Error; err == nil {
				for i := range tasks {
					taskByTID[tasks[i].TID] = &tasks[i]
				}
			}
		}
		// 阶段 4：批量加载涉及的分析作业（superseded 判定，避免 N+1）。
		jobByID := map[uint]*model.AnalysisJob{}
		{
			jobIDs := map[uint]bool{}
			for i := range stale {
				if stale[i].AnalysisJobID != nil && *stale[i].AnalysisJobID > 0 {
					jobIDs[*stale[i].AnalysisJobID] = true
				}
			}
			if len(jobIDs) > 0 {
				ids := make([]uint, 0, len(jobIDs))
				for id := range jobIDs {
					ids = append(ids, id)
				}
				var jobs []model.AnalysisJob
				if err := s.DB.WithContext(ctx).Where("id IN ?", ids).Find(&jobs).Error; err == nil {
					for i := range jobs {
						jobByID[jobs[i].ID] = &jobs[i]
					}
				}
			}
		}

		processed := 0
		for i := range stale {
			if ctx.Err() != nil {
				break
			}
			a := &stale[i]
			task := taskByTID[a.TaskTID]
			superseded := false
			if a.AnalysisJobID != nil {
				if job := jobByID[*a.AnalysisJobID]; job != nil && job.SupersededAt != nil {
					superseded = true
				}
			}
			class, exp, nb := s.lifecycleComputeExpiry(a, task, superseded)
			updates := map[string]interface{}{
				"retention":                class,
				"retention_policy_version": version,
				"retention_task_state":     lifecycleTaskState(task),
				"updated_at":               now,
			}
			if exp != nil {
				updates["expires_at"] = *exp
			} else {
				updates["expires_at"] = nil
			}
			if nb != nil {
				updates["retention_not_before"] = *nb
			} else {
				updates["retention_not_before"] = nil
			}
			// 幂等更新：行必须仍是非 deleted 且状态未进入删除终态
			if err := s.DB.WithContext(ctx).Model(&model.Artifact{}).
				Where("id = ? AND deleted_at IS NULL", a.ID).
				Updates(updates).Error; err != nil {
				s.Logger.Warn("生命周期重算：更新失败", zap.Uint("artifact_id", a.ID), zap.Error(err))
				continue
			}
			processed++
		}
		totalProcessed += processed
		if processed < len(stale) {
			// 本轮有行未处理（更新失败/ctx 取消），避免死循环
			break
		}
	}

	// 统计剩余 backlog
	var backlog int64
	_ = s.lifecycleStaleQuery(ctx, version).
		Count(&backlog).Error

	if totalProcessed > 0 {
		s.Logger.Info("生命周期重算完成", zap.Int("processed", totalProcessed), zap.Int64("backlog", backlog), zap.String("policy_version", version))
	}
	s.setLifecycleBacklog(backlog)
	return backlog
}

// ------------------------------------------------------------
// 清理状态机
// ------------------------------------------------------------

// startArtifactLifecycleCleaner 后台生命周期循环（重算 + 统计 + 按模式清理）。
// 启动后先立即执行一轮（快速完成历史回填，尽早进入观察期），之后按 interval 周期执行。
func (s *APIServer) startArtifactLifecycleCleaner() {
	if s == nil || s.Config == nil || !s.Config.Retention.Enabled {
		return
	}
	interval := time.Duration(s.Config.Retention.ReconcileIntervalSec) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	time.Sleep(15 * time.Second) // 等服务启动、migration 完成
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		s.runArtifactLifecycleCycle(ctx)
		cancel()
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		s.runArtifactLifecycleCycle(ctx)
		cancel()
	}
}

// runArtifactLifecycleCycle 单轮生命周期：重算 → 统计 → 按模式清理。
func (s *APIServer) runArtifactLifecycleCycle(ctx context.Context) {
	if s == nil || s.DB == nil || s.Config == nil {
		return
	}
	now := time.Now()
	s.setLifecycleLastRun(&now)
	if backlog := s.reconcileLifecycle(ctx); backlog < 0 {
		s.setLifecycleError("artifact lifecycle reconciliation failed")
		return
	}
	s.setLifecycleError("")
	if s.Config.Retention.LifecycleMode != "enforce" {
		// observe：只统计 + 记录候选，不自动删除。
		s.logLifecycleCandidates(ctx)
		s.refreshLifecycleMetrics(ctx)
		return
	}
	s.processDeletingRetries(ctx)
	s.processExpiredCandidates(ctx)
	s.processBlobDeletingRetries(ctx)
	s.refreshLifecycleMetrics(ctx)
}

// lifecycleCandidateQuery 返回到期候选（ready/failed、已到期、未 pin、not-before 已过）。
func (s *APIServer) lifecycleCandidateIDs(ctx context.Context, limit int, includeDeletingRetry bool) ([]model.Artifact, error) {
	now := time.Now()
	q := s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Select("artifacts.*").
		Joins("LEFT JOIN hotmethod_tasks t ON t.tid = artifacts.task_tid").
		Where("artifacts.deleted_at IS NULL")
	if includeDeletingRetry {
		// deleting 已经代表删除被授权/领取。即使任务随后被 pin 或软删除，
		// 也必须继续重试，否则会永久卡在 deleting 并泄漏对象。
		q = q.Where("artifacts.status = ?", model.ArtifactStatusDeleting).
			Where("(artifacts.next_delete_attempt_at IS NULL OR artifacts.next_delete_attempt_at <= ?)", now)
	} else {
		q = q.Where("(artifacts.retention_not_before IS NULL OR artifacts.retention_not_before <= ?)", now).
			Where("(t.tid IS NULL OR t.deleted_at IS NULL)").
			Where("(t.tid IS NULL OR t.artifacts_pinned = false)")
		// 候选 = 到期的 ready/failed + 终态任务遗留的 stale uploading（按 diagnostic 回收）
		uploadingStale := now.Add(-lifecycleUploadingStaleWindow)
		q = q.Where(`(
			(artifacts.status IN ? AND artifacts.expires_at IS NOT NULL AND artifacts.expires_at <= ?)
			OR
			(artifacts.status = ? AND t.status IN ? AND artifacts.created_at <= ?)
		)`,
			[]string{model.ArtifactStatusReady, model.ArtifactStatusFailed}, now,
			model.ArtifactStatusUploading, []int{TaskStatusDone, TaskStatusFailed, TaskStatusCanceled}, uploadingStale)
	}
	var artifacts []model.Artifact
	if err := q.Order("artifacts.expires_at ASC, artifacts.id ASC").Limit(limit).Find(&artifacts).Error; err != nil {
		return nil, err
	}
	return artifacts, nil
}

// lifecycleUploadingStaleWindow 终态任务遗留 uploading 视为 stale 的时间窗口（1h），
// 避免与在途完整通知（partial→ready 切回）产生竞态。
const lifecycleUploadingStaleWindow = 1 * time.Hour

// activeAnalysisJobInputIDs 返回 pending/running/retry 分析作业引用的输入 Artifact ID 集合。
func (s *APIServer) activeAnalysisJobInputIDs(ctx context.Context) (map[uint]bool, error) {
	set := map[uint]bool{}
	if s.DB == nil {
		return set, errors.New("database is nil")
	}
	var jobs []model.AnalysisJob
	if err := s.DB.WithContext(ctx).
		Where("status IN ?", []string{model.AnalysisJobStatusPending, model.AnalysisJobStatusRunning, model.AnalysisJobStatusRetry}).
		Find(&jobs).Error; err != nil {
		s.Logger.Warn("生命周期：查询活跃分析作业失败", zap.Error(err))
		return set, err
	}
	for _, job := range jobs {
		var ids []uint
		if err := json.Unmarshal(job.InputArtifactIDs, &ids); err == nil {
			for _, id := range ids {
				set[id] = true
			}
		}
	}
	return set, nil
}

// processExpiredCandidates enforce 模式：领取到期候选并执行删除状态机。
func (s *APIServer) processExpiredCandidates(ctx context.Context) {
	if s.DB == nil {
		return
	}
	limit := s.retentionBatchLimit()
	candidates, err := s.lifecycleCandidateIDs(ctx, limit, false)
	if err != nil {
		s.Logger.Warn("生命周期：查询到期候选失败", zap.Error(err))
		s.setLifecycleError(err.Error())
		return
	}
	if len(candidates) == 0 {
		return
	}
	activeInputs, err := s.activeAnalysisJobInputIDs(ctx)
	if err != nil {
		// 保护信息不可用时 fail closed，绝不能把“查不到引用”解释成“没有引用”。
		return
	}
	pinnedTasks := s.pinnedTaskSet(ctx)

	claimed := 0
	for i := range candidates {
		if ctx.Err() != nil {
			break
		}
		a := &candidates[i]
		if pinnedTasks[a.TaskTID] || activeInputs[a.ID] {
			continue // 受保护：任务已固定 / 活跃分析作业输入
		}
		if !s.claimArtifactForDeletion(ctx, a, true) {
			continue // 被并发 cleaner 抢先
		}
		claimed++
		reason := model.DeleteReasonExpired
		if a.Status == model.ArtifactStatusUploading {
			reason = model.DeleteReasonStaleUploading
		}
		s.processClaimedDeletion(ctx, a, reason)
	}
	if claimed > 0 {
		s.Logger.Info("生命周期清理完成", zap.Int("claimed", claimed), zap.String("mode", "enforce"))
	}
}

// processDeletingRetries enforce 模式：处理到期的 deleting 重试行。
func (s *APIServer) processDeletingRetries(ctx context.Context) {
	if s.DB == nil {
		return
	}
	limit := s.retentionBatchLimit()
	candidates, err := s.lifecycleCandidateIDs(ctx, limit, true)
	if err != nil {
		s.Logger.Warn("生命周期：查询删除重试行失败", zap.Error(err))
		s.setLifecycleError(err.Error())
		return
	}
	for i := range candidates {
		if ctx.Err() != nil {
			break
		}
		a := &candidates[i]
		if !s.claimArtifactForDeletion(ctx, a, false) {
			continue
		}
		reason := a.DeleteReason
		if reason == "" {
			reason = model.DeleteReasonExpired
		}
		s.processClaimedDeletion(ctx, a, reason)
	}
}

// claimArtifactForDeletion 条件更新领取：只有仍处于原状态且非 deleted 的行才会成功。
// 通过行级 CAS 避免并发 cleaner 重复处理。
func (s *APIServer) claimArtifactForDeletion(ctx context.Context, a *model.Artifact, protectTask bool) bool {
	if s.DB == nil || a == nil || a.ID == 0 {
		return false
	}
	claimed := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if protectTask && a.TaskTID != "" {
			var task model.HotmethodTask
			q := tx.Unscoped().Where("tid = ?", a.TaskTID)
			if tx.Dialector.Name() == "postgres" {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			err := q.First(&task).Error
			if err == nil && (task.DeletedAt.Valid || task.ArtifactsPinned) {
				return errArtifactClaimProtected
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		now := time.Now()
		res := tx.Model(&model.Artifact{}).
			Where("id = ? AND status = ? AND deleted_at IS NULL", a.ID, a.Status).
			Updates(map[string]interface{}{
				"status":                 model.ArtifactStatusDeleting,
				"updated_at":             now,
				"next_delete_attempt_at": nil,
				"last_delete_error":      "",
			})
		if res.Error != nil {
			return res.Error
		}
		claimed = res.RowsAffected == 1
		return nil
	})
	if err != nil {
		if !errors.Is(err, errArtifactClaimProtected) {
			s.Logger.Warn("生命周期：领取失败", zap.Uint("artifact_id", a.ID), zap.Error(err))
		}
		return false
	}
	return claimed
}

// processClaimedDeletion 对已领取的 deleting 行执行对象删除并落墓碑；失败则退避重试。
func (s *APIServer) processClaimedDeletion(ctx context.Context, a *model.Artifact, reason string) {
	if a == nil {
		return
	}
	// 阶段二：有 Blob 引用 → 走 Blob 生命周期（最后引用才删物理对象）。
	if a.BlobID != nil && *a.BlobID > 0 {
		s.processClaimedBlobDeletion(ctx, a, reason)
		return
	}
	// 共享 kallsyms：有其它活跃引用时只释放本行；最后一个引用必须先成功
	// 删除共享对象与 ledger，再写墓碑。任何失败都保留 deleting 供后续重试。
	if isKernelSymbolObjectKey(a.ObjectKey) {
		s.processClaimedKernelSymbolDeletion(ctx, a, reason)
		return
	}
	if !s.StorageConnected() {
		s.failArtifactDeletion(ctx, a, errors.New("object storage is disconnected"))
		return
	}
	if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, a.ObjectKey); err != nil {
		s.failArtifactDeletion(ctx, a, err)
		return
	}
	s.completeArtifactDeletion(ctx, a, reason)
}

// processClaimedBlobDeletion 阶段二 Blob 生命周期删除：
//  1. 先落 artifact 墓碑（减少引用）。
//  2. 若 Blob 仍有其它有效引用 → 结束（对象保留）。
//  3. 无引用 → CAS 领取 Blob（ready→deleting）→ 删物理对象 →
//     墓碑 Blob + 清理 ledger 行（kernel_symbol_files/symbol_files）。
//
// 删除失败保留 Blob deleting 状态按退避重试（processBlobDeletingRetries）。
func (s *APIServer) processClaimedBlobDeletion(ctx context.Context, a *model.Artifact, reason string) {
	if a == nil || a.BlobID == nil {
		return
	}
	var blob model.StorageBlob
	if err := s.DB.WithContext(ctx).Where("id = ?", *a.BlobID).First(&blob).Error; err != nil {
		// Blob 行丢失（异常）：回退到 legacy 直接删逻辑 key，避免对象泄漏。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			s.logBlobWarn("blob row missing, fallback to legacy delete",
				zap.Uint("artifact_id", a.ID),
				zap.String("object_key", redactBlobKey(a.ObjectKey)))
			if s.StorageConnected() {
				if delErr := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, a.ObjectKey); delErr != nil {
					s.failArtifactDeletion(ctx, a, delErr)
					return
				}
			}
			s.completeArtifactDeletion(ctx, a, reason)
			return
		}
		s.failArtifactDeletion(ctx, a, err)
		return
	}
	if blob.IsDeleted() {
		// 物理对象已删：直接落 artifact 墓碑。
		s.completeArtifactDeletion(ctx, a, reason)
		return
	}
	// 1) 先落 artifact 墓碑。
	s.completeArtifactDeletion(ctx, a, reason)
	// 2/3) 在同一事务内锁住 Blob、复查引用并领取删除。写入方也会锁同一行，
	// 因而不能在“引用计数为零”和 ready→deleting 之间插入新引用。
	claimedBlob, claimed, err := s.claimUnreferencedBlobDeletion(ctx, blob.ID)
	if err != nil {
		s.logBlobWarn("claim unreferenced blob failed", zap.Uint("blob_id", blob.ID), zap.Error(err))
		return // artifact 已墓碑；孤儿 Blob 对账会在宽限期后继续收敛
	}
	if !claimed {
		return // 仍有引用，或其它 cleaner 已领取
	}
	blob = claimedBlob
	if !s.StorageConnected() {
		s.failBlobDeletion(ctx, &blob, errors.New("object storage is disconnected"))
		return
	}
	if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, blob.ObjectKey); err != nil {
		s.failBlobDeletion(ctx, &blob, err)
		return
	}
	// 删除成功：墓碑 Blob + 清理 ledger 行。
	s.tombstoneBlob(ctx, &blob, reason)
	_ = s.DB.WithContext(ctx).Where("blob_id = ?", blob.ID).Delete(&model.KernelSymbolFile{}).Error
	_ = s.DB.WithContext(ctx).Where("blob_id = ?", blob.ID).Delete(&model.SymbolFile{}).Error
	incArtifactCleanupDeleted(reason)
	incArtifactCleanupDeletedBytes(a.Size)
	s.logBlobState("blob object deleted (last reference)",
		zap.Uint("blob_id", blob.ID),
		zap.String("object_key", redactBlobKey(blob.ObjectKey)),
		zap.Int64("stored_size", blob.StoredSize),
		zap.String("reason", reason))
}

// claimUnreferencedBlobDeletion 把“锁 Blob → 统计引用 → 领取删除”放在一个事务里。
// PostgreSQL 的 FOR UPDATE 与分析端登记引用使用相同的行锁协议。
func (s *APIServer) claimUnreferencedBlobDeletion(ctx context.Context, blobID uint) (model.StorageBlob, bool, error) {
	var blob model.StorageBlob
	claimed := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ? AND deleted_at IS NULL", blobID, model.BlobStatusReady).
			First(&blob).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		refs, err := countBlobRefsDB(tx, blob.ID)
		if err != nil {
			return err
		}
		if refs > 0 {
			return nil
		}
		res := tx.Model(&model.StorageBlob{}).
			Where("id = ? AND status = ? AND deleted_at IS NULL", blob.ID, model.BlobStatusReady).
			Updates(map[string]interface{}{
				"status":                 model.BlobStatusDeleting,
				"next_delete_attempt_at": nil,
				"last_delete_error":      "",
				"updated_at":             time.Now(),
			})
		if res.Error != nil {
			return res.Error
		}
		claimed = res.RowsAffected == 1
		return nil
	})
	return blob, claimed, err
}

// tombstoneBlob 删除成功：写 Blob 墓碑（行永久保留审计）。
func (s *APIServer) tombstoneBlob(ctx context.Context, blob *model.StorageBlob, reason string) {
	if blob == nil {
		return
	}
	now := time.Now()
	_ = s.DB.WithContext(ctx).Model(&model.StorageBlob{}).
		Where("id = ? AND status = ?", blob.ID, model.BlobStatusDeleting).
		Updates(map[string]interface{}{
			"status":                 model.BlobStatusDeleted,
			"deleted_at":             &now,
			"delete_reason":          firstNonEmpty(reason, model.DeleteReasonExpired),
			"next_delete_attempt_at": nil,
			"last_delete_error":      "",
			"updated_at":             now,
		})
}

// failBlobDeletion 删除失败：保留 deleting，按退避重试。
func (s *APIServer) failBlobDeletion(ctx context.Context, blob *model.StorageBlob, delErr error) {
	if blob == nil {
		return
	}
	now := time.Now()
	attempts := blob.DeleteAttempts + 1
	next := now.Add(deleteBackoff(attempts))
	_ = s.DB.WithContext(ctx).Model(&model.StorageBlob{}).
		Where("id = ? AND status = ?", blob.ID, model.BlobStatusDeleting).
		Updates(map[string]interface{}{
			"delete_attempts":        attempts,
			"last_delete_error":      truncateString(delErr.Error(), 1024),
			"next_delete_attempt_at": &next,
			"updated_at":             now,
		})
	incBlobGCFailures()
	s.setBlobError(delErr.Error())
	s.logBlobWarn("blob object delete failed, will retry",
		zap.Uint("blob_id", blob.ID),
		zap.String("object_key", redactBlobKey(blob.ObjectKey)),
		zap.Int("attempt", attempts),
		zap.String("next_attempt", next.Format(time.RFC3339)),
		zap.Error(delErr))
}

// processBlobDeletingRetries 处理到期的 deleting Blob 重试（生命周期循环内）。
func (s *APIServer) processBlobDeletingRetries(ctx context.Context) {
	if s == nil || s.DB == nil {
		return
	}
	now := time.Now()
	var blobs []model.StorageBlob
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND deleted_at IS NULL AND (next_delete_attempt_at IS NULL OR next_delete_attempt_at <= ?)", model.BlobStatusDeleting, now).
		Order("id ASC").Limit(s.retentionBatchLimit()).Find(&blobs).Error; err != nil {
		s.logBlobWarn("blob deleting retry query failed", zap.Error(err))
		return
	}
	for i := range blobs {
		if ctx.Err() != nil {
			break
		}
		blob := &blobs[i]
		// 重试前复查引用：若期间又出现新引用（如 revive），取消删除。
		refs, err := s.countBlobRefs(ctx, blob.ID)
		if err != nil {
			continue
		}
		if refs > 0 {
			// 恢复 ready（对象还在，继续供新引用使用）。
			_ = s.DB.WithContext(ctx).Model(&model.StorageBlob{}).
				Where("id = ? AND status = ?", blob.ID, model.BlobStatusDeleting).
				Updates(map[string]interface{}{
					"status":            model.BlobStatusReady,
					"delete_attempts":   0,
					"last_delete_error": "",
					"updated_at":        time.Now(),
				})
			continue
		}
		if !s.StorageConnected() {
			s.failBlobDeletion(ctx, blob, errors.New("object storage is disconnected"))
			continue
		}
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, blob.ObjectKey); err != nil {
			s.failBlobDeletion(ctx, blob, err)
			continue
		}
		s.tombstoneBlob(ctx, blob, firstNonEmpty(blob.DeleteReason, model.DeleteReasonExpired))
		_ = s.DB.WithContext(ctx).Where("blob_id = ?", blob.ID).Delete(&model.KernelSymbolFile{}).Error
		_ = s.DB.WithContext(ctx).Where("blob_id = ?", blob.ID).Delete(&model.SymbolFile{}).Error
	}
}

func (s *APIServer) processClaimedKernelSymbolDeletion(ctx context.Context, a *model.Artifact, reason string) {
	var otherRefs int64
	if err := s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Where("object_key = ? AND id <> ? AND deleted_at IS NULL", a.ObjectKey, a.ID).
		Count(&otherRefs).Error; err != nil {
		s.failArtifactDeletion(ctx, a, err)
		return
	}
	if otherRefs > 0 {
		s.completeArtifactDeletion(ctx, a, reason)
		return
	}
	if !s.StorageConnected() {
		s.failArtifactDeletion(ctx, a, errors.New("object storage is disconnected"))
		return
	}
	if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, a.ObjectKey); err != nil {
		s.failArtifactDeletion(ctx, a, err)
		return
	}
	if err := s.DB.WithContext(ctx).Where("object_key = ?", a.ObjectKey).Delete(&model.KernelSymbolFile{}).Error; err != nil {
		s.failArtifactDeletion(ctx, a, err)
		return
	}
	s.completeArtifactDeletion(ctx, a, reason)
}

// completeArtifactDeletion 删除成功：写入 deleted/deleted_at/delete_reason（墓碑永久保留）。
func (s *APIServer) completeArtifactDeletion(ctx context.Context, a *model.Artifact, reason string) {
	now := time.Now()
	res := s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Where("id = ? AND status = ?", a.ID, model.ArtifactStatusDeleting).
		Updates(map[string]interface{}{
			"status":                 model.ArtifactStatusDeleted,
			"deleted_at":             &now,
			"delete_reason":          reason,
			"next_delete_attempt_at": nil,
			"last_delete_error":      "",
			"updated_at":             now,
		})
	if res.Error != nil {
		s.Logger.Warn("生命周期：写入墓碑失败", zap.Uint("artifact_id", a.ID), zap.Error(res.Error))
		return
	}
	if res.RowsAffected != 1 {
		return
	}
	incArtifactCleanupDeleted(reason)
	incArtifactCleanupDeletedBytes(a.Size)
	s.Logger.Info("生命周期：产物已清理",
		zap.Uint("artifact_id", a.ID),
		zap.String("tid", a.TaskTID),
		zap.String("kind", a.Kind),
		zap.String("object_key", util.RedactObjectKey(a.ObjectKey)),
		zap.Int64("size", a.Size),
		zap.String("reason", reason))
}

// failArtifactDeletion 删除失败：保留 deleting，按 1m→5m→30m→2h→6h 退避重试。
func (s *APIServer) failArtifactDeletion(ctx context.Context, a *model.Artifact, deleteErr error) {
	now := time.Now()
	attempts := a.DeleteAttempts + 1
	next := now.Add(deleteBackoff(attempts))
	err := s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Where("id = ? AND status = ?", a.ID, model.ArtifactStatusDeleting).
		Updates(map[string]interface{}{
			"delete_attempts":        attempts,
			"last_delete_error":      truncateString(deleteErr.Error(), 1024),
			"next_delete_attempt_at": &next,
			"updated_at":             now,
		}).Error
	if err != nil {
		s.Logger.Warn("生命周期：记录删除失败重试失败", zap.Uint("artifact_id", a.ID), zap.Error(err))
		return
	}
	incArtifactCleanupFailures()
	s.setLifecycleError(deleteErr.Error())
	s.Logger.Warn("生命周期：对象删除失败，稍后重试",
		zap.Uint("artifact_id", a.ID),
		zap.String("object_key", util.RedactObjectKey(a.ObjectKey)),
		zap.Int("attempt", attempts),
		zap.String("next_attempt", next.Format(time.RFC3339)),
		zap.Error(deleteErr))
}

func deleteBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return deleteBackoffMin
	case attempt == 2:
		return deleteBackoff5m
	case attempt == 3:
		return deleteBackoff30m
	case attempt == 4:
		return deleteBackoff2h
	default:
		return deleteBackoff6h
	}
}

// taskDeletedArtifacts 任务主动删除：忽略 pin 与期限，进入相同删除状态机（reason=task_deleted）。
func (s *APIServer) taskDeletedArtifacts(ctx context.Context, tid string) {
	if s.DB == nil || tid == "" {
		return
	}
	var artifacts []model.Artifact
	if err := s.DB.WithContext(ctx).
		Where("task_tid = ? AND deleted_at IS NULL AND status <> ?", tid, model.ArtifactStatusDeleting).
		Find(&artifacts).Error; err != nil {
		s.Logger.Warn("任务删除：查询产物失败", zap.String("tid", tid), zap.Error(err))
		return
	}
	for i := range artifacts {
		if ctx.Err() != nil {
			break
		}
		a := &artifacts[i]
		if !s.claimArtifactForDeletion(ctx, a, false) {
			continue
		}
		s.processClaimedDeletion(ctx, a, model.DeleteReasonTaskDeleted)
	}
}

// pinnedTaskSet 返回已固定任务的 tid 集合。
func (s *APIServer) pinnedTaskSet(ctx context.Context) map[string]bool {
	set := map[string]bool{}
	var tids []string
	if err := s.DB.WithContext(ctx).Model(&model.HotmethodTask{}).
		Where("artifacts_pinned = true AND deleted_at IS NULL").
		Pluck("tid", &tids).Error; err != nil {
		return set
	}
	for _, tid := range tids {
		set[tid] = true
	}
	return set
}

// ------------------------------------------------------------
// observe 候选日志
// ------------------------------------------------------------

func (s *APIServer) logLifecycleCandidates(ctx context.Context) {
	if s.DB == nil {
		return
	}
	candidates, err := s.lifecycleCandidateIDs(ctx, s.retentionBatchLimit(), false)
	if err != nil {
		s.Logger.Warn("生命周期 observe：查询候选失败", zap.Error(err))
		return
	}
	if len(candidates) == 0 {
		return
	}
	var bytes int64
	byClass := map[string]int{}
	for _, a := range candidates {
		bytes += a.Size
		byClass[a.Retention]++
	}
	s.Logger.Info("生命周期 observe：到期候选（未删除）",
		zap.String("event", "lifecycle_observe_candidates"),
		zap.Int("candidates", len(candidates)),
		zap.Int64("bytes", bytes),
		zap.Any("by_class", byClass))
}

// ------------------------------------------------------------
// 统计与指标
// ------------------------------------------------------------

type artifactLifecycleStats struct {
	Mode             string     `json:"lifecycle_mode"`
	PolicyVersion    string     `json:"policy_version"`
	ReconcileBacklog int64      `json:"reconcile_backlog"`
	ReadyCount       int64      `json:"ready_count"`
	ReadyBytes       int64      `json:"ready_bytes"`
	PinnedCount      int64      `json:"pinned_count"`
	PinnedBytes      int64      `json:"pinned_bytes"`
	DueCount         int64      `json:"due_count"`
	DueBytes         int64      `json:"due_bytes"`
	DeletingCount    int64      `json:"deleting_count"`
	DeletingBytes    int64      `json:"deleting_bytes"`
	DeletedCount     int64      `json:"deleted_count"`
	DeletedBytes     int64      `json:"deleted_bytes"`
	SupersededCount  int64      `json:"superseded_count"` // 阶段 4：被替换旧代（result_superseded）
	SupersededBytes  int64      `json:"superseded_bytes"`
	LastRunAt        *time.Time `json:"last_run_at"`
	LastError        string     `json:"last_error"`
}

// 生命周期内存态（供 status 接口与指标）。
type artifactLifecycleState struct {
	mu        sync.Mutex
	stats     artifactLifecycleStats
	lastRunAt *time.Time
	lastError string
}

var lifecycleState artifactLifecycleState

func (s *APIServer) setLifecycleBacklog(backlog int64) {
	lifecycleState.mu.Lock()
	lifecycleState.stats.ReconcileBacklog = backlog
	lifecycleState.mu.Unlock()
}

func (s *APIServer) setLifecycleLastRun(at *time.Time) {
	lifecycleState.mu.Lock()
	if at != nil {
		lifecycleState.lastRunAt = at
	}
	lifecycleState.mu.Unlock()
}

func (s *APIServer) setLifecycleError(message string) {
	lifecycleState.mu.Lock()
	lifecycleState.lastError = truncateString(message, 1024)
	lifecycleState.mu.Unlock()
}

func (s *APIServer) lifecycleStatsSnapshot() artifactLifecycleStats {
	lifecycleState.mu.Lock()
	defer lifecycleState.mu.Unlock()
	out := lifecycleState.stats
	out.Mode = s.Config.Retention.LifecycleMode
	out.PolicyVersion = s.lifecyclePolicyVersion()
	if lifecycleState.lastRunAt != nil {
		t := *lifecycleState.lastRunAt
		out.LastRunAt = &t
	}
	out.LastError = lifecycleState.lastError
	return out
}

// collectLifecycleStats 计算当前统计（每次刷新/请求时实时查询）。
func (s *APIServer) collectLifecycleStats(ctx context.Context) artifactLifecycleStats {
	stats := artifactLifecycleStats{
		Mode:          s.Config.Retention.LifecycleMode,
		PolicyVersion: s.lifecyclePolicyVersion(),
	}
	now := time.Now()
	_ = s.lifecycleStaleQuery(ctx, s.lifecyclePolicyVersion()).
		Count(&stats.ReconcileBacklog).Error

	type cnt struct {
		Status string
		Cnt    int64
		Bytes  int64
	}
	var rows []cnt
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Select("status, count(*) as cnt, COALESCE(SUM(size),0) as bytes").
		Group("status").Scan(&rows).Error
	for _, r := range rows {
		switch r.Status {
		case model.ArtifactStatusReady:
			stats.ReadyCount, stats.ReadyBytes = r.Cnt, r.Bytes
		case model.ArtifactStatusDeleting:
			stats.DeletingCount, stats.DeletingBytes = r.Cnt, r.Bytes
		case model.ArtifactStatusDeleted:
			stats.DeletedCount, stats.DeletedBytes = r.Cnt, r.Bytes
		}
	}

	// 阶段 4：被替换旧代统计（result_superseded，非 deleted）。
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Select("COALESCE(SUM(size),0) as bytes").
		Where("retention = ? AND deleted_at IS NULL AND status = ?", model.RetentionClassResultSuperseded, model.ArtifactStatusReady).
		Scan(&stats.SupersededBytes).Error
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Where("retention = ? AND deleted_at IS NULL AND status = ?", model.RetentionClassResultSuperseded, model.ArtifactStatusReady).
		Count(&stats.SupersededCount).Error

	// pinned：任务固定集合下的非 deleted artifact
	pinned := s.pinnedTaskSet(ctx)
	if len(pinned) > 0 {
		tids := make([]string, 0, len(pinned))
		for tid := range pinned {
			tids = append(tids, tid)
		}
		var pRows []cnt
		_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
			Select("status, count(*) as cnt, COALESCE(SUM(size),0) as bytes").
			Where("task_tid IN ? AND deleted_at IS NULL", tids).
			Group("status").Scan(&pRows).Error
		for _, r := range pRows {
			stats.PinnedCount += r.Cnt
			stats.PinnedBytes += r.Bytes
		}
	}

	// due：到期候选（ready/failed、expired、not-before 已过、未 pin）
	var dueBytes struct{ Bytes int64 }
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Joins("LEFT JOIN hotmethod_tasks t ON t.tid = artifacts.task_tid").
		Select("COALESCE(SUM(artifacts.size),0) as bytes").
		Where("artifacts.deleted_at IS NULL").
		Where("artifacts.status IN ?", []string{model.ArtifactStatusReady, model.ArtifactStatusFailed}).
		Where("artifacts.expires_at IS NOT NULL AND artifacts.expires_at <= ?", now).
		Where("(artifacts.retention_not_before IS NULL OR artifacts.retention_not_before <= ?)", now).
		Where("(t.tid IS NULL OR t.deleted_at IS NULL)").
		Where("(t.tid IS NULL OR t.artifacts_pinned = false)").
		Scan(&dueBytes).Error
	stats.DueBytes = dueBytes.Bytes
	var dueCnt int64
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Joins("LEFT JOIN hotmethod_tasks t ON t.tid = artifacts.task_tid").
		Where("artifacts.deleted_at IS NULL").
		Where("artifacts.status IN ?", []string{model.ArtifactStatusReady, model.ArtifactStatusFailed}).
		Where("artifacts.expires_at IS NOT NULL AND artifacts.expires_at <= ?", now).
		Where("(artifacts.retention_not_before IS NULL OR artifacts.retention_not_before <= ?)", now).
		Where("(t.tid IS NULL OR t.deleted_at IS NULL)").
		Where("(t.tid IS NULL OR t.artifacts_pinned = false)").
		Count(&dueCnt).Error
	stats.DueCount = dueCnt

	lifecycleState.mu.Lock()
	lifecycleState.stats = stats
	lifecycleState.mu.Unlock()
	return stats
}

// refreshLifecycleMetrics 刷新 gauge 指标并缓存最新统计。
func (s *APIServer) refreshLifecycleMetrics(ctx context.Context) {
	if s.DB == nil {
		return
	}
	stats := s.collectLifecycleStats(ctx)
	updateArtifactLifecycleGauges(stats)
}

// ------------------------------------------------------------
// 辅助
// ------------------------------------------------------------

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func (s *APIServer) countTaskLiveArtifacts(tid string) int64 {
	var count int64
	_ = s.DB.Model(&model.Artifact{}).
		Where("task_tid = ? AND deleted_at IS NULL", tid).
		Count(&count).Error
	return count
}

// ------------------------------------------------------------
// Pin 接口
// ------------------------------------------------------------

// PinTaskArtifacts 任务级固定/取消固定。
// POST /api/v1/tasks/{tid}/artifacts/pin
// 仅任务 owner（可管理）或平台管理员可操作；写入 TaskStatusEvent 审计 payload。
func (s *APIServer) PinTaskArtifacts(c *gin.Context) {
	tid := c.Param("tid")
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.RespondHTTPError(c, http.StatusForbidden, ErrCodeAuthForbidden, "Viewer 不能修改产物固定状态")
		return
	}
	var req PinArtifactsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "请求参数错误: "+err.Error())
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len(req.Reason) > 256 {
		req.Reason = req.Reason[:256]
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

	now := time.Now()
	actor := firstNonEmpty(auth.Name, auth.UID)
	protected := int64(0)
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// 与自动 cleaner 使用同一任务行锁：pin 先拿锁则 cleaner 无法领取；
		// cleaner 已领取则 pin 返回 409，不能声称已保护正在删除的对象。
		var locked model.HotmethodTask
		q := tx.Where("tid = ?", tid)
		if tx.Dialector.Name() == "postgres" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&locked).Error; err != nil {
			return err
		}
		if req.Pinned {
			var deleting int64
			if err := tx.Model(&model.Artifact{}).
				Where("task_tid = ? AND status = ? AND deleted_at IS NULL", tid, model.ArtifactStatusDeleting).
				Count(&deleting).Error; err != nil {
				return err
			}
			if deleting > 0 {
				return errArtifactDeleting
			}
		}

		updates := map[string]interface{}{"artifacts_pinned": req.Pinned}
		if req.Pinned {
			updates["artifacts_pinned_at"] = &now
			updates["artifacts_pinned_by"] = actor
			updates["artifacts_pin_reason"] = req.Reason
		} else {
			updates["artifacts_pinned_at"] = nil
			updates["artifacts_pinned_by"] = ""
			updates["artifacts_pin_reason"] = ""
		}
		if err := tx.Model(&model.HotmethodTask{}).Where("tid = ?", tid).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.Artifact{}).Where("task_tid = ? AND deleted_at IS NULL", tid).Count(&protected).Error; err != nil {
			return err
		}
		action := "固定全部产物"
		if !req.Pinned {
			action = "取消固定"
		}
		return recordTaskStatusEventWithPayloadTx(tx, tid, task.Status, task.Status, action, "artifact_pin", map[string]interface{}{
			"pinned": req.Pinned, "by": actor, "at": now, "reason": req.Reason, "protected_artifacts": protected,
		})
	})
	if errors.Is(err, errArtifactDeleting) {
		s.RespondHTTPError(c, http.StatusConflict, ErrCodeArtifactUploadFailed, "已有产物进入清理流程，无法保证完整固定，请稍后刷新")
		return
	}
	if err != nil {
		s.Logger.Error("更新任务产物固定状态失败", zap.String("tid", tid), zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "更新固定状态失败")
		return
	}
	s.Logger.Info("任务产物固定状态变更",
		zap.String("tid", tid),
		zap.Bool("pinned", req.Pinned),
		zap.String("by", actor),
		zap.String("reason", req.Reason),
		zap.Int64("protected_artifacts", protected),
	)

	response := gin.H{
		"tid":                 tid,
		"pinned":              req.Pinned,
		"protected_artifacts": protected,
	}
	if req.Pinned {
		response["pinned_by"] = actor
		response["pinned_at"] = &now
		response["reason"] = req.Reason
	}
	s.RespondOK(c, response)
}

// recordTaskStatusEventWithPayload 带审计 payload 的状态事件（pin 审计）。
func (s *APIServer) recordTaskStatusEventWithPayload(tid string, fromStatus int, toStatus int, reason string, source string, payload map[string]interface{}) {
	if tid == "" {
		return
	}
	if source == "" {
		source = "apiserver"
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		return recordTaskStatusEventWithPayloadTx(tx, tid, fromStatus, toStatus, reason, source, payload)
	})
	if err != nil {
		s.Logger.Warn("记录任务状态事件失败", zap.String("tid", tid), zap.Error(err))
	}
}

func recordTaskStatusEventWithPayloadTx(tx *gorm.DB, tid string, fromStatus int, toStatus int, reason string, source string, payload map[string]interface{}) error {
	payloadJSON, _ := json.Marshal(payload)
	return tx.Create(&model.TaskStatusEvent{
		TID:          tid,
		FromStatus:   fromStatus,
		ToStatus:     toStatus,
		Reason:       reason,
		Source:       source,
		Sequence:     nextTaskEventSequenceTx(tx, tid),
		SourceModule: source,
		Payload:      payloadJSON,
		CreatedAt:    time.Now(),
	}).Error
}
