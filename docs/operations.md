# 日常与恢复

在部署目录运行。下文的 `dc`、`native`、`DEPLOY_DIR` 和 `RESTIC_IMAGE` 使用[部署指南](deployment.md)中的定义。每次操作先设置 `RESTIC_ENV_FILE` 选定一个仓库。

```sh
dc run --rm --no-deps backup validate
dc exec -T backup /bin/vaultwarden-less-backup status
export RESTIC_ENV_FILE="$DEPLOY_DIR/credentials/maintenance/aws-s3-maintenance.env"
native cat config
native snapshots
```

`source=ready` 表示取得了通过检查的源库副本。`protected` 表示该仓库已保存对应内容或已确认两者相同。`checked_at` 是这次检查的时间，`snapshot_at` 是最近快照的时间，两者可以相隔数月。`checking` 表示比较或上传正在进行。`failed`、`error` 和 `retry_at` 用于定位故障。在 `source=failed` 时，以各仓库最后一次确认时间判断保护进度。通知渠道投递失败时，`notification_errors` 列出渠道名和错误。查看单个仓库的实际快照使用原生 `snapshots`。

若各渠道同时报告 `state_invalid`、`state_read_failed` 或 `state_write_failed`，查看 `dc logs backup` 中的文件名和原因。两个仓库仍会接收新写入。停下 backup，保留现场，检查空间、文件权限和记录内容，修复问题后重新启动 backup。它会从可读记录继续投递。

## 从任一仓库恢复

在有 Docker 的独立主机准备发行包、维护凭据和仓库密码，加载镜像并填写环境文件。原 VPS、备份服务的状态目录和其他仓库都不参与恢复。先用 `native snapshots` 选择时间，再从 `native snapshots --json` 取得完整 ID，用 `native ls "$SNAPSHOT_ID"` 确认包含 `/db.sqlite3`。以下 Bash 示例在一个全新的绝对路径内恢复。恢复目录和缓存按固定属主准备。如目标配置了额外后端选项，请一并传给 restic。

<!-- native-restore-example -->
```sh
set -euo pipefail
: "${DEPLOY_DIR:?}" "${RESTIC_IMAGE:?}" "${RESTIC_ENV_FILE:?}" "${SNAPSHOT_ID:?}" "${RESTORE_DIR:?}"
[[ $SNAPSHOT_ID =~ ^[0-9a-f]{64}$ ]]
[[ $RESTORE_DIR = /* ]]
test "$(realpath -m -- "$RESTORE_DIR")" = "$RESTORE_DIR"
test ! -e "$RESTORE_DIR"
test ! -L "$RESTORE_DIR"
mkdir -m 0700 -- "$RESTORE_DIR"
mkdir -p -m 0700 -- "$DEPLOY_DIR/cache/maintenance"
chown 1000:1000 -- "$RESTORE_DIR" "$DEPLOY_DIR/cache/maintenance"
docker run --rm --read-only \
  --mount "type=bind,source=$DEPLOY_DIR/cache/maintenance,target=/cache" \
  --env TMPDIR=/cache \
  --env-file "$RESTIC_ENV_FILE" \
  --env RESTIC_CACHE_DIR=/cache/restic \
  --mount "type=bind,source=$DEPLOY_DIR/credentials/restic,target=/run/credentials/restic,readonly" \
  --mount "type=bind,source=$RESTORE_DIR,target=/restore" \
  --entrypoint /bin/restic "$RESTIC_IMAGE" \
  restore "$SNAPSHOT_ID" --verify --target /restore
test -s "$RESTORE_DIR/db.sqlite3" && test ! -L "$RESTORE_DIR/db.sqlite3" || exit 1
chmod 0600 -- "$RESTORE_DIR/db.sqlite3"
integrity=$(docker run --rm --read-only \
  --mount "type=bind,source=$RESTORE_DIR,target=/restore" \
  --entrypoint /bin/sqlite3 "$RESTIC_IMAGE" \
  '/restore/db.sqlite3' 'PRAGMA integrity_check;')
test "$integrity" = ok
printf '%s\n' "$integrity"
```

保留这份校验通过的文件。确认新实例的 `data/` 为空后，用 `install -o 1000 -g 1000 -m 0600 -- "$RESTORE_DIR/db.sqlite3" "$DEPLOY_DIR/data/db.sqlite3"` 放置已校验的库，使用与快照兼容的 Vaultwarden 版本和自己的环境文件，在隔离 HTTPS 地址启动。空目录让 Vaultwarden 自行生成服务器 RSA 密钥，避免混入另一个数据库的 WAL。

恢复会把所有账号带回同一个快照时间点。每位使用者用全新配置的官方客户端登录恢复实例，完成自己账号的验证和解锁，同步后核对本人的条目、密码、文件夹和收藏。在确认内容与预期一致后再切换正式入口。对第二个仓库独立做一次相同演练。

## 检查、保留和维修

逐个目标运行维护操作。把周期性检查交给已有的计划任务，或在固定维护日执行并记录结果。选用能读取仓库的维护凭据。建议分开安排两个仓库的大流量操作，留出一个已验证的可用副本。

```sh
native check
native check --read-data
native forget --group-by '' --keep-daily 7 --keep-weekly 5 --keep-monthly 12 --dry-run
```

保留周期是示例，按能接受的数据丢失窗口和容量决定。在预览结果中确认要保留的时间点，再以相同参数去掉 `--dry-run` 执行 `forget`，按需要运行 `native prune`。若有人同时运行维护或出现锁错误，先核实是否存在活动客户端。`native list locks` 列出锁；只有确认无对应进程时才使用 `native unlock`。仓库损坏时先从另一个仓库恢复并保留故障仓库的副本，根据具体错误采用上游的修复步骤。配置供应商生命周期时保留 restic 管理的活动对象。[restic 仓库维护](https://restic.readthedocs.io/en/stable/045_working_with_repos.html) · [快照保留](https://restic.readthedocs.io/en/stable/060_forget.html)

## 手工建立原生恢复点

需要在升级前、或新旧数据模型无法直接比较时，先停止写入并准备独立工作目录。以下会建立新快照，在每个目标各执行一次。自动备份和维护程序应在此期间停止对仓库写入。用当前部署的镜像、原生后端选项和对应仓库的维护凭据。

```sh
set -euo pipefail
dc stop proxy backup vaultwarden
stage=$(mktemp -d "$DEPLOY_DIR/cache/manual.XXXXXXXX")
chown 1000:1000 -- "$stage"
docker run --rm --read-only \
  --mount "type=bind,source=$DEPLOY_DIR/data,target=/source,readonly" \
  --mount "type=bind,source=$stage,target=/stage" \
  --entrypoint /bin/sqlite3 "$RESTIC_IMAGE" -batch -bail -readonly /source/db.sqlite3 '.backup /stage/db.sqlite3'
chmod 0600 -- "$stage/db.sqlite3"
check=$(docker run --rm --read-only \
  --mount "type=bind,source=$stage,target=/stage" \
  --entrypoint /bin/sqlite3 "$RESTIC_IMAGE" /stage/db.sqlite3 'PRAGMA integrity_check;')
test "$check" = ok
for target in aws-s3 backblaze-b2; do
  docker run --rm --read-only --tmpfs /tmp:mode=1777 \
    --env-file "$DEPLOY_DIR/credentials/maintenance/$target-maintenance.env" \
    --mount "type=bind,source=$DEPLOY_DIR/credentials/restic,target=/run/credentials/restic,readonly" \
    --mount "type=bind,source=$stage,target=/stage,readonly" --entrypoint /bin/restic "$RESTIC_IMAGE" \
    --no-cache backup --host vaultwarden-less --stdin-filename db.sqlite3 --stdin-from-command -- cat /stage/db.sqlite3
done
dc up -d
```

核对两个快照 ID、恢复验证并记录结果后，可释放本次 `stage`。若冷停机的旧库有 WAL 而缺少打开只读连接所需的旁文件，将整个已停止的数据目录复制到私有工作目录，让 SQLite 在工作副本上重建旁文件并执行 `.backup`。
