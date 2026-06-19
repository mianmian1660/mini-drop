// ============================================================
// server/task.go — 任务管理处理器
// 包含：创建/列表/详情/删除/重试任务
// W1 MVP 阶段：所有接口返回合理的 mock/真实数据
// ============================================================

package server

import (
	"context"
	"fmt"
	"net/http"
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
	Duration      uint64 `json:"duration"`      // 采集秒数
	Frequency     uint32 `json:"frequency"`     // 采样频率 Hz
	Callgraph     string `json:"callgraph"`     // fp / dwarf / lbr
	Event         string `json:"event"`         // cpu-cycles / cache-misses
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
		TID:           tid,
		Name:          req.Name,
		Type:          req.TaskType,
		ProfilerType:  req.ProfilerType,
		TargetIP:      req.TargetIP,
		RequestParams: paramsJSON,
		Status:        0,      // 新建
		StatusInfo:    "任务已创建，等待下发",
		AnalysisStatus: 0,     // 待分析
		UID:           uid,
		UserName:      userName,
		CreateTime:    now,
	}

	if err := s.DB.Create(task).Error; err != nil {
		s.Logger.Error("创建任务失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "创建任务失败: " + err.Error(),
		})
		return
	}

	// W3: 通过 gRPC 下发任务到 drop_server
	if s.GrpcConnected() {
		s.dispatchTask(task, req)
	} else {
		s.Logger.Warn("gRPC 未连接，任务仅写库未下发",
			zap.String("tid", tid),
		)
		// 标记为失败，等待后续手动重试或 cron 重发
		s.DB.Model(task).Updates(map[string]interface{}{
			"status":      3,
			"status_info": "gRPC 未连接，任务无法下发到 drop_server",
		})
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
		s.DB.Model(task).Updates(map[string]interface{}{
			"status":      3,
			"status_info": errMsg,
		})
		return
	}

	// 下发成功，更新状态为"已下发"
	s.Logger.Info("任务已下发到 drop_server",
		zap.String("tid", task.TID),
		zap.String("grpc_resp_code", fmt.Sprintf("%d", resp.Code)),
	)

	now := time.Now()
	s.DB.Model(task).Updates(map[string]interface{}{
		"status":      1, // 执行中
		"status_info": fmt.Sprintf("已下发到 drop_server, code=%d msg=%s", resp.Code, resp.Msg),
		"begin_time":  &now,
	})
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

// GetTaskDetail 获取任务详情（含产物下载链接）
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

	// W4: 如果存储已连接，列出该任务下的产物文件并生成下载链接
	result := gin.H{"task": task}
	if s.StorageConnected() {
		files, err := s.listTaskFiles(tid)
		if err != nil {
			s.Logger.Warn("列出任务文件失败", zap.String("tid", tid), zap.Error(err))
		} else {
			result["files"] = files
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": result,
	})
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
		s.DB.Model(&newTask).Updates(map[string]interface{}{
			"status":      3,
			"status_info": "gRPC 未连接，重试任务无法下发",
		})
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

		files = append(files, map[string]interface{}{
			"name":          filename,
			"size":          info.Size(),
			"last_modified": info.ModTime(),
			"content_type":  mimeType(filename),
			"download_url":  downloadURL,
			"source":        "local",
		})
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
		fileInfo := map[string]interface{}{
			"name":          obj.Name,
			"size":          obj.Size,
			"last_modified": obj.LastModified,
			"content_type":  obj.ContentType,
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
