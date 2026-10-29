# 备份可靠性验证

## 2026-10-29 本地结果

环境：macOS arm64，Go 1.26.2，系统 sqlite3。`go.mod` 保持 Go 1.24.0，无第三方模块依赖；新增标准库 API 已对照 Go 1.24 API 清单，CI 配置使用 Go 1.24.x，远端 CI 尚未运行。

```bash
go test -race -coverprofile=coverage.out ./...
go vet ./...
go tool cover -func=coverage.out
bash -n scripts/install.sh
git diff --check
```

实际结果：全部通过。85 个顶层测试，另含表驱动子用例；总语句覆盖率 85.8%（修复前 33.9%）。

| 包 | 覆盖率 |
| --- | --- |
| internal/backup | 83.2% |
| internal/cli | 91.5% |
| internal/config | 81.6% |
| internal/crypto | 87.7% |
| internal/scheduler | 87.1% |
| internal/storage | 86.6% |

`cmd/bakctl` 的 main 包未计入单元测试覆盖，但编译后的实际 CLI 已另行执行下述完整流程；安装脚本由 `scripts/install_test.go` 通过隔离子进程验证，不计入 Go 语句覆盖率。

## 已完成的行为验证

- 2 MiB 随机文件：加密备份、恢复、逐字节比较一致；错误密钥和已存在的目标均返回非零，未产生错误恢复文件。
- 目录：文件内容、嵌套目录、空目录及相对符号链接恢复一致。
- SQLite：保持源连接开启、WAL 内存在未 checkpoint 的已提交数据；实际 CLI 在线备份后，恢复库 `PRAGMA integrity_check` 返回 `ok`，3 行提交数据完整。
- `app` 与 `app_db` 共用前缀，`app --keep 1` 重复运行后仅清理自身旧备份，另一任务保留；删除任务和目标配置不删除已有对象。
- 配置文件权限为 0600；并行更新不丢任务或状态；旧快照写入被拒绝；被删除或替换的任务不会被执行状态写入复活。
- 旧版 file/directory/raw/AES-GCM 备份可以恢复；类型不明确时需要显式参数，不猜测；旧 tar 文件名与新格式 magic 冲突已纳入回归。
- 损坏、截断、错误密钥、加密头篡改、帧重排、归档路径穿越、重复条目和符号链接链越界均拒绝发布恢复目标。
- 源文件被替换为链接、特殊文件、源包含本地目标或 TMPDIR 等情况明确失败。
- S3 官方签名向量、中文/空格/空路径、1001 对象分页、上传读错误/长度不符/取消等用隔离 HTTP transport 验证。
- systemd 缺失、命令失败、部分安装回滚、跨配置 unit 归属、锁取消与首次启动周期均通过临时目录和替代执行器验证。
- 安装脚本四平台资产选择、SHA-256 校验、失败保留旧程序、目标目录冲突、临时文件清理均通过测试，未写入真实安装目录。

## 构建

已成功交叉编译 Linux amd64/arm64 与 macOS amd64/arm64：

```bash
for os in linux darwin; do
  for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -o "/tmp/bakctl-$os-$arch" ./cmd/bakctl
  done
done
```

Linux 二进制编译通过不代表真实 systemd 已运行。

## 部署前仍需完成

1. 使用测试 bucket/prefix 验证真实 S3/R2 上传、下载、SHA-256/内容比较与 `--keep` 删除权限；当前没有使用真实云凭据。
2. Linux 上确认 `systemctl list-timers 'bakctl-*'` 的下一次触发时间，并在 `journalctl -u bakctl-<job>.service` 查看真实成功执行；间隔 timer 首次在启用约 5 分钟后触发，calendar timer 可能持久化补跑。
3. 用兼容主版本的 `pg_dump` 备份测试 PostgreSQL，再通过 `psql --set=ON_ERROR_STOP=1` 导入隔离数据库并验证数据；本机没有 PostgreSQL 工具或服务，本次验证了命令参数、stdout/stderr 分离和失败传播。
4. 用接近生产容量的数据验证临时磁盘空间与执行时长。新版流式处理；v1 整块密文恢复仍需要相应内存。普通目录不提供文件系统快照一致性。

以上结果记录发布前本地验证，未部署生产环境。v1.1.0-rc.1 使用 GitHub Actions 在 Go 1.24 下再次验证并构建发布文件；安装方法和兼容性说明见 [发布说明](releases/v1.1.0-rc.1.md)。RC 不属于最新正式版本，使用安装脚本时必须显式指定 `BAKCTL_VERSION`。
