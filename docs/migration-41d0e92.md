# 从 41d0e92 的 Vaultwarden 1.37.3 部署迁移

本次来源是提交 `41d0e92ffcdea93cc88711dc49e580213dbcf90e` 对应的部署，服务端镜像升级到了 1.37.3。来源归档中 `vaultwarden-less/data/` 包含 `db.sqlite3`、`db.sqlite3-wal`、`db.sqlite3-shm`，原运行 UID:GID 为 `65532:65532`。目标是用官方 1.37.5 打开工作副本，把能解密的个人 vault 写入两个新的专用 restic 仓库。

保留原归档和原部署。记录归档的 `sha256sum`，确认归档是停写取得或用实际恢复验证其一致性。单凭文件齐全无法确认备份时刻。先在私有目录审阅旧环境配置，把仍需要的 DOMAIN、邮件等设置写入新的 `config/vaultwarden.env`。新部署使用独立入口和数据目录。以下 Bash 示例仅提取指定数据库及同时存在的 WAL/SHM，在工作副本上执行在线备份并检查结果。维护机需要 GNU tar 和 SQLite CLI。

<!-- native-import-example -->
```sh
set -euo pipefail
umask 077
: "${ARCHIVE:?}" "${IMPORT_STAGE:?}" "${DEPLOY_DIR:?}"
[[ $IMPORT_STAGE = /* && $DEPLOY_DIR = /* ]]
test ! -e "$IMPORT_STAGE" && test ! -L "$IMPORT_STAGE" || exit 1
test -d "$DEPLOY_DIR/data"
test ! -L "$DEPLOY_DIR/data"
test -z "$(find "$DEPLOY_DIR/data" -mindepth 1 -maxdepth 1 -print -quit)"
mkdir -m 0700 -- "$IMPORT_STAGE"
tar -tf "$ARCHIVE" > "$IMPORT_STAGE/members"
base=vaultwarden-less/data/db.sqlite3
members=("$base")
for suffix in -wal -shm; do
  if grep -Fxq "$base$suffix" "$IMPORT_STAGE/members"; then members+=("$base$suffix"); fi
done
tar -xf "$ARCHIVE" -C "$IMPORT_STAGE" --no-same-owner --no-same-permissions -- "${members[@]}"
for member in "${members[@]}"; do test -f "$IMPORT_STAGE/$member" && test ! -L "$IMPORT_STAGE/$member" || exit 1; done
cd "$IMPORT_STAGE"
sqlite3 -batch -bail "$base" '.backup standalone.sqlite3'
test "$(sqlite3 -batch -bail standalone.sqlite3 'PRAGMA integrity_check;')" = ok
install -m 0600 standalone.sqlite3 "$DEPLOY_DIR/data/db.sqlite3"
```

将新文件交给 Compose 的运行身份，保留 `standalone.sqlite3` 作为升级前副本。在这份一致副本上核对已有组织的数量。

```sh
sqlite3 -readonly "$IMPORT_STAGE/standalone.sqlite3" 'SELECT count(*) FROM organizations;'
```

结果为 `0` 时可直接按个人账号模式接管。若有组织，其成员关系会随库迁入。仅修改组织条目不会触发快照，切换前由所有者确认需要持续备份的共享条目已存入相应的个人 vault 或已单独备份。对照 `$IMPORT_STAGE/members` 查看 `vaultwarden-less/data/attachments/` 和 `vaultwarden-less/data/sends/`，将需要保留的文件与同一份数据库一起保管。

按[部署指南](deployment.md)核对镜像、配置、两个新仓库及其独立凭据。先运行新 Vaultwarden，让它完成迁移，再启动 backup 和 proxy。用正式账号的合法验证手段在隔离地址连接官方客户端，解锁并比对已知的个人条目、密码与文件夹。在每个仓库分别记录首份快照 ID，把每份恢复到不同的新目录，用全新客户端再次解密相同内容，再决定切换正式入口。迁移失败时继续使用原部署并保留工作副本以定位原因。本指南在操作者确认完成这次迁移后可以退役。
