# bakctl

<p align="center">
  <b>一行命令搞定服务器数据库与目录的定时加密备份</b><br>
  单二进制、零外部依赖，自动打包压缩加密并同步至 Cloudflare R2 / AWS S3 / 阿里云 OSS / WebDAV。
</p>

<p align="center">
  <a href="https://github.com/9star-academy/bakctl/releases"><img src="https://img.shields.io/github/v/release/9star-academy/bakctl?style=flat-square" alt="Release"></a>
  <a href="https://github.com/9star-academy/bakctl/blob/main/LICENSE"><img src="https://img.shields.io/github/license/9star-academy/bakctl?style=flat-square" alt="License"></a>
</p>

---

## 为什么做 bakctl？

独立开发者和小团队在 VPS 裸机上跑 SQLite、PostgreSQL、MySQL 或业务附件目录时，最怕机器失联或误删数据；但部署一套重量级备份中心又过于繁琐，手写 `mysqldump + tar + rclone + crontab` 脚本不仅容易出错，还常常在备份悄悄失败几个月后才被发现。

**`bakctl` 让你用一行命令注册好“热备 + 压缩 + 加密 + 远端上传 + 过期清理 + 失败通知”的完整流水线。**

## 核心特性

- **数据库安全热备**：原生支持 SQLite 在线热备（避免锁表损坏）、PostgreSQL (`pg_dump`) 与 MySQL/MariaDB 一致性快照。
- **高压缩与端到端加密**：内置 `zstd` 高速压缩与 AES-256-GCM 加密，即使网盘或对象存储泄露也无法解密数据。
- **多存储后端支持**：开箱支持 S3 兼容协议（Cloudflare R2、AWS S3、阿里云 OSS、腾讯云 COS、MinIO）及 WebDAV。
- **Systemd Timer 驱动**：复用 `systemd` 原生定时调度，无需拼写晦涩的 Crontab，日志可观测、状态可追踪。
- **自动生命周期与告警**：内置保留策略（如只保留最近 7 天 / 4 周备份），支持 Telegram、飞书、企业微信、Bark 备份结果推送。

## 快速开始

### 1. 安装

```bash
curl -fsSL https://raw.githubusercontent.com/9star-academy/bakctl/main/install.sh | sudo bash
```

### 2. 常用命令示例

```bash
# 1. 绑定远端存储目标（以 Cloudflare R2 / S3 为例）
bakctl target add my-r2 --type s3 --endpoint https://<id>.r2.cloudflarestorage.com --bucket backups

# 2. 一行命令添加定时备份任务（每天凌晨 3 点热备 SQLite 并保留最近 14 份）
sudo bakctl add app-db \
  --src sqlite:///var/lib/myapp/data.db \
  --to my-r2:myapp/ \
  --every "day@03:00" \
  --keep 14 \
  --encrypt-key "your-secret-passphrase"

# 3. 备份普通配置或附件目录
sudo bakctl add app-uploads --src /var/www/uploads --to my-r2:uploads/ --every "6h" --keep 7

# 4. 立即手动触发一次备份并查看进度
sudo bakctl run app-db

# 5. 查看所有备份任务状态与最近一次执行结果
bakctl list

# 6. 从远端存储一键拉取并解密恢复最新备份
bakctl restore app-db --dest ./data-restored.db
```

## 9star-academy 裸机轻量运维矩阵

| 工具 | 定位 |
| :--- | :--- |
| [**ShellReady**](https://github.com/9star-academy/ShellReady) | 一条命令配好新 Linux 服务器（BBR、trzsz、Starship、Atuin、zoxide） |
| [**secctl**](https://github.com/9star-academy/secctl) | 一行命令搞定裸机 SSH 加固、防火墙管理与 Docker 端口防泄漏 |
| [**servctl**](https://github.com/9star-academy/servctl) | 一行命令将任意可执行程序托管为 `systemd` 系统自启服务 |
| [**proxctl**](https://github.com/9star-academy/proxctl) | 一行命令搞定反向代理、静态站点托管与自动 HTTPS 证书 |
| [**bakctl**](https://github.com/9star-academy/bakctl) | 一行命令将数据库与目录定时加密备份至 S3 / R2 / OSS / WebDAV |

## License

MIT License
