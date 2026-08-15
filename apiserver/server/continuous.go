package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

type CreateContinuousSessionReq struct {
	Name                 string                 `json:"name"`
	TargetIP             string                 `json:"target_ip" binding:"required"`
	Hostname             string                 `json:"hostname"`
	ServiceName          string                 `json:"service_name"`
	SampleRateHz         uint32                 `json:"sample_rate_hz"`
	AggregationWindowSec uint32                 `json:"aggregation_window_sec"`
	UploadBatchSec       uint32                 `json:"upload_batch_sec"`
	RetentionHours       uint32                 `json:"retention_hours"`
	Labels               map[string]interface{} `json:"labels"`
	Capabilities         map[string]interface{} `json:"capabilities"`
}

type ContinuousBatchIngestReq struct {
	SessionSID  string                   `json:"session_sid" binding:"required"`
	BatchID     string                   `json:"batch_id"`
	TargetIP    string                   `json:"target_ip"`
	ObjectKey   string                   `json:"object_key"`
	StartTime   time.Time                `json:"start_time" binding:"required"`
	EndTime     time.Time                `json:"end_time" binding:"required"`
	WindowCount uint32                   `json:"window_count"`
	SampleCount uint64                   `json:"sample_count"`
	Windows     []ContinuousWindowIngest `json:"windows"`
}

type ContinuousWindowIngest struct {
	WindowStart time.Time              `json:"window_start"`
	WindowEnd   time.Time              `json:"window_end"`
	ObjectKey   string                 `json:"object_key"`
	SampleCount uint64                 `json:"sample_count"`
	Labels      map[string]interface{} `json:"labels"`
}

func (s *APIServer) CreateContinuousSession(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	var req CreateContinuousSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	req.TargetIP = strings.TrimSpace(req.TargetIP)
	if req.TargetIP == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "target_ip 不能为空")
		return
	}
	applyContinuousDefaults(&req)
	labels, _ := util.MarshalJSONB(req.Labels)
	caps, _ := util.MarshalJSONB(req.Capabilities)
	now := time.Now()
	session := model.ContinuousSession{
		SID:                  "cps-" + util.GenTID()[4:],
		Name:                 firstNonEmpty(req.Name, "Native Continuous Profiling"),
		TargetIP:             req.TargetIP,
		Hostname:             req.Hostname,
		ServiceName:          firstNonEmpty(req.ServiceName, "hotmethod"),
		SampleRateHz:         req.SampleRateHz,
		AggregationWindowSec: req.AggregationWindowSec,
		UploadBatchSec:       req.UploadBatchSec,
		RetentionHours:       req.RetentionHours,
		Labels:               labels,
		Capabilities:         caps,
		Status:               model.ContinuousSessionStatusRunning,
		UID:                  auth.UID,
		UserName:             auth.Name,
		StartedAt:            now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := s.DB.Create(&session).Error; err != nil {
		s.Logger.Error("创建 ContinuousSession 失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建 ContinuousSession 失败")
		return
	}
	s.RespondOK(c, gin.H{"session": session})
}

func (s *APIServer) ListContinuousSessions(c *gin.Context) {
	auth := s.AuthContext(c)
	var sessions []model.ContinuousSession
	query := s.DB.Order("created_at DESC")
	if !auth.IsPlatformAdmin() {
		owners := s.visibleOwnerUIDs(auth)
		if len(owners) > 0 {
			query = query.Where("uid IN ?", owners)
		} else {
			query = query.Where("uid = ?", auth.UID)
		}
	}
	if err := query.Find(&sessions).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 ContinuousSession 失败")
		return
	}
	if sessions == nil {
		sessions = []model.ContinuousSession{}
	}
	s.RespondOK(c, gin.H{"sessions": sessions, "total": len(sessions)})
}

func (s *APIServer) StopContinuousSession(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	sid := strings.TrimSpace(c.Param("sid"))
	session, ok := s.loadManageableContinuousSession(c, sid, auth)
	if !ok {
		return
	}
	if session.Status == model.ContinuousSessionStatusStopped {
		s.RespondOK(c, gin.H{"session": session, "already_stopped": true})
		return
	}
	now := time.Now()
	if err := s.DB.Model(&session).Updates(map[string]interface{}{
		"status":     model.ContinuousSessionStatusStopped,
		"stopped_at": &now,
		"updated_at": now,
	}).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "停止 ContinuousSession 失败")
		return
	}
	session.Status = model.ContinuousSessionStatusStopped
	session.StoppedAt = &now
	s.RespondOK(c, gin.H{"session": session})
}

func (s *APIServer) IngestContinuousBatch(c *gin.Context) {
	var req ContinuousBatchIngestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	if !req.StartTime.Before(req.EndTime) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "batch 时间范围不合法")
		return
	}
	var session model.ContinuousSession
	if err := s.DB.Where("sid = ?", req.SessionSID).First(&session).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "ContinuousSession 不存在")
		return
	}
	if req.BatchID == "" {
		req.BatchID = "cpb-" + util.GenTID()[4:]
	}
	if req.TargetIP == "" {
		req.TargetIP = session.TargetIP
	}
	if req.WindowCount == 0 {
		req.WindowCount = uint32(len(req.Windows))
	}
	now := time.Now()
	batch := model.ProfileBatch{
		BID:         req.BatchID,
		SessionSID:  req.SessionSID,
		TargetIP:    req.TargetIP,
		ObjectKey:   req.ObjectKey,
		StartTime:   req.StartTime,
		EndTime:     req.EndTime,
		WindowCount: req.WindowCount,
		SampleCount: req.SampleCount,
		Status:      model.ContinuousBatchStatusReady,
		CreatedAt:   now,
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "bid"}}, DoNothing: true}).Create(&batch).Error; err != nil {
			return err
		}
		for _, in := range req.Windows {
			if in.WindowStart.IsZero() || in.WindowEnd.IsZero() || !in.WindowStart.Before(in.WindowEnd) {
				continue
			}
			labels, _ := json.Marshal(in.Labels)
			window := model.ProfileWindow{
				SessionSID:  req.SessionSID,
				BatchBID:    req.BatchID,
				WindowStart: in.WindowStart,
				WindowEnd:   in.WindowEnd,
				ObjectKey:   firstNonEmpty(in.ObjectKey, req.ObjectKey),
				SampleCount: in.SampleCount,
				Labels:      labels,
				CreatedAt:   now,
			}
			if err := tx.Create(&window).Error; err != nil {
				return err
			}
		}
		return tx.Model(&model.ContinuousSession{}).
			Where("sid = ?", req.SessionSID).
			Updates(map[string]interface{}{"last_upload_at": &req.EndTime, "updated_at": now}).Error
	})
	if err != nil {
		s.Logger.Error("登记 ProfileBatch 失败", zap.String("sid", req.SessionSID), zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "登记 ProfileBatch 失败")
		return
	}
	s.RespondOK(c, gin.H{"batch_id": req.BatchID, "session_sid": req.SessionSID})
}

func (s *APIServer) GetContinuousTimeline(c *gin.Context) {
	session, ok := s.loadReadableContinuousSession(c, c.Param("sid"), s.AuthContext(c))
	if !ok {
		return
	}
	query := s.DB.Where("session_sid = ?", session.SID).Order("window_start ASC")
	if from, ok := parseOptionalTime(c, "from"); !ok {
		return
	} else if !from.IsZero() {
		query = query.Where("window_end >= ?", from)
	}
	if to, ok := parseOptionalTime(c, "to"); !ok {
		return
	} else if !to.IsZero() {
		query = query.Where("window_start <= ?", to)
	}
	var windows []model.ProfileWindow
	if err := query.Find(&windows).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 Continuous timeline 失败")
		return
	}
	if windows == nil {
		windows = []model.ProfileWindow{}
	}
	s.RespondOK(c, gin.H{"session": session, "windows": windows, "total": len(windows)})
}

func (s *APIServer) QueryContinuousProfile(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	s.RespondOK(c, gin.H{
		"query":          profileLabelSelector(q),
		"nodes":          []ProfileNode{},
		"items":          []ProfileTopItem{},
		"total":          0,
		"unit":           "samples",
		"empty":          true,
		"message":        "Native Continuous Profiling batch 查询已接入，采样器上传数据后可生成历史 Flamegraph/TopN",
		"source":         "mini-drop-native",
		"profile_source": "native",
		"generated_at":   time.Now(),
	})
}

func applyContinuousDefaults(req *CreateContinuousSessionReq) {
	if req.SampleRateHz == 0 {
		req.SampleRateHz = 19
	}
	if req.AggregationWindowSec == 0 {
		req.AggregationWindowSec = 10
	}
	if req.UploadBatchSec == 0 {
		req.UploadBatchSec = 60
	}
	if req.RetentionHours == 0 {
		req.RetentionHours = 24
	}
	if req.Labels == nil {
		req.Labels = map[string]interface{}{}
	}
	if req.Capabilities == nil {
		req.Capabilities = map[string]interface{}{}
	}
}

func (s *APIServer) loadReadableContinuousSession(c *gin.Context, sid string, auth AuthContext) (model.ContinuousSession, bool) {
	var session model.ContinuousSession
	if err := s.DB.Where("sid = ?", sid).First(&session).Error; err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "ContinuousSession 不存在")
		return session, false
	}
	if !s.canReadOwner(session.UID, auth) {
		s.forbid(c)
		return session, false
	}
	return session, true
}

func (s *APIServer) loadManageableContinuousSession(c *gin.Context, sid string, auth AuthContext) (model.ContinuousSession, bool) {
	session, ok := s.loadReadableContinuousSession(c, sid, auth)
	if !ok {
		return session, false
	}
	if !s.canManageOwner(session.UID, auth) {
		s.forbid(c)
		return session, false
	}
	return session, true
}

func parseOptionalTime(c *gin.Context, name string) (time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return time.Time{}, true
	}
	return parseProfileTime(c, name, time.Time{})
}
