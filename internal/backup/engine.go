package backup

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
	"github.com/9star-academy/bakctl/internal/crypto"
	"github.com/9star-academy/bakctl/internal/storage"
)

type BackupResult struct {
	JobName        string
	FileName       string
	OriginalSize   int64
	CompressedSize int64
	Encrypted      bool
	Duration       time.Duration
}

type Engine struct{}

func NewEngine() *Engine { return &Engine{} }

// RunBackup stages a complete, compressed (optionally encrypted) artifact before
// uploading it. Temporary plaintext is kept in a private directory and removed
// on every return path. New artifacts never reuse a second-resolution filename.
func (e *Engine) RunBackup(ctx context.Context, job *config.BackupJob, target *config.StorageTarget) (*BackupResult, error) {
	started := time.Now()
	prefix, err := jobPrefix(job)
	if err != nil {
		return nil, err
	}
	client, err := storage.NewClient(target)
	if err != nil {
		return nil, err
	}
	workspace, err := os.MkdirTemp("", "bakctl-backup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workspace)

	source, kind, mode, err := prepareSource(ctx, job.Source, workspace)
	if err != nil {
		return nil, fmt.Errorf("读取/备份源失败: %w", err)
	}
	if kind == kindDirectory {
		resolvedWorkspace, err := resolvedPath(workspace)
		if err != nil {
			return nil, err
		}
		if pathWithin(source, resolvedWorkspace) {
			return nil, fmt.Errorf("临时目录位于备份源内；请将 TMPDIR 设置为备份源之外的目录")
		}
		if target.Type == "local" {
			base := target.Path
			if base == "" {
				base = "./backups"
			}
			resolvedTarget, err := resolvedPath(filepath.Join(base, filepath.FromSlash(strings.TrimSuffix(prefix, "/"))))
			if err != nil {
				return nil, err
			}
			if pathWithin(source, resolvedTarget) {
				return nil, fmt.Errorf("本地备份目标必须位于备份源目录之外")
			}
		}
	}
	compressedPath := filepath.Join(workspace, "backup.gz")
	compressed, err := os.OpenFile(compressedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	gw := gzip.NewWriter(compressed)
	originalSize, writeErr := writeSource(ctx, gw, source, kind, mode)
	gzipErr := gw.Close()
	closeErr := compressed.Close()
	if writeErr != nil {
		return nil, fmt.Errorf("备份源失败: %w", writeErr)
	}
	if gzipErr != nil {
		return nil, gzipErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	payloadPath := compressedPath
	encrypted := job.EncryptKey != ""
	if encrypted {
		payloadPath = filepath.Join(workspace, "backup.enc")
		if err := transformFile(ctx, compressedPath, payloadPath, func(dst io.Writer, src io.Reader) error {
			return crypto.EncryptStream(dst, src, job.EncryptKey)
		}); err != nil {
			return nil, fmt.Errorf("数据加密失败: %w", err)
		}
	}
	payload, err := os.Open(payloadPath)
	if err != nil {
		return nil, err
	}
	defer payload.Close()
	info, err := payload.Stat()
	if err != nil {
		return nil, err
	}
	var random [8]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s_%s-%s.bak.gz", job.Name, time.Now().UTC().Format("20060102_150405.000000000"), hex.EncodeToString(random[:]))
	if encrypted {
		name += ".enc"
	}
	remotePath := prefix + name
	if err := client.Upload(ctx, remotePath, payload, info.Size()); err != nil {
		return nil, fmt.Errorf("上传至存储失败: %w", err)
	}
	result := &BackupResult{JobName: job.Name, FileName: remotePath, OriginalSize: originalSize, CompressedSize: info.Size(), Encrypted: encrypted, Duration: time.Since(started)}
	if job.Keep > 0 {
		if err := pruneOldBackups(ctx, client, job); err != nil {
			return result, fmt.Errorf("备份已上传，但清理旧备份失败: %w", err)
		}
	}
	result.Duration = time.Since(started)
	return result, nil
}

func pruneOldBackups(ctx context.Context, client storage.Client, job *config.BackupJob) error {
	files, err := ListJobBackups(ctx, client, job)
	if err != nil {
		return err
	}
	for i := 0; i < len(files)-job.Keep; i++ {
		if err := client.Delete(ctx, files[i].Path); err != nil {
			return fmt.Errorf("删除 %s: %w", files[i].Path, err)
		}
	}
	return nil
}

// Restore restores self-describing v2 backups. Ambiguous legacy archives require
// RestoreForSource or RestoreWithType; silently guessing file vs directory is unsafe.
func (e *Engine) Restore(ctx context.Context, client storage.Client, remotePath, encryptKey, destPath string) error {
	return e.RestoreWithType(ctx, client, remotePath, encryptKey, destPath, "")
}

// RestoreForSource uses the configured source only as a hint for legacy backups.
// New backups carry their own type, so the original source may be gone or changed.
func (e *Engine) RestoreForSource(ctx context.Context, client storage.Client, remotePath, encryptKey, destPath, source string) error {
	legacyType := ""
	if strings.HasPrefix(source, "sqlite://") || isPostgres(source) {
		legacyType = "raw"
	} else if info, err := os.Stat(source); err == nil {
		if info.IsDir() {
			legacyType = "directory"
		} else if info.Mode().IsRegular() {
			legacyType = "file"
		}
	}
	return e.RestoreWithType(ctx, client, remotePath, encryptKey, destPath, legacyType)
}

// RestoreWithType accepts file, directory or raw as a legacy-only type hint.
// It validates and stages the full result before publishing a new destination.
func (e *Engine) RestoreWithType(ctx context.Context, client storage.Client, remotePath, encryptKey, destPath, legacyType string) error {
	switch legacyType {
	case "", "file", "directory", "raw":
	default:
		return fmt.Errorf("无效的旧备份类型: %s", legacyType)
	}
	destPath, err := filepath.Abs(destPath)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destPath); err == nil {
		return fmt.Errorf("恢复目标已存在，拒绝覆盖: %s", destPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0700); err != nil {
		return err
	}
	workspace, err := os.MkdirTemp(filepath.Dir(destPath), ".bakctl-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	reader, err := client.Download(ctx, remotePath)
	if err != nil {
		return err
	}
	downloaded := filepath.Join(workspace, "download")
	err = writePrivateFile(downloaded, func(dst io.Writer) error {
		_, err := io.Copy(dst, &contextReader{ctx: ctx, reader: reader})
		return err
	})
	closeErr := reader.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	compressed := downloaded
	if strings.HasSuffix(remotePath, ".enc") {
		if encryptKey == "" {
			return fmt.Errorf("此备份已被加密，恢复时必须提供解密密码 (--key)")
		}
		compressed = filepath.Join(workspace, "decrypted.gz")
		if err := transformFile(ctx, downloaded, compressed, func(dst io.Writer, src io.Reader) error {
			return crypto.DecryptStream(dst, src, encryptKey)
		}); err != nil {
			return fmt.Errorf("解密失败: %w", err)
		}
	}
	unpacked := filepath.Join(workspace, "payload")
	if err := transformFile(ctx, compressed, unpacked, func(dst io.Writer, src io.Reader) error {
		gr, err := gzip.NewReader(src)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(dst, gr)
		closeErr := gr.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}); err != nil {
		return fmt.Errorf("解压缩或完整性检查失败: %w", err)
	}
	staged := filepath.Join(workspace, "result")
	directory, err := restorePayload(ctx, unpacked, staged, legacyType)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !directory {
		// Hard-link publication is atomic and cannot replace an existing path.
		if err := os.Link(staged, destPath); err != nil {
			return fmt.Errorf("发布恢复文件失败: %w", err)
		}
		return nil
	}
	// Serialize bakctl publications in this directory, then re-check the target.
	// Renaming a directory cannot replace a non-empty destination. The lock also
	// prevents two concurrent empty-directory restores from replacing each other.
	parent, err := os.Open(filepath.Dir(destPath))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := syscall.Flock(int(parent.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(parent.Fd()), syscall.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(destPath); err == nil {
		return fmt.Errorf("恢复目标已存在，拒绝覆盖: %s", destPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staged, destPath); err != nil {
		return fmt.Errorf("发布恢复目录失败: %w", err)
	}
	return nil
}

func transformFile(ctx context.Context, input, output string, transform func(io.Writer, io.Reader) error) error {
	source, err := os.Open(input)
	if err != nil {
		return err
	}
	defer source.Close()
	return writePrivateFile(output, func(dst io.Writer) error {
		return transform(dst, &contextReader{ctx: ctx, reader: source})
	})
}

func writePrivateFile(path string, write func(io.Writer) error) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = write(file)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
