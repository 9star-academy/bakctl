package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"
)

// StorageTarget defines a destination for backup files.
type StorageTarget struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	Region    string `json:"region,omitempty"`
	Path      string `json:"path,omitempty"`
}

type BackupJob struct {
	Name        string    `json:"name"`
	Source      string    `json:"source"`
	Target      string    `json:"target"`
	Schedule    string    `json:"schedule,omitempty"`
	Keep        int       `json:"keep"`
	EncryptKey  string    `json:"encrypt_key,omitempty"`
	WebhookURL  string    `json:"webhook_url,omitempty"` // Legacy configuration; notifications are not implemented.
	CreatedAt   time.Time `json:"created_at"`
	LastRunAt   time.Time `json:"last_run_at,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"`
	LastMessage string    `json:"last_message,omitempty"`
	LastSize    int64     `json:"last_size,omitempty"`
}

// Config is a snapshot. Use Update for mutations against the latest on-disk state.
type Config struct {
	Targets  map[string]*StorageTarget `json:"targets"`
	Jobs     map[string]*BackupJob     `json:"jobs"`
	filePath string
	original []byte
	mu       sync.RWMutex
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("名称须为 1–128 位字母、数字、点、下划线或连字符，并以字母或数字开头")
	}
	return nil
}

func DefaultConfigPath() string {
	if path := os.Getenv("BAKCTL_CONFIG"); path != "" {
		return path
	}
	if os.Geteuid() == 0 {
		return "/etc/bakctl/config.json"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "bakctl", "config.json")
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Targets: make(map[string]*StorageTarget), Jobs: make(map[string]*BackupJob), filePath: abs}
	data, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	if cfg.Targets == nil {
		cfg.Targets = make(map[string]*StorageTarget)
	}
	if cfg.Jobs == nil {
		cfg.Jobs = make(map[string]*BackupJob)
	}
	for name, t := range cfg.Targets {
		if t == nil {
			return nil, fmt.Errorf("存储目标 %s 为空", name)
		}
	}
	for name, j := range cfg.Jobs {
		if j == nil {
			return nil, fmt.Errorf("备份任务 %s 为空", name)
		}
	}
	cfg.original = append([]byte(nil), data...)
	return cfg, nil
}

func (c *Config) Path() string { return c.filePath }

// lock uses a separate stable inode: the JSON file itself is replaced by rename.
func (c *Config) lock() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(c.filePath), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(c.filePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

// Save refuses stale snapshots instead of silently discarding concurrent edits.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock, err := c.lock()
	if err != nil {
		return err
	}
	defer unlock(lock)
	data, err := os.ReadFile(c.filePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !bytes.Equal(data, c.original) {
		return fmt.Errorf("配置已被其他进程修改，请重新加载后重试")
	}
	return c.write()
}

// Update reloads and applies a short mutation while holding a process-wide file lock.
func (c *Config) Update(fn func(*Config) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock, err := c.lock()
	if err != nil {
		return err
	}
	defer unlock(lock)
	latest, err := Load(c.filePath)
	if err != nil {
		return err
	}
	if err = fn(latest); err != nil {
		return err
	}
	if err = latest.write(); err != nil {
		return err
	}
	c.Targets, c.Jobs, c.original = latest.Targets, latest.Jobs, latest.original
	return nil
}

func (c *Config) write() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.filePath), ".bakctl-config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, c.filePath); err != nil {
		return err
	}
	c.original = append([]byte(nil), data...)
	dir, err := os.Open(filepath.Dir(c.filePath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (c *Config) AddTarget(target *StorageTarget) error {
	if target == nil {
		return fmt.Errorf("存储目标不能为空")
	}
	if err := ValidateName(target.Name); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.Targets[target.Name]; exists {
		return fmt.Errorf("存储目标 %s 已存在，不能覆盖已有备份位置", target.Name)
	}
	copy := *target
	c.Targets[target.Name] = &copy
	return nil
}

func (c *Config) GetTarget(name string) (*StorageTarget, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.Targets[name]
	if !ok {
		return nil, false
	}
	copy := *t
	return &copy, true
}

func (c *Config) AddJob(job *BackupJob) error {
	if job == nil {
		return fmt.Errorf("备份任务不能为空")
	}
	if err := ValidateName(job.Name); err != nil {
		return err
	}
	if job.Keep < 0 {
		return fmt.Errorf("保留份数不能为负数")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.Jobs[job.Name]; exists {
		return fmt.Errorf("任务 %s 已存在；请先删除任务再重新配置（备份文件会保留）", job.Name)
	}
	copy := *job
	copy.CreatedAt = time.Now().UTC()
	c.Jobs[job.Name] = &copy
	return nil
}

func (c *Config) GetJob(name string) (*BackupJob, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	j, ok := c.Jobs[name]
	if !ok {
		return nil, false
	}
	copy := *j
	return &copy, true
}

func (c *Config) ListJobs() []*BackupJob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := make([]*BackupJob, 0, len(c.Jobs))
	for _, j := range c.Jobs {
		copy := *j
		list = append(list, &copy)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// RecordRun updates only execution fields and never recreates a removed/replaced job.
func (c *Config) RecordRun(completed *BackupJob) error {
	return c.Update(func(latest *Config) error {
		current, ok := latest.Jobs[completed.Name]
		if !ok {
			return fmt.Errorf("任务 %s 已删除，未写入过时的执行状态", completed.Name)
		}
		if !current.CreatedAt.Equal(completed.CreatedAt) || current.Source != completed.Source || current.Target != completed.Target || current.EncryptKey != completed.EncryptKey || current.Keep != completed.Keep || current.Schedule != completed.Schedule {
			return fmt.Errorf("任务 %s 已变更，未写入过时的执行状态", completed.Name)
		}
		current.LastRunAt, current.LastStatus, current.LastMessage, current.LastSize = completed.LastRunAt, completed.LastStatus, completed.LastMessage, completed.LastSize
		return nil
	})
}

// LockJob prevents simultaneous runs/pruning for a job using this config file.
// A process crash releases the kernel lock; the small lock file may safely remain.
func (c *Config) LockJob(name string) (func(), error) {
	return c.lockJob(context.Background(), name, false)
}

// WaitJob lets a timer activation wait for a short configuration or restore
// operation, without overlapping another execution. SIGTERM cancels the wait.
func (c *Config) WaitJob(ctx context.Context, name string) (func(), error) {
	return c.lockJob(ctx, name, true)
}

func (c *Config) lockJob(ctx context.Context, name string, wait bool) (func(), error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(c.filePath), ".bakctl-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(c.filePath + "\x00" + name))
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(digest[:])+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { unlock(f) }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			f.Close()
			return nil, err
		}
		if !wait {
			f.Close()
			return nil, fmt.Errorf("任务 %s 正在执行，请稍后重试", name)
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
