// ============================================================
// util — 工具函数集合
// 包含：数据库连接、TID 生成、日志初始化
// ============================================================

package util

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitDB 初始化数据库连接并配置连接池
// dsn: PostgreSQL 连接字符串
// maxOpen/maxIdle/maxLifetime: 连接池参数
func InitDB(dsn string, maxOpen, maxIdle, maxLifetimeSec int) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		// 生产环境用 warn 级别，避免 SQL 刷屏
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	// 获取底层 sql.DB 配置连接池
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取底层数据库连接失败: %w", err)
	}

	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(time.Duration(maxLifetimeSec) * time.Second)

	return db, nil
}

// GenTID 生成任务唯一 ID（格式：tid-年月日-短UUID）
// 例如：tid-20260617-a1b2c3d4
func GenTID() string {
	shortUUID := uuid.New().String()[:8]
	return fmt.Sprintf("tid-%s-%s", time.Now().Format("20060102"), shortUUID)
}

// InitLogger 初始化 Zap 日志实例
// level: debug/info/warn/error
// format: json 或 console
func InitLogger(level, format string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	var encoder zapcore.Encoder
	if format == "console" {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	}

	// 输出到 stdout
	writeSyncer := zapcore.AddSync(zapcore.Lock(os.Stdout))

	core := zapcore.NewCore(
		encoder,
		writeSyncer,
		zapLevel,
	)

	logger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
	return logger, nil
}

// MarshalJSONB 将结构体序列化为 JSON 字节（用于 PostgreSQL JSONB 字段）
func MarshalJSONB(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// UnmarshalJSONB 将 JSON 字节反序列化为结构体
func UnmarshalJSONB(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
