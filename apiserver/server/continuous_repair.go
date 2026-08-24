// ============================================================
// server/continuous_repair.go — 阶段一：历史重复自动修复
// ============================================================
// 修复目标：同一逻辑窗口（session + signal_type + 起止时间）在 profile_windows
// 中最多保留一份，其余记入 continuous_repair_audits 审计表后排除出查询。
//
//   - dry-run（默认）：只读列出重复窗口、受影响 batch、对象与 Parquet 小时块，
//     不修改任何数据。
//   - apply：先暂停 Continuous ingest（通过 capacity halted 全局开关，不停止
//     用户业务进程），对每个重复组按"最早 ACK 为 canonical"规则保留一份、排除
//     其余；为 canonical 回填稳定 legacy window_id；重建受影响的 coverage
//     segments 与 Parquet raw block（新块校验通过后原子激活，旧块 superseded）；
//     修复前后输出行数、分信号计数和摘要对账报告。
//
// 任何无法读取原始 payload 的组都会中止 apply，不做猜测性删除。
// ============================================================

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
)

const continuousRepairTenant = "default"

type continuousRepairReq struct {
	Mode       string     `json:"mode" binding:"required"` // dry-run | apply
	SessionSID string     `json:"session_sid"`
	From       *time.Time `json:"from"`
	To         *time.Time `json:"to"`
}

type continuousRepairGroup struct {
	SessionSID string
	SignalType string
	Start      time.Time
	End        time.Time
	Rows       []model.ProfileWindow
	Kept       model.ProfileWindow
	Excluded   []model.ProfileWindow
	Reason     string
}

type continuousRepairReport struct {
	RunID            string                       `json:"run_id"`
	Mode             string                       `json:"mode"`
	Groups           int                          `json:"groups"`
	Excluded         int                          `json:"excluded_windows"`
	Kept             int                          `json:"kept_windows"`
	AuditRows        int                          `json:"audit_rows"`
	AffectedHours    int                          `json:"affected_hours"`
	Before           continuousRepairCounts       `json:"before"`
	After            continuousRepairCounts       `json:"after"`
	Details          []continuousRepairGroupBrief `json:"details"`
	SupersededBlocks []string                     `json:"superseded_blocks,omitempty"`
}

type continuousRepairCounts struct {
	Windows   int            `json:"windows"`
	Batches   int            `json:"batches"`
	BySignal  map[string]int `json:"by_signal"`
	BySession map[string]int `json:"by_session"`
}

type continuousRepairGroupBrief struct {
	SessionSID string    `json:"session_sid"`
	SignalType string    `json:"signal_type"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	WindowID   string    `json:"window_id"`
	KeptBatch  string    `json:"kept_batch_bid"`
	Excluded   []string  `json:"excluded_batch_bids"`
	Reason     string    `json:"reason"`
}

// RepairContinuousDuplicates HTTP 入口：dry-run 只读；apply 执行修复。
// 内部运维端点，仅限 Agent/内部认证上下文调用。
func (s *APIServer) RepairContinuousDuplicates(c *gin.Context) {
	// 这是会修改所有租户 Continuous 元数据与 Parquet 派生块的运维操作，
	// 普通 Agent/Operator 即使能访问 internal 路由也不得执行。
	if !s.AuthContext(c).IsPlatformAdmin() {
		s.forbid(c)
		return
	}
	var req continuousRepairReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	if req.Mode != "dry-run" && req.Mode != "apply" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "mode 仅支持 dry-run/apply")
		return
	}
	report, err := s.runContinuousRepair(c.Request.Context(), req.Mode, req.SessionSID, req.From, req.To)
	if err != nil {
		s.Logger.Error("Continuous 重复修复失败", zap.String("mode", req.Mode), zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "Continuous 重复修复失败: "+err.Error())
		return
	}
	s.RespondOK(c, report)
}

func (s *APIServer) runContinuousRepair(ctx context.Context, mode, sessionSID string, from, to *time.Time) (continuousRepairReport, error) {
	report := continuousRepairReport{
		RunID:   "repair-" + time.Now().Format("20060102-150405"),
		Mode:    mode,
		Details: []continuousRepairGroupBrief{},
		Before:  continuousRepairCounts{BySignal: map[string]int{}, BySession: map[string]int{}},
		After:   continuousRepairCounts{BySignal: map[string]int{}, BySession: map[string]int{}},
	}
	report.Before = s.continuousRepairSnapshot(ctx, sessionSID, from, to)

	groups, err := s.findContinuousDuplicateWindows(ctx, sessionSID, from, to)
	if err != nil {
		return report, err
	}
	// 统一预计算每组 kept/excluded/reason（dry-run 与 apply 共用同一套
	// "最早 ACK 为 canonical" 规则）。
	prepared := make([]continuousRepairGroup, 0, len(groups))
	for _, group := range groups {
		if len(group.Rows) < 2 {
			continue
		}
		sort.SliceStable(group.Rows, func(i, j int) bool {
			ri := s.continuousBatchReceivedAt(ctx, group.Rows[i].BatchBID)
			rj := s.continuousBatchReceivedAt(ctx, group.Rows[j].BatchBID)
			if ri.Equal(rj) {
				return group.Rows[i].ID < group.Rows[j].ID
			}
			return ri.Before(rj)
		})
		group.Kept = group.Rows[0]
		group.Excluded = append([]model.ProfileWindow{}, group.Rows[1:]...)
		group.Reason = "conflicting_keep_earliest_ack"
		identical := true
		for _, row := range group.Excluded {
			if !continuousWindowsSameContent(group.Kept, row) {
				identical = false
				break
			}
		}
		if identical {
			group.Reason = "payload_identical_keep_earliest"
		}
		prepared = append(prepared, group)
	}
	report.Groups = len(prepared)

	if mode == "dry-run" {
		for _, group := range prepared {
			report.Details = append(report.Details, continuousRepairGroupBrief{
				SessionSID: group.SessionSID, SignalType: group.SignalType,
				Start: group.Start, End: group.End, WindowID: group.Kept.WindowID,
				KeptBatch: group.Kept.BatchBID,
				Excluded:  batchBIDs(group.Excluded), Reason: group.Reason,
			})
			report.Excluded += len(group.Excluded)
			report.Kept += 1
		}
		report.AuditRows = report.Excluded
		report.After = report.Before
		return report, nil
	}

	// apply 前必须确认每一条参与修复的 DB window 都能解析到原始 payload；
	// 任何对象缺失都整次中止，不能依据孤立元数据猜测并删除。
	for _, group := range prepared {
		if err := s.verifyContinuousRepairGroupPayloads(ctx, group); err != nil {
			return report, err
		}
	}

	// ---- apply ----
	// 暂停 Continuous ingest（不停止用户业务进程）：Agent 已产生的窗口继续
	// 上报/ACK，但新窗口不会产生。修复完成后由运维恢复。
	s.pauseContinuousIngest()
	defer s.resumeContinuousIngest()

	affectedHours := map[string]time.Time{} // hourStart(UTC) → hourStart
	audits := []model.ContinuousRepairAudit{}
	var firstErr error
	for _, group := range prepared {
		// canonical 已按最早 ACK 排序并选定，直接执行。
		kept := group.Kept
		excluded := group.Excluded
		reason := group.Reason

		// 为 canonical 回填稳定 legacy window_id（历史 v2 行为空）。
		if kept.WindowID == "" {
			kept.WindowID = continuousLegacyWindowID(kept)
		}

		groupAudits := make([]model.ContinuousRepairAudit, 0, len(excluded))
		for _, row := range excluded {
			groupAudits = append(groupAudits, model.ContinuousRepairAudit{
				RunID: report.RunID, Tenant: continuousRepairTenant,
				SessionSID:       group.SessionSID,
				LogicalWindowKey: continuousLogicalWindowKey(group.SessionSID, group.SignalType, group.Start, group.End),
				WindowID:         kept.WindowID, SignalType: group.SignalType,
				WindowStart: group.Start, WindowEnd: group.End,
				KeptBatchBID: kept.BatchBID, ExcludedBatchBID: row.BatchBID,
				KeptContentSHA256: kept.ContentSHA256, ExcludedContentSHA256: row.ContentSHA256,
				Reason: reason, CreatedAt: time.Now(),
			})
		}

		// 审计、canonical 回填和重复行删除必须同一事务提交；审计写失败时
		// 不允许留下无法追溯的数据删除。
		if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if len(groupAudits) > 0 {
				if err := tx.CreateInBatches(groupAudits, 500).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&model.ProfileWindow{}).
				Where("id = ?", kept.ID).
				Update("window_id", kept.WindowID).Error; err != nil {
				return err
			}
			for _, row := range excluded {
				if err := tx.Delete(&model.ProfileWindow{}, row.ID).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.Logger.Error("Continuous 修复删除重复窗口失败",
				zap.String("session", group.SessionSID), zap.String("signal", group.SignalType), zap.Error(err))
			continue
		}

		audits = append(audits, groupAudits...)
		hour := group.Start.UTC().Truncate(time.Hour)
		affectedHours[hour.Format(time.RFC3339)] = hour
		report.Details = append(report.Details, continuousRepairGroupBrief{
			SessionSID: group.SessionSID, SignalType: group.SignalType,
			Start: group.Start, End: group.End, WindowID: kept.WindowID,
			KeptBatch: kept.BatchBID, Excluded: batchBIDs(excluded), Reason: reason,
		})
		report.Excluded += len(excluded)
		report.Kept += 1
	}

	report.AuditRows = len(audits)

	// 重建受影响的 coverage segments（按 session+signal+hour）与 Parquet raw 块。
	hourStarts := make([]time.Time, 0, len(affectedHours))
	for _, hour := range affectedHours {
		hourStarts = append(hourStarts, hour)
	}
	sort.Slice(hourStarts, func(i, j int) bool { return hourStarts[i].Before(hourStarts[j]) })
	for _, hour := range hourStarts {
		// 按 (session, signal) 重建该小时 coverage。
		if err := s.rebuildAffectedCoverage(ctx, hour, groups); err != nil && firstErr == nil {
			firstErr = err
		}
		// 重建 Parquet raw 块（新块校验通过后原子激活，旧块 superseded）。
		built, err := s.pqBuildRawHour(ctx, continuousRepairTenant, hour)
		if err != nil {
			s.Logger.Warn("Continuous 修复重建 Parquet raw 小时失败",
				zap.Time("hour", hour), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if built {
			report.SupersededBlocks = append(report.SupersededBlocks, hour.Format(time.RFC3339))
		}
		report.AffectedHours++
	}

	report.After = s.continuousRepairSnapshot(ctx, sessionSID, from, to)
	if firstErr != nil {
		return report, firstErr
	}
	return report, nil
}

func (s *APIServer) verifyContinuousRepairGroupPayloads(ctx context.Context, group continuousRepairGroup) error {
	for _, row := range group.Rows {
		if row.ObjectKey == "" {
			return fmt.Errorf("repair preflight: window %d 缺少 object_key", row.ID)
		}
		batches, err := s.loadContinuousBatches(ctx, row.ObjectKey)
		if err != nil {
			return fmt.Errorf("repair preflight: 读取 %s 失败: %w", row.ObjectKey, err)
		}
		batch, _, ok := continuousResolveBatch(row, batches, continuousBatchIndex(batches))
		if !ok || batch == nil {
			return fmt.Errorf("repair preflight: window %d 无法解析 batch %s", row.ID, row.BatchBID)
		}
		matched := false
		for _, payloadWindow := range batch.Windows {
			if !payloadWindow.WindowStart.Equal(row.WindowStart) || !payloadWindow.WindowEnd.Equal(row.WindowEnd) {
				continue
			}
			for _, signal := range continuousWindowSignalRows(payloadWindow) {
				if signal.SignalType == row.SignalType {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return fmt.Errorf("repair preflight: window %d 在原始 payload 中不存在", row.ID)
		}
	}
	return nil
}

// continuousBatchReceivedAt 查询 batch 的接收时间（用于"最早 ACK 为 canonical"）。
func (s *APIServer) continuousBatchReceivedAt(ctx context.Context, bid string) time.Time {
	if bid == "" {
		return time.Time{}
	}
	var batch model.ProfileBatch
	if err := s.DB.WithContext(ctx).Select("received_at").Where("bid = ?", bid).First(&batch).Error; err != nil {
		return time.Time{}
	}
	return batch.ReceivedAt
}

// continuousWindowsSameContent 判断两个窗口内容是否一致：优先用 content_sha256
// （v3 新协议）；历史 v2 无摘要时退化为 object_key + signal_counts + sample_count。
func continuousWindowsSameContent(a, b model.ProfileWindow) bool {
	if a.ContentSHA256 != "" && b.ContentSHA256 != "" {
		return a.ContentSHA256 == b.ContentSHA256
	}
	if a.ObjectKey != b.ObjectKey {
		return false
	}
	if a.SampleCount != b.SampleCount {
		return false
	}
	return true
}

// continuousLegacyWindowID 为历史 canonical 窗口回填稳定 legacy window_id，
// 使部分唯一索引 (session, window_id, signal_type) 对修复后的行生效。
func continuousLegacyWindowID(window model.ProfileWindow) string {
	sum := sha256.Sum256([]byte(window.SessionSID + "|" + window.SignalType + "|" +
		window.WindowStart.UTC().Format(time.RFC3339Nano) + "|" + window.WindowEnd.UTC().Format(time.RFC3339Nano)))
	return "cpw-legacy-" + hex.EncodeToString(sum[:16])
}

func continuousLogicalWindowKey(sessionSID, signalType string, start, end time.Time) string {
	return sessionSID + "|" + signalType + "|" + start.UTC().Format(time.RFC3339Nano) + "|" + end.UTC().Format(time.RFC3339Nano)
}

func batchBIDs(rows []model.ProfileWindow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.BatchBID)
	}
	return out
}

// continuousRepairWindowKey 重复窗口分组的扫描结构（字段必须导出，GORM 无法
// 给未导出字段赋值；用 gorm column 标签固定列名，避免 GORM 命名推断把
// SessionSID 误映射成 session_s_id）。
type continuousRepairWindowKey struct {
	SessionSID  string    `gorm:"column:session_sid"`
	SignalType  string    `gorm:"column:signal_type"`
	WindowStart time.Time `gorm:"column:window_start"`
	WindowEnd   time.Time `gorm:"column:window_end"`
}

// findContinuousDuplicateWindows 找出同一逻辑窗口（session+signal+起止时间）
// 出现多次的窗口组（含全部参与行，供 dry-run 列表与 apply 处理）。
func (s *APIServer) findContinuousDuplicateWindows(ctx context.Context, sessionSID string, from, to *time.Time) ([]continuousRepairGroup, error) {
	query := s.DB.WithContext(ctx).Model(&model.ProfileWindow{})
	if sessionSID != "" {
		query = query.Where("session_sid = ?", sessionSID)
	}
	if from != nil {
		query = query.Where("window_end >= ?", *from)
	}
	if to != nil {
		query = query.Where("window_start <= ?", *to)
	}
	var dupKeys []continuousRepairWindowKey
	err := query.
		Select("session_sid, signal_type, window_start, window_end, COUNT(*) AS cnt").
		Group("session_sid, signal_type, window_start, window_end").
		Having("COUNT(*) > 1").
		Order("session_sid ASC, window_start ASC").
		Scan(&dupKeys).Error
	if err != nil {
		return nil, err
	}
	groups := make([]continuousRepairGroup, 0, len(dupKeys))
	for _, k := range dupKeys {
		var rows []model.ProfileWindow
		if err := s.DB.WithContext(ctx).
			Where("session_sid = ? AND signal_type = ? AND window_start = ? AND window_end = ?",
				k.SessionSID, k.SignalType, k.WindowStart, k.WindowEnd).
			Order("id ASC").Find(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) < 2 {
			continue
		}
		groups = append(groups, continuousRepairGroup{
			SessionSID: k.SessionSID, SignalType: k.SignalType, Start: k.WindowStart, End: k.WindowEnd, Rows: rows,
		})
	}
	return groups, nil
}

// continuousRepairSnapshot 修复前后对账快照（行数、分信号计数、分 session 计数）。
func (s *APIServer) continuousRepairSnapshot(ctx context.Context, sessionSID string, from, to *time.Time) continuousRepairCounts {
	counts := continuousRepairCounts{BySignal: map[string]int{}, BySession: map[string]int{}}
	q := s.DB.WithContext(ctx).Model(&model.ProfileWindow{})
	if sessionSID != "" {
		q = q.Where("session_sid = ?", sessionSID)
	}
	if from != nil {
		q = q.Where("window_end >= ?", *from)
	}
	if to != nil {
		q = q.Where("window_start <= ?", *to)
	}
	type row struct {
		SessionSID string `gorm:"column:session_sid"`
		SignalType string `gorm:"column:signal_type"`
		Cnt        int    `gorm:"column:cnt"`
	}
	var rows []row
	_ = q.Select("session_sid, signal_type, COUNT(*) AS cnt").
		Group("session_sid, signal_type").Scan(&rows).Error
	for _, r := range rows {
		counts.Windows += r.Cnt
		counts.BySignal[r.SignalType] += r.Cnt
		counts.BySession[r.SessionSID] += r.Cnt
	}
	var batches int64
	batchQuery := s.DB.WithContext(ctx).Model(&model.ProfileBatch{})
	if sessionSID != "" {
		batchQuery = batchQuery.Where("session_sid = ?", sessionSID)
	}
	_ = batchQuery.Count(&batches).Error
	counts.Batches = int(batches)
	return counts
}

// rebuildAffectedCoverage 为受影响的 (session, signal, hour) 重建 coverage。
func (s *APIServer) rebuildAffectedCoverage(ctx context.Context, hour time.Time, groups []continuousRepairGroup) error {
	seen := map[string]bool{}
	for _, group := range groups {
		groupHour := group.Start.UTC().Truncate(time.Hour)
		if !groupHour.Equal(hour) {
			continue
		}
		key := group.SessionSID + "|" + group.SignalType
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return s.pqRebuildCoverageSegmentsTx(tx, continuousRepairTenant, hour, group.SignalType,
				"repair:"+hour.Format("20060102-150405"), 1)
		}); err != nil {
			return err
		}
	}
	return nil
}

// pauseContinuousIngest / resumeContinuousIngest 维护窗口内暂停 Continuous
// ingest（不停止用户业务进程），修复期间阻止新窗口产生。
func (s *APIServer) pauseContinuousIngest() {
	// 复用容量暂停开关：Agent 进入 waiting/server_storage_pressure，停止产生
	// 新窗口；已产生窗口继续上报/ACK。
	s.setCapacityPaused(true)
}

func (s *APIServer) resumeContinuousIngest() {
	s.setCapacityPaused(false)
}

// setCapacityPaused 是容量暂停的内部写入口（叠加在 capacityHalted 之上）。
func (s *APIServer) setCapacityPaused(paused bool) {
	continuousRepairPause.Store(paused)
}

// continuousRepairPause 维护期暂停标志（叠加在 capacityHalted 之上）。
var continuousRepairPause atomic.Bool
