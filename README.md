# vaultwarden-less

一台 VPS 可供 1–3 人各自使用独立的 Vaultwarden 账号和个人密码库。restic 将数据库分别备份到不同供应商、不同地域的对象存储，每个仓库都能独立恢复这些个人密码库。发布包包含 Linux amd64 的官方 Vaultwarden 1.37.5 镜像、请求代理和备份镜像，VPS 使用 Docker Compose 离线安装。

```text
官方 Bitwarden 客户端 → HTTPS 入口 → 本机代理 → Vaultwarden → SQLite
                                      ↓ 请求提示              ↓ 一致副本
                                      备份进程 ──────────┬─ restic 仓库 A
                                                         └─ restic 仓库 B
```

任一账号的个人条目、文件夹、收藏、归档或解密材料发生变化时，每个可用仓库写入一个原生快照。每份快照保存实例在该时刻的完整原生数据库 `/db.sqlite3`。恢复后，每位使用者用自己的主密码解锁并核对自己的条目。仓库密码和对象存储恢复凭据由管理员单独保管。

默认配置关闭组织创建、附件上传和 Send 的创建与编辑。已有条目附件和文件型 Send 的文件存放在数据库之外，单独恢复数据库无法取回这些文件。[备份范围](docs/contracts.md#什么算个人-vault-变化)

| 要做的事 | 入口 |
| --- | --- |
| 在新主机部署，准备两个独立仓库 | [部署](docs/deployment.md) |
| 配置凭据、直发通知和规则 | [配置](docs/configuration.md) |
| 看保护情况、恢复、校验和保留快照 | [日常与恢复](docs/operations.md) |
| 审计哪些请求和数据会触发备份 | [备份规则](docs/contracts.md) |
| 升级 Vaultwarden、restic 或项目本身 | [维护与升级](docs/maintenance.md) |
| 从 1.37.3 的旧部署归档首次迁移 | [一次性迁移](docs/migration-41d0e92.md) |
