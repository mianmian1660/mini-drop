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
}

var sentinelValidMetrics = map[string]bool{"p50": true, "p95": true, "p99": true}

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
	if _, ok := detectionSignalTaskKind[req.Signal]; !ok {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "signal 暂不支持自动判异: "+req.Signal)
		return
	}
	if !sentinelValidMetrics[req.Metric] {
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

	now := time.Now()
	rule := &model.SentinelRule{
		SID:             "sr-" + util.GenTID()[4:],
		Name:            req.Name,
		TargetIP:        req.TargetIP,
		Signal:          req.Signal,
		Metric:          req.Metric,
		FloorValue:      req.FloorValue,
		CooldownSeconds: req.CooldownSeconds,
		Enabled:         true,
		UID:             auth.UID,
		UserName:        auth.Name,
		CreatedAt:       now,
		UpdatedAt:       now,
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
		ruleQuery := s.DB.Model(&model.SentinelRule{}).Select("sid").Where("target_ip = ?", targetIP)
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
