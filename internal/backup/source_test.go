package backup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/9star-academy/bakctl/internal/storage"
)

func TestSQLiteWALOnlineBackupIncludesCommittedRows(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not installed")
	}
	e, job, target := fixture(t, "unused")
	database := filepath.Join(t.TempDir(), "live.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sqlite, "-batch", database)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			t.Errorf("live sqlite: %v", err)
		}
	}()
	sql := "PRAGMA journal_mode=WAL;\nCREATE TABLE sample(x INTEGER);\nPRAGMA wal_checkpoint(TRUNCATE);\nINSERT INTO sample VALUES(42);\n.print READY\n"
	if _, err := fmt.Fprint(stdin, sql); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	ready := false
	for lines.Scan() {
		if lines.Text() == "READY" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("sqlite setup failed: %v", lines.Err())
	}
	if info, err := os.Stat(database + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("WAL not populated: %v", err)
	}
	job.Source = "sqlite://" + database
	res, err := e.RunBackup(ctx, job, target)
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(target)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := e.Restore(ctx, client, res.FileName, "", restored); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, sqlite, restored, "PRAGMA integrity_check; SELECT count(*) FROM sample;").CombinedOutput()
	if err != nil || string(output) != "ok\n1\n" {
		t.Fatalf("restored SQLite=%q err=%v", output, err)
	}
}

func TestSQLiteToolFailureDoesNotFallback(t *testing.T) {
	e, job, target := fixture(t, "database content")
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "sqlite3"), []byte("#!/bin/sh\necho 'database locked' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	job.Source = "sqlite://" + job.Source
	if _, err := e.RunBackup(context.Background(), job, target); err == nil || !strings.Contains(err.Error(), "database locked") {
		t.Fatalf("expected SQLite error, got %v", err)
	}
}

func TestPostgresSeparatesStderrAndNormalizesURI(t *testing.T) {
	e, job, target := fixture(t, "unused")
	tools := t.TempDir()
	log := filepath.Join(t.TempDir(), "args")
	t.Setenv("BAKCTL_TEST_PG_ARGS", log)
	script := "#!/bin/sh\nprintf '%s' \"$1\" > \"$BAKCTL_TEST_PG_ARGS\"\nprintf 'CREATE TABLE data(id int);\\n'\nprintf 'WARNING: test message\\n' >&2\n"
	if err := os.WriteFile(filepath.Join(tools, "pg_dump"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	job.Source = "pg://localhost/test"
	res, err := e.RunBackup(context.Background(), job, target)
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(target)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "database.sql")
	if err := e.Restore(context.Background(), client, res.FileName, "", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "CREATE TABLE data(id int);\n" {
		t.Fatalf("SQL=%q err=%v", got, err)
	}
	args, err := os.ReadFile(log)
	if err != nil || string(args) != "postgres://localhost/test" {
		t.Fatalf("args=%q err=%v", args, err)
	}
}

func TestBackupSourceFailures(t *testing.T) {
	for _, kind := range []string{"missing", "unsupported URI", "cancelled", "outside symlink", "invalid job", "invalid prefix", "unknown target"} {
		t.Run(kind, func(t *testing.T) {
			e, job, target := fixture(t, "data")
			ctx := context.Background()
			switch kind {
			case "missing":
				job.Source = filepath.Join(t.TempDir(), "missing")
			case "unsupported URI":
				job.Source = "mysql://localhost/db"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "outside symlink":
				job.Source = t.TempDir()
				if err := os.Symlink("../outside", filepath.Join(job.Source, "link")); err != nil {
					t.Fatal(err)
				}
			case "invalid job":
				job.Name = "../escape"
			case "invalid prefix":
				job.Target = "local:../escape"
			case "unknown target":
				target.Type = "unsupported"
			}
			if _, err := e.RunBackup(ctx, job, target); err == nil {
				t.Fatal("expected backup failure")
			}
		})
	}
}

func TestBackupRejectsRecursiveTargetsAndWorkspace(t *testing.T) {
	for _, kind := range []string{"target", "prefix", "target symlink", "workspace"} {
		t.Run(kind, func(t *testing.T) {
			e, job, target := fixture(t, "unused")
			root := t.TempDir()
			job.Source = filepath.Join(root, "source")
			if err := os.Mkdir(job.Source, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(job.Source, "data"), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "target":
				target.Path = filepath.Join(job.Source, "backups")
			case "prefix":
				target.Path = root
				job.Target = "local:source/backups"
			case "target symlink":
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(job.Source, alias); err != nil {
					t.Fatal(err)
				}
				target.Path = filepath.Join(alias, "backups")
			case "workspace":
				t.Setenv("TMPDIR", job.Source)
			}
			if _, err := e.RunBackup(context.Background(), job, target); err == nil {
				t.Fatal("recursive backup must fail before archiving")
			}
			entries, err := os.ReadDir(job.Source)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "bakctl-backup-") {
					t.Fatal("temporary workspace not removed")
				}
			}
		})
	}
}

type swapSourceWriter struct {
	bytes.Buffer
	source, target string
	swapped        bool
}

func (w *swapSourceWriter) Write(data []byte) (int, error) {
	if !w.swapped && bytes.HasPrefix(data, []byte("victim")) {
		w.swapped = true
		if err := os.Remove(w.source); err != nil {
			return 0, err
		}
		if err := os.Symlink(w.target, w.source); err != nil {
			return 0, err
		}
	}
	return w.Buffer.Write(data)
}

func TestSourceReplacementCannotReadOutsideDirectory(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(source, "victim")
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(victim, []byte("original data!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside secret"), 0600); err != nil {
		t.Fatal(err)
	}
	writer := &swapSourceWriter{source: victim, target: outside}
	if _, err := writeSource(context.Background(), writer, source, kindDirectory, 0700); err == nil {
		t.Fatal("source replacement must fail backup")
	}
	if bytes.Contains(writer.Bytes(), []byte("outside secret")) {
		t.Fatal("backup read outside source after replacement")
	}
}

func TestSourceSymlinkGraphCannotEscape(t *testing.T) {
	e, job, target := fixture(t, "unused")
	source := t.TempDir()
	job.Source = source
	if err := os.Mkdir(filepath.Join(source, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(source, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/link/../outside", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunBackup(context.Background(), job, target); err == nil {
		t.Fatal("escaping symlink graph must fail backup")
	}
}

func TestDirectoryRejectsNamesThatCannotBeRestoredSafely(t *testing.T) {
	e, job, target := fixture(t, "unused")
	job.Source = t.TempDir()
	if err := os.WriteFile(filepath.Join(job.Source, "back\\slash"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunBackup(context.Background(), job, target); err == nil {
		t.Fatal("backup must reject archive names that restoration would reject")
	}
}
