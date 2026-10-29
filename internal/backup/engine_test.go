package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/9star-academy/bakctl/internal/config"
	"github.com/9star-academy/bakctl/internal/storage"
)

func TestEngineBackupAndRestore(t *testing.T) {
	tmpDir := t.TempDir()
	sourceFile := filepath.Join(tmpDir, "test_data.txt")
	testContent := "hello world backup testing data"
	if err := os.WriteFile(sourceFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to write test source: %v", err)
	}

	backupDir := filepath.Join(tmpDir, "remote_storage")

	target := &config.StorageTarget{
		Name: "test-local",
		Type: "local",
		Path: backupDir,
	}

	job := &config.BackupJob{
		Name:       "myjob",
		Source:     sourceFile,
		Target:     "test-local:app_backups",
		EncryptKey: "test-secret-pass",
		Keep:       2,
	}

	engine := NewEngine()
	ctx := context.Background()

	// 1. Run backup
	res, err := engine.RunBackup(ctx, job, target)
	if err != nil {
		t.Fatalf("RunBackup failed: %v", err)
	}

	if !res.Encrypted {
		t.Fatalf("expected backup to be encrypted")
	}

	// 2. Restore
	client, _ := storage.NewClient(target)
	restoredFile := filepath.Join(tmpDir, "restored.txt")
	err = engine.Restore(ctx, client, res.FileName, job.EncryptKey, restoredFile)
	if err != nil {
		t.Fatalf("Restore failed: %v", err)
	}

	restoredData, err := os.ReadFile(restoredFile)
	if err != nil {
		t.Fatalf("read restored file failed: %v", err)
	}

	if string(restoredData) != testContent {
		t.Fatalf("restored data mismatch: got %q, want %q", restoredData, testContent)
	}
}
