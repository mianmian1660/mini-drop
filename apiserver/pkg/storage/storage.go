// ============================================================
// pkg/storage/storage.go — 对象存储抽象接口
// ============================================================
// 定义统一的存储操作，方便切换 MinIO / COS / S3 等后端
// 当前 MVP 阶段：仅实现 MinIO
// ============================================================

package storage

import (
	"context"
	"io"
	"time"
)

// FileInfo 存储文件基本信息
type FileInfo struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	ContentType  string    `json:"content_type"`
}

// Storage 对象存储接口
// 所有后端（MinIO/COS/S3）必须实现此接口
type Storage interface {
	// EnsureBucket 确保存储桶存在，不存在则创建
	EnsureBucket(ctx context.Context, bucket string) error

	// PutObject 上传文件到指定路径
	// key: 对象路径（如 "tid-xxx/perf.data"）
	// reader: 文件内容
	// size: 文件大小（-1 表示未知）
	// contentType: MIME 类型
	PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) error

	// GetObject 下载文件
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)

	// PresignedGetURL 生成预签名下载 URL（有效期秒数）
	PresignedGetURL(ctx context.Context, bucket, key string, expires time.Duration) (string, error)

	// ListObjects 列出指定前缀下的所有文件
	ListObjects(ctx context.Context, bucket, prefix string) ([]FileInfo, error)

	// DeleteObject 删除文件
	DeleteObject(ctx context.Context, bucket, key string) error

	// ObjectExists 检查文件是否存在
	ObjectExists(ctx context.Context, bucket, key string) (bool, error)
}
