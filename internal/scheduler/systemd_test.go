package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testScheduler(t *testing.T) (*SystemdScheduler, *[]string) {
	t.Helper()
	calls := []string{}
	s := NewScheduler()
	s.unitDir = t.TempDir()
	s.goos = "linux"
	s.configPath = ""
	s.customConfig = false
	s.executable = func() (string, error) { return "/opt/bak ctl/bakctl", nil }
	s.lookPath = func(string) (string, error) { return "/usr/bin/systemctl", nil }
	s.command = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	return s, &calls
}

func assertNoUnitFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".bakctl-") && strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		t.Errorf("unexpected unit or temporary file remains: %s", entry.Name())
	}
}

func TestInstallTimerWritesUnitsAndStartsTimer(t *testing.T) {
	s, calls := testScheduler(t)
	s.configPath = filepath.Join(t.TempDir(), `config $value%name.json`)
	s.customConfig = true
	if err := s.InstallTimer(context.Background(), "app_db", "day@03:00"); err != nil {
		t.Fatal(err)
	}
	service, err := os.ReadFile(filepath.Join(s.unitDir, "bakctl-app_db.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ExecStart="/opt/bak ctl/bakctl" run "app_db" --wait`, `Environment="BAKCTL_CONFIG=`, `config $value%%name.json"`} {
		if !strings.Contains(string(service), want) {
			t.Errorf("service missing %q:\n%s", want, service)
		}
	}
	timer, err := os.ReadFile(filepath.Join(s.unitDir, "bakctl-app_db.timer"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(timer), "OnCalendar=*-*-* 03:00:00") {
		t.Errorf("wrong timer:\n%s", timer)
	}
	if got := strings.Join(*calls, ";"); got != "daemon-reload;enable --now bakctl-app_db.timer" {
		t.Errorf("commands=%s", got)
	}
}

func TestInstallTimerReportsFailuresAndRestoresExistingFiles(t *testing.T) {
	for _, step := range []string{"daemon-reload", "enable --now bakctl-job.timer"} {
		t.Run(step, func(t *testing.T) {
			s, _ := testScheduler(t)
			for _, ext := range []string{"service", "timer"} {
				if err := os.WriteFile(filepath.Join(s.unitDir, "bakctl-job."+ext), []byte("old-"+ext), 0640); err != nil {
					t.Fatal(err)
				}
			}
			failed := false
			s.command = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if strings.Join(args, " ") == step && !failed {
					failed = true
					return []byte("systemd is unavailable"), errors.New("exit 1")
				}
				return nil, nil
			}
			if err := s.InstallTimer(context.Background(), "job", "6h"); err == nil || !strings.Contains(err.Error(), "systemd is unavailable") {
				t.Fatalf("error=%v", err)
			}
			for _, ext := range []string{"service", "timer"} {
				data, err := os.ReadFile(filepath.Join(s.unitDir, "bakctl-job."+ext))
				if err != nil || string(data) != "old-"+ext {
					t.Fatalf("old %s not restored: data=%q err=%v", ext, data, err)
				}
			}
		})
	}
}

func TestFailedNewInstallCleansUnits(t *testing.T) {
	s, calls := testScheduler(t)
	original := s.command
	s.command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := original(ctx, name, args...)
		if args[0] == "enable" {
			return nil, errors.New("enable failed")
		}
		return out, err
	}
	if err := s.InstallTimer(context.Background(), "job", "6h"); err == nil {
		t.Fatal("expected enable failure")
	}
	for _, ext := range []string{"service", "timer"} {
		if _, err := os.Stat(filepath.Join(s.unitDir, "bakctl-job."+ext)); !os.IsNotExist(err) {
			t.Fatalf("partial %s remains: %v", ext, err)
		}
	}
	if !strings.Contains(strings.Join(*calls, ";"), "disable --now bakctl-job.timer") {
		t.Fatalf("partial enable not cleaned: %v", *calls)
	}
}

func TestInstallTimerRejectsUnsupportedHostMissingSystemctlAndInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name, job, schedule string
		setup               func(*SystemdScheduler)
	}{
		{"macOS", "app", "daily", func(s *SystemdScheduler) { s.goos = "darwin" }},
		{"no-systemctl", "app", "daily", func(s *SystemdScheduler) {
			s.lookPath = func(string) (string, error) { return "", errors.New("not found") }
		}},
		{"bad-name", "../app", "daily", nil},
		{"bad-time", "app", "day@25:00", nil},
		{"injected-schedule", "app", "6h\nUnit=other.service", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := testScheduler(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			if err := s.InstallTimer(context.Background(), tc.job, tc.schedule); err == nil {
				t.Fatal("expected failure")
			}
			entries, _ := os.ReadDir(s.unitDir)
			if len(entries) != 0 || len(*calls) != 0 {
				t.Fatalf("invalid request changed state: files=%d calls=%v", len(entries), *calls)
			}
		})
	}
}

func TestScheduleValidation(t *testing.T) {
	for _, value := range []string{"day", "daily", "day@00:00", "day@23:59", "6h", "30m", "7d", "1s"} {
		if err := ValidateSchedule(value); err != nil {
			t.Errorf("valid %q: %v", value, err)
		}
	}
	for _, value := range []string{"", "0h", "-1h", "1.5h", "99999999999999999999999d", "day@3:00", "day@24:00", "day@00:60", "whenever", "6h\n", " daily"} {
		if err := ValidateSchedule(value); err == nil {
			t.Errorf("accepted invalid %q", value)
		}
	}
}

func TestJobNameValidation(t *testing.T) {
	for _, name := range []string{"app", "a.b_c-d", "A1", "1app"} {
		if err := ValidateJobName(name); err != nil {
			t.Errorf("valid %q: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "-app", "app/a", "app\nExecStart=x", "job name", "job%t", "数据库", strings.Repeat("a", 129)} {
		if err := ValidateJobName(name); err == nil {
			t.Errorf("invalid name accepted: %q", name)
		}
	}
}

func TestRemoveTimerPreservesFilesOnControlFailure(t *testing.T) {
	for _, action := range []string{"stop", "disable"} {
		t.Run(action, func(t *testing.T) {
			s, _ := testScheduler(t)
			if err := s.InstallTimer(context.Background(), "job", "daily"); err != nil {
				t.Fatal(err)
			}
			s.command = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[0] == action {
					return nil, errors.New("control failed")
				}
				return nil, nil
			}
			if err := s.RemoveTimer(context.Background(), "job"); err == nil {
				t.Fatal("expected control error")
			}
			if _, err := os.Stat(filepath.Join(s.unitDir, "bakctl-job.timer")); err != nil {
				t.Fatalf("timer prematurely removed: %v", err)
			}
		})
	}
}

func TestRemoveTimer(t *testing.T) {
	s, calls := testScheduler(t)
	if err := s.InstallTimer(context.Background(), "job", "daily"); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	if err := s.RemoveTimer(context.Background(), "job"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, ";"); got != "stop bakctl-job.timer;disable bakctl-job.timer;daemon-reload" {
		t.Fatalf("commands=%s", got)
	}
	assertNoUnitFiles(t, s.unitDir)
	if err := s.RemoveTimer(context.Background(), "job"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
}

func TestUnitSymlinkIsNotOverwritten(t *testing.T) {
	s, calls := testScheduler(t)
	victim := filepath.Join(t.TempDir(), "unrelated")
	os.WriteFile(victim, []byte("preserve"), 0600)
	if err := os.Symlink(victim, filepath.Join(s.unitDir, "bakctl-job.service")); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallTimer(context.Background(), "job", "daily"); err == nil {
		t.Fatal("accepted symlink unit")
	}
	data, _ := os.ReadFile(victim)
	if string(data) != "preserve" || len(*calls) != 0 {
		t.Fatalf("unrelated state changed: data=%q calls=%v", data, *calls)
	}
}

func TestSystemctlProcessErrorIsReported(t *testing.T) {
	s := NewScheduler()
	s.goos = "linux"
	s.unitDir = t.TempDir()
	s.configPath = ""
	fake := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'fixture control failure' >&2\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.lookPath = func(string) (string, error) { return fake, nil }
	err := s.InstallTimer(context.Background(), "app", "daily")
	if err == nil || !strings.Contains(err.Error(), "fixture control failure") || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("process failure not propagated: %v", err)
	}
	assertNoUnitFiles(t, s.unitDir)
}

func TestExecutablePathQuotingAndValidation(t *testing.T) {
	s, _ := testScheduler(t)
	s.executable = func() (string, error) { return `/opt/a $value%name/quoted"file`, nil }
	if err := s.InstallTimer(context.Background(), "app", "daily"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.unitDir, "bakctl-app.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `ExecStart="/opt/a $$value%%name/quoted\"file" run "app" --wait`) {
		t.Fatalf("unsafe executable expansion/quoting: %s", data)
	}
	for _, tc := range []struct {
		name, path string
		err        error
	}{
		{"lookup-failed", "", errors.New("executable unavailable")},
		{"relative", "bakctl", nil},
		{"control-character", "/opt/bak\nctl", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := testScheduler(t)
			s.executable = func() (string, error) { return tc.path, tc.err }
			if err := s.InstallTimer(context.Background(), "app", "daily"); err == nil {
				t.Fatal("expected invalid executable error")
			}
			if len(*calls) != 0 {
				t.Fatalf("invalid executable invoked systemctl: %v", *calls)
			}
		})
	}
}

func TestRemoveReloadFailureRestoresUnitFiles(t *testing.T) {
	s, _ := testScheduler(t)
	if err := s.InstallTimer(context.Background(), "app", "daily"); err != nil {
		t.Fatal(err)
	}
	failed := false
	s.command = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "daemon-reload" && !failed {
			failed = true
			return nil, errors.New("reload failed")
		}
		return nil, nil
	}
	if err := s.RemoveTimer(context.Background(), "app"); err == nil {
		t.Fatal("remove hid reload failure")
	}
	for _, ext := range []string{"service", "timer"} {
		if _, err := os.Stat(filepath.Join(s.unitDir, "bakctl-app."+ext)); err != nil {
			t.Fatalf("unit not restored for retry: %v", err)
		}
	}
}

func TestConfigOwnershipPreventsForeignUnitChanges(t *testing.T) {
	s, calls := testScheduler(t)
	s.configPath = filepath.Join(t.TempDir(), "first.json")
	s.customConfig = true
	if err := s.InstallTimer(context.Background(), "shared", "daily"); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(s.unitDir, "bakctl-shared.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(original), "# bakctl-config-sha256: ") {
		t.Fatalf("unit has no ownership marker: %s", original)
	}
	s.configPath = filepath.Join(t.TempDir(), "second.json")
	*calls = nil
	if err := s.InstallTimer(context.Background(), "shared", "6h"); err == nil {
		t.Fatal("foreign config overwrote unit")
	}
	if err := s.RemoveTimer(context.Background(), "shared"); err == nil {
		t.Fatal("foreign config removed unit")
	}
	after, err := os.ReadFile(filepath.Join(s.unitDir, "bakctl-shared.service"))
	if err != nil || string(after) != string(original) || len(*calls) != 0 {
		t.Fatalf("foreign operation changed state: err=%v calls=%v", err, *calls)
	}
}

func TestCustomConfigCannotManageUnmarkedLegacyUnits(t *testing.T) {
	s, calls := testScheduler(t)
	for _, ext := range []string{"service", "timer"} {
		if err := os.WriteFile(filepath.Join(s.unitDir, "bakctl-app."+ext), []byte("legacy unit"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	s.configPath = filepath.Join(t.TempDir(), "custom.json")
	s.customConfig = true
	if err := s.InstallTimer(context.Background(), "app", "daily"); err == nil {
		t.Fatal("custom config overwrote unmarked legacy unit")
	}
	if err := s.RemoveTimer(context.Background(), "app"); err == nil {
		t.Fatal("custom config removed unmarked legacy unit")
	}
	if len(*calls) != 0 {
		t.Fatalf("foreign operation invoked systemctl: %v", *calls)
	}
	s.customConfig = false
	s.configPath = ""
	if err := s.RemoveTimer(context.Background(), "app"); err != nil {
		t.Fatalf("default config cannot manage legacy units: %v", err)
	}
}

func TestSharedUnitLockHonorsCancellationAcrossConfigs(t *testing.T) {
	s, calls := testScheduler(t)
	s.configPath = filepath.Join(t.TempDir(), "second.json")
	s.customConfig = true
	// Simulate another process/config holding the same system-wide job lock.
	file, err := os.OpenFile(filepath.Join(s.unitDir, ".bakctl-shared.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.InstallTimer(ctx, "shared", "daily"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked install error=%v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("contended install ran systemctl: %v", *calls)
	}
	assertNoUnitFiles(t, s.unitDir)
}

func TestIntervalTimerStartsRelativeToActivation(t *testing.T) {
	s, _ := testScheduler(t)
	if err := s.InstallTimer(context.Background(), "interval", "6h"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.unitDir, "bakctl-interval.timer"))
	if err != nil {
		t.Fatal(err)
	}
	timer := string(data)
	if !strings.Contains(timer, "OnActiveSec=5m\n") || !strings.Contains(timer, "OnUnitActiveSec=6h\n") || strings.Contains(timer, "OnBootSec=") {
		t.Fatalf("interval timer must wait after activation instead of firing immediately on an older boot:\n%s", timer)
	}
}
