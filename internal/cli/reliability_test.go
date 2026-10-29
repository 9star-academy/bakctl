package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/9star-academy/bakctl/internal/backup"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/9star-academy/bakctl/internal/config"
)

func TestSaveFailureReturnsNonzero(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "blocked")
	cfg, err := config.Load(filepath.Join(parent, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	app := &App{Config: cfg, Out: out, Err: errOut}
	code := app.Run(context.Background(), []string{"target", "add", "local", "--type", "local", "--path", filepath.Join(dir, "backups")})
	if code == 0 {
		t.Fatalf("save failed but success returned: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

type fakeScheduler struct {
	installErr, removeErr error
	installs, removes     int
}

func (s *fakeScheduler) InstallTimer(context.Context, string, string) error {
	s.installs++
	return s.installErr
}
func (s *fakeScheduler) RemoveTimer(context.Context, string) error { s.removes++; return s.removeErr }

type hookEngine struct {
	*backup.Engine
	run func() (*backup.BackupResult, error)
}

func (e *hookEngine) RunBackup(context.Context, *config.BackupJob, *config.StorageTarget) (*backup.BackupResult, error) {
	return e.run()
}

func testApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return &App{Config: cfg, Out: out, Err: errOut, Engine: backup.NewEngine(), Scheduler: &fakeScheduler{}}, out, errOut, dir
}
func invoke(t *testing.T, a *App, want int, args ...string) {
	t.Helper()
	code := a.Run(context.Background(), args)
	if code != want {
		t.Fatalf("args=%v exit=%d want=%d stdout=%s stderr=%s", args, code, want, a.Out, a.Err)
	}
}
func setupJob(t *testing.T, a *App, dir, name string) {
	t.Helper()
	if _, ok := a.Config.GetTarget("local"); !ok {
		invoke(t, a, 0, "target", "add", "local", "--type", "local", "--path", filepath.Join(dir, "remote"))
	}
	src := filepath.Join(dir, name+".txt")
	if err := os.WriteFile(src, []byte("source "+name), 0600); err != nil {
		t.Fatal(err)
	}
	invoke(t, a, 0, "add", name, "--src", src, "--to", "local:dbs", "--encrypt-key", "test-passphrase")
}

func TestCLIEndToEndDataAndIsolation(t *testing.T) {
	a, out, errOut, dir := testApp(t)
	setupJob(t, a, dir, "app")
	setupJob(t, a, dir, "app_db")
	invoke(t, a, 0, "run", "app")
	invoke(t, a, 0, "run", "app_db")
	dest := filepath.Join(dir, "restored.txt")
	invoke(t, a, 0, "restore", "app", "--to", dest)
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "source app" {
		t.Fatalf("restored another task: %q", got)
	}
	invoke(t, a, 1, "restore", "app", "--to", dest)
	invoke(t, a, 1, "restore", "app", "--to", filepath.Join(dir, "wrong"), "--key", "wrong")
	if _, err := os.Stat(filepath.Join(dir, "wrong")); !os.IsNotExist(err) {
		t.Fatal("wrong key created destination")
	}
	invoke(t, a, 0, "restore", "app", "--dest", filepath.Join(dir, "alias"))
	invoke(t, a, 1, "restore", "app", "--to", filepath.Join(dir, "invalid"), "--legacy-type", "bad")
	out.Reset()
	errOut.Reset()
	invoke(t, a, 0, "list")
	if !strings.Contains(out.String(), "SUCCESS") {
		t.Fatal("missing successful status")
	}
	invoke(t, a, 1, "target", "rm", "local")
	invoke(t, a, 0, "rm", "app")
	invoke(t, a, 0, "rm", "app_db")
	invoke(t, a, 0, "target", "rm", "local")
	entries, err := os.ReadDir(filepath.Join(dir, "remote", "dbs"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("removing jobs deleted backup artifacts: %v %v", entries, err)
	}
}

func TestScheduleFailureIsNonzeroAndRemovalKeepsConfig(t *testing.T) {
	a, _, errOut, dir := testApp(t)
	invoke(t, a, 0, "target", "add", "local", "--type", "local", "--path", filepath.Join(dir, "remote"))
	fake := &fakeScheduler{installErr: errors.New("systemctl failed")}
	a.Scheduler = fake
	invoke(t, a, 1, "add", "scheduled", "--src", filepath.Join(dir, "file"), "--to", "local", "--every", "day@03:00")
	if fake.installs != 1 || !strings.Contains(errOut.String(), "自动调度未启用") {
		t.Fatal("schedule failure hidden")
	}
	fresh, _ := config.Load(a.Config.Path())
	if _, ok := fresh.GetJob("scheduled"); !ok {
		t.Fatal("expected saved config for diagnosis")
	}
	fake.removeErr = errors.New("cannot stop timer")
	invoke(t, a, 1, "rm", "scheduled")
	fresh, _ = config.Load(a.Config.Path())
	if _, ok := fresh.GetJob("scheduled"); !ok {
		t.Fatal("failed stop lost config")
	}
	fake.removeErr = nil
	invoke(t, a, 0, "rm", "scheduled")
}

func TestCompletedBackupCannotHideStatusSaveFailure(t *testing.T) {
	for _, runFails := range []bool{false, true} {
		t.Run(fmt.Sprint(runFails), func(t *testing.T) {
			a, out, errOut, dir := testApp(t)
			setupJob(t, a, dir, "job")
			a.Engine = &hookEngine{Engine: backup.NewEngine(), run: func() (*backup.BackupResult, error) {
				if err := os.WriteFile(a.Config.Path(), []byte("invalid-json"), 0600); err != nil {
					t.Fatal(err)
				}
				if runFails {
					return nil, errors.New("upload failed")
				}
				return &backup.BackupResult{FileName: "job.bak.gz"}, nil
			}}
			out.Reset()
			invoke(t, a, 1, "run", "job")
			if strings.Contains(out.String(), "备份成功完成") || !strings.Contains(errOut.String(), "状态") {
				t.Fatalf("false success: %s %s", out, errOut)
			}
		})
	}
}

func TestCLIBackupFailureRecordsFailure(t *testing.T) {
	a, _, _, dir := testApp(t)
	setupJob(t, a, dir, "job")
	os.Remove(filepath.Join(dir, "job.txt"))
	invoke(t, a, 1, "run", "job")
	fresh, _ := config.Load(a.Config.Path())
	job, _ := fresh.GetJob("job")
	if job.LastStatus != "FAILED" || job.LastMessage == "" || job.LastRunAt.IsZero() {
		t.Fatalf("failure not recorded: %+v", job)
	}
	release, err := a.Config.LockJob("job")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	invoke(t, a, 1, "run", "job")
	invoke(t, a, 1, "rm", "job")
}

func TestCLIInputValidation(t *testing.T) {
	a, _, _, dir := testApp(t)
	invoke(t, a, 0, "target", "add", "local", "--type", "local", "--path", filepath.Join(dir, "remote"))
	cases := [][]string{
		{"unknown"}, {"target"}, {"target", "unknown"}, {"target", "add"}, {"target", "rm"}, {"target", "rm", "missing"},
		{"target", "add", "../bad", "--type", "local", "--path", dir},
		{"target", "add", "bad", "--type", "webdav"}, {"target", "add", "bad", "--type", "local"},
		{"target", "add", "bad", "--type", "s3"}, {"target", "add", "bad", "--wat"},
		{"add"}, {"add", "job"}, {"add", "../bad", "--src", dir, "--to", "local"},
		{"add", "job", "--src", dir, "--to", "missing"},
		{"add", "job", "--src", dir, "--to", "local:../escape"},
		{"add", "job", "--src", dir, "--to", "local", "--keep", "-1"},
		{"add", "job", "--src", dir, "--to", "local", "--every", "bad"},
		{"add", "job", "--src", dir, "--to", "local", "--webhook", "https://example.invalid"},
		{"add", "job", "--src", "mysql://localhost/db", "--to", "local"},
		{"add", "job", "--src", "sqlite://", "--to", "local"},
		{"add", "job", "--src", "postgres://", "--to", "local"},
		{"add", "job", "--wat"}, {"run"}, {"run", "missing"}, {"rm"}, {"rm", "missing"}, {"restore"}, {"restore", "missing"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) { invoke(t, a, 1, args...) })
	}
	for _, args := range [][]string{nil, {"help"}, {"--version"}, {"target", "list"}, {"list"}} {
		invoke(t, a, 0, args...)
	}
	setupJob(t, a, dir, "job")
	invoke(t, a, 1, "add", "job", "--src", dir, "--to", "local")
	invoke(t, a, 1, "restore", "job")
	invoke(t, a, 1, "restore", "job", "--wat")
	invoke(t, a, 1, "restore", "job", "--to", filepath.Join(dir, "empty"))
}

func TestSourceNormalizationAndRedaction(t *testing.T) {
	for _, source := range []string{"postgres://user:private@localhost/db", "pg://user:private@localhost/db", "postgresql://user:private@localhost/db", "postgres://localhost/db?password=private", "postgres://localhost/db?Password=private"} {
		normalized, err := normalizeSource(source)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(displaySource(normalized), "private") {
			t.Fatal("source leaks password")
		}
	}
	for _, source := range []string{"relative.txt", "sqlite://relative.db"} {
		normalized, err := normalizeSource(source)
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(strings.TrimPrefix(normalized, "sqlite://")) {
			t.Fatalf("relative source persisted: %q", normalized)
		}
	}
	for _, destination := range []string{"bad:name/../escape", "local:./path", "local:a\\b", "local:a\nb", "local:..", "local:a//b"} {
		if _, _, err := normalizeDestination(destination); err == nil {
			t.Errorf("accepted %q", destination)
		}
	}
	if name, to, err := normalizeDestination("local:my backups/"); err != nil || name != "local" || to != "local:my backups" {
		t.Fatalf("normal prefix: %s %s %v", name, to, err)
	}
}

func TestNewLoadsExplicitConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BAKCTL_CONFIG", path)
	a, err := New("test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Config.Path() != path || a.Out == nil || a.Err == nil {
		t.Fatal("default wiring")
	}
	os.WriteFile(path, []byte("bad"), 0600)
	if _, err := New("test", nil, nil); err == nil {
		t.Fatal("invalid config ignored")
	}
}

func TestCLIRunWaitCanBeCancelled(t *testing.T) {
	a, _, errOut, dir := testApp(t)
	setupJob(t, a, dir, "job")
	release, err := a.Config.LockJob("job")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := a.Run(ctx, []string{"run", "job", "--wait"}); code != 1 || !strings.Contains(errOut.String(), "canceled") {
		t.Fatalf("wait result: code=%d stderr=%s", code, errOut)
	}
	invoke(t, a, 1, "run", "job", "--unknown")
	invoke(t, a, 1, "run", "job", "extra")
}
