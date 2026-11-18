# 配置

| 部署文件 | 内容 | 应用方式 |
| --- | --- | --- |
| `compose.yml`、`compose.env` | 镜像、共享 UID:GID、宿主回环端口和路径 | `docker compose --env-file compose.env up -d` |
| `config/vaultwarden.env` | Vaultwarden 的环境变量，含外部 HTTPS DOMAIN、注册及邮件设置 | `docker compose --env-file compose.env up -d --force-recreate vaultwarden` |
| `config/backup.yaml` | 目标名称、凭据路径、后端选项、通知 | 重建 backup，确保文件替换后重新绑定 |
| `credentials/backup/*.env` | 每个目标单独的 restic 运行环境 | 重启 backup |
| `credentials/restic/*-repository-password.txt` | 各仓库的原始密码 | 下次 restic 命令读取；换密钥使用原生 `restic key` |
| `rules/routes.txt`、`rules/personal-vault.sql` | 请求筛选和个人数据范围 | 分别重启 proxy、backup。规则目录整体只读挂载 |

Vaultwarden 读取 `config/vaultwarden.env`，网页 vault 和管理面板默认关闭。Compose 固定数据路径，并将 `CONFIG_FILE` 指向只读根文件系统上一个不存在的文件。接管旧目录时先审查旧 `data/config.json`，把需要的配置转换为环境变量，从工作数据目录移走该 JSON。官方健康检查会单独读取这里的 DOMAIN。[Vaultwarden 配置模板](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/.env.template)

## 个人账号与使用权限

| 模板设置 | Vaultwarden 1.37.5 的行为 |
| --- | --- |
| `SIGNUPS_ALLOWED=false`、`SIGNUPS_DOMAINS_WHITELIST=` | 关闭公开注册，已开通的个人账号照常登录。 |
| `ORG_CREATION_USERS=none`、`INVITATIONS_ALLOWED=false` | 禁止创建组织，也禁止通过组织邀请新建账号。已有组织及成员关系继续有效，组织管理员仍可邀请本实例已注册的用户。 |
| `USER_ATTACHMENT_LIMIT=0`、`ORG_ATTACHMENT_LIMIT=0` | 拒绝向个人条目和组织条目上传附件。原有附件文件仍在时，有权限的用户可以下载。 |
| `SENDS_ALLOWED=false` | 拒绝新建或编辑文本型和文件型 Send。已有分享链接按原有条件生效，所有者可以删除对应的 Send。 |

这些设置由 Vaultwarden 在服务端执行。账号开通步骤见[部署指南](deployment.md#开通个人账号)。[组织邀请](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/api/core/organizations.rs#L1033-L1069) · [附件限额](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/api/core/ciphers.rs#L1241-L1258) · [Send 权限](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/api/core/sends.rs#L93-L109)

## 备份目标与凭据

每个目标的 `env_file` 使用 Docker env-file 格式，逐行写入字面量 `NAME=value`。仓库密码文件只放密码本身。维护凭据位于 `credentials/maintenance/`，单独提供给手工运行的 restic 容器。Compose 使用另一套插值规则，含美元符号的字面值可用单引号包住。配置、凭据和缓存应置于私有目录，真实值只存放在部署端。

`backup.yaml` 根键为 `targets`、`notifications`。每个目标需要唯一的 `name`、容器内的 `env_file`，可带字符串列表 `options`，每项传给 restic 的 `--option`。一个目标的配置错误单独显示，其他有效目标继续备份。`validate` 在本地检查配置、凭据文件、路由格式和 SQL 规则可读性。启动后的第一次一致副本检查 SQL 与数据库的实际兼容性。

## 直接通知

在 `backup.yaml` 的 `notifications.channels` 中启用模板所示的渠道。企业微信使用 webhook，钉钉和飞书支持签名密钥，Bark 使用 APNs 设备 token、P-256 PKCS#8 密钥、Key ID、Team ID 和 topic 发往 Apple。所有通道默认关闭。每个通道只接收目标名称、结果、时间和错误类别。

首次故障发送 Fault，确认恢复发送 Recovery。未恢复的问题默认每 6 小时发送 Reminder，每日当地时间 21:00 发送 Daily，默认时区 Asia/Shanghai。每个目标分别显示最近比较时间与快照时间，长期未更改的个人库可以持续受保护。每个通道独立合并待发消息，一轮最多三次尝试，遵循服务商的重试时间。通知报告与待投递内容保存在 `state/`，重启后继续。整个 VPS 停机时，机内进程无法发送通知；缺失日报可以从接收端发现。
