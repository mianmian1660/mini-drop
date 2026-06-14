// ============================================================
// apiserver (API 后台) 小白版注释
// ============================================================
// 这个程序是 Web 前端和底层 Drop 系统之间的"中间人"
// 做的事情：
//   1. 启动一个 HTTP 服务（用 Gin 框架）
//   2. 连接 PostgreSQL 数据库
//   3. 提供 REST API 给前端调用
//   4. 通过 gRPC 调用 drop_server
// 语言：Go（谷歌开发的，编译快、并发强）
// ============================================================

package main  // Go 程序的入口必须在 main 包里

import (
	"fmt"       // 格式化输出
	"log"       // 日志
	"net/http"  // HTTP 相关
	"os"        // 操作系统相关（读环境变量等）

	"github.com/gin-gonic/gin"           // Gin：轻量级 Web 框架
	"go.uber.org/zap"                     // Zap：高性能日志库
	"gorm.io/driver/postgres"             // GORM 的 PostgreSQL 驱动
	"gorm.io/gorm"                        // GORM：Go 的 ORM 框架（操作数据库）

	"github.com/mini-drop/apiserver/model" // 数据模型
)

// APIServer 结构体：把服务需要的所有东西装在一起
// 类似 C++ 的 class，Go 用 struct + 方法
type APIServer struct {
	DB     *gorm.DB    // 数据库连接
	Logger *zap.Logger // 日志记录器
	Router *gin.Engine // HTTP 路由器
}

func main() {
	// ---------- 1. 初始化日志 ----------
	logger, err := zap.NewProduction()  // 生产模式：JSON 格式
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	defer logger.Sync()  // 程序退出时刷新日志缓冲

	// ---------- 2. 连接数据库 ----------
	// 先尝试从环境变量读连接字符串，没有就用默认值
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		dsn = "host=localhost user=postgres password=dev dbname=drop sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		logger.Fatal("数据库连接失败", zap.Error(err))
	}

	// 自动建表：如果表不存在就创建，字段变了就更新
	if err := model.AutoMigrate(db); err != nil {
		logger.Fatal("数据库迁移失败", zap.Error(err))
	}

	// ---------- 3. 初始化 HTTP 路由 ----------
	gin.SetMode(gin.ReleaseMode)  // 生产模式（不打印调试信息）
	router := gin.Default()        // 创建默认路由器（自带日志和恢复中间件）

	// 健康检查接口（Kubernetes/Docker 用来判断服务是否存活）
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// API 路由组（所有接口都在 /api/v1 下）
	api := router.Group("/api/v1")
	{
		// GET /api/v1/agents — 获取 Agent 列表
		api.GET("/agents", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"code": 0, "data": []interface{}{}})
		})
		// GET /api/v1/tasks — 获取任务列表
		api.GET("/tasks", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"code": 0, "data": []interface{}{}})
		})
	}

	// ---------- 4. 启动服务 ----------
	server := &APIServer{
		DB:     db,
		Logger: logger,
		Router: router,
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8191"
	}

	logger.Info("apiserver 启动", zap.String("port", port))
	if err := server.Router.Run(fmt.Sprintf(":%s", port)); err != nil {
		logger.Fatal("服务启动失败", zap.Error(err))
	}
}
