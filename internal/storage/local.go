package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
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

// Object names use slash-separated relative paths on both local and S3 storage.
func validateLocalKey(name string, prefix bool) error {
	if prefix {
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			return nil
		}
	}
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("无效的本地备份路径: %q", name)
	}
	return nil
}

func (l *LocalClient) rootPath(create bool) (string, error) {
	if create {
		if err := os.MkdirAll(l.baseDir, 0700); err != nil {
			return "", err
		}
	}
	// A configured storage root may itself be a mount or symlink. Resolve that
	// explicit root once, then reject symlinks in object paths below it.
	root, err := filepath.EvalSymlinks(l.baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Abs(root)
}

func localPath(root, name string, createParents bool) (string, error) {
	parts := strings.Split(name, "/")
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if createParents && i < len(parts)-1 {
				if err := os.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
					return "", err
				}
				info, err = os.Lstat(current)
			} else {
				continue
			}
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("本地备份路径不能经过符号链接: %s", current)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("本地备份父路径不是目录: %s", current)
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return "", fmt.Errorf("本地备份目标不是普通文件: %s", current)
		}
	}
	return current, nil
}

func (l *LocalClient) Upload(ctx context.Context, remotePath string, reader io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateLocalKey(remotePath, false); err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("备份大小不能为负数")
	}
	root, err := l.rootPath(true)
	if err != nil {
		return err
	}
	dest, err := localPath(root, remotePath, true)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".bakctl-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	n, err := io.Copy(f, contextReader{ctx, reader})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("备份大小不一致: 预期 %d 字节，实际 %d 字节", size, n)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Check again before publishing; partial writes never replace an existing
	// object. Renaming within the same directory is atomic.
	if _, err := localPath(root, remotePath, false); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), dest); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (l *LocalClient) Download(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateLocalKey(remotePath, false); err != nil {
		return nil, err
	}
	root, err := l.rootPath(false)
	if err != nil {
		return nil, err
	}
	if _, err := localPath(root, remotePath, false); err != nil {
		return nil, err
	}
	// OpenRoot also guards against a symlink being changed after validation.
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(filepath.FromSlash(remotePath))
	if err != nil {
		return nil, err
	}
	return &contextReadCloser{contextReader: contextReader{ctx, f}, closer: f}, nil
}

func (l *LocalClient) List(ctx context.Context, prefix string) ([]FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateLocalKey(prefix, true); err != nil {
		return nil, err
	}
	root, err := l.rootPath(false)
	if err != nil {
		return nil, err
	}
	var files []FileInfo
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("本地备份目录包含符号链接: %s", path)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("本地备份目录包含非普通文件: %s", path)
		}
		if strings.HasPrefix(info.Name(), ".bakctl-upload-") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, prefix) {
			files = append(files, FileInfo{Path: rel, Size: info.Size(), LastModified: info.ModTime()})
		}
		return nil
	})
	return files, err
}

func (l *LocalClient) Delete(ctx context.Context, remotePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateLocalKey(remotePath, false); err != nil {
		return err
	}
	root, err := l.rootPath(false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := localPath(root, remotePath, false); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.Remove(filepath.FromSlash(remotePath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除本地备份文件失败: %w", err)
	}
	return nil
}
