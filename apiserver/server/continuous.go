package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	WindowStart time.Time               `json:"window_start"`
	WindowEnd   time.Time               `json:"window_end"`
	ObjectKey   string                  `json:"object_key"`
	SampleCount uint64                  `json:"sample_count"`
	Labels      map[string]interface{}  `json:"labels"`
	Samples     []ContinuousStackSample `json:"samples"`
}

type ContinuousStackSample struct {
	Stack       []string               `json:"stack"`
	StackString string                 `json:"stack_string"`
	Count       uint64                 `json:"count"`
	Comm        string                 `json:"comm"`
	PID         int                    `json:"pid"`
	Exe         string                 `json:"exe"`
	Labels      map[string]interface{} `json:"labels"`
}

type continuousStoredBatch struct {
	SessionSID string                   `json:"session_sid"`
	BatchID    string                   `json:"batch_id"`
	TargetIP   string                   `json:"target_ip"`
	StartTime  time.Time                `json:"start_time"`
	EndTime    time.Time                `json:"end_time"`
	Windows    []ContinuousWindowIngest `json:"windows"`
}

type continuousAggregate struct {
	Total      float64
	Top        map[string]*ProfileTopItem
	Root       *continuousTreeNode
	LabelValue map[string]map[string]bool
	ObjectKeys []string
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
	if req.ObjectKey == "" {
		req.ObjectKey = continuousBatchObjectKey(req.SessionSID, req.BatchID)
	}
	if req.WindowCount == 0 {
		req.WindowCount = uint32(len(req.Windows))
	}
	if err := s.storeContinuousBatchPayload(c.Request.Context(), req); err != nil {
		s.Logger.Error("保存 Continuous ProfileBatch payload 失败", zap.String("sid", req.SessionSID), zap.Error(err))
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "保存 Continuous ProfileBatch payload 失败")
		return
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

func (s *APIServer) storeContinuousBatchPayload(ctx context.Context, req ContinuousBatchIngestReq) error {
	if !s.StorageConnected() {
		return errProfileUnavailable
	}
	payload := continuousStoredBatch{
		SessionSID: req.SessionSID,
		BatchID:    req.BatchID,
		TargetIP:   req.TargetIP,
		StartTime:  req.StartTime,
		EndTime:    req.EndTime,
		Windows:    req.Windows,
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
			for _, sample := range window.Samples {
				if !continuousSampleMatches(sample, window.Labels, q.Filters) {
					continue
				}
				continuousAddSample(&agg, sample, window.Labels)
			}
		}
	}
	return agg, true, nil
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
	url, err := s.Storage.PresignedGetURL(ctx, s.Config.Storage.Bucket, objectKeys[0], time.Duration(s.Config.Storage.PresignExpireSec)*time.Second)
	if err != nil {
		return ""
	}
	return url
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
