package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/9star-academy/bakctl/internal/backup"
	"github.com/9star-academy/bakctl/internal/config"
	"github.com/9star-academy/bakctl/internal/scheduler"
)

func TestCLITargetAndJobWorkflow(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config failed: %v", err)
	}

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}

	app := &App{
		Version:   "1.0.0",
		Out:       out,
		Err:       errOut,
		Config:    cfg,
		Engine:    backup.NewEngine(),
		Scheduler: scheduler.NewScheduler(),
	}

	ctx := context.Background()

	// 1. Add storage target
	code := app.Run(ctx, []string{"target", "add", "backup-drive", "--type", "local", "--path", tmpDir})
	if code != 0 {
		t.Fatalf("target add failed: %d, err: %s", code, errOut.String())
	}

	// 2. Add test source file
	srcFile := filepath.Join(tmpDir, "database.db")
	_ = os.WriteFile(srcFile, []byte("sqlite database test content"), 0644)

	// 3. Add backup job
	out.Reset()
	code = app.Run(ctx, []string{"add", "my-app-db", "--src", srcFile, "--to", "backup-drive:dbs/", "--encrypt-key", "secret123"})
	if code != 0 {
		t.Fatalf("add job failed: %d, err: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "配置成功") {
		t.Fatalf("expected success message, got: %s", out.String())
	}

	// 4. Run backup now
	out.Reset()
	code = app.Run(ctx, []string{"run", "my-app-db"})
	if code != 0 {
		t.Fatalf("run backup failed: %d, err: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "备份成功完成") {
		t.Fatalf("expected backup success, got: %s", out.String())
	}

	// 5. List jobs
	out.Reset()
	code = app.Run(ctx, []string{"list"})
	if code != 0 {
		t.Fatalf("list jobs failed: %d", code)
	}
	if !strings.Contains(out.String(), "my-app-db") || !strings.Contains(out.String(), "SUCCESS") {
		t.Fatalf("list output mismatch: %s", out.String())
	}

	// 6. Restore
	out.Reset()
	restoredPath := filepath.Join(tmpDir, "restored_output.db")
	code = app.Run(ctx, []string{"restore", "my-app-db", "--to", restoredPath, "--key", "secret123"})
	if code != 0 {
		t.Fatalf("restore failed: %d, err: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "数据已成功解密恢复") {
		t.Fatalf("expected restore success, got: %s", out.String())
	}
}
