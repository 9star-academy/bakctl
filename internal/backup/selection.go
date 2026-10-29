package backup

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
	"github.com/9star-academy/bakctl/internal/storage"
)

var backupSuffix = regexp.MustCompile(`^([0-9]{8}_[0-9]{6})(?:\.([0-9]{9})-([0-9a-f]{16}))?\.bak\.gz(?:\.enc)?$`)

func jobPrefix(job *config.BackupJob) (string, error) {
	if job.Name == "" || job.Name == "." || job.Name == ".." || strings.ContainsAny(job.Name, "/\\\x00\r\n") {
		return "", fmt.Errorf("无效的任务名称")
	}
	_, prefix, found := strings.Cut(job.Target, ":")
	if !found {
		return "", nil
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "." || part == ".." || part == "" || strings.ContainsAny(part, "\\\x00\r\n") {
			return "", fmt.Errorf("无效的备份前缀")
		}
	}
	return prefix + "/", nil
}

// ListJobBackups returns only valid artifacts of this exact job, oldest first.
// Prefix matches alone are insufficient: app_ also matches app_db_. Timestamp
// validation keeps other objects out of both restore selection and retention.
func ListJobBackups(ctx context.Context, client storage.Client, job *config.BackupJob) ([]storage.FileInfo, error) {
	prefix, err := jobPrefix(job)
	if err != nil {
		return nil, err
	}
	expected := prefix + job.Name + "_"
	files, err := client.List(ctx, expected)
	if err != nil {
		return nil, err
	}
	selected := make([]storage.FileInfo, 0, len(files))
	for _, file := range files {
		if !strings.HasPrefix(file.Path, expected) {
			continue
		}
		suffix := strings.TrimPrefix(file.Path, expected)
		if path.Base(suffix) != suffix {
			continue
		}
		match := backupSuffix.FindStringSubmatch(suffix)
		if match == nil {
			continue
		}
		if _, err := time.Parse("20060102_150405", match[1]); err != nil {
			continue
		}
		selected = append(selected, file)
	}
	sort.Slice(selected, func(i, j int) bool {
		if !selected[i].LastModified.Equal(selected[j].LastModified) {
			return selected[i].LastModified.Before(selected[j].LastModified)
		}
		return selected[i].Path < selected[j].Path
	})
	return selected, nil
}
