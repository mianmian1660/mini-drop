// ============================================================
// server/sentinel_rule.go — 哨兵规则 CRUD + 判异事件查询
// ============================================================
// 对应 docs/sentinel-rule-frontend-design.md §6 的四个接口。只是
// model.SentinelRule / model.DetectionEvent 的薄封装，不涉及判异逻辑本身
// （判异循环见 detection.go）。
// ============================================================

package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// CreateSentinelRuleReq 创建哨兵规则请求。
type CreateSentinelRuleReq struct {
	Name            string  `json:"name" binding:"required"`
	TargetIP        string  `json:"target_ip" binding:"required"`
	Signal          string  `json:"signal" binding:"required"`
	Metric          string  `json:"metric" binding:"required"`
	FloorValue      float64 `json:"floor_value" binding:"required"`
	CooldownSeconds int     `json:"cooldown_seconds"`
	// KFactor 滚动基线判异灵敏度（§10.2），不传或传 <=0 时用 SentinelRule 的 GORM 默认值（5）。
	KFactor float64 `json:"k_factor"`
	// PersistenceWindows/PersistenceMinHits 持续性判断（§10.3），不传或传 <=0 时默认 1/1
	// （等价于只看最新一个窗口）。PersistenceMinHits 不能大于 PersistenceWindows，
	// 否则这条规则永远不可能触发（最多命中 PersistenceWindows 次）。
	PersistenceWindows int `json:"persistence_windows"`
	PersistenceMinHits int `json:"persistence_min_hits"`
}

var sentinelValidMetrics = map[string]bool{"p50": true, "p95": true, "p99": true}

// sentinelValidDBSnapshotMetrics db_snapshot 信号专用的 metric 取值（见 §10.1），
// 语义和 histogram 类的 p50/p95/p99 完全不同，不能共用同一张校验表。
var sentinelValidDBSnapshotMetrics = map[string]bool{"lock_wait": true, "digest": true}

// ----------------------------------------------------------
// CreateSentinelRule 创建哨兵规则
// POST /api/v1/sentinel-rules
// ----------------------------------------------------------
func (s *APIServer) CreateSentinelRule(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	var req CreateSentinelRuleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	// db_snapshot 不在 detectionSignalTaskKind 里——它判异命中后不建诊断任务（§10.1：
	// script_diagnostic 的 Runner 未接入，见 evaluateDBSnapshotRule 注释），所以单独放行。
	if _, ok := detectionSignalTaskKind[req.Signal]; !ok && req.Signal != "db_snapshot" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "signal 暂不支持自动判异: "+req.Signal)
		return
	}
	if req.Signal == "db_snapshot" {
		if !sentinelValidDBSnapshotMetrics[req.Metric] {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "db_snapshot 的 metric 仅支持 lock_wait/digest")
			return
		}
	} else if !sentinelValidMetrics[req.Metric] {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "metric 仅支持 p50/p95/p99")
		return
	}
	if req.FloorValue <= 0 {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "floor_value 必须大于 0")
		return
	}
	if req.CooldownSeconds <= 0 {
		req.CooldownSeconds = 900
	}
	if req.KFactor <= 0 {
		req.KFactor = detectionDefaultKFactor
	}
	if req.PersistenceWindows <= 0 {
		req.PersistenceWindows = 1
	}
	if req.PersistenceMinHits <= 0 {
		req.PersistenceMinHits = 1
	}
	if req.PersistenceMinHits > req.PersistenceWindows {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument,
			"persistence_min_hits 不能大于 persistence_windows，否则规则永远不会触发")
		return
	}

	now := time.Now()
	rule := &model.SentinelRule{
		SID:                "sr-" + util.GenTID()[4:],
		Name:               req.Name,
		TargetIP:           req.TargetIP,
		Signal:             req.Signal,
		Metric:             req.Metric,
		FloorValue:         req.FloorValue,
		KFactor:            req.KFactor,
		CooldownSeconds:    req.CooldownSeconds,
		PersistenceWindows: req.PersistenceWindows,
		PersistenceMinHits: req.PersistenceMinHits,
		Enabled:            true,
		UID:                auth.UID,
		UserName:           auth.Name,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := s.DB.Create(rule).Error; err != nil {
		s.Logger.Error("创建哨兵规则失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建哨兵规则失败")
		return
	}
	rule.CanManage = true
	s.RespondOK(c, rule)
}

// ----------------------------------------------------------
// ListSentinelRules 按目标/信号查询哨兵规则
// GET /api/v1/sentinel-rules?target_ip=&signal=
// ----------------------------------------------------------
func (s *APIServer) ListSentinelRules(c *gin.Context) {
	auth := s.AuthContext(c)
	query := s.DB.Model(&model.SentinelRule{}).Order("created_at DESC")
	if targetIP := strings.TrimSpace(c.Query("target_ip")); targetIP != "" {
		query = query.Where("target_ip = ?", targetIP)
	}
	if signal := strings.TrimSpace(c.Query("signal")); signal != "" {
		query = query.Where("signal = ?", signal)
	}

	var rules []model.SentinelRule
	if err := query.Find(&rules).Error; err != nil {
		s.Logger.Error("查询哨兵规则失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询失败")
		return
	}
	if rules == nil {
		rules = []model.SentinelRule{}
	}
	for i := range rules {
		rules[i].CanManage = s.canManageOwner(rules[i].UID, auth)
	}
	s.RespondOK(c, gin.H{"rules": rules, "total": len(rules)})
}

// ----------------------------------------------------------
// DeleteSentinelRule 删除（停用）哨兵规则
// DELETE /api/v1/sentinel-rules/:sid
// 只停止未来判异，不级联删除 detection_events：审计记录要留痕，
// 见 docs/sentinel-rule-frontend-design.md §3。
// ----------------------------------------------------------
func (s *APIServer) DeleteSentinelRule(c *gin.Context) {
	sid := c.Param("sid")
	var rule model.SentinelRule
	if err := s.DB.Where("sid = ?", sid).First(&rule).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "哨兵规则不存在: "+sid)
		return
	}
	if !s.canManageOwner(rule.UID, s.AuthContext(c)) {
		s.forbid(c)
		return
	}
	result := s.DB.Where("sid = ?", sid).Delete(&model.SentinelRule{})
	if result.RowsAffected == 0 {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "哨兵规则不存在: "+sid)
		return
	}
	s.Logger.Info("哨兵规则已删除", zap.String("sid", sid))
	s.RespondOK(c, gin.H{"message": "哨兵规则已删除，不再自动触发深度诊断"})
}

// ----------------------------------------------------------
// ListDetectionEvents 查询判异事件（审计记录）
// GET /api/v1/sentinel-rules/events?rule_sid= 或 ?target_ip=&signal=，可选 from=&to=（RFC3339）
// ----------------------------------------------------------
func (s *APIServer) ListDetectionEvents(c *gin.Context) {
	ruleSID := strings.TrimSpace(c.Query("rule_sid"))
	targetIP := strings.TrimSpace(c.Query("target_ip"))
	signal := strings.TrimSpace(c.Query("signal"))
	if ruleSID == "" && targetIP == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "需要提供 rule_sid 或 target_ip 参数")
		return
	}

	query := s.DB.Model(&model.DetectionEvent{}).Order("evaluated_at DESC")
	if ruleSID != "" {
		query = query.Where("rule_sid = ?", ruleSID)
	} else {
		// Unscoped：规则被删除（软删除）后审计记录仍要留痕（见 DeleteSentinelRule 注释），
		// 默认作用域会把软删除规则的 sid 过滤掉，导致按 target_ip 查询时历史事件"消失"。
		ruleQuery := s.DB.Unscoped().Model(&model.SentinelRule{}).Select("sid").Where("target_ip = ?", targetIP)
		if signal != "" {
			ruleQuery = ruleQuery.Where("signal = ?", signal)
		}
		query = query.Where("rule_sid IN (?)", ruleQuery)
	}
	if fromRaw := c.Query("from"); fromRaw != "" {
		from, err := time.Parse(time.RFC3339, fromRaw)
		if err != nil {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "from 参数格式错误，需为 RFC3339: "+err.Error())
			return
		}
		query = query.Where("evaluated_at >= ?", from)
	}
	if toRaw := c.Query("to"); toRaw != "" {
		to, err := time.Parse(time.RFC3339, toRaw)
		if err != nil {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "to 参数格式错误，需为 RFC3339: "+err.Error())
			return
		}
		query = query.Where("evaluated_at <= ?", to)
	}

	limit := 200
	if limitRaw := c.Query("limit"); limitRaw != "" {
		if v, err := strconv.Atoi(limitRaw); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}

	var events []model.DetectionEvent
	if err := query.Limit(limit).Find(&events).Error; err != nil {
		s.Logger.Error("查询判异事件失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询失败")
		return
	}
	if events == nil {
		events = []model.DetectionEvent{}
	}
	s.RespondOK(c, gin.H{"events": events, "total": len(events)})
}

// ----------------------------------------------------------
// GetDetectionHealth 哨兵判异循环自检
// GET /api/v1/sentinel-rules/health
// 数据库持续故障时判异循环会静默跳过触发（保守处理，见 detectionHasActiveTask 注释），
// 这个端点把"哨兵是不是还活着"暴露成一个可轮询的信号，配合 evaluateSentinelRules 里
// 连续失败升级为 Error 日志的机制，运维不需要盯着日志也能发现哨兵已经失效（见 §10.6）。
// ----------------------------------------------------------
func (s *APIServer) GetDetectionHealth(c *gin.Context) {
	lastEvalAt, lastSuccessAt, consecutiveFailures, lastError := s.detectionHealthSnapshot()
	healthy := consecutiveFailures < detectionHealthFailureAlertThreshold
	resp := gin.H{
		"healthy":              healthy,
		"consecutive_failures": consecutiveFailures,
		"last_eval_at":         lastEvalAt,
		"last_success_at":      lastSuccessAt,
	}
	if lastError != "" {
		resp["last_error"] = lastError
	}
	if lastEvalAt.IsZero() {
		// 判异循环还没跑过第一轮（进程刚启动），不算不健康，只是还没数据。
		resp["healthy"] = true
	}
	s.RespondOK(c, resp)
}
