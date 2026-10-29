package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
)

// FileInfo represents a remote backup file.
type FileInfo struct {
	Path         string
	Size         int64
	LastModified time.Time
}

// Client defines the common storage interface.
type Client interface {
	Upload(ctx context.Context, remotePath string, reader io.Reader, size int64) error
	Download(ctx context.Context, remotePath string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]FileInfo, error)
	Delete(ctx context.Context, remotePath string) error
}

// NewClient initializes a storage client for a given target.
func NewClient(target *config.StorageTarget) (Client, error) {
	if target == nil {
		return nil, fmt.Errorf("存储配置不能为空")
	}
	switch target.Type {
	case "local":
		return NewLocalClient(target.Path), nil
	case "s3":
		return NewS3Client(target)
	default:
		return nil, fmt.Errorf("不支持的存储类型: %s", target.Type)
	}
}
