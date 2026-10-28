package scheduler

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type SystemdScheduler struct{}

func NewScheduler() *SystemdScheduler {
	return &SystemdScheduler{}
}

func (s *SystemdScheduler) InstallTimer(ctx context.Context, jobName string, schedule string) error {
	binPath, err := os.Executable()
	if err != nil {
		binPath = "/usr/local/bin/bakctl"
	}

	timerSpec := parseSchedule(schedule)

	// 1. Service unit
	serviceContent := fmt.Sprintf(`[Unit]
Description=bakctl backup job - %s
After=network.target

[Service]
Type=oneshot
ExecStart=%s run %s
`, jobName, binPath, jobName)

	serviceFile := fmt.Sprintf("/etc/systemd/system/bakctl-%s.service", jobName)
	if err := os.WriteFile(serviceFile, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("创建 service 失败: %w", err)
	}

	// 2. Timer unit
	timerContent := fmt.Sprintf(`[Unit]
Description=bakctl timer for job - %s

[Timer]
%s
Persistent=true

[Install]
WantedBy=timers.target
`, jobName, timerSpec)

	timerFile := fmt.Sprintf("/etc/systemd/system/bakctl-%s.timer", jobName)
	if err := os.WriteFile(timerFile, []byte(timerContent), 0644); err != nil {
		return fmt.Errorf("创建 timer 失败: %w", err)
	}

	// 3. Reload & enable
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
		_ = exec.CommandContext(ctx, "systemctl", "enable", "--now", fmt.Sprintf("bakctl-%s.timer", jobName)).Run()
	}

	return nil
}

func (s *SystemdScheduler) RemoveTimer(ctx context.Context, jobName string) {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.CommandContext(ctx, "systemctl", "stop", fmt.Sprintf("bakctl-%s.timer", jobName)).Run()
		_ = exec.CommandContext(ctx, "systemctl", "disable", fmt.Sprintf("bakctl-%s.timer", jobName)).Run()
	}
	_ = os.Remove(fmt.Sprintf("/etc/systemd/system/bakctl-%s.timer", jobName))
	_ = os.Remove(fmt.Sprintf("/etc/systemd/system/bakctl-%s.service", jobName))
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	}
}

func parseSchedule(s string) string {
	s = strings.TrimSpace(s)
	// Example: "6h" or "30m"
	if strings.HasSuffix(s, "h") || strings.HasSuffix(s, "m") || strings.HasSuffix(s, "d") {
		return fmt.Sprintf("OnUnitActiveSec=%s\nOnBootSec=5m", s)
	}
	// Example: "day@03:00" or "daily"
	if strings.HasPrefix(s, "day@") {
		timePart := strings.TrimPrefix(s, "day@")
		return fmt.Sprintf("OnCalendar=*-*-* %s:00", timePart)
	}
	if s == "daily" || s == "day" {
		return "OnCalendar=*-*-* 03:00:00"
	}
	// Fallback to raw OnCalendar
	return fmt.Sprintf("OnCalendar=%s", s)
}
