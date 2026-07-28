// ============================================================
// server — HTTP 服务核心
// APIServer 结构体持有所有依赖，并注册全部路由
// ============================================================

package server

import (
	"context"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/middleware"
	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/pkg/storage"
	pb "github.com/mini-drop/apiserver/proto/control"
	"github.com/mini-drop/apiserver/util"
)

// APIServer 是 HTTP 服务的顶层结构体
// 持有数据库、日志、配置、gRPC 客户端、对象存储等依赖
type APIServer struct {
	DB         *gorm.DB
	Logger     *zap.Logger
	Config     *config.Config
	Router     *gin.Engine
	GRPCConn   *grpc.ClientConn        // gRPC 连接（到 drop_server）
	ControlCli pb.ControlClient        // Control 服务客户端
	Storage    storage.Storage         // 对象存储（MinIO）
	Cron       *cron.Cron              // 定时任务调度器（W5）
	CronJobs   map[string]cron.EntryID // SID → cron EntryID 映射（支持动态停止/删除）
}

// New 创建一个新的 APIServer 实例
func New(db *gorm.DB, logger *zap.Logger, cfg *config.Config) *APIServer {
	// 设置 Gin 运行模式
	gin.SetMode(cfg.Server.Mode)

	router := gin.New()

	// 全局中间件：RequestID → Recovery → CORS → AccessLog（6.4 节推荐顺序）
	// RequestID 必须排最前面，这样 Recovery/AccessLog 才能拿到同一个 request_id。
	router.Use(middleware.RequestID())
	router.Use(middleware.Recovery(logger))
	router.Use(middleware.CORS())
	router.Use(middleware.AccessLog(logger))

	s := &APIServer{
		DB:     db,
		Logger: logger,
		Config: cfg,
		Router: router,
	}

	// 初始化对象存储（MinIO）
	if err := s.initStorage(); err != nil {
		logger.Warn("MinIO 初始化失败（文件上传/签名功能不可用）",
			zap.String("endpoint", cfg.Storage.Endpoint),
			zap.Error(err),
		)
		// 不阻止启动，降级运行
	}

	// 初始化 gRPC 连接（连接失败不阻止启动，后续 CreateTask 会回退到仅写库模式）
	s.initGRPC()

	// A2: 启动一次性对账（4.6 节：Server 重启后从 DB 恢复待调度工作）
	// 等待一段时间让 gRPC 有机会连接上，再扫描"卡在 Created 状态"的孤儿任务。
	go func() {
		time.Sleep(10 * time.Second)
		s.reconcileOrphanedTasks()
	}()

	// 启动任务状态轮询器（W3：定期检查 Running 任务是否应标记为完成）
	go s.startTaskPoller()

	// W3: 启动 Agent 自动发现（每 30 秒同步一次）
	go s.startAgentDiscoverer()

	// 初始化定时任务调度器（W5：恢复 DB 中的 cron 任务并启动）
	s.initCron()

	s.registerRoutes()
	return s
}

// initGRPC 初始化到 drop_server 的 gRPC 连接（非阻塞，带重试）
// 连接在后台进行，HTTP 服务立即启动
func (s *APIServer) initGRPC() {
	// 先标记为未连接，后台 goroutine 连接成功后更新
	s.GRPCConn = nil
	s.ControlCli = nil

	go func() {
		// 重试连接，最多尝试 10 次，每次间隔 2 秒
		for i := 0; i < 10; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config.GRPC.TimeoutSec)*time.Second)

			conn, err := grpc.DialContext(ctx,
				s.Config.GRPC.Addr,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithBlock(),
			)
			cancel()

			if err != nil {
				s.Logger.Warn("gRPC 连接 drop_server 失败，将重试...",
					zap.String("addr", s.Config.GRPC.Addr),
					zap.Int("attempt", i+1),
					zap.Error(err),
				)
				time.Sleep(2 * time.Second)
				continue
			}

			s.GRPCConn = conn
			s.ControlCli = pb.NewControlClient(conn)
			s.Logger.Info("gRPC 连接 drop_server 成功",
				zap.String("addr", s.Config.GRPC.Addr),
				zap.Int("attempts", i+1),
			)
			return
		}

		s.Logger.Error("gRPC 连接 drop_server 最终失败（任务将仅写库，不下发）",
			zap.String("addr", s.Config.GRPC.Addr),
		)
	}()
}

// Close 关闭资源（gRPC 连接、cron 调度器等）
func (s *APIServer) Close() {
	if s.Cron != nil {
		s.Cron.Stop()
	}
	if s.GRPCConn != nil {
		if err := s.GRPCConn.Close(); err != nil {
			s.Logger.Error("关闭 gRPC 连接失败", zap.Error(err))
		}
	}
}

// initStorage 初始化 MinIO 对象存储（W4）
// 连接 MinIO 并确保 drop-data 桶存在
// 使用 publicEndpoint 生成浏览器可访问的预签名 URL
func (s *APIServer) initStorage() error {
	cfg := s.Config.Storage
	if cfg.Endpoint == "" {
		return fmt.Errorf("存储 endpoint 未配置")
	}

	// 如果未配置 public_endpoint，默认使用 endpoint
	publicEndpoint := cfg.PublicEndpoint
	if publicEndpoint == "" {
		publicEndpoint = cfg.Endpoint
	}

	minioStore, err := storage.NewMinIOStorageWithPublic(
		cfg.Endpoint,
		publicEndpoint,
		cfg.AccessKey,
		cfg.SecretKey,
		cfg.UseSSL,
	)
	if err != nil {
		return fmt.Errorf("连接 MinIO 失败: %w", err)
	}

	// 确保 bucket 存在
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := minioStore.EnsureBucket(ctx, cfg.Bucket); err != nil {
		return fmt.Errorf("创建 Bucket 失败: %w", err)
	}

	s.Storage = minioStore
	s.Logger.Info("MinIO 存储初始化成功",
		zap.String("endpoint", cfg.Endpoint),
		zap.String("public_endpoint", publicEndpoint),
		zap.String("bucket", cfg.Bucket),
	)
	return nil
}

// StorageConnected 返回对象存储是否已连接
func (s *APIServer) StorageConnected() bool {
	return s.Storage != nil
}

// GrpcConnected 返回 gRPC 是否已连接
func (s *APIServer) GrpcConnected() bool {
	return s.ControlCli != nil
}

// startTaskPoller 后台任务状态轮询器（W3）
// 定期扫描 RUNNING / UPLOADING 任务，补齐 PENDING -> RUNNING -> UPLOADING -> DONE/FAILED 状态链。
// 生产环境中应替换为 gRPC 回调或消息队列通知
func (s *APIServer) startTaskPoller() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.pollRunningTasks()
	}
}

// pollRunningTasks 检查 RUNNING / UPLOADING 任务是否应推进状态。
func (s *APIServer) pollRunningTasks() {
	var tasks []model.HotmethodTask

	if err := s.DB.Where("status IN ?", []int{TaskStatusRunning, TaskStatusUploading}).Find(&tasks).Error; err != nil {
		s.Logger.Error("轮询任务状态失败", zap.Error(err))
		return
	}

	now := time.Now()
	for _, task := range tasks {
		if task.BeginTime == nil {
			continue
		}

		// 解析任务参数获取采集时长
		var params PerfParams
		if err := util.UnmarshalJSONB(task.RequestParams, &params); err != nil {
			s.Logger.Warn("解析任务参数失败", zap.String("tid", task.TID), zap.Error(err))
			continue
		}

		// 计算预期完成时间 = beginTime + duration + 30s 上传缓冲
		expectedDuration := time.Duration(params.Duration) * time.Second
		uploadBuffer := 30 * time.Second
		uploadStart := task.BeginTime.Add(expectedDuration)
		deadline := task.BeginTime.Add(expectedDuration).Add(uploadBuffer)

		if task.Status == TaskStatusRunning && !now.Before(uploadStart) {
			_ = s.transitionTaskStatus(&task, TaskStatusUploading, "采集窗口结束，等待 Agent 上传产物", "task_poller", nil)
			s.Logger.Info("任务进入上传阶段",
				zap.String("tid", task.TID),
				zap.String("name", task.Name),
			)
			continue
		}

		if task.Status == TaskStatusUploading {
			hasArtifacts := s.taskHasArtifacts(task.TID)
			if hasArtifacts || now.After(deadline) {
				reason := "采集产物已上传，任务完成"
				if !hasArtifacts {
					reason = "上传等待窗口结束，任务自动标记完成"
				}
				endTime := now
				_ = s.transitionTaskStatus(&task, TaskStatusDone, reason, "task_poller", map[string]interface{}{"end_time": &endTime})
				s.Logger.Info("任务自动标记为完成",
					zap.String("tid", task.TID),
					zap.String("name", task.Name),
					zap.Bool("has_artifacts", hasArtifacts),
				)
			}
		}
	}
}

// reconcileOrphanedTasks 启动时执行一次性对账（A2，新复刻指南 4.6 节）。
//
// CreateTask 的实现是"写库 -> 同步调 gRPC 下发"，如果进程恰好在写库成功、
// 下发完成之前崩溃/重启，任务会永久停留在 Created(0) 状态，没有任何机制
// 让它继续往前走。这里扫描这类"孤儿任务"，重新尝试下发；gRPC 仍不可用
// 就显式标记失败，不允许任务无声无息地卡住。
func (s *APIServer) reconcileOrphanedTasks() {
	cutoff := time.Now().Add(-30 * time.Second)

	var tasks []model.HotmethodTask
	if err := s.DB.Where("status = ? AND create_time < ?", TaskStatusCreated, cutoff).Find(&tasks).Error; err != nil {
		s.Logger.Error("启动对账查询失败", zap.Error(err))
		return
	}
	if len(tasks) == 0 {
		s.Logger.Info("启动对账：没有发现停留在 Created 状态的孤儿任务")
		return
	}

	s.Logger.Info("启动对账：发现停留在 Created 状态的孤儿任务", zap.Int("count", len(tasks)))

	for i := range tasks {
		task := &tasks[i]

		var params PerfParams
		if err := util.UnmarshalJSONB(task.RequestParams, &params); err != nil {
			s.Logger.Warn("启动对账：解析任务参数失败", zap.String("tid", task.TID), zap.Error(err))
			_ = s.transitionTaskStatus(task, TaskStatusFailed, formatErrorReason(ErrCodeTaskInvalidArgument, "Server 重启对账：任务参数无法解析"), "startup_reconciler", nil)
			continue
		}

		req := CreateTaskReq{
			Name: task.Name, TaskType: task.Type, ProfilerType: task.ProfilerType,
			TargetIP: task.TargetIP, TargetPID: params.TargetPID, Duration: params.Duration,
			Frequency: params.Frequency, Callgraph: params.Callgraph, Event: params.Event,
			Subprocess: params.Subprocess,
		}

		if s.GrpcConnected() {
			s.Logger.Info("启动对账：重新下发孤儿任务", zap.String("tid", task.TID))
			s.dispatchTask(task, req)
		} else {
			_ = s.transitionTaskStatus(task, TaskStatusFailed, formatErrorReason(ErrCodeDependencyUnavailable, "Server 重启对账：gRPC 仍未连接，任务无法下发"), "startup_reconciler", nil)
		}
	}
}

func (s *APIServer) taskHasArtifacts(tid string) bool {
	if tid == "" {
		return false
	}
	if s.StorageConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		objects, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, tid+"/")
		if err == nil && len(objects) > 0 {
			return true
		}
		if err != nil {
			s.Logger.Warn("检查任务产物失败", zap.String("tid", tid), zap.Error(err))
		}
	}
	return len(s.listLocalFiles(tid)) > 0
}

// startAgentDiscoverer 后台 Agent 自动发现（W3: 每 30 秒通过 gRPC 探测在线 Agent）
func (s *APIServer) startAgentDiscoverer() {
	// 启动后等待 gRPC 连接建立
	time.Sleep(5 * time.Second)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if s.ControlCli == nil {
			continue
		}

		// 获取 DB 中已有的 Agent IP 列表
		var agents []model.AgentInfo
		if err := s.DB.Select("ip_addr").Find(&agents).Error; err != nil {
			continue
		}

		ips := map[string]bool{"127.0.0.1": true}
		for _, a := range agents {
			ips[a.IPAddr] = true
		}

		for ip := range ips {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			req := &pb.StatAgentRequest{TargetIP: ip}
			resp, err := s.ControlCli.StatAgent(ctx, req)
			cancel()

			if err != nil || resp.GetCode() != 0 {
				// Agent 不可达 → 标记离线并写审计
				var existing model.AgentInfo
				if s.DB.Where("ip_addr = ?", ip).First(&existing).Error == nil && existing.Online {
					s.DB.Model(&existing).Update("online", false)
					s.recordAgentAudit(existing.IPAddr, existing.Hostname, "offline", "30s 自动发现探测失败，判定 Agent 离线")
				}
				continue
			}

			// Agent 在线 → 更新或创建
			var existing model.AgentInfo
			result := s.DB.Where("ip_addr = ?", ip).First(&existing)
			now := time.Now()
			if result.Error == nil {
				if !existing.Online {
					s.recordAgentAudit(existing.IPAddr, existing.Hostname, "recovered", "30s 自动发现探测成功，Agent 恢复在线")
				}
				s.DB.Model(&existing).Updates(map[string]interface{}{
					"online":    true,
					"last_seen": now,
				})
			} else {
				agent := model.AgentInfo{
					Hostname:    ip,
					IPAddr:      ip,
					Online:      true,
					Version:     "1.0.0",
					Environment: "production",
					LastSeen:    now,
				}
				s.DB.Create(&agent)
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "registered", "30s 自动发现发现新 Agent")
				s.Logger.Info("自动发现新 Agent", zap.String("ip", ip))
			}
		}
	}
}

// registerRoutes 注册所有 API 路由
//
// A4: /api/v1 下除 /auth/check 外的所有路由都挂 CheckLogin 中间件，
// 缺失 Drop_user_uid 时统一 401，不再无条件放通。
// /auth/check 必须留在鉴权组之外——它自己就是用来告诉前端"有没有登录"的，
// 如果也被拦住，前端永远拿不到"未登录"这个明确信号。
func (s *APIServer) registerRoutes() {
	// 健康检查（不需要鉴权）
	s.Router.GET("/healthz", s.Healthz)

	// 鉴权回调（不挂 CheckLogin，这是唯一的例外）
	s.Router.GET("/api/v1/auth/check", s.AuthCheck)

	// 其余 /api/v1 路由：统一要求带身份信息
	api := s.Router.Group("/api/v1")
	api.Use(s.CheckLogin)
	{
		// 用户信息
		api.GET("/users", s.GetCurrentUser)

		// Agent 管理
		api.GET("/agents", s.ListAgents)
		api.GET("/agents/audits", s.ListAgentAudits)
		api.GET("/agent/detail", s.GetAgentDetail)
		api.GET("/agent/stat", s.StatAgent)

		// 任务管理
		api.POST("/tasks", s.CreateTask)
		api.GET("/tasks", s.ListTasks)
		api.GET("/tasks/:tid", s.GetTaskDetail)
		api.DELETE("/tasks/:tid", s.DeleteTask)
		api.POST("/tasks/:tid/retry", s.RetryTask)

		// Continuous Profiling 时间轴 (W6)
		api.GET("/tasks/timeline", s.GetTimeline)

		// 文件管理（W4: MinIO 存储集成 + 本地文件降级）
		api.GET("/cosfiles", s.ListCOSFiles)
		api.GET("/cosfiles/view", s.ViewCOSFile)
		api.GET("/cosfiles/download", s.DownloadCOSFile)
		api.POST("/cosfiles/upload", s.UploadTestFile)
		api.GET("/files/:filename", s.ServeLocalFile) // W4: 本地文件服务

		// 用户组管理（W5）
		api.POST("/groups", s.CreateGroup)
		api.GET("/groups", s.ListGroups)
		api.GET("/groups/:gid", s.GetGroupDetail)
		api.PUT("/groups/:gid", s.UpdateGroup)
		api.DELETE("/groups/:gid", s.DeleteGroup)
		api.POST("/groups/:gid/members", s.AddGroupMember)
		api.DELETE("/groups/:gid/members/:uid", s.RemoveGroupMember)

		// 定时任务管理（W5）
		api.POST("/schedule/task", s.CreateSchedule)
		api.GET("/schedule/tasks", s.ListSchedules)
		api.DELETE("/schedule/:sid", s.DeleteSchedule)
		api.POST("/schedule/:sid/toggle", s.ToggleSchedule)
	}

	s.Logger.Info("路由注册完成",
		zap.Int("api_count", 23),
	)
}
