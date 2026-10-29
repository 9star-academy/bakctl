# bakctl

轻量的服务器备份命令行工具：备份普通文件、目录、SQLite 和 PostgreSQL，使用 gzip 压缩、可选 AES-256-GCM 加密，保存到本地目录或 S3 兼容对象存储，并通过 Linux systemd timer 定时执行。

## 当前支持范围

| 能力 | 说明 |
| --- | --- |
| 文件与目录 | 恢复原文件内容或完整目录结构；支持归档内的相对符号链接，拒绝指向归档外的链接及特殊设备文件 |
| SQLite | 调用 `sqlite3` 在线 `.backup`，包含已提交的 WAL 数据；缺少工具或热备失败时明确失败，不复制运行中的主库降级 |
| PostgreSQL | 调用 `pg_dump` 输出 SQL；恢复命令下载并还原 SQL 文件，导入数据库需另行执行 `psql` |
| 存储 | 本地目录、AWS S3、Cloudflare R2 及兼容 S3 SigV4 的服务；其他厂商需自行验证兼容性 |
| 加密 | 新备份使用带随机盐的 PBKDF2-SHA256 与分块 AES-256-GCM；校验错误密码、篡改和截断 |
| 保留策略 | `--keep N` 按任务保留最新 N 份；`0` 表示不自动删除；清理失败会返回非零 |
| 调度 | Linux systemd，支持 `day@03:00`、`daily`、`6h`、`30m`、`7d` 等；macOS 可手动备份和恢复 |

**尚未实现：MySQL/MariaDB、WebDAV、zstd、按天/周保留、Telegram/飞书/企业微信/Bark/Webhook 通知。** `--webhook` 不会静默接受无效配置。Go 程序没有第三方模块依赖，但数据库备份需要上述命令行工具，定时执行需要 Linux systemd。

若旧配置包含 `webhook_url`，需移除该字段后运行；当前版本会明确报错，不会报告并未发送的通知已成功。

## 安装

当前验收版本：**v1.1.0-rc.1**。这是预发布版，请显式指定版本安装（需要 curl、sha256sum 或 macOS 的 shasum）：

```bash
curl -fsSL https://raw.githubusercontent.com/9star-academy/bakctl/v1.1.0-rc.1/scripts/install.sh -o /tmp/bakctl-install.sh
sudo env BAKCTL_VERSION=v1.1.0-rc.1 bash /tmp/bakctl-install.sh
bakctl --version
```

安装脚本下载 GitHub Release，并校验该版本的 `SHA256SUMS` 后原子替换程序。网络或校验失败会保留已安装版本，不会退回某个硬编码旧版本。旧版 Release 没有校验和时安装会明确失败，请从当前源码构建或等待包含校验和的新发布。

预期输出 `bakctl version v1.1.0-rc.1`。不指定 `BAKCTL_VERSION` 时脚本查询最新正式 Release，不会选择 RC；完整说明见 [发布说明](docs/releases/v1.1.0-rc.1.md)。

**安装脚本不会获得尚未发布的源码改动。** 也可使用 Go 1.24 或更新版本从源码构建：

```bash
git clone https://github.com/9star-academy/bakctl.git
cd bakctl
go test ./...
go build -o bakctl ./cmd/bakctl
sudo install -m 0755 bakctl /usr/local/bin/bakctl
```

可设置 `BAKCTL_VERSION=vX.Y.Z` 固定已发布版本，或使用 `BAKCTL_VERSION=v1.1.0-rc.1 BAKCTL_INSTALL_DIR="$HOME/.local/bin" bash /tmp/bakctl-install.sh` 将本验收版安装到个人目录。

## 快速开始：本地目录备份

系统级任务的所有命令统一使用 `sudo`，以免混用普通用户和 root 的配置。

```bash
# 1. 选择位于备份源以外的目录；异盘或远端存储能覆盖本机磁盘故障场景
sudo bakctl target add local-disk --type local --path /var/backups/bakctl

# 2. 每天 03:00 备份，保留 14 份；不传 --every 即为手动任务
sudo bakctl add uploads \
  --src /var/www/uploads \
  --to local-disk:uploads \
  --every 'day@03:00' \
  --keep 14 \
  --encrypt-key '<请替换为高强度备份密码>'

# 3. 立即运行并查看结果
sudo bakctl run uploads
sudo bakctl list

# 4. 恢复到一个尚不存在的新目录，避免覆盖线上文件
sudo bakctl restore uploads --to /var/tmp/uploads-restored

# 5. 比较源目录和恢复结果；也应确认权限、符号链接和业务可读性
sudo diff -r /var/www/uploads /var/tmp/uploads-restored
```

单文件任务的 `--to` 恢复参数是文件路径；目录任务则是目录路径。`--dest` 保留为 `--to` 的兼容别名。已存在的目标，包括空目录或符号链接，都会被拒绝。数据先在私有临时位置解密、校验和解包，再发布恢复结果；出错不会用半份内容覆盖原数据。

## S3 / Cloudflare R2

```bash
# Cloudflare R2；请替换所有占位值
sudo bakctl target add my-r2 \
  --type s3 \
  --endpoint 'https://<account-id>.r2.cloudflarestorage.com' \
  --bucket backups \
  --access-key '<access-key-id>' \
  --secret-key '<secret-access-key>' \
  --region auto

# AWS S3 请指定 bucket 所在区域的 endpoint 和 region
sudo bakctl target add my-s3 \
  --type s3 \
  --endpoint 'https://s3.ap-southeast-1.amazonaws.com' \
  --bucket '<bucket-name>' \
  --access-key '<access-key-id>' \
  --secret-key '<secret-access-key>' \
  --region ap-southeast-1
```

目标存储需允许相应对象前缀的上传、下载、列举和删除操作；使用 `--keep 0` 时不会清理对象。列表支持分页，恢复和清理仅选择当前任务的完整备份文件名，`app` 不会匹配 `app_db`。

任务和目标名称需为 1–128 位字母、数字、点、下划线或连字符，并以字母或数字开头。同名 target 和 job 会被拒绝，避免意外替换备份位置或密钥。删除任务不会删除已经存储的备份；重新配置时保留原目标、任务名、前缀和密码，才能继续发现并恢复原备份。仍被任务引用的 target 不能删除。

## SQLite 与 PostgreSQL

安装适合服务器的数据库客户端。例如 Debian/Ubuntu：

```bash
sudo apt-get install sqlite3 postgresql-client
```

PostgreSQL 的 `pg_dump` 主版本应与服务端兼容。数据库连接权限和认证由数据库工具处理。

```bash
sudo bakctl add app-db \
  --src sqlite:///var/lib/myapp/data.db \
  --to my-r2:sqlite \
  --every '6h' --keep 14 \
  --encrypt-key '<备份密码>'
sudo bakctl run app-db
sudo bakctl restore app-db --to /var/tmp/app-restored.db
sudo sqlite3 /var/tmp/app-restored.db 'PRAGMA integrity_check;'

sudo bakctl add pg-db \
  --src 'postgres://backup@127.0.0.1:5432/app' \
  --to my-r2:postgres --every 'day@03:00' --keep 14
sudo bakctl run pg-db
sudo bakctl restore pg-db --to /var/tmp/app-restored.sql

# 在专门的测试数据库验收；bakctl 不会自动导入或覆盖生产数据库
sudo cat /var/tmp/app-restored.sql | psql --dbname=restore_test --set=ON_ERROR_STOP=1
```

`pg://` 是 `postgres://` 的兼容别名，也支持 `postgresql://`。建议使用受保护的 `.pgpass` 或 PostgreSQL 自身认证配置，避免将数据库密码放到命令行。系统 timer 使用系统服务的用户与环境，需保证该环境也能访问客户端和凭据。

## 配置、调度与失败排查

- root 默认配置：`/etc/bakctl/config.json`。
- 普通用户默认配置：`~/.config/bakctl/config.json`。
- 可通过 `BAKCTL_CONFIG=/absolute/path/config.json` 指定配置。自定义配置的 systemd unit 会记录绝对路径，避免定时运行时读错配置。
- 配置文件以 `0600` 写入，包含存储凭据和备份密码，必须保护该文件及其父目录。命令行密码也可能进入 shell 历史。请另外离线保管解密密码。
- 配置修改使用进程锁、重新加载与原子写入。相同配置中的同一任务不能同时备份、恢复或删除；手动命令冲突时会报错，不同任务可以并行。systemd 以 `run <任务名> --wait` 等待任务锁，避免启用 timer 或持久化补跑时与配置操作冲突；手动运行也可使用此参数。不要让多个独立主机/配置同时写同一个存储前缀和任务名。
- systemd unit 名称为 `bakctl-<任务名>.service/.timer`，在系统范围共享；不同自定义配置不能接管同名 unit。

```bash
systemctl list-timers 'bakctl-*'
systemctl status bakctl-uploads.timer
journalctl -u bakctl-uploads.service --since today
sudo bakctl rm uploads
sudo bakctl target list
```

配置写入、上传、保留清理或定时器安装失败都会返回非零退出码。定时器安装失败时，任务配置仍保留便于排查；修复权限或 systemd 环境后，删除该任务再重新添加。不要只看“配置已保存”就认为 timer 已启用，应确认 `systemctl list-timers` 中的下一次执行时间。

`daily` / `day` 等价于每天 03:00；`day@HH:MM` 使用服务器本地时区。间隔形式支持正整数 `s/m/h/d`，首次在启用约 5 分钟后触发（`OnActiveSec=5m`），其后以上次服务启动时间为基准计算间隔；systemd 精度设置可能带来小幅延迟。任意原始 `OnCalendar` 表达式目前不支持。

## 旧版备份兼容与恢复验收

当前备份包含格式版本和源类型，新版能够明确区分单文件、目录和数据库输出，并能读取 v1 的 gzip/AES-GCM 备份。**旧版程序无法读取新格式**，升级后请保留新版二进制用于恢复。

v1 tar 没有类型信息。原始路径还存在时，任务配置可提供类型提示；灾难恢复时原路径已丢失且无法确定类型，必须显式指定，程序不会猜测：

```bash
sudo bakctl restore old-file --to /var/tmp/file-restored --legacy-type file
sudo bakctl restore old-directory --to /var/tmp/dir-restored --legacy-type directory
sudo bakctl restore old-db --to /var/tmp/db-restored --legacy-type raw
```

新格式的备份、压缩、加解密和上传采用流式处理及私有临时文件，避免把整个数据集装入内存；临时磁盘必须有足够空间容纳数据和中间产物。读取 v1 整块加密文件仍需要与该旧文件大小相当的内存。

普通目录不是文件系统快照：对持续变化的数据，工具会检测部分文件变化并报错，但不能保证多文件业务事务的一致性。数据库应使用对应的在线备份方式；需要目录级一致性时先暂停写入或使用文件系统快照。不要把本地备份目标或 `TMPDIR` 放进备份源内；例如备份 `/tmp` 时需要指定源目录之外的临时目录。绝对、循环或越出目录边界的符号链接会被拒绝。旧配置中的相对源路径需重新配置为绝对路径。

上线前至少完成一次恢复演练：文件校验内容，目录比较结构和链接，SQLite 检查 `integrity_check` 与关键业务行数，PostgreSQL 导入隔离数据库并查询关键数据。仅看到上传成功不能代替恢复验收。

## 开发验证

```bash
go test -race -coverprofile=coverage.out ./...
go vet ./...
go tool cover -func=coverage.out
bash -n scripts/install.sh
```

测试涵盖文件/目录往返、SQLite WAL、旧格式兼容、错误密码与损坏/截断拒绝、路径穿越、任务隔离、同秒重复执行、配置并发、S3 官方签名向量和分页、systemd 失败传播及安装校验失败。SQLite 集成测试在缺少 `sqlite3` 时跳过，CI 会安装并检查该工具。

S3 测试使用隔离的 HTTP transport，systemd 测试使用临时目录与替代执行器。这些测试不代表已在你的云账号或实际 Linux 服务管理器上通过验收，部署时还应完成真实上传、下载、清理和 timer 触发检查。

本次修复的验证记录和部署验收步骤见 [docs/testing.md](docs/testing.md)。

## License

MIT，见 [LICENSE](LICENSE)。
