// ============================================================
// server — HTTP 服务核心
// APIServer 结构体持有所有依赖，并注册全部路由
// ============================================================

package server

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/middleware"
)

// APIServer 是 HTTP 服务的顶层结构体
// 持有数据库、日志、配置等依赖，通过方法注册路由
type APIServer struct {
	DB     *gorm.DB
	Logger *zap.Logger
	Config *config.Config
	Router *gin.Engine
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

	s.registerRoutes()
	return s
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

		// 文件管理
		api.GET("/cosfiles", s.ListCOSFiles)
	}

	s.Logger.Info("路由注册完成",
		zap.Int("api_count", 11),
	)
}
