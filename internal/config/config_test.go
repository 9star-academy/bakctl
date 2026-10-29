package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStaleSaveCannotOverwriteNewJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first, _ := Load(path)
	first.AddJob(&BackupJob{Name: "a", Source: "/tmp/a", Target: "local"})
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	running, _ := Load(path)
	editor, _ := Load(path)
	editor.AddJob(&BackupJob{Name: "new-job", Source: "/tmp/new", Target: "local"})
	if err := editor.Save(); err != nil {
		t.Fatal(err)
	}
	running.Jobs["a"].LastStatus = "SUCCESS"
	if err := running.Save(); err == nil {
		t.Fatal("stale save accepted; must reject overwriting a newer configuration")
	}
	fresh, _ := Load(path)
	if _, ok := fresh.GetJob("new-job"); !ok {
		t.Fatal("new job lost")
	}
}

func TestConfigAtomicPermissionsAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddTarget(&StorageTarget{Name: "local", Type: "local", Path: "/tmp/backups"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode=%o", info.Mode().Perm())
	}
	bad := filepath.Join(t.TempDir(), "blocked")
	broken, _ := Load(filepath.Join(bad, "config.json"))
	if err := os.WriteFile(bad, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := broken.Save(); err == nil {
		t.Fatal("expected persistence failure")
	}
}

func TestConcurrentTransactionsPreserveAllChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	initial, _ := Load(path)
	if err := initial.Save(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := Load(path)
			if err != nil {
				errs <- err
				return
			}
			errs <- c.Update(func(latest *Config) error {
				return latest.AddTarget(&StorageTarget{Name: fmt.Sprintf("t%d", i), Type: "local", Path: "/tmp"})
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	final, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Targets) != 20 {
		t.Fatalf("lost updates: %d targets", len(final.Targets))
	}
}

func TestRecordRunMergesAndDoesNotReviveJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first, _ := Load(path)
	for _, name := range []string{"a", "b"} {
		if err := first.AddJob(&BackupJob{Name: name, Source: "/tmp/" + name, Target: "local", Keep: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	a, _ := Load(path)
	b, _ := Load(path)
	ja, _ := a.GetJob("a")
	jb, _ := b.GetJob("b")
	ja.LastStatus = "SUCCESS"
	ja.LastRunAt = time.Now()
	ja.LastSize = 100
	jb.LastStatus = "FAILED"
	jb.LastMessage = "expected failure"
	if err := a.RecordRun(ja); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordRun(jb); err != nil {
		t.Fatal(err)
	}
	current, _ := Load(path)
	if current.Jobs["a"].LastStatus != "SUCCESS" || current.Jobs["b"].LastStatus != "FAILED" {
		t.Fatal("execution status was lost")
	}
	if err := current.Update(func(c *Config) error { delete(c.Jobs, "a"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordRun(ja); err == nil {
		t.Fatal("removed job status accepted")
	}
	if err := current.Update(func(c *Config) error { return c.AddJob(&BackupJob{Name: "a", Source: "changed", Target: "local"}) }); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordRun(ja); err == nil {
		t.Fatal("replaced job status accepted")
	}
	reloaded, _ := Load(path)
	if reloaded.Jobs["a"].Source != "changed" || reloaded.Jobs["a"].LastStatus != "" {
		t.Fatal("replacement job modified")
	}
}

func TestUpdateFailureLeavesFileAndSnapshotUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, _ := Load(path)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	want := errors.New("abort")
	if err := cfg.Update(func(latest *Config) error { latest.Targets["x"] = &StorageTarget{Name: "x"}; return want }); !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || len(cfg.Targets) != 0 {
		t.Fatal("failed mutation was published")
	}
	if err := os.WriteFile(path, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(*Config) error { return nil }); err == nil {
		t.Fatal("overwrote invalid config")
	}
}

func TestConfigValidationAndCopies(t *testing.T) {
	cfg, _ := Load(filepath.Join(t.TempDir(), "config.json"))
	for _, name := range []string{"", "../bad", "bad name", "a/b", strings.Repeat("x", 129)} {
		if err := ValidateName(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, name := range []string{"a", "db_prod", "job-1.prod"} {
		if err := ValidateName(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.AddTarget(nil); err == nil {
		t.Fatal("nil target")
	}
	if err := cfg.AddJob(nil); err == nil {
		t.Fatal("nil job")
	}
	if err := cfg.AddJob(&BackupJob{Name: "bad", Keep: -1}); err == nil {
		t.Fatal("negative keep")
	}
	target := &StorageTarget{Name: "local", Type: "local"}
	cfg.AddTarget(target)
	target.Type = "changed"
	got, _ := cfg.GetTarget("local")
	got.Type = "changed"
	if cfg.Targets["local"].Type != "local" {
		t.Fatal("target alias")
	}
	cfg.AddJob(&BackupJob{Name: "job", Source: "original"})
	if err := cfg.AddJob(&BackupJob{Name: "job"}); err == nil {
		t.Fatal("duplicate job")
	}
	job, _ := cfg.GetJob("job")
	job.Source = "changed"
	jobs := cfg.ListJobs()
	jobs[0].Source = "changed"
	if cfg.Jobs["job"].Source != "original" {
		t.Fatal("job alias")
	}
	if _, ok := cfg.GetJob("missing"); ok {
		t.Fatal("missing job")
	}
	if _, ok := cfg.GetTarget("missing"); ok {
		t.Fatal("missing target")
	}
}

func TestConfigLoadErrorsAndOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BAKCTL_CONFIG", path)
	cfg, err := Load("")
	if err != nil || cfg.Path() != path {
		t.Fatalf("load override: %v %v", cfg, err)
	}
	for _, data := range []string{"{bad", `{"jobs":{"a":null}}`, `{"targets":{"a":null}}`} {
		os.WriteFile(path, []byte(data), 0600)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid configuration %q", data)
		}
	}
	os.WriteFile(path, []byte(`{"jobs":null,"targets":null}`), 0600)
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jobs == nil || cfg.Targets == nil {
		t.Fatal("nil maps")
	}
	if _, err := Load(filepath.Dir(path)); err == nil {
		t.Fatal("accepted directory")
	}
}

func TestJobLockExclusionAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	a, _ := Load(path)
	b, _ := Load(path)
	release, err := a.LockJob("job")
	if err != nil {
		t.Fatal(err)
	}
	if unlock, err := b.LockJob("job"); err == nil {
		unlock()
		t.Fatal("concurrent same-job lock accepted")
	}
	other, err := b.LockJob("other")
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	next, err := b.LockJob("job")
	if err != nil {
		t.Fatal(err)
	}
	next()
	if _, err := a.LockJob("../bad"); err == nil {
		t.Fatal("unsafe lock name")
	}
}

func TestDuplicateTargetCannotRerouteExistingJobs(t *testing.T) {
	cfg, _ := Load(filepath.Join(t.TempDir(), "config.json"))
	if err := cfg.AddTarget(&StorageTarget{Name: "local", Type: "local", Path: "/original"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddTarget(&StorageTarget{Name: "local", Type: "local", Path: "/replacement"}); err == nil {
		t.Fatal("duplicate target silently rerouted backups")
	}
	target, _ := cfg.GetTarget("local")
	if target.Path != "/original" {
		t.Fatal("original target overwritten")
	}
}

func TestWaitJobRespectsCancellationAndAcquiresAfterRelease(t *testing.T) {
	cfg, _ := Load(filepath.Join(t.TempDir(), "config.json"))
	release, err := cfg.LockJob("job")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock, err := cfg.WaitJob(ctx, "job")
		if err == nil {
			unlock()
		}
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation: %v", err)
	}
	release()
	unlock, err := cfg.WaitJob(context.Background(), "job")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
