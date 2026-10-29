package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/9star-academy/bakctl/internal/backup"
	"github.com/9star-academy/bakctl/internal/config"
	"github.com/9star-academy/bakctl/internal/scheduler"
	"github.com/9star-academy/bakctl/internal/storage"
)

const helpText = `bakctl — 一行命令搞定服务器数据库与目录的定时加密备份

用法:
  bakctl target add <名称> [选项]       配置云端存储目标 (Cloudflare R2 / AWS S3 / 本地)
  bakctl target list                    查看已配置的存储目标
  bakctl target rm <名称>               删除存储目标
  bakctl add <任务名> [选项]            注册定时备份任务 (支持 SQLite / PG / 文件目录)
  bakctl run <任务名>                   立即手动执行一次备份任务
  bakctl list                           查看所有备份任务状态、大小及最近执行情况
  bakctl restore <任务名> --to <路径>    拉取最新备份并自动解密恢复
  bakctl rm <任务名>                    删除备份任务及对应的 systemd 定时器
  bakctl --version                      查看版本信息

示例:
  # 1. 绑定 Cloudflare R2 / AWS S3 存储
  sudo bakctl target add my-r2 --type s3 --endpoint https://<id>.r2.cloudflarestorage.com \
    --bucket my-backups --access-key <KEY> --secret-key <SECRET>

  # 2. 一行命令添加定时加密备份任务 (每天凌晨 3 点热备 SQLite，保留 14 份)
  sudo bakctl add app-db --src sqlite:///var/lib/app.db --to my-r2:dbs/ \
    --every "day@03:00" --keep 14 --encrypt-key "mypassword"

  # 3. 立即运行与查看
  sudo bakctl run app-db
  sudo bakctl list

  # 4. 从远端拉取解密恢复
  sudo bakctl restore app-db --to ./restored.db --key "mypassword"
`

type Scheduler interface {
	InstallTimer(context.Context, string, string) error
	RemoveTimer(context.Context, string) error
}

type BackupEngine interface {
	RunBackup(context.Context, *config.BackupJob, *config.StorageTarget) (*backup.BackupResult, error)
	RestoreForSource(context.Context, storage.Client, string, string, string, string) error
	RestoreWithType(context.Context, storage.Client, string, string, string, string) error
}

type App struct {
	Version   string
	Out       io.Writer
	Err       io.Writer
	Config    *config.Config
	Engine    BackupEngine
	Scheduler Scheduler
}

func New(version string, out, errOut io.Writer) (*App, error) {
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}

	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}

	return &App{
		Version:   version,
		Out:       out,
		Err:       errOut,
		Config:    cfg,
		Engine:    backup.NewEngine(),
		Scheduler: scheduler.NewScheduler(),
	}, nil
}

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Out, helpText)
		return 0
	}

	cmd := args[0]
	subArgs := args[1:]

	switch cmd {
	case "target":
		return a.runTarget(ctx, subArgs)
	case "add":
		return a.runAdd(ctx, subArgs)
	case "run":
		return a.runBackupNow(ctx, subArgs)
	case "list", "ls":
		return a.runList(ctx)
	case "rm", "delete":
		return a.runRemove(ctx, subArgs)
	case "restore":
		return a.runRestore(ctx, subArgs)
	case "-v", "--version", "version":
		fmt.Fprintf(a.Out, "bakctl version %s\n", a.Version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(a.Out, helpText)
		return 0
	default:
		fmt.Fprintf(a.Err, "未知命令: %s\n运行 'bakctl --help' 查看帮助\n", cmd)
		return 1
	}
}

func (a *App) runTarget(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "用法: bakctl target <add|list|rm> [选项]")
		return 1
	}

	action := args[0]
	switch action {
	case "list":
		if len(a.Config.Targets) == 0 {
			fmt.Fprintln(a.Out, "当前未配置任何存储目标。使用 'bakctl target add' 添加。")
			return 0
		}
		fmt.Fprintf(a.Out, "%-16s %-10s %-30s %s\n", "目标名称", "类型", "终端节点 / 路径", "Bucket")
		fmt.Fprintln(a.Out, "--------------------------------------------------------------------------------")
		for _, t := range a.Config.Targets {
			endpoint := t.Endpoint
			if t.Type == "local" {
				endpoint = t.Path
			}
			fmt.Fprintf(a.Out, "%-16s %-10s %-30s %s\n", t.Name, t.Type, endpoint, t.Bucket)
		}
		return 0

	case "add":
		if len(args) < 2 {
			fmt.Fprintln(a.Err, "用法: bakctl target add <名称> --type <s3|local> [选项]")
			return 1
		}
		name := args[1]
		fs := flag.NewFlagSet("target add", flag.ContinueOnError)
		fs.SetOutput(a.Err)

		tType := fs.String("type", "s3", "存储类型 (s3 或 local)")
		endpoint := fs.String("endpoint", "", "S3 兼容终端节点 (如 https://<id>.r2.cloudflarestorage.com)")
		bucket := fs.String("bucket", "", "S3 Bucket 名称")
		accessKey := fs.String("access-key", "", "S3 Access Key ID")
		secretKey := fs.String("secret-key", "", "S3 Secret Access Key")
		region := fs.String("region", "", "S3 Region（AWS 默认 us-east-1；自定义 endpoint 默认 auto）")
		localPath := fs.String("path", "", "本地存储根目录")

		if err := fs.Parse(args[2:]); err != nil {
			return 1
		}

		target := &config.StorageTarget{
			Name:      name,
			Type:      *tType,
			Endpoint:  *endpoint,
			Bucket:    *bucket,
			AccessKey: *accessKey,
			SecretKey: *secretKey,
			Region:    *region,
			Path:      *localPath,
		}

		if err := validateTarget(target); err != nil {
			fmt.Fprintf(a.Err, "无效存储目标: %v\n", err)
			return 1
		}
		if err := a.Config.Update(func(latest *config.Config) error { return latest.AddTarget(target) }); err != nil {
			fmt.Fprintf(a.Err, "添加目标失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Out, "✔ 存储目标 %s 添加成功 (%s)\n", name, *tType)
		return 0

	case "rm":
		if len(args) < 2 {
			fmt.Fprintln(a.Err, "用法: bakctl target rm <名称>")
			return 1
		}
		if err := a.Config.Update(func(latest *config.Config) error {
			if _, ok := latest.Targets[args[1]]; !ok {
				return fmt.Errorf("未找到存储目标: %s", args[1])
			}
			for _, job := range latest.Jobs {
				if strings.SplitN(job.Target, ":", 2)[0] == args[1] {
					return fmt.Errorf("存储目标仍被任务 %s 使用", job.Name)
				}
			}
			delete(latest.Targets, args[1])
			return nil
		}); err != nil {
			fmt.Fprintf(a.Err, "删除存储目标失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Out, "✔ 存储目标 %s 已删除\n", args[1])
		return 0

	default:
		fmt.Fprintf(a.Err, "未知操作: %s\n", action)
		return 1
	}
}

func (a *App) runAdd(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "用法: bakctl add <任务名> --src <路径> --to <目标:路径> [--every <时间>] [--keep <份数>] [--encrypt-key <密码>]")
		return 1
	}

	jobName := args[0]
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(a.Err)

	src := fs.String("src", "", "备份源 (目录路径、sqlite:///path、或 pg://uri)")
	to := fs.String("to", "", "远端目标 (格式: target_name:prefix/)")
	every := fs.String("every", "", "定时周期 (如 'day@03:00' 或 '6h')")
	keep := fs.Int("keep", 7, "远端保留备份份数 (超期自动清理)")
	encryptKey := fs.String("encrypt-key", "", "AES-256 加密密码 (留空则不加密)")
	webhook := fs.String("webhook", "", "告警 Webhook 推送地址")

	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	if *src == "" || *to == "" {
		fmt.Fprintln(a.Err, "❌ 错误: 必须指定 --src (备份源) 与 --to (远端目标)")
		return 1
	}

	if err := config.ValidateName(jobName); err != nil {
		fmt.Fprintf(a.Err, "无效任务名称: %v\n", err)
		return 1
	}
	if *keep < 0 {
		fmt.Fprintln(a.Err, "--keep 必须大于等于 0；0 表示不自动清理")
		return 1
	}
	if *webhook != "" {
		fmt.Fprintln(a.Err, "当前版本尚未实现 Webhook 通知，不能启用 --webhook")
		return 1
	}
	if *every != "" {
		if err := scheduler.ValidateSchedule(*every); err != nil {
			fmt.Fprintf(a.Err, "无效调度周期: %v\n", err)
			return 1
		}
	}
	normalizedSource, err := normalizeSource(*src)
	if err != nil {
		fmt.Fprintf(a.Err, "无效备份源: %v\n", err)
		return 1
	}
	targetName, normalizedTarget, err := normalizeDestination(*to)
	if err != nil {
		fmt.Fprintf(a.Err, "无效备份目标: %v\n", err)
		return 1
	}
	release, err := a.Config.LockJob(jobName)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	defer release()

	job := &config.BackupJob{
		Name:       jobName,
		Source:     normalizedSource,
		Target:     normalizedTarget,
		Schedule:   *every,
		Keep:       *keep,
		EncryptKey: *encryptKey,
		WebhookURL: *webhook,
	}

	if err := a.Config.Update(func(latest *config.Config) error {
		if _, ok := latest.GetTarget(targetName); !ok {
			return fmt.Errorf("未找到存储目标 %s；请用相同用户配置 target", targetName)
		}
		return latest.AddJob(job)
	}); err != nil {
		fmt.Fprintf(a.Err, "添加任务失败: %v\n", err)
		return 1
	}

	fmt.Fprintf(a.Out, "✔ 备份任务 %s 配置成功\n", jobName)
	fmt.Fprintf(a.Out, "  - 备份源: %s\n", displaySource(normalizedSource))
	fmt.Fprintf(a.Out, "  - 目标端: %s (保留最新 %d 份)\n", *to, *keep)
	if *encryptKey != "" {
		fmt.Fprintln(a.Out, "  - 加密保护: ✔ 已启用 (AES-256-GCM 端到端加密)")
	}

	if *every != "" {
		if err := a.Scheduler.InstallTimer(ctx, jobName, *every); err != nil {
			fmt.Fprintf(a.Err, "安装 systemd 定时器失败: %v\n配置已保存但自动调度未启用；修正环境后删除任务并重新添加。\n", err)
			return 1
		} else {
			fmt.Fprintf(a.Out, "  - 自动调度: ✔ 已成功注册并启动 systemd timer (%s)\n", *every)
		}
	}

	return 0
}

func (a *App) runBackupNow(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "用法: bakctl run <任务名>")
		return 1
	}

	jobName := args[0]
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	wait := fs.Bool("wait", false, "等待当前任务锁（systemd 使用）")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(a.Err, "run 包含多余的位置参数")
		return 1
	}
	var release func()
	var err error
	if *wait {
		release, err = a.Config.WaitJob(ctx, jobName)
	} else {
		release, err = a.Config.LockJob(jobName)
	}
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	defer release()
	latestConfig, err := config.Load(a.Config.Path())
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	a.Config = latestConfig
	job, ok := a.Config.GetJob(jobName)
	if !ok {
		fmt.Fprintf(a.Err, "未找到任务: %s\n", jobName)
		return 1
	}

	targetName := strings.Split(job.Target, ":")[0]
	target, ok := a.Config.GetTarget(targetName)
	if !ok {
		fmt.Fprintf(a.Err, "未找到关联的存储目标: %s\n", targetName)
		return 1
	}

	if job.WebhookURL != "" {
		fmt.Fprintln(a.Err, "当前版本不支持 Webhook 通知，请先移除配置中的 webhook_url")
		return 1
	}
	fmt.Fprintf(a.Out, "==> 正在执行备份任务 [%s]...\n", jobName)
	res, err := a.Engine.RunBackup(ctx, job, target)

	job.LastRunAt = time.Now()
	if err != nil {
		job.LastStatus = "FAILED"
		job.LastMessage = err.Error()
		if saveErr := a.Config.RecordRun(job); saveErr != nil {
			fmt.Fprintf(a.Err, "保存失败状态失败: %v\n", saveErr)
		}
		fmt.Fprintf(a.Err, "❌ 备份失败: %v\n", err)
		return 1
	}

	job.LastStatus = "SUCCESS"
	job.LastSize = res.CompressedSize
	job.LastMessage = "OK"
	if saveErr := a.Config.RecordRun(job); saveErr != nil {
		fmt.Fprintf(a.Err, "备份已上传，但保存执行状态失败: %v\n", saveErr)
		return 1
	}

	encStr := "未加密"
	if res.Encrypted {
		encStr = "AES-256 加密"
	}

	fmt.Fprintf(a.Out, "✔ 备份成功完成！(耗时 %s)\n", res.Duration.Round(time.Millisecond))
	fmt.Fprintf(a.Out, "  - 产物文件: %s\n", res.FileName)
	fmt.Fprintf(a.Out, "  - 压缩前大小: %.2f MB\n", float64(res.OriginalSize)/(1024*1024))
	fmt.Fprintf(a.Out, "  - 最终大小: %.2f MB (%s)\n", float64(res.CompressedSize)/(1024*1024), encStr)

	return 0
}

func (a *App) runList(ctx context.Context) int {
	jobs := a.Config.ListJobs()
	if len(jobs) == 0 {
		fmt.Fprintln(a.Out, "当前暂无配置的备份任务。使用 'bakctl add' 开始创建。")
		return 0
	}

	fmt.Fprintf(a.Out, "%-16s %-12s %-24s %-10s %-12s %s\n", "任务名称", "周期", "备份源", "加密", "上次状态", "上次执行时间")
	fmt.Fprintln(a.Out, "------------------------------------------------------------------------------------------------")

	for _, j := range jobs {
		sched := j.Schedule
		if sched == "" {
			sched = "手动"
		}
		enc := "否"
		if j.EncryptKey != "" {
			enc = "AES-256"
		}
		status := j.LastStatus
		if status == "" {
			status = "未运行"
		}
		lastTime := "-"
		if !j.LastRunAt.IsZero() {
			lastTime = j.LastRunAt.Format("01-02 15:04")
		}

		fmt.Fprintf(a.Out, "%-16s %-12s %-24s %-10s %-12s %s\n",
			j.Name,
			sched,
			truncate(displaySource(j.Source), 22),
			enc,
			status,
			lastTime,
		)
	}
	return 0
}

func (a *App) runRemove(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "用法: bakctl rm <任务名>")
		return 1
	}

	jobName := args[0]
	release, err := a.Config.LockJob(jobName)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	defer release()
	latest, err := config.Load(a.Config.Path())
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	job, ok := latest.GetJob(jobName)
	if !ok {
		fmt.Fprintf(a.Err, "未找到任务: %s\n", jobName)
		return 1
	}
	if job.Schedule != "" {
		if err := a.Scheduler.RemoveTimer(ctx, jobName); err != nil {
			fmt.Fprintf(a.Err, "删除定时器失败，任务配置已保留: %v\n", err)
			return 1
		}
	}
	if err := a.Config.Update(func(current *config.Config) error {
		if j, ok := current.Jobs[jobName]; ok && !j.CreatedAt.Equal(job.CreatedAt) {
			return fmt.Errorf("任务已被其他进程修改，请重试")
		}
		delete(current.Jobs, jobName)
		return nil
	}); err != nil {
		fmt.Fprintf(a.Err, "删除任务配置失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.Out, "✔ 备份任务 %s 及定时器已删除（已有备份文件保留）\n", jobName)
	return 0
}

func (a *App) runRestore(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "用法: bakctl restore <任务名> --to <本地恢复路径> [--key <密码>]")
		return 1
	}

	jobName := args[0]
	release, err := a.Config.LockJob(jobName)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	defer release()
	latestConfig, err := config.Load(a.Config.Path())
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	a.Config = latestConfig
	job, ok := a.Config.GetJob(jobName)
	if !ok {
		fmt.Fprintf(a.Err, "未找到任务: %s\n", jobName)
		return 1
	}

	targetName := strings.Split(job.Target, ":")[0]
	target, ok := a.Config.GetTarget(targetName)
	if !ok {
		fmt.Fprintf(a.Err, "未找到存储目标: %s\n", targetName)
		return 1
	}

	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	dest := fs.String("to", "", "本地恢复路径，必须尚不存在")
	fs.StringVar(dest, "dest", "", "--to 的兼容别名")
	legacyType := fs.String("legacy-type", "", "旧版备份类型：file、directory 或 raw")
	key := fs.String("key", job.EncryptKey, "解密密码")

	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	if *dest == "" {
		fmt.Fprintln(a.Err, "❌ 错误: 必须指定恢复路径 (--to)")
		return 1
	}

	client, err := storage.NewClient(target)
	if err != nil {
		fmt.Fprintf(a.Err, "连接存储失败: %v\n", err)
		return 1
	}

	files, err := backup.ListJobBackups(ctx, client, job)
	if err != nil || len(files) == 0 {
		fmt.Fprintf(a.Err, "❌ 远端未找到该任务的任何备份文件: %v\n", err)
		return 1
	}

	// Pick latest
	latest := files[len(files)-1].Path
	fmt.Fprintf(a.Out, "==> 正在从远端拉取最新备份: %s ...\n", latest)

	absDest, err := filepath.Abs(*dest)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if *legacyType != "" {
		err = a.Engine.RestoreWithType(ctx, client, latest, *key, absDest, *legacyType)
	} else {
		err = a.Engine.RestoreForSource(ctx, client, latest, *key, absDest, job.Source)
	}
	if err != nil {
		fmt.Fprintf(a.Err, "❌ 恢复失败: %v\n", err)
		return 1
	}

	fmt.Fprintf(a.Out, "✔ 数据已成功解密恢复至: %s\n", absDest)
	return 0
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func validateTarget(target *config.StorageTarget) error {
	if err := config.ValidateName(target.Name); err != nil {
		return err
	}
	switch target.Type {
	case "local":
		if target.Path == "" {
			return fmt.Errorf("本地目标必须指定 --path")
		}
		abs, err := filepath.Abs(target.Path)
		if err != nil {
			return err
		}
		target.Path = abs
	case "s3":
		if target.Bucket == "" || target.AccessKey == "" || target.SecretKey == "" {
			return fmt.Errorf("S3 目标必须指定 --bucket、--access-key 和 --secret-key")
		}
	default:
		return fmt.Errorf("只支持 local 和 s3")
	}
	_, err := storage.NewClient(target)
	return err
}

func normalizeSource(source string) (string, error) {
	if strings.HasPrefix(source, "sqlite://") {
		p := strings.TrimPrefix(source, "sqlite://")
		if p == "" {
			return "", fmt.Errorf("SQLite 路径不能为空")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		return "sqlite://" + abs, nil
	}
	if strings.HasPrefix(source, "pg://") {
		source = "postgres://" + strings.TrimPrefix(source, "pg://")
	}
	if strings.HasPrefix(source, "postgres://") || strings.HasPrefix(source, "postgresql://") {
		parsed, err := url.Parse(source)
		if err != nil || parsed.Host == "" {
			return "", fmt.Errorf("PostgreSQL URI 无效")
		}
		return source, nil
	}
	if strings.Contains(source, "://") {
		return "", fmt.Errorf("不支持的备份源类型（支持普通路径、sqlite:// 和 postgres://）")
	}
	return filepath.Abs(source)
}

func normalizeDestination(destination string) (string, string, error) {
	parts := strings.SplitN(destination, ":", 2)
	if err := config.ValidateName(parts[0]); err != nil {
		return "", "", err
	}
	prefix := ""
	if len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	if prefix != "" {
		if strings.ContainsAny(prefix, "\\\r\n\x00") || path.Clean(prefix) != prefix {
			return "", "", fmt.Errorf("目标前缀必须为相对路径且不能包含 .. 或控制字符")
		}
		for _, part := range strings.Split(prefix, "/") {
			if part == ".." || part == "." {
				return "", "", fmt.Errorf("目标前缀不能包含 . 或 ..")
			}
		}
		return parts[0], parts[0] + ":" + prefix, nil
	}
	return parts[0], parts[0], nil
}

func displaySource(source string) string {
	u, err := url.Parse(source)
	if err != nil {
		return source
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}
	query := u.Query()
	for key := range query {
		if strings.EqualFold(key, "password") {
			query.Set(key, "REDACTED")
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}
