// ============================================================
// pkg/storage/minio.go — MinIO 存储实现
// ============================================================

package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOStorage MinIO 对象存储实现
type MinIOStorage struct {
	client   *minio.Client
	endpoint string
}

// NewMinIOStorage 创建一个新的 MinIO 存储客户端
// endpoint: MinIO 地址（如 localhost:9000）
// accessKey / secretKey: MinIO 认证凭证
// useSSL: 是否使用 HTTPS
func NewMinIOStorage(endpoint, accessKey, secretKey string, useSSL bool) (*MinIOStorage, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO 客户端失败: %w", err)
	}

	return &MinIOStorage{
		client:   client,
		endpoint: endpoint,
	}, nil
}

// EnsureBucket 确保存储桶存在
func (m *MinIOStorage) EnsureBucket(ctx context.Context, bucket string) error {
	exists, err := m.client.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("检查 Bucket 失败: %w", err)
	}

	if !exists {
		err = m.client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
		if err != nil {
			return fmt.Errorf("创建 Bucket 失败: %w", err)
		}
	}

	return nil
}

// PutObject 上传文件
func (m *MinIOStorage) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) error {
	opts := minio.PutObjectOptions{
		ContentType: contentType,
	}
	if size > 0 {
		// size 已知时直接传
		_, err := m.client.PutObject(ctx, bucket, key, reader, size, opts)
		return err
	}
	// size 未知时用 -1
	_, err := m.client.PutObject(ctx, bucket, key, reader, -1, opts)
	return err
}

// GetObject 下载文件
func (m *MinIOStorage) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	obj, err := m.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("下载文件失败: %w", err)
	}
	return obj, nil
}

// PresignedGetURL 生成预签名下载 URL
func (m *MinIOStorage) PresignedGetURL(ctx context.Context, bucket, key string, expires time.Duration) (string, error) {
	u, err := m.client.PresignedGetObject(ctx, bucket, key, expires, nil)
	if err != nil {
		return "", fmt.Errorf("生成预签名 URL 失败: %w", err)
	}
	return u.String(), nil
}

// ListObjects 列出指定前缀下的所有文件
func (m *MinIOStorage) ListObjects(ctx context.Context, bucket, prefix string) ([]FileInfo, error) {
	var files []FileInfo

	objCh := m.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	for obj := range objCh {
		if obj.Err != nil {
			return nil, fmt.Errorf("列出文件失败: %w", obj.Err)
		}
		files = append(files, FileInfo{
			Name:         obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			ContentType:  obj.ContentType,
		})
	}

	// 确保返回空数组而非 null
	if files == nil {
		files = []FileInfo{}
	}

	return files, nil
}

// DeleteObject 删除文件
func (m *MinIOStorage) DeleteObject(ctx context.Context, bucket, key string) error {
	return m.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

// ObjectExists 检查文件是否存在
func (m *MinIOStorage) ObjectExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := m.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Endpoint 返回 MinIO 地址（用于日志）
func (m *MinIOStorage) Endpoint() string {
	return m.endpoint
}
