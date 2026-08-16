package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
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

const continuousMaxDBCount = uint64(1<<63 - 1)
const continuousMaxReasonableProfileSampleCount = uint64(1_000_000_000)

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
	SessionSID    string                   `json:"session_sid" binding:"required"`
	BatchID       string                   `json:"batch_id"`
	TargetIP      string                   `json:"target_ip"`
	ObjectKey     string                   `json:"object_key"`
	StartTime     time.Time                `json:"start_time" binding:"required"`
	EndTime       time.Time                `json:"end_time" binding:"required"`
	WindowCount   uint32                   `json:"window_count"`
	SampleCount   uint64                   `json:"sample_count"`
	SchemaVersion uint32                   `json:"schema_version"`
	SignalTypes   []string                 `json:"signal_types"`
	Backends      map[string]string        `json:"backends"`
	Windows       []ContinuousWindowIngest `json:"windows"`
}

type ContinuousWindowIngest struct {
	WindowStart   time.Time                   `json:"window_start"`
	WindowEnd     time.Time                   `json:"window_end"`
	ObjectKey     string                      `json:"object_key"`
	SampleCount   uint64                      `json:"sample_count"`
	SignalType    string                      `json:"signal_type"`
	SchemaVersion uint32                      `json:"schema_version"`
	Backend       string                      `json:"backend"`
	Labels        map[string]interface{}      `json:"labels"`
	Samples       []ContinuousStackSample     `json:"samples"`
	Profiles      []ContinuousProfileIngest   `json:"profiles"`
	Histograms    []ContinuousHistogramIngest `json:"histograms"`
}

type ContinuousStackSample struct {
	Stack       []string               `json:"stack"`
	StackString string                 `json:"stack_string"`
	Count       uint64                 `json:"count"`
	Comm        string                 `json:"comm"`
	PID         int                    `json:"pid"`
	Exe         string                 `json:"exe"`
	StackScope  string                 `json:"stack_scope"`
	Backend     string                 `json:"backend"`
	Labels      map[string]interface{} `json:"labels"`
}

type ContinuousProfileIngest struct {
	SignalType string                  `json:"signal_type"`
	Backend    string                  `json:"backend"`
	StackScope string                  `json:"stack_scope"`
	Samples    []ContinuousStackSample `json:"samples"`
	Labels     map[string]interface{}  `json:"labels"`
}

type ContinuousHistogramIngest struct {
	SignalType  string                      `json:"signal_type"`
	Backend     string                      `json:"backend"`
	Unit        string                      `json:"unit"`
	EventCount  uint64                      `json:"event_count"`
	Buckets     []ContinuousHistogramBucket `json:"buckets"`
	Summary     ContinuousHistogramSummary  `json:"summary"`
	Labels      map[string]interface{}      `json:"labels"`
	Unavailable bool                        `json:"unavailable"`
	Reason      string                      `json:"reason"`
}

type ContinuousHistogramBucket struct {
	Range string  `json:"range"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
	Count uint64  `json:"count"`
}

type ContinuousHistogramSummary struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type continuousStoredBatch struct {
	SessionSID    string                   `json:"session_sid"`
	BatchID       string                   `json:"batch_id"`
	TargetIP      string                   `json:"target_ip"`
	StartTime     time.Time                `json:"start_time"`
	EndTime       time.Time                `json:"end_time"`
	SchemaVersion uint32                   `json:"schema_version"`
	SignalTypes   []string                 `json:"signal_types,omitempty"`
	Backends      map[string]string        `json:"backends,omitempty"`
	Windows       []ContinuousWindowIngest `json:"windows"`
}

type continuousAggregate struct {
	Total      float64
	Top        map[string]*ProfileTopItem
	Root       *continuousTreeNode
	LabelValue map[string]map[string]bool
	ObjectKeys []string
	Backends   map[string]bool
}

type continuousTreeNode struct {
	Name     string
	Value    float64
	Self     float64
	Children map[string]*continuousTreeNode
	Order    []*continuousTreeNode
}

func (s *APIServer) CreateContinuousSession(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	s.createContinuousSession(c, auth.UID, auth.Name)
}

func (s *APIServer) CreateInternalContinuousSession(c *gin.Context) {
	s.createContinuousSession(c, "", "system-agent")
}

func (s *APIServer) createContinuousSession(c *gin.Context, ownerUID string, userName string) {
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
		UID:                  ownerUID,
		UserName:             userName,
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
			query = query.Where("(uid IN ? OR uid = '' OR uid IS NULL)", owners)
		} else {
			query = query.Where("(uid = ? OR uid = '' OR uid IS NULL)", auth.UID)
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
	if req.ObjectKey == "" {
		req.ObjectKey = continuousBatchObjectKey(req.SessionSID, req.BatchID)
	}
	if req.WindowCount == 0 {
		req.WindowCount = uint32(len(req.Windows))
	}
	if req.SchemaVersion == 0 {
		req.SchemaVersion = 1
	}
	req.SignalTypes = normalizeContinuousSignalTypes(req)
	if req.Backends == nil {
		req.Backends = map[string]string{}
	}
	if err := s.storeContinuousBatchPayload(c.Request.Context(), req); err != nil {
		s.Logger.Error("保存 Continuous ProfileBatch payload 失败", zap.String("sid", req.SessionSID), zap.Error(err))
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "保存 Continuous ProfileBatch payload 失败")
		return
	}
	now := time.Now()
	batch := model.ProfileBatch{
		BID:           req.BatchID,
		SessionSID:    req.SessionSID,
		TargetIP:      req.TargetIP,
		ObjectKey:     req.ObjectKey,
		StartTime:     req.StartTime,
		EndTime:       req.EndTime,
		WindowCount:   req.WindowCount,
		SampleCount:   clampContinuousCount(req.SampleCount),
		SchemaVersion: req.SchemaVersion,
		SignalTypes:   mustJSONBytes(req.SignalTypes),
		Backends:      mustJSONBytes(req.Backends),
		Status:        model.ContinuousBatchStatusReady,
		CreatedAt:     now,
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
			for _, signal := range continuousWindowSignalRows(in) {
				window := model.ProfileWindow{
					SessionSID:    req.SessionSID,
					BatchBID:      req.BatchID,
					WindowStart:   in.WindowStart,
					WindowEnd:     in.WindowEnd,
					ObjectKey:     firstNonEmpty(in.ObjectKey, req.ObjectKey),
					SampleCount:   clampContinuousCount(continuousWindowSampleCount(in, signal.SignalType)),
					SignalType:    signal.SignalType,
					SchemaVersion: firstNonZeroUint32(in.SchemaVersion, req.SchemaVersion),
					Backend:       signal.Backend,
					Labels:        labels,
					CreatedAt:     now,
				}
				if err := tx.Create(&window).Error; err != nil {
					return err
				}
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
	s.cleanupContinuousRetention(c.Request.Context(), session)
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
	fg, found, err := s.queryNativeContinuousFlamegraph(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	topn, _, err := s.queryNativeContinuousTopN(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		s.RespondOK(c, gin.H{
			"query":          profileLabelSelector(q),
			"nodes":          []ProfileNode{},
			"items":          []ProfileTopItem{},
			"total":          0,
			"unit":           "samples",
			"empty":          true,
			"message":        "Native Continuous Profiling 暂无覆盖该时间范围的 10s window",
			"source":         "mini-drop-native",
			"profile_source": "native",
			"generated_at":   time.Now(),
		})
		return
	}
	s.RespondOK(c, gin.H{
		"query":          fg.Query,
		"nodes":          fg.Nodes,
		"items":          topn.Items,
		"total":          fg.Total,
		"unit":           fg.Unit,
		"empty":          fg.Empty,
		"message":        fg.Message,
		"source":         fg.Source,
		"profile_source": fg.ProfileSource,
		"profile_url":    fg.ProfileURL,
		"generated_at":   fg.GeneratedAt,
	})
}

func (s *APIServer) ViewContinuousProfileObject(c *gin.Context) {
	key := strings.TrimSpace(c.Query("key"))
	sid := continuousSessionSIDFromObjectKey(key)
	if sid == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "非法 Continuous Profile 对象路径")
		return
	}
	if _, ok := s.loadReadableContinuousSession(c, sid, s.AuthContext(c)); !ok {
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "文件不存在")
		return
	}
	defer reader.Close()

	c.Header("Content-Type", mimeType(key))
	c.Header("Content-Disposition", contentDisposition("inline", path.Base(key)))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理输出 Continuous Profile 对象失败", zap.String("key", key), zap.Error(err))
	}
}

func (s *APIServer) storeContinuousBatchPayload(ctx context.Context, req ContinuousBatchIngestReq) error {
	if !s.StorageConnected() {
		return errProfileUnavailable
	}
	payload := continuousStoredBatch{
		SessionSID:    req.SessionSID,
		BatchID:       req.BatchID,
		TargetIP:      req.TargetIP,
		StartTime:     req.StartTime,
		EndTime:       req.EndTime,
		SchemaVersion: req.SchemaVersion,
		SignalTypes:   req.SignalTypes,
		Backends:      req.Backends,
		Windows:       req.Windows,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.Storage.PutObject(ctx, s.Config.Storage.Bucket, req.ObjectKey, bytes.NewReader(body), int64(len(body)), "application/json")
}

func continuousBatchObjectKey(sessionSID, batchID string) string {
	return "continuous/" + sessionSID + "/" + batchID + ".json"
}

func (s *APIServer) cleanupContinuousRetention(ctx context.Context, session model.ContinuousSession) {
	retentionHours := session.RetentionHours
	if retentionHours == 0 {
		retentionHours = 24
	}
	cutoff := time.Now().Add(-time.Duration(retentionHours) * time.Hour)

	var expiredBatches []model.ProfileBatch
	if err := s.DB.Where("session_sid = ? AND end_time < ?", session.SID, cutoff).Find(&expiredBatches).Error; err != nil {
		s.Logger.Warn("Native Continuous Profiling retention 查询过期 batch 失败", zap.String("sid", session.SID), zap.Error(err))
		return
	}
	if err := s.DB.Where("session_sid = ? AND window_end < ?", session.SID, cutoff).Delete(&model.ProfileWindow{}).Error; err != nil {
		s.Logger.Warn("Native Continuous Profiling retention 删除过期 window 失败", zap.String("sid", session.SID), zap.Error(err))
		return
	}

	for _, batch := range expiredBatches {
		var remaining int64
		if err := s.DB.Model(&model.ProfileWindow{}).Where("session_sid = ? AND batch_bid = ?", session.SID, batch.BID).Count(&remaining).Error; err != nil {
			s.Logger.Warn("Native Continuous Profiling retention 检查 batch 引用失败", zap.String("sid", session.SID), zap.String("batch", batch.BID), zap.Error(err))
			continue
		}
		if remaining > 0 {
			continue
		}
		if err := s.DB.Where("bid = ?", batch.BID).Delete(&model.ProfileBatch{}).Error; err != nil {
			s.Logger.Warn("Native Continuous Profiling retention 删除 batch 失败", zap.String("sid", session.SID), zap.String("batch", batch.BID), zap.Error(err))
			continue
		}
		if batch.ObjectKey != "" && s.StorageConnected() {
			if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, batch.ObjectKey); err != nil {
				s.Logger.Warn("Native Continuous Profiling retention 删除对象失败", zap.String("sid", session.SID), zap.String("object_key", batch.ObjectKey), zap.Error(err))
			}
		}
	}
}

func (s *APIServer) queryNativeContinuousFlamegraph(ctx context.Context, q ProfileQuery) (ProfileFlamegraph, bool, error) {
	agg, found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil || !found {
		return ProfileFlamegraph{}, found, err
	}
	nodes := continuousTreeToProfileNodes(agg.Root, "")
	out := ProfileFlamegraph{
		Nodes:         nodes,
		Total:         agg.Total,
		Unit:          "samples",
		Backend:       continuousBackendList(agg.Backends),
		Empty:         len(nodes) == 0 || agg.Total == 0,
		Source:        "mini-drop-native",
		ProfileSource: "native",
		ProfileURL:    s.continuousProfileURL(ctx, agg.ObjectKeys),
		Query:         profileLabelSelector(q),
		GeneratedAt:   time.Now(),
	}
	if out.Empty {
		out.Message = "Native Continuous Profiling 暂无匹配样本"
	}
	return out, true, nil
}

func (s *APIServer) queryNativeContinuousTopN(ctx context.Context, q ProfileQuery) (ProfileTopN, bool, error) {
	agg, found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil || !found {
		return ProfileTopN{}, found, err
	}
	items := make([]ProfileTopItem, 0, len(agg.Top))
	for _, item := range agg.Top {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Value == items[j].Value {
			return items[i].Name < items[j].Name
		}
		return items[i].Value > items[j].Value
	})
	if len(items) > 100 {
		items = items[:100]
	}
	out := ProfileTopN{
		Items:         items,
		Total:         agg.Total,
		Unit:          "samples",
		Backend:       continuousBackendList(agg.Backends),
		Empty:         len(items) == 0 || agg.Total == 0,
		Source:        "mini-drop-native",
		ProfileSource: "native",
		ProfileURL:    s.continuousProfileURL(ctx, agg.ObjectKeys),
		Query:         profileLabelSelector(q),
		GeneratedAt:   time.Now(),
	}
	if out.Empty {
		out.Message = "Native Continuous Profiling 暂无匹配样本"
	}
	return out, true, nil
}

func (s *APIServer) queryNativeContinuousLabelValues(ctx context.Context, q ProfileQuery, label string) (ProfileLabelValues, bool, error) {
	if !isAllowedProfileFilterLabel(label) {
		return ProfileLabelValues{
			Label:       label,
			Values:      []string{},
			Available:   false,
			Message:     "Native Continuous Profiling 仅支持 comm/pid/exe 过滤标签",
			Source:      "mini-drop-native",
			Query:       profileLabelSelector(q),
			GeneratedAt: time.Now(),
		}, true, nil
	}
	agg, found, err := s.queryNativeContinuousAggregate(ctx, q)
	if err != nil || !found {
		return ProfileLabelValues{}, found, err
	}
	values := make([]string, 0, len(agg.LabelValue[label]))
	for value := range agg.LabelValue[label] {
		values = append(values, value)
	}
	sort.Strings(values)
	out := ProfileLabelValues{
		Label:       label,
		Values:      values,
		Available:   len(values) > 0,
		Source:      "mini-drop-native",
		Query:       profileLabelSelector(q),
		GeneratedAt: time.Now(),
	}
	if len(values) == 0 {
		out.Message = "Native Continuous Profiling 暂无可用过滤标签"
	}
	return out, true, nil
}

func (s *APIServer) queryNativeContinuousAggregate(ctx context.Context, q ProfileQuery) (continuousAggregate, bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.DB.Model(&model.ContinuousSession{}).Select("sid").Where("target_ip = ?", q.Host)
	if !q.CanReadAll {
		if len(q.OwnerUIDs) > 0 {
			sessionQuery = sessionQuery.Where("(uid IN ? OR uid = '' OR uid IS NULL)", q.OwnerUIDs)
		} else {
			sessionQuery = sessionQuery.Where("(uid = '' OR uid IS NULL)")
		}
	}
	err := s.DB.Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", "cpu_profile").
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Order("window_start ASC").
		Find(&windows).Error
	if err != nil {
		return continuousAggregate{}, false, err
	}
	if len(windows) == 0 {
		return continuousAggregate{}, false, nil
	}
	if !s.StorageConnected() {
		return continuousAggregate{}, true, errProfileUnavailable
	}
	agg := continuousAggregate{
		Top: map[string]*ProfileTopItem{},
		Root: &continuousTreeNode{
			Name:     "root",
			Children: map[string]*continuousTreeNode{},
		},
		LabelValue: map[string]map[string]bool{
			"comm": {},
			"pid":  {},
			"exe":  {},
		},
		Backends: map[string]bool{},
	}
	byObject := map[string][]model.ProfileWindow{}
	objectOrder := []string{}
	for _, window := range windows {
		if window.ObjectKey == "" {
			continue
		}
		if _, ok := byObject[window.ObjectKey]; !ok {
			objectOrder = append(objectOrder, window.ObjectKey)
		}
		byObject[window.ObjectKey] = append(byObject[window.ObjectKey], window)
	}
	for _, objectKey := range objectOrder {
		batch, err := s.loadContinuousStoredBatch(ctx, objectKey)
		if err != nil {
			return continuousAggregate{}, true, err
		}
		agg.ObjectKeys = append(agg.ObjectKeys, objectKey)
		for _, window := range batch.Windows {
			if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
				continue
			}
			for _, sample := range continuousProfileSamplesForQuery(window, q) {
				if !continuousSampleMatches(sample, window.Labels, q.Filters) {
					continue
				}
				continuousAddSample(&agg, sample, window.Labels)
			}
		}
	}
	return agg, true, nil
}

func (s *APIServer) QueryContinuousHistogram(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	signalType := strings.ToLower(strings.TrimSpace(c.Query("signal_type")))
	if signalType == "" {
		signalType = strings.ToLower(strings.TrimSpace(c.Query("profile_type")))
	}
	if signalType != "io_latency" && signalType != "sched_latency" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "signal_type 仅支持 io_latency/sched_latency")
		return
	}
	data, found, err := s.queryNativeContinuousHistogram(c.Request.Context(), q, signalType)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		s.RespondOK(c, gin.H{
			"query":        profileLabelSelector(q),
			"signal_type":  signalType,
			"empty":        true,
			"message":      "Native Continuous eBPF 暂无覆盖该时间范围的 histogram window",
			"source":       "mini-drop-native",
			"generated_at": time.Now(),
			"buckets":      []ContinuousHistogramBucket{},
			"trend":        []gin.H{},
		})
		return
	}
	s.RespondOK(c, data)
}

func (s *APIServer) queryNativeContinuousHistogram(ctx context.Context, q ProfileQuery, signalType string) (gin.H, bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.DB.Model(&model.ContinuousSession{}).Select("sid").Where("target_ip = ?", q.Host)
	if !q.CanReadAll {
		if len(q.OwnerUIDs) > 0 {
			sessionQuery = sessionQuery.Where("(uid IN ? OR uid = '' OR uid IS NULL)", q.OwnerUIDs)
		} else {
			sessionQuery = sessionQuery.Where("(uid = '' OR uid IS NULL)")
		}
	}
	err := s.DB.Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", signalType).
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Order("window_start ASC").
		Find(&windows).Error
	if err != nil {
		return nil, false, err
	}
	if len(windows) == 0 {
		return nil, false, nil
	}
	if !s.StorageConnected() {
		return nil, true, errProfileUnavailable
	}

	type bucketAgg struct {
		Range string
		Low   float64
		High  float64
		Count uint64
	}
	merged := map[string]*bucketAgg{}
	trend := []gin.H{}
	backends := map[string]bool{}
	var totalEvents uint64
	var unavailableReason string
	objectKeys := []string{}
	seenObject := map[string]bool{}

	for _, objectKey := range orderedContinuousObjectKeys(windows) {
		batch, err := s.loadContinuousStoredBatch(ctx, objectKey)
		if err != nil {
			return nil, true, err
		}
		if !seenObject[objectKey] {
			objectKeys = append(objectKeys, objectKey)
			seenObject[objectKey] = true
		}
		for _, window := range batch.Windows {
			if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
				continue
			}
			for _, hist := range window.Histograms {
				if strings.ToLower(strings.TrimSpace(hist.SignalType)) != signalType {
					continue
				}
				if hist.Backend != "" {
					backends[hist.Backend] = true
				}
				if hist.Unavailable && unavailableReason == "" {
					unavailableReason = hist.Reason
				}
				totalEvents = addContinuousCount(totalEvents, hist.EventCount)
				for _, bucket := range hist.Buckets {
					key := bucket.Range + "|" + strconv.FormatFloat(bucket.Low, 'f', -1, 64) + "|" + strconv.FormatFloat(bucket.High, 'f', -1, 64)
					item := merged[key]
					if item == nil {
						item = &bucketAgg{Range: bucket.Range, Low: bucket.Low, High: bucket.High}
						merged[key] = item
					}
					item.Count = addContinuousCount(item.Count, bucket.Count)
				}
				trend = append(trend, gin.H{
					"window_start": window.WindowStart,
					"window_end":   window.WindowEnd,
					"event_count":  hist.EventCount,
					"p50":          hist.Summary.P50,
					"p95":          hist.Summary.P95,
					"p99":          hist.Summary.P99,
					"backend":      hist.Backend,
					"unavailable":  hist.Unavailable,
					"reason":       hist.Reason,
				})
			}
		}
	}
	buckets := make([]ContinuousHistogramBucket, 0, len(merged))
	for _, item := range merged {
		buckets = append(buckets, ContinuousHistogramBucket{Range: item.Range, Low: item.Low, High: item.High, Count: item.Count})
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Low == buckets[j].Low {
			return buckets[i].High < buckets[j].High
		}
		return buckets[i].Low < buckets[j].Low
	})
	summary := summarizeContinuousBuckets(buckets)
	backendList := make([]string, 0, len(backends))
	for backend := range backends {
		backendList = append(backendList, backend)
	}
	sort.Strings(backendList)
	empty := len(buckets) == 0 || totalEvents == 0
	message := ""
	if empty {
		message = firstNonEmpty(unavailableReason, "Native Continuous eBPF 暂无 histogram 样本")
	} else if unavailableReason != "" {
		message = "部分窗口不可用: " + unavailableReason
	}
	return gin.H{
		"query":        profileLabelSelector(q),
		"signal_type":  signalType,
		"buckets":      buckets,
		"summary":      summary,
		"trend":        trend,
		"event_count":  totalEvents,
		"unit":         "us",
		"backend":      strings.Join(backendList, ","),
		"backends":     backendList,
		"empty":        empty,
		"message":      message,
		"source":       "mini-drop-native",
		"profile_url":  s.continuousProfileURL(ctx, objectKeys),
		"generated_at": time.Now(),
	}, true, nil
}

func (s *APIServer) loadContinuousStoredBatch(ctx context.Context, objectKey string) (continuousStoredBatch, error) {
	rc, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, objectKey)
	if err != nil {
		return continuousStoredBatch{}, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if err != nil {
		return continuousStoredBatch{}, err
	}
	var batch continuousStoredBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return continuousStoredBatch{}, err
	}
	return batch, nil
}

func continuousSampleMatches(sample ContinuousStackSample, windowLabels map[string]interface{}, filters map[string]interface{}) bool {
	for _, key := range []string{"comm", "pid", "exe"} {
		want := labelString(filters, key)
		if want == "" {
			continue
		}
		if continuousSampleLabel(sample, windowLabels, key) != want {
			return false
		}
	}
	return true
}

func continuousProfileSamplesForQuery(window ContinuousWindowIngest, q ProfileQuery) []ContinuousStackSample {
	if len(window.Profiles) == 0 {
		return window.Samples
	}
	out := []ContinuousStackSample{}
	scope := strings.ToLower(strings.TrimSpace(q.StackScope))
	if scope == "all" {
		scope = ""
	}
	for _, profile := range window.Profiles {
		if strings.ToLower(strings.TrimSpace(firstNonEmpty(profile.SignalType, "cpu_profile"))) != "cpu_profile" {
			continue
		}
		profileScope := strings.ToLower(strings.TrimSpace(profile.StackScope))
		if scope != "" && profileScope != "" && profileScope != scope {
			continue
		}
		for _, sample := range profile.Samples {
			if sample.StackScope == "" {
				sample.StackScope = profile.StackScope
			}
			if sample.Backend == "" {
				sample.Backend = profile.Backend
			}
			if sample.Labels == nil {
				sample.Labels = profile.Labels
			}
			out = append(out, sample)
		}
	}
	return out
}

func continuousSampleLabel(sample ContinuousStackSample, windowLabels map[string]interface{}, key string) string {
	if value := labelString(sample.Labels, key); value != "" {
		return value
	}
	switch key {
	case "comm":
		if sample.Comm != "" {
			return sample.Comm
		}
	case "pid":
		if sample.PID > 0 {
			return strconv.Itoa(sample.PID)
		}
	case "exe":
		if sample.Exe != "" {
			return sample.Exe
		}
	}
	return labelString(windowLabels, key)
}

func continuousAddSample(agg *continuousAggregate, sample ContinuousStackSample, windowLabels map[string]interface{}) {
	if !continuousSampleLooksValid(sample) {
		return
	}
	count := float64(sample.Count)
	if count <= 0 {
		count = 1
	}
	stack := continuousSampleStack(sample)
	if len(stack) == 0 {
		stack = []string{firstNonEmpty(continuousSampleLabel(sample, windowLabels, "comm"), continuousSampleLabel(sample, windowLabels, "exe"), "unknown")}
	}
	agg.Total += count
	for _, key := range []string{"comm", "pid", "exe"} {
		if value := continuousSampleLabel(sample, windowLabels, key); value != "" {
			agg.LabelValue[key][value] = true
		}
	}
	if backend := strings.TrimSpace(sample.Backend); backend != "" {
		agg.Backends[backend] = true
	}
	for i, frame := range stack {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			frame = "unknown"
		}
		item := agg.Top[frame]
		if item == nil {
			item = &ProfileTopItem{Name: frame, Unit: "samples"}
			agg.Top[frame] = item
		}
		item.Value += count
		if i == len(stack)-1 {
			item.Self += count
		}
	}
	node := agg.Root
	node.Value += count
	for i, frame := range stack {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			frame = "unknown"
		}
		if node.Children == nil {
			node.Children = map[string]*continuousTreeNode{}
		}
		child := node.Children[frame]
		if child == nil {
			child = &continuousTreeNode{Name: frame, Children: map[string]*continuousTreeNode{}}
			node.Children[frame] = child
			node.Order = append(node.Order, child)
		}
		child.Value += count
		if i == len(stack)-1 {
			child.Self += count
		}
		node = child
	}
}

func continuousBackendList(backends map[string]bool) string {
	if len(backends) == 0 {
		return ""
	}
	out := make([]string, 0, len(backends))
	for backend := range backends {
		if strings.TrimSpace(backend) != "" {
			out = append(out, backend)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func continuousSampleLooksValid(sample ContinuousStackSample) bool {
	if sample.Count > continuousMaxReasonableProfileSampleCount {
		return false
	}
	for _, frame := range continuousSampleStack(sample) {
		if continuousFrameLooksInvalid(frame) {
			return false
		}
	}
	if continuousFrameLooksInvalid(sample.StackString) {
		return false
	}
	return true
}

func continuousFrameLooksInvalid(frame string) bool {
	frame = strings.TrimSpace(frame)
	if frame == "" {
		return false
	}
	lower := strings.ToLower(frame)
	return strings.HasPrefix(frame, "ERROR:") ||
		strings.HasPrefix(lower, "stdin:") ||
		strings.Contains(lower, "failed to look up stack id") ||
		strings.Contains(lower, "unknown error")
}

func continuousSampleStack(sample ContinuousStackSample) []string {
	if len(sample.Stack) > 0 {
		out := make([]string, 0, len(sample.Stack))
		for _, frame := range sample.Stack {
			if strings.TrimSpace(frame) != "" {
				out = append(out, strings.TrimSpace(frame))
			}
		}
		return out
	}
	if sample.StackString == "" {
		return nil
	}
	parts := strings.Split(sample.StackString, ";")
	out := make([]string, 0, len(parts))
	for _, frame := range parts {
		if strings.TrimSpace(frame) != "" {
			out = append(out, strings.TrimSpace(frame))
		}
	}
	return out
}

func continuousTreeToProfileNodes(root *continuousTreeNode, prefix string) []ProfileNode {
	if root == nil {
		return []ProfileNode{}
	}
	children := append([]*continuousTreeNode(nil), root.Order...)
	sort.Slice(children, func(i, j int) bool {
		if children[i].Value == children[j].Value {
			return children[i].Name < children[j].Name
		}
		return children[i].Value > children[j].Value
	})
	out := make([]ProfileNode, 0, len(children))
	for idx, child := range children {
		id := strconv.Itoa(idx)
		if prefix != "" {
			id = prefix + "." + id
		}
		out = append(out, ProfileNode{
			ID:       id,
			Name:     child.Name,
			Value:    child.Value,
			Self:     child.Self,
			Children: continuousTreeToProfileNodes(child, id),
		})
	}
	return out
}

func (s *APIServer) continuousProfileURL(ctx context.Context, objectKeys []string) string {
	if len(objectKeys) == 0 || !s.StorageConnected() {
		return ""
	}
	key := strings.TrimSpace(objectKeys[0])
	if continuousSessionSIDFromObjectKey(key) == "" {
		return ""
	}
	return "/api/v1/continuous/raw?key=" + url.QueryEscape(key)
}

func continuousSessionSIDFromObjectKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		return ""
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "continuous" || parts[1] == "" || path.Ext(parts[2]) != ".json" {
		return ""
	}
	return parts[1]
}

func windowOverlaps(start, end, from, to time.Time) bool {
	return !end.Before(from) && !start.After(to)
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

type continuousSignalRow struct {
	SignalType string
	Backend    string
}

func continuousWindowSignalRows(window ContinuousWindowIngest) []continuousSignalRow {
	seen := map[string]bool{}
	rows := []continuousSignalRow{}
	add := func(signalType, backend string) {
		signalType = strings.ToLower(strings.TrimSpace(firstNonEmpty(signalType, "cpu_profile")))
		if signalType == "" {
			signalType = "cpu_profile"
		}
		key := signalType + "|" + backend
		if seen[key] {
			return
		}
		seen[key] = true
		rows = append(rows, continuousSignalRow{SignalType: signalType, Backend: backend})
	}
	if len(window.Samples) > 0 {
		add(firstNonEmpty(window.SignalType, "cpu_profile"), continuousLegacyProfileBackend(window))
	}
	for _, profile := range window.Profiles {
		add(firstNonEmpty(profile.SignalType, "cpu_profile"), firstNonEmpty(profile.Backend, window.Backend))
	}
	for _, hist := range window.Histograms {
		add(hist.SignalType, firstNonEmpty(hist.Backend, window.Backend))
	}
	if len(rows) == 0 {
		add(firstNonEmpty(window.SignalType, "cpu_profile"), window.Backend)
	}
	return rows
}

func continuousLegacyProfileBackend(window ContinuousWindowIngest) string {
	if window.Backend != "" {
		return window.Backend
	}
	for _, sample := range window.Samples {
		if sample.Backend != "" {
			return sample.Backend
		}
	}
	return ""
}

func continuousWindowSampleCount(window ContinuousWindowIngest, signalType string) uint64 {
	signalType = strings.ToLower(strings.TrimSpace(signalType))
	var count uint64
	if signalType == "cpu_profile" {
		for _, sample := range window.Samples {
			count = addContinuousCount(count, firstNonZeroUint64(sample.Count, 1))
		}
		for _, profile := range window.Profiles {
			if strings.ToLower(strings.TrimSpace(firstNonEmpty(profile.SignalType, "cpu_profile"))) != "cpu_profile" {
				continue
			}
			for _, sample := range profile.Samples {
				count = addContinuousCount(count, firstNonZeroUint64(sample.Count, 1))
			}
		}
	}
	for _, hist := range window.Histograms {
		if strings.ToLower(strings.TrimSpace(hist.SignalType)) == signalType {
			count = addContinuousCount(count, hist.EventCount)
		}
	}
	if count == 0 {
		return clampContinuousCount(window.SampleCount)
	}
	return count
}

func addContinuousCount(total uint64, value uint64) uint64 {
	value = clampContinuousCount(value)
	if total >= continuousMaxDBCount || value >= continuousMaxDBCount {
		return continuousMaxDBCount
	}
	if total > continuousMaxDBCount-value {
		return continuousMaxDBCount
	}
	return total + value
}

func clampContinuousCount(value uint64) uint64 {
	if value > continuousMaxDBCount {
		return continuousMaxDBCount
	}
	return value
}

func normalizeContinuousSignalTypes(req ContinuousBatchIngestReq) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(signal string) {
		signal = strings.ToLower(strings.TrimSpace(signal))
		if signal == "" {
			return
		}
		if seen[signal] {
			return
		}
		seen[signal] = true
		out = append(out, signal)
	}
	for _, signal := range req.SignalTypes {
		add(signal)
	}
	for _, window := range req.Windows {
		if len(window.Samples) > 0 {
			add(firstNonEmpty(window.SignalType, "cpu_profile"))
		}
		for _, profile := range window.Profiles {
			add(firstNonEmpty(profile.SignalType, "cpu_profile"))
		}
		for _, hist := range window.Histograms {
			add(hist.SignalType)
		}
	}
	if len(out) == 0 {
		out = []string{"cpu_profile"}
	}
	return out
}

func orderedContinuousObjectKeys(windows []model.ProfileWindow) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, window := range windows {
		if window.ObjectKey == "" || seen[window.ObjectKey] {
			continue
		}
		seen[window.ObjectKey] = true
		out = append(out, window.ObjectKey)
	}
	return out
}

func summarizeContinuousBuckets(buckets []ContinuousHistogramBucket) ContinuousHistogramSummary {
	if len(buckets) == 0 {
		return ContinuousHistogramSummary{}
	}
	var total uint64
	min := buckets[0].Low
	max := buckets[0].High
	for _, bucket := range buckets {
		total += bucket.Count
		if bucket.Low < min {
			min = bucket.Low
		}
		if bucket.High > max {
			max = bucket.High
		}
	}
	valueAt := func(target float64) float64 {
		if total == 0 {
			return 0
		}
		threshold := uint64(float64(total)*target + 0.999999)
		if threshold == 0 {
			threshold = 1
		}
		var seen uint64
		for _, bucket := range buckets {
			seen += bucket.Count
			if seen >= threshold {
				return (bucket.Low + bucket.High) / 2
			}
		}
		last := buckets[len(buckets)-1]
		return (last.Low + last.High) / 2
	}
	return ContinuousHistogramSummary{
		Min: min,
		Max: max,
		P50: valueAt(0.50),
		P95: valueAt(0.95),
		P99: valueAt(0.99),
	}
}

func mustJSONBytes(value interface{}) []byte {
	if value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return body
}

func firstNonZeroUint64(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
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
