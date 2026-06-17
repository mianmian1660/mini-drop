// ============================================================
// apiserver (API 后台) — 程序入口
// ============================================================
// 职责：
//   1. 加载配置（YAML + 环境变量覆盖）
//   2. 初始化日志、数据库
//   3. 自动建表（GORM AutoMigrate）
//   4. 注册路由并启动 HTTP 服务
//
// 运行：./apiserver -c apiserver.yaml
//       或纯环境变量运行（Docker 模式）
// ============================================================

package main

import (
	"flag"
	"fmt"
	"log"

	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/server"
	"github.com/mini-drop/apiserver/util"
)

func main() {
	// 命令行参数
	configPath := flag.String("c", "", "配置文件路径（默认搜索 ./apiserver.yaml）")
	flag.Parse()

	// ---------- 1. 加载配置 ----------
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// ---------- 2. 初始化日志 ----------
	logger, err := util.InitLogger(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	defer logger.Sync()

	logger.Info("配置加载成功",
		zap.Int("port", cfg.Server.Port),
		zap.String("mode", cfg.Server.Mode),
		zap.String("grpc_addr", cfg.GRPC.Addr),
	)

	// ---------- 3. 连接数据库 ----------
	db, err := util.InitDB(
		cfg.Database.DSN,
		cfg.Database.MaxOpenConns,
		cfg.Database.MaxIdleConns,
		cfg.Database.ConnMaxLifetimeSec,
	)
	if err != nil {
		logger.Fatal("数据库连接失败", zap.Error(err))
	}
	logger.Info("数据库连接成功")

	// ---------- 4. 自动建表 ----------
	if err := model.AutoMigrate(db); err != nil {
		logger.Fatal("数据库迁移失败", zap.Error(err))
	}
	logger.Info("数据库迁移完成（7 张表已就绪）")

	// ---------- 5. 初始化 HTTP 服务 ----------
	srv := server.New(db, logger, cfg)

	// ---------- 6. 启动 ----------
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	logger.Info("apiserver 启动中...", zap.String("addr", addr))

	if err := srv.Router.Run(addr); err != nil {
		logger.Fatal("服务启动失败", zap.Error(err))
	}
}
