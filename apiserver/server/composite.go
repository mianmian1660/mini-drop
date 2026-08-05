package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

const (
	AggregationAllRequired = "ALL_REQUIRED"
	AggregationBestEffort  = "BEST_EFFORT"
	AggregationQuorum      = "QUORUM"
	AggregationDAG         = "DAG"

	TriggerTypeComposite uint32 = 6
)

type CompositeTaskReq struct {
	Name              string          `json:"name" binding:"required"`
	TargetIP          string          `json:"target_ip" binding:"required"`
	Children          []CreateTaskReq `json:"children"`
	AggregationPolicy string          `json:"aggregation_policy"`
	Quorum            int             `json:"quorum"`
}

type compositeSummary struct {
	Total    int `json:"total"`
	Done     int `json:"done"`
	Failed   int `json:"failed"`
	Canceled int `json:"canceled"`
	Running  int `json:"running"`
	Created  int `json:"created"`
	Terminal int `json:"terminal"`
	Required int `json:"required"`
	Quorum   int `json:"quorum"`
}

func (s *APIServer) CreateCompositeTask(c *gin.Context) {
	auth := s.AuthContext(c)
	if !auth.CanWrite() {
		s.forbid(c)
		return
	}
	var req CompositeTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "请求参数错误: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.TargetIP = strings.TrimSpace(req.TargetIP)
	if req.Name == "" || req.TargetIP == "" {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "缺少复合任务名称或目标 Agent")
		return
	}
	policy, err := normalizeAggregationPolicy(req.AggregationPolicy)
	if err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, err.Error())
		return
	}
	req.AggregationPolicy = policy
	children, err := s.normalizeCompositeChildren(req)
	if err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, err.Error())
		return
	}
	if len(children) == 0 {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeAgentIncompatible, "目标 Agent 没有可展开的子任务能力")
		return
	}
	quorum := req.Quorum
	if policy == AggregationQuorum {
		if quorum <= 0 {
			quorum = (len(children) + 1) / 2
		}
		if quorum > len(children) {
			s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "quorum 不能大于子任务数量")
			return
		}
	}

	now := time.Now()
	parent := model.HotmethodTask{
		TID:            util.GenTID(),
		Name:           req.Name,
		TaskKind:       "composite",
		RequestID:      requestIDFromGin(c),
		Type:           TriggerTypeComposite,
		ProfilerType:   ProfilerPerf,
		TargetIP:       req.TargetIP,
		RequestParams:  []byte(`{"composite":true}`),
		Status:         TaskStatusRunning,
		StatusInfo:     "复合任务已展开，等待子任务完成",
		AnalysisStatus: 0,
		UID:            auth.UID,
		UserName:       auth.Name,
		CreateTime:     now,
	}
	childTIDs := make([]string, 0, len(children))
	childTasks := make([]model.HotmethodTask, 0, len(children))
	outboxes := make([]model.Outbox, 0, len(children))
	maxDeadline := now
	for i := range children {
		childReq := children[i]
		childTID := util.GenTID()
		childTIDs = append(childTIDs, childTID)
		paramsJSON, _ := util.MarshalJSONB(PerfParams{
			TargetPID:  childReq.TargetPID,
			Duration:   childReq.Duration,
			Frequency:  childReq.Frequency,
			Callgraph:  childReq.Callgraph,
			Event:      childReq.Event,
			Subprocess: childReq.Subprocess,
			PprofURL:   childReq.PprofURL,
		})
		deadline := now.Add(time.Duration(childReq.Duration+30) * time.Second)
		if deadline.After(maxDeadline) {
			maxDeadline = deadline
		}
		childTasks = append(childTasks, model.HotmethodTask{
			TID:            childTID,
			Name:           childReq.Name,
			TaskKind:       childReq.TaskKind,
			RequestID:      parent.RequestID,
			Type:           childReq.TaskType,
			ProfilerType:   childReq.ProfilerType,
			TargetIP:       childReq.TargetIP,
			RequestParams:  paramsJSON,
			Status:         TaskStatusCreated,
			StatusInfo:     "复合任务子任务已创建，等待下发",
			AnalysisStatus: 0,
			UID:            auth.UID,
			UserName:       auth.Name,
			CreateTime:     now.Add(time.Duration(i) * time.Millisecond),
			MasterTaskTID:  parent.TID,
			DeadlineUnixMS: int64(deadline.UnixMilli()),
			ResourceBudget: []byte(childReq.ResourceBudget),
		})
		payload, _ := util.MarshalJSONB(childReq)
		outboxes = append(outboxes, model.Outbox{
			Aggregate:   model.OutboxAggregateTask,
			AggregateID: childTID,
			Event:       model.OutboxEventDispatchTask,
			Payload:     payload,
			Status:      model.OutboxStatusPending,
			CreatedAt:   now,
		})
	}
	parent.DeadlineUnixMS = maxDeadline.UnixMilli()
	subTIDsJSON, _ := util.MarshalJSONB(childTIDs)
	dagJSON, _ := util.MarshalJSONB(defaultCompositeDAG(childTIDs))
	summaryJSON, _ := util.MarshalJSONB(compositeSummary{Total: len(childTIDs), Running: len(childTIDs), Required: len(childTIDs), Quorum: quorum})
	multi := model.MultiTask{
		TID:               parent.TID,
		SubTIDs:           subTIDsJSON,
		Type:              TriggerTypeComposite,
		Status:            TaskStatusRunning,
		AnalysisStatus:    0,
		TriggerType:       TriggerTypeComposite,
		AggregationPolicy: policy,
		Quorum:            quorum,
		DAG:               dagJSON,
		Summary:           summaryJSON,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&parent).Error; err != nil {
			return err
		}
		if err := tx.Create(&multi).Error; err != nil {
			return err
		}
		if err := tx.Create(&childTasks).Error; err != nil {
			return err
		}
		return tx.Create(&outboxes).Error
	}); err != nil {
		s.Logger.Error("创建复合任务失败", zap.Error(err))
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "创建复合任务失败")
		return
	}
	s.recordTaskStatusEvent(parent.TID, -1, TaskStatusRunning, "复合任务已展开，等待子任务完成", "composite")
	for _, child := range childTasks {
		s.recordTaskStatusEvent(child.TID, -1, TaskStatusCreated, "复合任务子任务已创建，等待下发", "composite")
	}
	s.RespondOK(c, gin.H{"tid": parent.TID, "children": childTIDs, "aggregation_policy": policy, "quorum": quorum})
}

func (s *APIServer) ListTaskChildren(c *gin.Context) {
	task, serr := s.taskService().requireReadableTask(c.Param("tid"), s.AuthContext(c))
	if serr != nil {
		s.RespondHTTPError(c, serr.HTTPStatus, serr.Code, serr.Message)
		return
	}
	payload, err := s.compositeChildrenPayload(task.TID)
	if err != nil {
		s.RespondHTTPError(c, http.StatusNotFound, ErrCodeTargetNotFound, "复合任务不存在: "+task.TID)
		return
	}
	s.RespondOK(c, payload)
}

func (s *APIServer) normalizeCompositeChildren(req CompositeTaskReq) ([]CreateTaskReq, error) {
	rawChildren := req.Children
	if len(rawChildren) == 0 {
		rawChildren = s.defaultCompositeChildren(req)
	}
	out := make([]CreateTaskReq, 0, len(rawChildren))
	for i := range rawChildren {
		child := rawChildren[i]
		child.TargetIP = req.TargetIP
		if strings.TrimSpace(child.Name) == "" {
			child.Name = fmt.Sprintf("%s / 子任务 %d", req.Name, i+1)
		}
		if child.Duration == 0 {
			child.Duration = 10
		}
		if child.Frequency == 0 {
			child.Frequency = 99
		}
		if child.Callgraph == "" {
			child.Callgraph = "fp"
		}
		if err := normalizeAndValidateCollector(&child); err != nil {
			return nil, fmt.Errorf("子任务 %d 参数错误: %w", i+1, err)
		}
		kind, _ := taskKindByID(child.TaskKind)
		if !s.agentSupportsTaskKind(req.TargetIP, kind) {
			if req.AggregationPolicy == AggregationBestEffort {
				continue
			}
			return nil, fmt.Errorf("目标 Agent 不支持子任务 %s", child.TaskKind)
		}
		out = append(out, child)
	}
	return out, nil
}

func (s *APIServer) defaultCompositeChildren(req CompositeTaskReq) []CreateTaskReq {
	candidates := []CreateTaskReq{
		{Name: req.Name + " / CPU", TaskKind: TaskKindPerfCPU, TargetIP: req.TargetIP, Duration: 10, Frequency: 99, Callgraph: "fp"},
		{Name: req.Name + " / IO", TaskKind: TaskKindEBPFIO, TargetIP: req.TargetIP, Duration: 10, Frequency: 1, Callgraph: "fp"},
		{Name: req.Name + " / 调度", TaskKind: TaskKindEBPFSched, TargetIP: req.TargetIP, Duration: 10, Frequency: 1, Callgraph: "fp"},
	}
	out := make([]CreateTaskReq, 0, len(candidates))
	for _, candidate := range candidates {
		kind, _ := taskKindByID(candidate.TaskKind)
		if s.agentSupportsTaskKind(req.TargetIP, kind) {
			out = append(out, candidate)
		}
	}
	return out
}

func normalizeAggregationPolicy(raw string) (string, error) {
	policy := strings.ToUpper(strings.TrimSpace(raw))
	if policy == "" {
		policy = AggregationAllRequired
	}
	switch policy {
	case AggregationAllRequired, AggregationBestEffort, AggregationQuorum, AggregationDAG:
		return policy, nil
	default:
		return "", fmt.Errorf("不支持的聚合策略: %s", raw)
	}
}

func defaultCompositeDAG(tids []string) []gin.H {
	out := make([]gin.H, 0, len(tids))
	for _, tid := range tids {
		out = append(out, gin.H{"tid": tid, "depends_on": []string{}})
	}
	return out
}

func (s *APIServer) compositeChildrenPayload(tid string) (gin.H, error) {
	var mt model.MultiTask
	if err := s.DB.Where("tid = ?", tid).First(&mt).Error; err != nil {
		return nil, err
	}
	var children []model.HotmethodTask
	if err := s.DB.Where("master_task_tid = ?", tid).Order("create_time ASC").Find(&children).Error; err != nil {
		return nil, err
	}
	status, analysisStatus, summary := aggregateCompositeStatus(mt, children)
	summaryJSON, _ := util.MarshalJSONB(summary)
	updates := map[string]interface{}{"status": status, "analysis_status": analysisStatus, "summary": summaryJSON, "updated_at": time.Now()}
	_ = s.DB.Model(&model.MultiTask{}).Where("tid = ?", tid).Updates(updates).Error

	var parent model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&parent).Error; err == nil && parent.Status != status {
		reason := fmt.Sprintf("复合任务聚合状态更新：%d/%d 子任务成功", summary.Done, summary.Total)
		_ = s.transitionTaskStatus(&parent, status, reason, "composite", map[string]interface{}{"analysis_status": analysisStatus})
	}
	items := make([]gin.H, 0, len(children))
	for _, child := range children {
		items = append(items, gin.H{
			"tid":             child.TID,
			"name":            child.Name,
			"task_kind":       child.TaskKind,
			"status":          child.Status,
			"analysis_status": child.AnalysisStatus,
			"target_ip":       child.TargetIP,
			"create_time":     child.CreateTime,
			"result_url":      "/task/result?tid=" + child.TID,
		})
	}
	var dag interface{}
	_ = json.Unmarshal(mt.DAG, &dag)
	return gin.H{
		"tid":                tid,
		"children":           items,
		"total":              len(items),
		"aggregation_policy": mt.AggregationPolicy,
		"quorum":             mt.Quorum,
		"dag":                dag,
		"summary":            summary,
		"status":             status,
		"analysis_status":    analysisStatus,
	}, nil
}

func aggregateCompositeStatus(mt model.MultiTask, children []model.HotmethodTask) (int, int, compositeSummary) {
	summary := compositeSummary{Total: len(children), Required: len(children), Quorum: mt.Quorum}
	for _, child := range children {
		switch child.Status {
		case TaskStatusDone:
			summary.Done++
			summary.Terminal++
		case TaskStatusFailed:
			summary.Failed++
			summary.Terminal++
		case TaskStatusCanceled:
			summary.Canceled++
			summary.Terminal++
		case TaskStatusRunning, TaskStatusUploading:
			summary.Running++
		default:
			summary.Created++
		}
	}
	if summary.Total == 0 {
		return TaskStatusFailed, 3, summary
	}
	policy := mt.AggregationPolicy
	if policy == "" {
		policy = AggregationAllRequired
	}
	quorum := mt.Quorum
	if quorum <= 0 {
		quorum = (summary.Total + 1) / 2
	}
	summary.Quorum = quorum
	switch policy {
	case AggregationBestEffort:
		if summary.Terminal < summary.Total {
			return TaskStatusRunning, 0, summary
		}
		if summary.Done > 0 {
			return TaskStatusDone, 2, summary
		}
		return TaskStatusFailed, 3, summary
	case AggregationQuorum:
		if summary.Done >= quorum {
			return TaskStatusDone, 2, summary
		}
		if summary.Terminal == summary.Total || summary.Done+summary.Total-summary.Terminal < quorum {
			return TaskStatusFailed, 3, summary
		}
		return TaskStatusRunning, 0, summary
	default:
		if summary.Failed > 0 || summary.Canceled > 0 {
			return TaskStatusFailed, 3, summary
		}
		if summary.Done == summary.Total {
			return TaskStatusDone, 2, summary
		}
		return TaskStatusRunning, 0, summary
	}
}

func (s *APIServer) refreshCompositeParent(child model.HotmethodTask) {
	if child.MasterTaskTID == "" {
		return
	}
	var count int64
	if err := s.DB.Model(&model.MultiTask{}).Where("tid = ?", child.MasterTaskTID).Count(&count).Error; err != nil || count == 0 {
		return
	}
	if _, err := s.compositeChildrenPayload(child.MasterTaskTID); err != nil {
		s.Logger.Warn("刷新复合任务父状态失败", zap.String("parent_tid", child.MasterTaskTID), zap.Error(err))
	}
}
