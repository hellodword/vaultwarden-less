# vaultwarden-less

改完一个密码，代理会从客户端请求中识别可能的写入，提醒备份进程尽快检查。备份进程取得一致的 SQLite 副本，确认个人库内容变化后，把完整数据库分别存入两家不同地域、提供 SLA 的对象存储。一个仓库暂时不可用时，另一个仍能保存新密码。

一台 VPS 可供 1–3 人各自使用独立的个人账号。Linux amd64 发布包包含官方 Vaultwarden 1.37.5、请求代理和备份镜像，通过 Docker Compose 离线部署。

```text
官方 Bitwarden 客户端 → HTTPS 入口 → 本机代理 → Vaultwarden → SQLite
                                      ↓ 请求提示              ↓ 一致副本
                                      备份进程 ──────────┬─ restic 仓库 A
                                                         └─ restic 仓库 B
```

每个 restic 仓库都能单独恢复，每份快照只含 `/db.sqlite3`。平时只登录或同步，个人库相同时沿用原快照。恢复后，每位使用者用自己的主密码解锁并核对自己的条目。仓库密码和对象存储恢复凭据由管理员单独保管。

默认配置关闭组织创建、附件上传和 Send 的创建与编辑。已有条目附件和文件型 Send 的文件存放在数据库之外，单独恢复数据库无法取回这些文件。[备份范围](docs/contracts.md#什么算个人-vault-变化)

| 要做的事 | 入口 |
| --- | --- |
| 在新主机部署，准备两个独立仓库 | [部署](docs/deployment.md) |
| 配置凭据、直发通知和规则 | [配置](docs/configuration.md) |
| 看保护情况、恢复、校验和保留快照 | [日常与恢复](docs/operations.md) |
| 审计哪些请求和数据会触发备份 | [备份规则](docs/contracts.md) |
| 升级 Vaultwarden、restic 或项目本身 | [维护与升级](docs/maintenance.md) |
| 接管已有数据目录或 SQLite 归档 | [接管已有 SQLite](docs/deployment.md#接管已有-sqlite) |
