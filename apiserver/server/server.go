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
	GRPCConn   *grpc.ClientConn       // gRPC 连接（到 drop_server）
	ControlCli pb.ControlClient       // Control 服务客户端
	Storage    storage.Storage        // 对象存储（MinIO）
}

// New 创建一个新的 APIServer 实例
func New(db *gorm.DB, logger *zap.Logger, cfg *config.Config) *APIServer {
	// 设置 Gin 运行模式
	gin.SetMode(cfg.Server.Mode)

	router := gin.New()

	// 全局中间件：Recovery → CORS → AccessLog
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

	// 启动任务状态轮询器（W3：定期检查 Running 任务是否应标记为完成）
	go s.startTaskPoller()

	s.registerRoutes()
	return s
}

// initGRPC 初始化到 drop_server 的 gRPC 连接（非阻塞）
// 连接在后台进行，HTTP 服务立即启动
func (s *APIServer) initGRPC() {
	// 先标记为未连接，后台 goroutine 连接成功后更新
	s.GRPCConn = nil
	s.ControlCli = nil

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config.GRPC.TimeoutSec)*time.Second)
		defer cancel()

		conn, err := grpc.DialContext(ctx,
			s.Config.GRPC.Addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
		)
		if err != nil {
			s.Logger.Warn("gRPC 连接 drop_server 失败（任务将仅写库，不下发）",
				zap.String("addr", s.Config.GRPC.Addr),
				zap.Error(err),
			)
			return
		}

		s.GRPCConn = conn
		s.ControlCli = pb.NewControlClient(conn)
		s.Logger.Info("gRPC 连接 drop_server 成功",
			zap.String("addr", s.Config.GRPC.Addr),
		)
	}()
}

// Close 关闭资源（gRPC 连接等）
func (s *APIServer) Close() {
	if s.GRPCConn != nil {
		if err := s.GRPCConn.Close(); err != nil {
			s.Logger.Error("关闭 gRPC 连接失败", zap.Error(err))
		}
	}
}

// initStorage 初始化 MinIO 对象存储（W4）
// 连接 MinIO 并确保 drop-data 桶存在
func (s *APIServer) initStorage() error {
	cfg := s.Config.Storage
	if cfg.Endpoint == "" {
		return fmt.Errorf("存储 endpoint 未配置")
	}

	minioStore, err := storage.NewMinIOStorage(
		cfg.Endpoint,
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
// 定期扫描 status=1(RUNNING) 的任务，将超时或已完成的任务标记为终态
// 生产环境中应替换为 gRPC 回调或消息队列通知
func (s *APIServer) startTaskPoller() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.pollRunningTasks()
	}
}

// pollRunningTasks 检查 RUNNING 任务是否应标记为完成/失败
func (s *APIServer) pollRunningTasks() {
	var tasks []model.HotmethodTask

	// 查出所有 status=1 (RUNNING) 的任务
	if err := s.DB.Where("status = ?", 1).Find(&tasks).Error; err != nil {
		s.Logger.Error("轮询任务状态失败", zap.Error(err))
		return
	}

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
		deadline := task.BeginTime.Add(expectedDuration).Add(uploadBuffer)

		if time.Now().After(deadline) {
			// 超时，标记为完成（MVP 阶段简化处理）
			now := time.Now()
			s.DB.Model(&task).Updates(map[string]interface{}{
				"status":      2,
				"status_info": "采集完成（超时自动标记）",
				"end_time":    &now,
			})
			s.Logger.Info("任务自动标记为完成",
				zap.String("tid", task.TID),
				zap.String("name", task.Name),
			)
		}
	}
}

// registerRoutes 注册所有 API 路由
func (s *APIServer) registerRoutes() {
	// 健康检查（不需要鉴权）
	s.Router.GET("/healthz", s.Healthz)

	// API v1 路由组
	api := s.Router.Group("/api/v1")
	{
		// 鉴权回调（不需要登录中间件）
		api.GET("/auth/check", s.AuthCheck)

		// 用户信息
		api.GET("/users", s.GetCurrentUser)

		// Agent 管理
		api.GET("/agents", s.ListAgents)
		api.GET("/agent/stat", s.StatAgent)

		// 任务管理
		api.POST("/tasks", s.CreateTask)
		api.GET("/tasks", s.ListTasks)
		api.GET("/tasks/:tid", s.GetTaskDetail)
		api.DELETE("/tasks/:tid", s.DeleteTask)
		api.POST("/tasks/:tid/retry", s.RetryTask)

		// 文件管理（W4: MinIO 存储集成）
		api.GET("/cosfiles", s.ListCOSFiles)
		api.POST("/cosfiles/upload", s.UploadTestFile)
	}

	s.Logger.Info("路由注册完成",
		zap.Int("api_count", 12),
	)
}
