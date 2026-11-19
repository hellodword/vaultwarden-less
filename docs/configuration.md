# 配置

在部署目录操作。服务以 `1000:1000` 运行，读取的配置和在线凭据用 `0600`，其所在目录用 `0700`。

| 文件 | 用途 | 修改后生效 |
| --- | --- | --- |
| `compose.yml` | 镜像、挂载、宿主机监听端口 | `docker compose up -d --wait` |
| `config/vaultwarden.env` | 域名、注册和邮件等 Vaultwarden 设置 | `docker compose up -d --no-deps --force-recreate --wait vaultwarden` |
| `config/backup.yaml` | 仓库列表、restic 后端选项、通知 | `docker compose up -d --no-deps --force-recreate --wait backup` |
| `credentials/backup/` | 每仓一份在线凭据，可另放 Bark 的 `.p8` 文件 | `docker compose restart backup` |
| `credentials/maintenance/` | 可选的维护凭据，只交给手动运行的 restic 容器 | 下次手工命令读取 |
| `rules/routes.txt`、`rules/personal-vault.sql` | 请求筛选、个人库内容比较 | 分别执行 `docker compose restart proxy`、`docker compose restart backup` |

`backup.yaml` 是单文件挂载。重建容器可以读到编辑器通过重命名保存的新文件。凭据和规则按目录挂载，重启相应进程即可重新读取。检查配置用 `docker compose run --rm --no-deps backup validate`，首次备份还会在真实数据库的一致副本上执行 SQL 规则。[仓库接入步骤](deployment.md#接入一个仓库)

## 对象存储凭据

`templates/credentials/backup/s3.env.example` 适用于 S3 接口，每个仓库填一份。`RESTIC_REPOSITORY` 写成 `s3:https://端点/bucket/前缀`，`RESTIC_PASSWORD` 用于解密仓库，`AWS_ACCESS_KEY_ID` 和 `AWS_SECRET_ACCESS_KEY` 用于访问对象存储。恢复数据库后，每个人仍用自己的主密码解锁个人 vault。

> **Docker `--env-file` 会把引号当成值的一部分。** 写 `RESTIC_REPOSITORY=s3:https://…`。如果写成 `RESTIC_REPOSITORY="s3:https://…"`，restic 读到的第一个字符就是双引号，可能报 `invalid backend`。密码和访问密钥同样按字面填写，`$` 也会原样保留。backup 读取目标的 `env_file` 时使用相同的解析规则。[Docker env-file 语法](https://pkg.go.dev/github.com/docker/cli/pkg/kvfile)

| 存储 | 端点从哪里取 |
| --- | --- |
| Cloudflare R2 | 在账户中复制 bucket 的 S3 API 端点。通常为 `<account-id>.r2.cloudflarestorage.com`。使用该 bucket 的 S3 访问密钥。[R2 S3 API](https://developers.cloudflare.com/r2/api/s3/api/) |
| Backblaze B2 | 使用 bucket 所在区域显示的 S3 端点，如 `s3.us-west-002.backblazeb2.com`。keyID 和 applicationKey 分别填入两个 AWS 凭据字段。[B2 端点和访问密钥](https://www.backblaze.com/docs/en/cloud-storage-get-started-with-a-backblaze-integration) |
| Amazon S3 | 使用 bucket 所在区域的 S3 端点，如 `s3.us-east-1.amazonaws.com`，为备份主体配置该前缀的权限。[S3 区域端点](https://docs.aws.amazon.com/general/latest/gr/s3.html) |

服务商要求显式指定签名区域时，可在凭据文件增加 `AWS_DEFAULT_REGION`。每个目标也可以在 `backup.yaml` 中设 `options`，例如 `options: [s3.bucket-lookup=path]`。手工命令使用同一份 env 文件，并带上对应的 `-o` 选项。[restic S3 配置](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#s3-compatible-storage)

在线凭据需要列举、读取和写入仓库，还要能创建和删除 restic 锁。可按供应商的权限模型限制对快照和数据包的删除。要单独保管清理权限时，在部署目录创建私有的 `credentials/maintenance/`，把对应文件命名为 `primary.env` 等。仓库地址和密码与在线文件相同，访问密钥换成维护主体的。管理员保管这个目录，只有手工运行的 restic 才会读它。仓库密码、端点、区域以及可用于恢复的对象存储凭据要另存一份到 VPS 之外。

## 个人账号和外部地址

`config/vaultwarden.env` 是 Compose 服务的 `env_file`。Compose 会处理引号和插值，例如可用单引号包住含 `$` 的 SMTP 密码。`DOMAIN` 可留空。需要 Vaultwarden 生成固定的外部链接时，填客户端实际使用的 HTTPS 地址。使用子路径时，公开 URL、反代转发的路径和 Vaultwarden 的挂载路径要一致。宿主机默认只在 `127.0.0.1:8080` 接收反代的连接。[Vaultwarden 配置模板](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/.env.template)

| 默认设置 | 对个人实例的影响 |
| --- | --- |
| `SIGNUPS_ALLOWED=false`、`SIGNUPS_DOMAINS_WHITELIST=` | 仅已开通的账号能登录。需要开户时临时开放注册。[开通账号](deployment.md#启动与开通账号) |
| `ORG_CREATION_USERS=none`、`INVITATIONS_ALLOWED=false` | 禁止创建组织和通过组织邀请开新账号。已有组织仍可邀请已注册的用户。[组织邀请](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/api/core/organizations.rs#L1033-L1069) |
| `USER_ATTACHMENT_LIMIT=0`、`ORG_ATTACHMENT_LIMIT=0` | 拒绝上传个人、组织条目附件。已有文件仍在时，有权限的人可以下载。[附件限额](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/api/core/ciphers.rs#L1241-L1258) |
| `SENDS_ALLOWED=false` | 拒绝创建和编辑 Send。已有链接仍按各自的访问条件生效。[Send 权限](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/api/core/sends.rs#L93-L109) |

网页 vault 和管理面板默认关闭。Compose 通过 `CONFIG_FILE` 固定服务配置来源。官方镜像的健康检查还会读取 `data/config.json` 中的域名。接管旧数据时，把仍需使用的设置写入环境文件，将旧 JSON 留在原数据副本。

## 通知

在 `config/backup.yaml` 中按 `channels: []` 下的注释填写所用渠道。每个渠道起一个独立名称。企业微信填写机器人 webhook，钉钉和飞书可填签名密钥。Bark 直接向 Apple APNs 投递，需要设备 token、P-256 PKCS#8 格式的 `.p8`、Key ID、Team ID 和 topic。容器内的 `/run/credentials/backup/bark-apns-auth-key.p8` 对应部署目录的 `credentials/backup/bark-apns-auth-key.p8`，属主 `1000:1000`、权限 `0600`。

```sh
docker compose run --rm --no-deps backup validate &&
docker compose up -d --no-deps --force-recreate --wait backup
```

首次故障发 Fault，故障解除发 Recovery。默认按 Asia/Shanghai 时区在 21:00 发 Daily，未恢复的故障每 6 小时发 Reminder。可在同一文件调整 `timezone`、`daily_at` 和 `reminder_interval`。报告包含目标、结果、时间和错误类别。每个渠道分别记录投递进度，重启后续发。整台 VPS 离线期间收不到日报。[查看状态和处理故障](operations.md#查看保护状态)
