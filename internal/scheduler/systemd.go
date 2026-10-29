package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
)

// SystemdScheduler installs system-wide timers. Dependencies are kept private so
// tests can exercise failures without writing /etc or running real systemctl.
type SystemdScheduler struct {
	unitDir      string
	goos         string
	configPath   string
	customConfig bool
	executable   func() (string, error)
	lookPath     func(string) (string, error)
	command      func(context.Context, string, ...string) ([]byte, error)
}

func NewScheduler() *SystemdScheduler {
	return &SystemdScheduler{
		unitDir:      "/etc/systemd/system",
		goos:         runtime.GOOS,
		configPath:   config.DefaultConfigPath(),
		customConfig: os.Getenv("BAKCTL_CONFIG") != "",
		executable:   os.Executable,
		lookPath:     exec.LookPath,
		command: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

var jobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var intervalPattern = regexp.MustCompile(`^([1-9][0-9]*)([smhd])$`)
var dailyPattern = regexp.MustCompile(`^day@([0-9]{2}):([0-9]{2})$`)

// ValidateJobName ensures the name is safe in systemd filenames and unit text.
func ValidateJobName(name string) error {
	if !jobNamePattern.MatchString(name) {
		return fmt.Errorf("任务名称必须以字母或数字开头，仅含字母、数字、点、下划线或连字符，长度不超过 128")
	}
	return nil
}

// ValidateSchedule accepts the documented calendar and interval forms. Arbitrary
// unit text is deliberately not accepted as a schedule.
func ValidateSchedule(schedule string) error {
	_, err := parseSchedule(schedule)
	return err
}

func parseSchedule(schedule string) (string, error) {
	if schedule == "day" || schedule == "daily" {
		return "OnCalendar=*-*-* 03:00:00", nil
	}
	if matches := dailyPattern.FindStringSubmatch(schedule); matches != nil {
		hour, _ := strconv.Atoi(matches[1])
		minute, _ := strconv.Atoi(matches[2])
		if hour <= 23 && minute <= 59 {
			return fmt.Sprintf("OnCalendar=*-*-* %02d:%02d:00", hour, minute), nil
		}
	}
	if matches := intervalPattern.FindStringSubmatch(schedule); matches != nil {
		count, err := strconv.ParseInt(matches[1], 10, 64)
		seconds := map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86400}[matches[2]]
		// systemd uses microseconds internally. Limit to Go's duration range,
		// which is ample for a backup schedule and avoids numeric overflow.
		if err == nil && count <= int64((1<<63-1)/time.Second)/seconds {
			// Delay the first run relative to activation. OnBootSec would fire
			// immediately on hosts up for >5m, while add still holds its job lock.
			return fmt.Sprintf("OnUnitActiveSec=%s\nOnActiveSec=5m", schedule), nil
		}
	}
	return "", fmt.Errorf("无效的定时周期 %q；使用 daily、day@HH:MM 或正整数间隔（例如 6h、30m、7d）", schedule)
}

func (s *SystemdScheduler) systemctl() (string, error) {
	if s.goos != "linux" {
		return "", fmt.Errorf("systemd 定时备份仅支持 Linux；当前系统为 %s", s.goos)
	}
	bin, err := s.lookPath("systemctl")
	if err != nil {
		return "", fmt.Errorf("找不到 systemctl，无法配置定时备份: %w", err)
	}
	return bin, nil
}

func (s *SystemdScheduler) control(ctx context.Context, bin string, args ...string) error {
	output, err := s.command(ctx, bin, args...)
	if err != nil {
		return fmt.Errorf("systemctl %s 失败: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *SystemdScheduler) InstallTimer(ctx context.Context, jobName string, schedule string) error {
	if err := ValidateJobName(jobName); err != nil {
		return err
	}
	timerSpec, err := parseSchedule(schedule)
	if err != nil {
		return err
	}
	ctl, err := s.systemctl()
	if err != nil {
		return err
	}
	binPath, err := s.executable()
	if err != nil {
		return fmt.Errorf("无法确定 bakctl 可执行文件路径: %w", err)
	}
	if !filepath.IsAbs(binPath) {
		return fmt.Errorf("bakctl 可执行文件路径必须为绝对路径")
	}
	quotedBinary, err := quoteUnitValue(binPath, true)
	if err != nil {
		return err
	}
	quotedJob, _ := quoteUnitValue(jobName, true)
	environment := ""
	if s.customConfig {
		configPath, err := s.configFilePath()
		if err != nil {
			return fmt.Errorf("无法解析 BAKCTL_CONFIG 路径: %w", err)
		}
		assignment, err := quoteUnitValue("BAKCTL_CONFIG="+configPath, false)
		if err != nil {
			return err
		}
		environment = "Environment=" + assignment + "\n"
	}

	owner, err := s.ownerMarker()
	if err != nil {
		return err
	}
	serviceContent := fmt.Sprintf(`%s
[Unit]
Description=bakctl backup job - %s
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
%sExecStart=%s run %s --wait
`, owner, jobName, environment, quotedBinary, quotedJob)
	timerContent := fmt.Sprintf(`%s
[Unit]
Description=bakctl timer for job - %s

[Timer]
%s
Persistent=true

[Install]
WantedBy=timers.target
`, owner, jobName, timerSpec)

	unlock, err := s.lockUnits(ctx, jobName)
	if err != nil {
		return err
	}
	defer unlock()
	previous, err := s.readUnits(jobName)
	if err != nil {
		return err
	}
	if err := writeUnit(previous[0].path, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("创建 service 失败: %w", err)
	}
	if err := writeUnit(previous[1].path, []byte(timerContent), 0644); err != nil {
		return errors.Join(fmt.Errorf("创建 timer 失败: %w", err), restoreUnits(previous))
	}
	if err := s.control(ctx, ctl, "daemon-reload"); err != nil {
		return errors.Join(err, s.rollback(ctx, ctl, jobName, previous, false))
	}
	if err := s.control(ctx, ctl, "enable", "--now", "bakctl-"+jobName+".timer"); err != nil {
		return errors.Join(err, s.rollback(ctx, ctl, jobName, previous, !previous[1].exists))
	}
	return nil
}

// RemoveTimer leaves unit files in place if stopping/disabling fails, allowing
// the user to inspect and retry the operation instead of losing configuration.
func (s *SystemdScheduler) RemoveTimer(ctx context.Context, jobName string) error {
	if err := ValidateJobName(jobName); err != nil {
		return err
	}
	previous, err := s.readUnits(jobName)
	if err != nil {
		return err
	}
	if !previous[0].exists && !previous[1].exists {
		return nil
	}
	ctl, err := s.systemctl()
	if err != nil {
		return err
	}
	unlock, err := s.lockUnits(ctx, jobName)
	if err != nil {
		return err
	}
	defer unlock()
	// Another process may have installed or removed units while we waited.
	previous, err = s.readUnits(jobName)
	if err != nil {
		return err
	}
	if !previous[0].exists && !previous[1].exists {
		return nil
	}
	if previous[1].exists {
		if err := s.control(ctx, ctl, "stop", "bakctl-"+jobName+".timer"); err != nil {
			return err
		}
		if err := s.control(ctx, ctl, "disable", "bakctl-"+jobName+".timer"); err != nil {
			return err
		}
	}
	for _, unit := range previous {
		if unit.exists {
			if err := os.Remove(unit.path); err != nil {
				return errors.Join(fmt.Errorf("删除 unit 失败: %w", err), restoreUnits(previous))
			}
		}
	}
	if err := s.control(ctx, ctl, "daemon-reload"); err != nil {
		return errors.Join(err, s.rollback(ctx, ctl, jobName, previous, false))
	}
	return nil
}

// quoteUnitValue quotes one systemd value and escapes %-specifiers. ExecStart
// also expands $, unlike Environment=, so literal dollars need a second escape.
func quoteUnitValue(value string, command bool) (string, error) {
	for _, r := range value {
		if r < 32 || r == 127 {
			return "", fmt.Errorf("systemd 路径不能包含控制字符")
		}
	}
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(value)
	if command {
		value = strings.ReplaceAll(value, "$", "$$")
	}
	return `"` + value + `"`, nil
}

type unitSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func (s *SystemdScheduler) readUnits(jobName string) ([]unitSnapshot, error) {
	owner, err := s.ownerMarker()
	if err != nil {
		return nil, err
	}
	units := make([]unitSnapshot, 0, 2)
	for _, ext := range []string{"service", "timer"} {
		unit := unitSnapshot{path: filepath.Join(s.unitDir, "bakctl-"+jobName+"."+ext)}
		info, err := os.Lstat(unit.path)
		if os.IsNotExist(err) {
			units = append(units, unit)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取 unit 失败: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("拒绝修改非普通 unit 文件: %s", unit.path)
		}
		unit.data, err = os.ReadFile(unit.path)
		if err != nil {
			return nil, err
		}
		marked := false
		for _, line := range strings.Split(string(unit.data), "\n") {
			if strings.HasPrefix(line, "# bakctl-config-sha256:") {
				marked = true
				if line != owner {
					return nil, fmt.Errorf("unit 属于其他配置，拒绝修改: %s", unit.path)
				}
			}
		}
		if !marked && s.customConfig {
			return nil, fmt.Errorf("自定义配置不能修改未标记归属的旧版 unit: %s", unit.path)
		}
		unit.mode, unit.exists = info.Mode().Perm(), true
		units = append(units, unit)
	}
	return units, nil
}

func (s *SystemdScheduler) configFilePath() (string, error) {
	path := s.configPath
	if path == "" {
		path = config.DefaultConfigPath()
	}
	return filepath.Abs(path)
}

func (s *SystemdScheduler) ownerMarker() (string, error) {
	path, err := s.configFilePath()
	if err != nil {
		return "", fmt.Errorf("无法解析配置路径: %w", err)
	}
	return fmt.Sprintf("# bakctl-config-sha256: %x", sha256.Sum256([]byte(path))), nil
}

// Keep the lock inode in place: unlinking it would let another process lock a
// different inode while an existing waiter still holds this one. The per-unit
// lock also covers installations from different BAKCTL_CONFIG files.
func (s *SystemdScheduler) lockUnits(ctx context.Context, jobName string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(s.unitDir, ".bakctl-"+jobName+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("锁定 systemd unit 失败: %w", err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			file.Close()
			return nil, fmt.Errorf("锁定 systemd unit 失败: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func writeUnit(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".bakctl-unit-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func restoreUnits(units []unitSnapshot) error {
	var result error
	for _, unit := range units {
		if unit.exists {
			result = errors.Join(result, writeUnit(unit.path, unit.data, unit.mode))
		} else if err := os.Remove(unit.path); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (s *SystemdScheduler) rollback(ctx context.Context, ctl, jobName string, previous []unitSnapshot, cleanupNewTimer bool) error {
	// A cancellation should stop the requested operation, but still leave a
	// short opportunity to remove a partially enabled new timer.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var result error
	if cleanupNewTimer {
		result = s.control(cleanupCtx, ctl, "disable", "--now", "bakctl-"+jobName+".timer")
	}
	result = errors.Join(result, restoreUnits(previous))
	result = errors.Join(result, s.control(cleanupCtx, ctl, "daemon-reload"))
	return result
}
