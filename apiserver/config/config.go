// ============================================================
// config — 配置加载模块
// 使用 Viper 读取 apiserver.yaml，并支持环境变量覆盖
// ============================================================

package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config 是全局配置结构体，包含所有运行时配置
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	GRPC     GRPCConfig     `mapstructure:"grpc"`
	Storage  StorageConfig  `mapstructure:"storage"`
	Log      LogConfig      `mapstructure:"log"`
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Port int    `mapstructure:"port"`
	Mode string `mapstructure:"mode"`
}

// DatabaseConfig 数据库连接配置
type DatabaseConfig struct {
	DSN               string `mapstructure:"dsn"`
	MaxOpenConns      int    `mapstructure:"max_open_conns"`
	MaxIdleConns      int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetimeSec int   `mapstructure:"conn_max_lifetime_sec"`
}

// GRPCConfig gRPC 客户端配置
type GRPCConfig struct {
	Addr       string `mapstructure:"addr"`
	TimeoutSec int    `mapstructure:"timeout_sec"`
}

// StorageConfig 对象存储配置
type StorageConfig struct {
	Endpoint         string `mapstructure:"endpoint"`
	PublicEndpoint   string `mapstructure:"public_endpoint"` // 浏览器可访问的地址（预签名URL用）
	AccessKey        string `mapstructure:"access_key"`
	SecretKey        string `mapstructure:"secret_key"`
	UseSSL           bool   `mapstructure:"use_ssl"`
	Bucket           string `mapstructure:"bucket"`
	PresignExpireSec int    `mapstructure:"presign_expire_sec"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// Load 加载配置文件并返回 Config 结构体
// 优先使用环境变量覆盖 YAML 中的值
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// 设置配置文件路径
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		// 默认搜索路径
		v.SetConfigName("apiserver")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("/etc/apiserver/")
	}

	// 支持环境变量覆盖（如 PG_DSN, DROP_GRPC 等）
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 读取配置文件（文件不存在不报错，可以用纯环境变量运行）
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
		// 配置文件不存在，不影响（Docker 里全部走环境变量）
	}

	cfg := &Config{}

	// 绑定默认值
	v.SetDefault("server.port", 8191)
	v.SetDefault("server.mode", "release")
	v.SetDefault("database.dsn", "host=localhost user=postgres password=dev dbname=drop sslmode=disable")
	v.SetDefault("database.max_open_conns", 100)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.conn_max_lifetime_sec", 3600)
	v.SetDefault("grpc.addr", "localhost:50051")
	v.SetDefault("grpc.timeout_sec", 5)
	v.SetDefault("storage.endpoint", "localhost:9000")
	v.SetDefault("storage.public_endpoint", "localhost:9000")
	v.SetDefault("storage.access_key", "drop")
	v.SetDefault("storage.secret_key", "dropdrop")
	v.SetDefault("storage.use_ssl", false)
	v.SetDefault("storage.bucket", "drop-data")
	v.SetDefault("storage.presign_expire_sec", 900)
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("log.output", "stdout")

	// 环境变量覆盖（优先级最高）
	// PG_DSN → database.dsn
	if envDSN := os.Getenv("PG_DSN"); envDSN != "" {
		v.Set("database.dsn", envDSN)
	}
	if envGRPC := os.Getenv("DROP_GRPC"); envGRPC != "" {
		v.Set("grpc.addr", envGRPC)
	}
	if envS3 := os.Getenv("S3_ENDPOINT"); envS3 != "" {
		v.Set("storage.endpoint", envS3)
		// 如果未单独配置 public_endpoint，默认用 S3_ENDPOINT
		if os.Getenv("S3_PUBLIC_ENDPOINT") == "" {
			v.Set("storage.public_endpoint", envS3)
		}
	}
	if envS3Pub := os.Getenv("S3_PUBLIC_ENDPOINT"); envS3Pub != "" {
		v.Set("storage.public_endpoint", envS3Pub)
	}
	if envAK := os.Getenv("S3_ACCESS_KEY"); envAK != "" {
		v.Set("storage.access_key", envAK)
	}
	if envSK := os.Getenv("S3_SECRET_KEY"); envSK != "" {
		v.Set("storage.secret_key", envSK)
	}
	if envPort := os.Getenv("PORT"); envPort != "" {
		v.Set("server.port", envPort)
	}

	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}

	return cfg, nil
}
