# 审计备份规则

项目维护两份可读规则。它们和发布包一起分发，部署时复制到 `rules/`。代理和备份进程分别在启动时读取。变更记录使用普通 Git diff。

## 请求何时提示检查

[routes.txt](../rules/routes.txt) 每行包含 HTTP 方法、一个空格和绝对路径。以 `#` 开始的内容是审计注释。`{id}` 匹配一个非空路径段，匹配区分大小写，不跨斜杠。代理按转义后的路径匹配，忽略查询串，编码路径、点段和重复斜杠都进入检查。

名单中的成功请求可跳过检查，例如 `GET /api/sync`。其余请求在开始与结束时各发提示，名单中的请求失败或中断也发提示。令牌端点 `POST /identity/connect/token` 可能随客户端流程设置密钥，保留检查。代理在内存合并信号，投递失败会重试。备份进程在请求安静 30 秒后检查，连续请求最长等待 2 分钟。启动时立即检查，之后每 6 小时从源库重新检查，覆盖停机、提示丢失以及绕过代理的写入。

## 什么算个人 vault 变化

[personal-vault.sql](../rules/personal-vault.sql) 是可直接交给 SQLite 的 SQL。输入是只读数据库别名 `previous` 和 `current`，输出一行一列，`0` 表示相同，`1` 表示变化。它对两个方向做集合差，能识别新增、编辑、删除和还原。

| 数据 | 比较内容 |
| --- | --- |
| 个人条目 | 所有人、创建时间、类型、密钥、名称、字段、登录或其他加密内容、密码历史、回收站状态和重新提示 |
| 个人整理 | 文件夹与名称、条目所属文件夹、收藏、归档时间。关联须属于同一用户的个人条目 |
| 账号解密 | 账号 ID、邮箱、受保护账户密钥、密钥 ID、公私钥、客户端 KDF 类型及参数 |

规则比较实例中所有账号的个人数据。仅改变设备、登录会话、服务端密码验证字段、2FA、组织条目、Send 或附件记录，不会形成新的恢复点。每次上传完整 SQLite 库，快照包含当时的文本型 Send 密文，也包含条目附件和文件型 Send 在数据库中的元数据。[Vaultwarden 1.37.5 数据模型](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/db/schema.rs) · [个人条目的处理代码](https://github.com/dani-garcia/vaultwarden/blob/1.37.5/src/api/core/ciphers.rs)

条目附件的文件内容在 `data/attachments/`，文件型 Send 的文件内容在 `data/sends/`。这两个目录位于数据库之外，保留既有文件需要将它们与对应的数据库一起保存。[Vaultwarden 备份说明](https://github.com/dani-garcia/vaultwarden/wiki/Backing-up-your-vault#the-attachments-dir)

## 从一致副本到独立快照

SQLite 的在线 `.backup` 会读入已提交的 WAL，随后在独立副本上执行 `PRAGMA integrity_check`，并用规则比较副本自身。每个目标读取自己的原生仓库 ID 和最新快照 ID；需要时通过 `restic dump ID /db.sqlite3` 取回比较基准。下载和上传成功的副本可以缓存在磁盘私有目录中，下一次检查仍先查询仓库。清空工作缓存或重启后可从远端重建它。

初始化好的空仓库收到首份快照。比较有变化时，使用 `restic backup --stdin-filename db.sqlite3 --stdin-from-command -- cat …`，源命令失败即中止。其余情况维持原生快照 ID。SQL 错误、损坏的数据库和不可读的仓库产生故障，不会被解释为相同。上传按目标并发，每个目标一次处理一份副本，忙碌期间只保留最新候选。目标失败后按 5、10、20、40、60 分钟退避，恢复后检查当前内容。源库失败会在 5 分钟后重试。[SQLite Online Backup](https://www.sqlite.org/backup.html) · [restic 从命令读取备份](https://restic.readthedocs.io/en/stable/040_backup.html#reading-data-from-a-command)
