package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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

func NewEngine() *Engine {
	return &Engine{}
}

func (e *Engine) RunBackup(ctx context.Context, job *config.BackupJob, target *config.StorageTarget) (*BackupResult, error) {
	start := time.Now()

	// 1. Dump source into buffer
	sourceData, err := e.dumpSource(ctx, job.Source)
	if err != nil {
		return nil, fmt.Errorf("读取/备份源失败: %w", err)
	}
	originalSize := int64(len(sourceData))

	// 2. Compress with Gzip
	var compressedBuf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&compressedBuf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gw.Write(sourceData); err != nil {
		return nil, err
	}
	_ = gw.Close()

	payload := compressedBuf.Bytes()
	isEncrypted := false

	// 3. Encrypt if key is set
	if job.EncryptKey != "" {
		key := crypto.DeriveKey(job.EncryptKey)
		encrypted, err := crypto.Encrypt(payload, key)
		if err != nil {
			return nil, fmt.Errorf("数据加密失败: %w", err)
		}
		payload = encrypted
		isEncrypted = true
	}

	compressedSize := int64(len(payload))

	// 4. Construct remote file name: <prefix>/<job-name>_<YYYYMMDD_HHMMSS>.bak[.enc]
	ext := ".bak.gz"
	if isEncrypted {
		ext += ".enc"
	}
	timestamp := time.Now().Format("20060102_150405")
	fileName := fmt.Sprintf("%s_%s%s", job.Name, timestamp, ext)

	prefix := ""
	if parts := strings.Split(job.Target, ":"); len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	if prefix != "" {
		prefix = prefix + "/"
	}
	remotePath := prefix + fileName

	// 5. Upload to storage
	client, err := storage.NewClient(target)
	if err != nil {
		return nil, err
	}

	if err := client.Upload(ctx, remotePath, bytes.NewReader(payload), compressedSize); err != nil {
		return nil, fmt.Errorf("上传至云端存储失败: %w", err)
	}

	// 6. Enforce Retention Policy
	if job.Keep > 0 {
		e.pruneOldBackups(ctx, client, prefix+job.Name+"_", job.Keep)
	}

	return &BackupResult{
		JobName:        job.Name,
		FileName:       remotePath,
		OriginalSize:   originalSize,
		CompressedSize: compressedSize,
		Encrypted:      isEncrypted,
		Duration:       time.Since(start),
	}, nil
}

func (e *Engine) dumpSource(ctx context.Context, src string) ([]byte, error) {
	if strings.HasPrefix(src, "sqlite://") {
		dbPath := strings.TrimPrefix(src, "sqlite://")
		return e.dumpSQLite(ctx, dbPath)
	}

	if strings.HasPrefix(src, "pg://") || strings.HasPrefix(src, "postgres://") {
		return e.dumpPostgres(ctx, src)
	}

	// Local file or directory
	return e.archivePath(src)
}

func (e *Engine) dumpSQLite(ctx context.Context, dbPath string) ([]byte, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("SQLite 数据库文件不存在: %s", dbPath)
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("bakctl_sqlite_%d.db", time.Now().UnixNano()))
	defer os.Remove(tmpFile)

	// Try sqlite3 CLI online hot backup
	if _, err := exec.LookPath("sqlite3"); err == nil {
		cmd := exec.CommandContext(ctx, "sqlite3", dbPath, fmt.Sprintf(".backup '%s'", tmpFile))
		if err := cmd.Run(); err == nil {
			return os.ReadFile(tmpFile)
		}
	}

	// Direct copy fallback
	return os.ReadFile(dbPath)
}

func (e *Engine) dumpPostgres(ctx context.Context, uri string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "pg_dump", uri)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("pg_dump 执行失败: %s (%w)", string(out), err)
	}
	return out, nil
}

func (e *Engine) archivePath(srcPath string) ([]byte, error) {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if !fi.IsDir() {
		// Single file
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{
			Name: filepath.Base(srcPath),
			Mode: int64(fi.Mode()),
			Size: fi.Size(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
		_ = tw.Close()
		return buf.Bytes(), nil
	}

	// Directory tree
	err = filepath.Walk(srcPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(srcPath, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		fileData, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tw.Write(fileData)
		return err
	})

	_ = tw.Close()
	return buf.Bytes(), err
}

func (e *Engine) pruneOldBackups(ctx context.Context, client storage.Client, prefix string, keep int) {
	files, err := client.List(ctx, prefix)
	if err != nil || len(files) <= keep {
		return
	}

	// Sort newest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].LastModified.After(files[j].LastModified)
	})

	// Delete from index keep to end
	for i := keep; i < len(files); i++ {
		_ = client.Delete(ctx, files[i].Path)
	}
}

// Restore downloads, decrypts, and extracts a backup file
func (e *Engine) Restore(ctx context.Context, client storage.Client, remotePath string, encryptKey string, destPath string) error {
	reader, err := client.Download(ctx, remotePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}

	// Decrypt if .enc
	if strings.HasSuffix(remotePath, ".enc") {
		if encryptKey == "" {
			return fmt.Errorf("此备份已被加密，恢复时必须提供解密密码 (--key)")
		}
		key := crypto.DeriveKey(encryptKey)
		decrypted, err := crypto.Decrypt(data, key)
		if err != nil {
			return fmt.Errorf("解密失败: %w", err)
		}
		data = decrypted
	}

	// Decompress Gzip
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("解压缩失败: %w", err)
	}
	defer gr.Close()

	decompressed, err := io.ReadAll(gr)
	if err != nil {
		return err
	}

	// Write to destPath
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}

	return os.WriteFile(destPath, decompressed, 0644)
}
