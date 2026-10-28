package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
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
  bakctl target add <名称> [选项]       配置云端存储目标 (Cloudflare R2 / AWS S3 / OSS / 本地)
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
  bakctl target add my-r2 --type s3 --endpoint https://<id>.r2.cloudflarestorage.com \
    --bucket my-backups --access-key <KEY> --secret-key <SECRET>

  # 2. 一行命令添加定时加密备份任务 (每天凌晨 3 点热备 SQLite，保留 14 份)
  sudo bakctl add app-db --src sqlite:///var/lib/app.db --to my-r2:dbs/ \
    --every "day@03:00" --keep 14 --encrypt-key "mypassword"

  # 3. 立即运行与查看
  sudo bakctl run app-db
  bakctl list

  # 4. 从远端拉取解密恢复
  bakctl restore app-db --to ./restored.db --key "mypassword"
`

type App struct {
	Version   string
	Out       io.Writer
	Err       io.Writer
	Config    *config.Config
	Engine    *backup.Engine
	Scheduler *scheduler.SystemdScheduler
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
		region := fs.String("region", "auto", "S3 Region")
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

		if err := a.Config.AddTarget(target); err != nil {
			fmt.Fprintf(a.Err, "添加目标失败: %v\n", err)
			return 1
		}
		_ = a.Config.Save()
		fmt.Fprintf(a.Out, "✔ 存储目标 %s 添加成功 (%s)\n", name, *tType)
		return 0

	case "rm":
		if len(args) < 2 {
			fmt.Fprintln(a.Err, "用法: bakctl target rm <名称>")
			return 1
		}
		delete(a.Config.Targets, args[1])
		_ = a.Config.Save()
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

	targetName := strings.Split(*to, ":")[0]
	if _, ok := a.Config.GetTarget(targetName); !ok {
		fmt.Fprintf(a.Err, "❌ 错误: 未找到存储目标 '%s'。请先通过 'bakctl target add %s' 配置\n", targetName, targetName)
		return 1
	}

	job := &config.BackupJob{
		Name:       jobName,
		Source:     *src,
		Target:     *to,
		Schedule:   *every,
		Keep:       *keep,
		EncryptKey: *encryptKey,
		WebhookURL: *webhook,
	}

	if err := a.Config.AddJob(job); err != nil {
		fmt.Fprintf(a.Err, "添加任务失败: %v\n", err)
		return 1
	}
	_ = a.Config.Save()

	fmt.Fprintf(a.Out, "✔ 备份任务 %s 配置成功\n", jobName)
	fmt.Fprintf(a.Out, "  - 备份源: %s\n", *src)
	fmt.Fprintf(a.Out, "  - 目标端: %s (保留最新 %d 份)\n", *to, *keep)
	if *encryptKey != "" {
		fmt.Fprintln(a.Out, "  - 加密保护: ✔ 已启用 (AES-256-GCM 端到端加密)")
	}

	if *every != "" {
		if err := a.Scheduler.InstallTimer(ctx, jobName, *every); err != nil {
			fmt.Fprintf(a.Err, "⚠️ 安装 systemd 定时器失败: %v\n", err)
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

	fmt.Fprintf(a.Out, "==> 正在执行备份任务 [%s]...\n", jobName)
	res, err := a.Engine.RunBackup(ctx, job, target)

	job.LastRunAt = time.Now()
	if err != nil {
		job.LastStatus = "FAILED"
		job.LastMessage = err.Error()
		_ = a.Config.Save()
		fmt.Fprintf(a.Err, "❌ 备份失败: %v\n", err)
		return 1
	}

	job.LastStatus = "SUCCESS"
	job.LastSize = res.CompressedSize
	job.LastMessage = "OK"
	_ = a.Config.Save()

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
			truncate(j.Source, 22),
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
	if _, ok := a.Config.Jobs[jobName]; ok {
		delete(a.Config.Jobs, jobName)
		_ = a.Config.Save()
		a.Scheduler.RemoveTimer(ctx, jobName)
		fmt.Fprintf(a.Out, "✔ 备份任务 %s 及定时器已删除\n", jobName)
		return 0
	}

	fmt.Fprintf(a.Err, "未找到任务: %s\n", jobName)
	return 1
}

func (a *App) runRestore(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "用法: bakctl restore <任务名> --to <本地恢复路径> [--key <密码>]")
		return 1
	}

	jobName := args[0]
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
	dest := fs.String("to", "", "本地恢复解压路径")
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

	prefix := ""
	if parts := strings.Split(job.Target, ":"); len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	if prefix != "" {
		prefix = prefix + "/"
	}

	files, err := client.List(ctx, prefix+job.Name+"_")
	if err != nil || len(files) == 0 {
		fmt.Fprintf(a.Err, "❌ 远端未找到该任务的任何备份文件: %v\n", err)
		return 1
	}

	// Pick latest
	latest := files[len(files)-1].Path
	fmt.Fprintf(a.Out, "==> 正在从远端拉取最新备份: %s ...\n", latest)

	absDest, _ := filepath.Abs(*dest)
	if err := a.Engine.Restore(ctx, client, latest, *key, absDest); err != nil {
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
