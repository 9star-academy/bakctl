package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
	"github.com/9star-academy/bakctl/internal/storage"
)

func fixture(t *testing.T, content string) (*Engine, *config.BackupJob, *config.StorageTarget) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return NewEngine(), &config.BackupJob{Name: "app", Source: src, Target: "local:shared", Keep: 2}, &config.StorageTarget{Type: "local", Path: filepath.Join(dir, "backups")}
}

func TestRestoreOriginalFileBytes(t *testing.T) {
	e, job, target := fixture(t, "hello original")
	res, err := e.RunBackup(context.Background(), job, target)
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(target)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := e.Restore(context.Background(), client, res.FileName, "", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello original" {
		t.Fatalf("restored %d bytes; want exact original bytes", len(got))
	}
}

func TestSQLiteMissingToolFails(t *testing.T) {
	e, job, target := fixture(t, "not a SQLite database")
	job.Source = "sqlite://" + job.Source
	t.Setenv("PATH", t.TempDir())
	if _, err := e.RunBackup(context.Background(), job, target); err == nil {
		t.Fatal("missing sqlite3 must not fall back to an unsafe copy")
	}
}

func TestRetentionDoesNotDeleteOtherJobs(t *testing.T) {
	e, job, target := fixture(t, "data")
	dir := filepath.Join(target.Path, "shared")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "app_db_20260101_000000.bak.gz")
	if err := os.WriteFile(other, []byte("other job"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}
	job.Keep = 1
	if _, err := e.RunBackup(context.Background(), job, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("other job backup was deleted: %v", err)
	}
}

func TestRepeatedBackupHasUniqueObject(t *testing.T) {
	e, job, target := fixture(t, "data")
	a, err := e.RunBackup(context.Background(), job, target)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.RunBackup(context.Background(), job, target)
	if err != nil {
		t.Fatal(err)
	}
	if a.FileName == b.FileName {
		t.Fatalf("two backups overwrite %s", a.FileName)
	}
}

func TestDirectoryRoundTrip(t *testing.T) {
	e, job, target := fixture(t, "unused")
	job.Source = filepath.Join(t.TempDir(), "source-dir")
	if err := os.MkdirAll(filepath.Join(job.Source, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job.Source, "data"), []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(job.Source, "link")); err != nil {
		t.Fatal(err)
	}
	res, err := e.RunBackup(context.Background(), job, target)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := storage.NewClient(target)
	dest := filepath.Join(t.TempDir(), "restored")
	if err := e.Restore(context.Background(), client, res.FileName, "", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "data"))
	if err != nil || string(got) != "contents" {
		t.Fatalf("contents=%q err=%v", got, err)
	}
	link, err := os.Readlink(filepath.Join(dest, "link"))
	if err != nil || link != "data" {
		t.Fatalf("link=%q err=%v", link, err)
	}
	info, err := os.Stat(filepath.Join(dest, "empty"))
	if err != nil || !info.IsDir() {
		t.Fatalf("empty directory not restored: %v", err)
	}
}

func TestRestoreDoesNotOverwriteExistingDestination(t *testing.T) {
	e, job, target := fixture(t, "backup")
	res, err := e.RunBackup(context.Background(), job, target)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := storage.NewClient(target)
	dest := filepath.Join(t.TempDir(), "valuable")
	if err := os.WriteFile(dest, []byte("valuable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Restore(context.Background(), client, res.FileName, "", dest); err == nil {
		t.Fatal("must refuse overwriting existing destination")
	}
	got, err := os.ReadFile(dest)
	if err != nil || strings.TrimSpace(string(got)) != "valuable" {
		t.Fatalf("destination changed: %q, %v", got, err)
	}
}
