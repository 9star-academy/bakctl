package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StorageTarget defines a destination for backup files.
type StorageTarget struct {
	Name      string `json:"name"`
	Type      string `json:"type"` // "s3", "webdav", "local"
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	Region    string `json:"region,omitempty"`
	Path      string `json:"path,omitempty"` // For local or webdav base folder
}

// BackupJob defines a backup task.
type BackupJob struct {
	Name        string    `json:"name"`
	Source      string    `json:"source"` // directory path or sqlite:///path/db.sqlite or pg://...
	Target      string    `json:"target"` // format: "target_name:prefix_path"
	Schedule    string    `json:"schedule,omitempty"`
	Keep        int       `json:"keep"` // retention count (default 7)
	EncryptKey  string    `json:"encrypt_key,omitempty"`
	WebhookURL  string    `json:"webhook_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	LastRunAt   time.Time `json:"last_run_at,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"` // "SUCCESS", "FAILED"
	LastMessage string    `json:"last_message,omitempty"`
	LastSize    int64     `json:"last_size,omitempty"`
}

// Config holds all targets and backup jobs.
type Config struct {
	Targets  map[string]*StorageTarget `json:"targets"`
	Jobs     map[string]*BackupJob     `json:"jobs"`
	filePath string
	mu       sync.RWMutex
}

// DefaultConfigPath returns /etc/bakctl/config.json or ~/.config/bakctl/config.json
func DefaultConfigPath() string {
	if os.Geteuid() == 0 {
		return "/etc/bakctl/config.json"
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".config", "bakctl", "config.json")
}

// Load reads or initializes the config.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	cfg := &Config{
		Targets:  make(map[string]*StorageTarget),
		Jobs:     make(map[string]*BackupJob),
		filePath: path,
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.filePath = path
	if cfg.Targets == nil {
		cfg.Targets = make(map[string]*StorageTarget)
	}
	if cfg.Jobs == nil {
		cfg.Jobs = make(map[string]*BackupJob)
	}
	return cfg, nil
}

// Save writes config to disk safely.
func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("创建目录失败 %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", c.filePath, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpFile, c.filePath)
}

func (c *Config) AddTarget(target *StorageTarget) error {
	if target.Name == "" {
		return fmt.Errorf("存储目标名称不能为空")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Targets[target.Name] = target
	return nil
}

func (c *Config) GetTarget(name string) (*StorageTarget, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.Targets[name]
	return t, ok
}

func (c *Config) AddJob(job *BackupJob) error {
	if job.Name == "" {
		return fmt.Errorf("任务名称不能为空")
	}
	if job.Keep <= 0 {
		job.Keep = 7
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	job.CreatedAt = time.Now()
	c.Jobs[job.Name] = job
	return nil
}

func (c *Config) GetJob(name string) (*BackupJob, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	j, ok := c.Jobs[name]
	return j, ok
}

func (c *Config) ListJobs() []*BackupJob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var list []*BackupJob
	for _, j := range c.Jobs {
		list = append(list, j)
	}
	return list
}
