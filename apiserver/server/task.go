// ============================================================
// server/task.go — 任务管理处理器
// 包含：创建/列表/详情/删除/重试任务
// W1 MVP 阶段：所有接口返回合理的 mock/真实数据
// ============================================================

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	pb_common "github.com/mini-drop/apiserver/proto/common"
	pb_control "github.com/mini-drop/apiserver/proto/control"
	pb_hotmethod "github.com/mini-drop/apiserver/proto/hotmethod"
	"github.com/mini-drop/apiserver/util"
)

// CreateTaskReq 创建任务请求体
type CreateTaskReq struct {
	Name          string `json:"name" binding:"required"`
	TaskType      uint32 `json:"task_type"`     // 0=通用 1=Java 2=Tracing
	ProfilerType  uint32 `json:"profiler_type"` // 0=perf 1=async-profiler 2=pprof
	TargetIP      string `json:"target_ip" binding:"required"`
	TargetPID     int32  `json:"target_pid"`
	Duration      uint64 `json:"duration"`  // 采集秒数
	Frequency     uint32 `json:"frequency"` // 采样频率 Hz
	Callgraph     string `json:"callgraph"` // fp / dwarf / lbr
	Event         string `json:"event"`     // cpu-cycles / cache-misses
	Subprocess    bool   `json:"subprocess"`
	ContainerName string `json:"container_name"`
}

// PerfParams 性能采集参数，会被序列化为 JSONB 存入 request_params 字段
type PerfParams struct {
	TargetPID  int32  `json:"target_pid"`
	Duration   uint64 `json:"duration"`
	Frequency  uint32 `json:"frequency"`
	Callgraph  string `json:"callgraph"`
	Event      string `json:"event"`
	Subprocess bool   `json:"subprocess"`
}

// CreateTask 创建性能采集任务
// POST /api/v1/tasks
func (s *APIServer) CreateTask(c *gin.Context) {
	var req CreateTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "请求参数错误: " + err.Error(),
		})
		return
	}

	// 设置默认值
	if req.Duration == 0 {
		req.Duration = 10
	}
	if req.Frequency == 0 {
		req.Frequency = 99
	}
	if req.Callgraph == "" {
		req.Callgraph = "fp"
	}

	tid := util.GenTID()
	uid := c.GetHeader("Drop_user_uid")
	if uid == "" {
		uid = "default-user"
	}
	userName := c.GetHeader("Drop_user_name")
	if userName == "" {
		userName = "默认用户"
	}

	// 将性能采集参数序列化为 JSONB（只存采参，不存 Name/TargetIP 等已落列的字段）
	paramsJSON, err := util.MarshalJSONB(PerfParams{
		TargetPID:  req.TargetPID,
		Duration:   req.Duration,
		Frequency:  req.Frequency,
		Callgraph:  req.Callgraph,
		Event:      req.Event,
		Subprocess: req.Subprocess,
	})
	if err != nil {
		s.Logger.Error("序列化任务参数失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "服务器内部错误",
		})
		return
	}

	now := time.Now()

	task := &model.HotmethodTask{
		TID:            tid,
		Name:           req.Name,
		Type:           req.TaskType,
		ProfilerType:   req.ProfilerType,
		TargetIP:       req.TargetIP,
		RequestParams:  paramsJSON,
		Status:         0, // 新建
		StatusInfo:     "任务已创建，等待下发",
		AnalysisStatus: 0, // 待分析
		UID:            uid,
		UserName:       userName,
		CreateTime:     now,
	}

	if err := s.DB.Create(task).Error; err != nil {
		s.Logger.Error("创建任务失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "创建任务失败: " + err.Error(),
		})
		return
	}
	s.recordTaskStatusEvent(task.TID, -1, TaskStatusCreated, "任务已创建，等待下发", "apiserver")

	// W3: 通过 gRPC 下发任务到 drop_server
	if s.GrpcConnected() {
		s.dispatchTask(task, req)
	} else {
		s.Logger.Warn("gRPC 未连接，任务仅写库未下发",
			zap.String("tid", tid),
		)
		// 标记为失败，等待后续手动重试或 cron 重发
		_ = s.transitionTaskStatus(task, TaskStatusFailed, "gRPC 未连接，任务无法下发到 drop_server", "apiserver", nil)
	}

	s.Logger.Info("任务创建成功",
		zap.String("tid", tid),
		zap.String("target_ip", req.TargetIP),
		zap.String("name", req.Name),
	)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"tid": tid,
		},
	})
}

// dispatchTask 通过 gRPC 将任务下发到 drop_server
// 如果下发失败，更新数据库状态为失败
func (s *APIServer) dispatchTask(task *model.HotmethodTask, req CreateTaskReq) {
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
		TaskID:        task.TID,
		TaskType:      req.TaskType,
		ProfilerType:  req.ProfilerType,
		SampleArgv:    recordArgv,
		ContainerName: req.ContainerName,
		TimeoutSec:    uint32(req.Duration + 30), // 多给 30s 上传时间
		CosConfig:     cosCfg,
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
			zap.String("target_ip", req.TargetIP),
			zap.Error(err),
		)
		_ = s.transitionTaskStatus(task, TaskStatusFailed, errMsg, "apiserver", nil)
		return
	}

	// 下发成功，更新状态为"已下发"
	s.Logger.Info("任务已下发到 drop_server",
		zap.String("tid", task.TID),
		zap.String("grpc_resp_code", fmt.Sprintf("%d", resp.Code)),
	)

	now := time.Now()
	_ = s.transitionTaskStatus(task, TaskStatusRunning,
		fmt.Sprintf("已下发到 drop_server, code=%d msg=%s", resp.Code, resp.Msg),
		"apiserver",
		map[string]interface{}{"begin_time": &now},
	)
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

	var tasks []model.HotmethodTask
	var total int64

	query := s.DB.Model(&model.HotmethodTask{})

	// 按关键词搜索（任务名称 / 任务 ID）
	if keyword := c.Query("keyword"); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where("name LIKE ? OR tid LIKE ? OR target_ip LIKE ?", like, like, like)
	}

	// 按状态筛选
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	// 按用户筛选（权限控制）
	if uid := c.GetHeader("Drop_user_uid"); uid != "" {
		query = query.Where("uid = ?", uid)
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

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"tasks":    tasks,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
}

// GetTaskDetail 获取任务详情（含产物下载链接 + TopN 热点数据）
// GET /api/v1/tasks/:tid
func (s *APIServer) GetTaskDetail(c *gin.Context) {
	tid := c.Param("tid")

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "任务不存在: " + tid,
		})
		return
	}

	result := gin.H{"task": taskDetailResponse(task)}
	files := []map[string]interface{}{}
	var topFuncs []map[string]interface{}
	var bpfData map[string]interface{}
	var suggestions []map[string]interface{}
	statusEvents := s.fetchTaskStatusEvents(tid)

	// W4: 优先从对象存储列出产物，存储不可用或无产物时回退本地目录。
	if s.StorageConnected() {
		storageFiles, err := s.listTaskFiles(tid)
		if err != nil {
			s.Logger.Warn("列出任务文件失败", zap.String("tid", tid), zap.Error(err))
		} else {
			files = storageFiles

			// 尝试从 MinIO 读取 top.json → TopN 热点数据
			topFuncs = s.fetchTopFunctions(tid)
			bpfData = s.fetchBPFData(tid)
			suggestions = s.fetchSuggestions(tid)
		}
	}
	if len(files) == 0 {
		files = s.listLocalFiles(tid)
		if len(topFuncs) == 0 {
			topFuncs = s.fetchLocalTopFunctions(tid)
		}
		if bpfData == nil {
			bpfData = s.fetchLocalBPFData(tid)
		}
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchLocalSuggestions(tid)
	}
	if len(suggestions) == 0 {
		suggestions = s.fetchDBSuggestions(tid)
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
	result["status_events"] = statusEvents
	result["files"] = files

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": result,
	})
}

func taskDetailResponse(task model.HotmethodTask) gin.H {
	var params map[string]interface{}
	if len(task.RequestParams) > 0 {
		_ = json.Unmarshal(task.RequestParams, &params)
	}

	return gin.H{
		"id":              task.ID,
		"tid":             task.TID,
		"name":            task.Name,
		"type":            task.Type,
		"profiler_type":   task.ProfilerType,
		"target_ip":       task.TargetIP,
		"request_params":  params,
		"status":          task.Status,
		"status_info":     task.StatusInfo,
		"analysis_status": task.AnalysisStatus,
		"uid":             task.UID,
		"user_name":       task.UserName,
		"create_time":     task.CreateTime,
		"begin_time":      task.BeginTime,
		"end_time":        task.EndTime,
		"master_task_tid": task.MasterTaskTID,
	}
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

func normalizeTopFunctions(topData map[string]interface{}) []map[string]interface{} {
	for _, key := range []string{"self_time_top", "top_functions", "inclusive_time_top"} {
		items, ok := topData[key].([]interface{})
		if !ok {
			continue
		}
		funcs := make([]map[string]interface{}, 0, len(items))
		for _, item := range items {
			if m, ok := item.(map[string]interface{}); ok {
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

// DeleteTask 软删除任务
// DELETE /api/v1/tasks/:tid
func (s *APIServer) DeleteTask(c *gin.Context) {
	tid := c.Param("tid")

	// 使用 GORM 软删除
	result := s.DB.Where("tid = ?", tid).Delete(&model.HotmethodTask{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "删除任务失败: " + result.Error.Error(),
		})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "任务不存在或已删除: " + tid,
		})
		return
	}

	s.Logger.Info("任务已删除", zap.String("tid", tid))

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "任务已删除",
	})
}

// RetryTask 重试失败的任务（用同参数重新创建）
// POST /api/v1/tasks/:tid/retry
func (s *APIServer) RetryTask(c *gin.Context) {
	tid := c.Param("tid")

	// 查找原任务
	var oldTask model.HotmethodTask
	if err := s.DB.Unscoped().Where("tid = ?", tid).First(&oldTask).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "原任务不存在: " + tid,
		})
		return
	}

	// 用相同参数创建新任务
	newTID := util.GenTID()
	now := time.Now()

	newTask := model.HotmethodTask{
		TID:            newTID,
		Name:           oldTask.Name + "(重试)",
		Type:           oldTask.Type,
		ProfilerType:   oldTask.ProfilerType,
		TargetIP:       oldTask.TargetIP,
		RequestParams:  oldTask.RequestParams,
		Status:         0,
		StatusInfo:     "重试任务，等待下发",
		AnalysisStatus: 0,
		UID:            oldTask.UID,
		UserName:       oldTask.UserName,
		CreateTime:     now,
		MasterTaskTID:  tid, // 记录父任务
	}

	if err := s.DB.Create(&newTask).Error; err != nil {
		s.Logger.Error("重试任务创建失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "重试任务创建失败: " + err.Error(),
		})
		return
	}
	s.recordTaskStatusEvent(newTask.TID, -1, TaskStatusCreated, "重试任务已创建，等待下发", "apiserver")

	// W3: 重试任务也通过 gRPC 下发
	if s.GrpcConnected() {
		// 从原任务的 request_params 重建采集参数
		var oldParams PerfParams
		if err := util.UnmarshalJSONB(oldTask.RequestParams, &oldParams); err != nil {
			s.Logger.Warn("解析原任务参数失败，使用默认值", zap.Error(err))
		}
		req := CreateTaskReq{
			Name:         newTask.Name,
			TaskType:     newTask.Type,
			ProfilerType: newTask.ProfilerType,
			TargetIP:     newTask.TargetIP,
			TargetPID:    oldParams.TargetPID,
			Duration:     oldParams.Duration,
			Frequency:    oldParams.Frequency,
			Callgraph:    oldParams.Callgraph,
			Event:        oldParams.Event,
			Subprocess:   oldParams.Subprocess,
		}
		s.dispatchTask(&newTask, req)
	} else {
		_ = s.transitionTaskStatus(&newTask, TaskStatusFailed, "gRPC 未连接，重试任务无法下发", "apiserver", nil)
	}

	s.Logger.Info("任务重试成功",
		zap.String("old_tid", tid),
		zap.String("new_tid", newTID),
	)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"tid": newTID,
		},
	})
}

// ListCOSFiles 列出任务产物文件并提供签名下载链接
// GET /api/v1/cosfiles?tid=xxx
// W4: MinIO 不可用时回退到本地文件系统
func (s *APIServer) ListCOSFiles(c *gin.Context) {
	tid := c.Query("tid")
	if tid == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "缺少 tid 参数",
		})
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
			c.JSON(http.StatusInternalServerError, gin.H{
				"code":    500,
				"message": "列出文件失败: " + err.Error(),
			})
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

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": response,
	})
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

		// 生成本地下载 URL
		downloadURL := fmt.Sprintf("/api/v1/files/%s", filename)

		fileInfo := map[string]interface{}{
			"name":          filename,
			"size":          info.Size(),
			"last_modified": info.ModTime(),
			"content_type":  mimeType(filename),
			"download_url":  downloadURL,
			"source":        "local",
		}
		if filepath.Ext(filename) == ".svg" {
			fileInfo["view_url"] = downloadURL
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
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "非法路径"})
		return
	}

	localPath := filepath.Join("/tmp/drop-output", filename)

	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "文件不存在: " + filename})
		return
	}

	ext := filepath.Ext(filename)
	switch ext {
	case ".svg":
		c.Header("Content-Type", "image/svg+xml")
	case ".json":
		c.Header("Content-Type", "application/json")
	case ".md":
		c.Header("Content-Type", "text/markdown; charset=utf-8")
	case ".txt":
		c.Header("Content-Type", "text/plain; charset=utf-8")
	default:
		c.Header("Content-Type", "application/octet-stream")
	}
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", filename))
	c.File(localPath)
}

// ViewCOSFile 通过 apiserver 代理查看对象存储中的小型可视化产物。
// 主要用于修正历史 SVG 对象的 Content-Type，避免浏览器因 nosniff 拒绝渲染。
// GET /api/v1/cosfiles/view?key=tid/flamegraph.svg
func (s *APIServer) ViewCOSFile(c *gin.Context) {
	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 key 参数"})
		return
	}
	if strings.Contains(key, "..") || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "非法对象路径"})
		return
	}
	if filepath.Ext(key) != ".svg" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "仅支持查看 SVG 可视化产物"})
		return
	}
	if !s.StorageConnected() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "对象存储未连接"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reader, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "文件不存在: " + key})
		return
	}
	defer reader.Close()

	c.Header("Content-Type", mimeType(key))
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", filepath.Base(key)))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		s.Logger.Warn("代理输出对象存储文件失败", zap.String("key", key), zap.Error(err))
	}
}

// mimeType 根据文件扩展名返回 MIME 类型
func mimeType(filename string) string {
	ext := filepath.Ext(filename)
	switch ext {
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".md":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".html":
		return "text/html"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// listTaskFiles 列出指定 tid 下的所有产物文件，并生成签名下载 URL
func (s *APIServer) listTaskFiles(tid string) ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bucket := s.Config.Storage.Bucket
	prefix := tid + "/" // MinIO 中以 tid/ 为前缀存放该任务的所有产物

	objects, err := s.Storage.ListObjects(ctx, bucket, prefix)
	if err != nil {
		return nil, err
	}

	expireDuration := time.Duration(s.Config.Storage.PresignExpireSec) * time.Second

	var files []map[string]interface{}
	for _, obj := range objects {
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

		// 生成预签名下载 URL
		presignedURL, err := s.Storage.PresignedGetURL(ctx, bucket, obj.Name, expireDuration)
		if err != nil {
			s.Logger.Warn("生成签名 URL 失败",
				zap.String("key", obj.Name),
				zap.Error(err),
			)
			fileInfo["download_url"] = ""
			fileInfo["url_error"] = err.Error()
		} else {
			fileInfo["download_url"] = presignedURL
		}
		if filepath.Ext(obj.Name) == ".svg" {
			fileInfo["view_url"] = "/api/v1/cosfiles/view?key=" + url.QueryEscape(obj.Name)
		}
		files = append(files, fileInfo)
	}

	if files == nil {
		files = []map[string]interface{}{}
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

// ============================================================
// GetTimeline — Continuous Profiling 时间轴
// GET /api/v1/tasks/timeline?master_tid=xxx
// 返回某个 master_task（定时任务）下所有子任务的时间序列
// ============================================================
func (s *APIServer) GetTimeline(c *gin.Context) {
	masterTID := c.Query("master_tid")
	if masterTID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 master_tid 参数"})
		return
	}

	var tasks []model.HotmethodTask
	err := s.DB.Where("master_task_tid = ? AND deleted_at IS NULL", masterTID).
		Order("create_time ASC").
		Find(&tasks).Error
	if err != nil {
		s.Logger.Error("查询时间轴失败", zap.String("master_tid", masterTID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询失败"})
		return
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
	}

	timeline := make([]TimelinePoint, 0, len(tasks))
	for _, t := range tasks {
		tp := TimelinePoint{
			TID:            t.TID,
			Name:           t.Name,
			Status:         t.Status,
			CreateTime:     t.CreateTime,
			AnalysisStatus: t.AnalysisStatus,
		}
		if t.BeginTime != nil {
			tp.BeginTime = t.BeginTime
		}
		if t.EndTime != nil {
			tp.EndTime = t.EndTime
		}
		// DONE 且 analysis_status >= 2 (分析完成) 视为有结果，UPLOADING 仍需继续轮询。
		tp.HasResult = t.Status == TaskStatusDone && t.AnalysisStatus >= 2
		timeline = append(timeline, tp)
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"master_tid": masterTID,
			"total":      len(timeline),
			"points":     timeline,
		},
	})
}
