package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type LocalClient struct {
	baseDir string
}

func NewLocalClient(baseDir string) *LocalClient {
	if baseDir == "" {
		baseDir = "./backups"
	}
	return &LocalClient{baseDir: baseDir}
}

func (l *LocalClient) fullPath(remotePath string) string {
	clean := filepath.Clean(strings.TrimPrefix(remotePath, "/"))
	return filepath.Join(l.baseDir, clean)
}

func (l *LocalClient) Upload(ctx context.Context, remotePath string, reader io.Reader, size int64) error {
	dest := l.fullPath(remotePath)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, reader)
	return err
}

func (l *LocalClient) Download(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	dest := l.fullPath(remotePath)
	return os.Open(dest)
}

func (l *LocalClient) List(ctx context.Context, prefix string) ([]FileInfo, error) {
	searchDir := l.fullPath(prefix)
	var files []FileInfo

	err := filepath.Walk(l.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.baseDir, path)
		if err != nil {
			return nil
		}
		if prefix == "" || strings.HasPrefix(rel, prefix) {
			files = append(files, FileInfo{
				Path:         rel,
				Size:         info.Size(),
				LastModified: info.ModTime(),
			})
		}
		return nil
	})

	_ = searchDir
	return files, err
}

func (l *LocalClient) Delete(ctx context.Context, remotePath string) error {
	dest := l.fullPath(remotePath)
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除本地备份文件失败 %s: %w", dest, err)
	}
	return nil
}
