# vaultwarden-less

在一台 VPS 上运行 Vaultwarden，供 1–3 人各自使用个人 vault。每次改动个人条目，备份进程会检查 SQLite 数据库，把一致副本分别写入配置好的 restic 仓库。只登录或同步时，个人库内容相同就沿用原快照。

要在 VPS 之外留两份独立副本，可以选两家提供 SLA 的对象存储，分开地域、账号和仓库密码。一家暂时不可用时，其余仓库继续备份。每个仓库都能单独恢复，快照里只有一份完整的 `/db.sqlite3`。还原数据库后，每个人用自己的客户端解锁并核对条目。

默认关闭公开注册、组织创建、附件上传和 Send 的创建与编辑。已有附件和文件型 Send 的文件在数据库之外，需要另行保存。[备份范围](docs/contracts.md#什么算个人-vault-变化)

| 要做的事 | 手册 |
| --- | --- |
| 安装、导入已有 SQLite、开通账号、逐个接入对象存储 | [部署与首次备份](docs/deployment.md) |
| 填写仓库凭据、设置域名和通知 | [配置](docs/configuration.md) |
| 看最近一次备份、处理故障、校验和清理仓库 | [日常运维](docs/operations.md) |
| 在另一台机器上恢复个人 vault | [从快照恢复](docs/recovery.md) |
| 升级、回退、构建发布包 | [维护与升级](docs/maintenance.md) |
| 审阅触发备份的请求和 SQL 比较范围 | [备份规则](docs/contracts.md) |
