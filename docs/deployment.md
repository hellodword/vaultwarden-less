# 部署与首次备份

## 选择三个存放位置

保留 VPS 上的工作库，在两家提供 SLA 的对象存储分别开通 bucket，选取不同地理区域、独立账号或访问主体，为每处生成独立的 restic 仓库密码。模板给出 AWS S3 和 Backblaze B2 的 S3 接口。按所选区域、存储类别和账户条款核对 SLA。供应商的可用性承诺与能恢复出哪一个时间点是两件事，恢复时间点以各仓库实际快照为准。[Amazon S3 SLA](https://aws.amazon.com/s3/sla/) · [Backblaze SLA](https://www.backblaze.com/company/policy/sla)

每个仓库独占一个空前缀。在线备份主体需要列举、读取、写入仓库对象并创建和删除 restic 锁。按供应商能力限制删除快照和数据包的权限。维护主体单独保管，可执行保留和清理。管理员在 VPS 之外保管 bucket、区域、端点、仓库密码和维护访问凭据。每位使用者自行保管主密码及已启用的账号验证恢复手段。

## 离线安装

在构建机运行 `just release`，把 `result/` 的实际内容复制到 VPS 上的只读发布目录。VPS 需要 Linux amd64、Docker 和 Compose v2。以下 Bash 命令需要 Docker 访问权和设置文件属主的权限。三项服务及维护容器都以 `1000:1000` 运行。

```sh
set -euo pipefail
export BUNDLE=/srv/releases/vaultwarden-less
export DEPLOY_DIR=/srv/vaultwarden-less
umask 077
mkdir -m 0700 "$DEPLOY_DIR"
cd "$DEPLOY_DIR"
cp "$BUNDLE/compose.yml" "$BUNDLE/templates/compose.env" .
cp -R "$BUNDLE/templates/config" "$BUNDLE/templates/credentials" "$BUNDLE/rules" .
chmod -R u+rwX,go-rwx config credentials rules
chmod u+rw compose.yml compose.env
install -d -m 0700 -o 1000 -g 1000 data cache state
mkdir -m 0700 credentials/maintenance
for image in vaultwarden backup proxy; do docker load -i "$BUNDLE/images/$image.tar.gz"; done
docker image inspect --format '{{.Os}}/{{.Architecture}} {{index .Config.Labels "org.opencontainers.image.version"}}' vaultwarden/server:1.37.5
```

最后一条命令应输出 `linux/amd64 1.37.5`。构建时使用的官方平台 manifest 摘要固定在 `nix/packages.nix`。[升级手册](maintenance.md#更新上游)说明如何用上游配置摘要复核发布包。

填写 `config/vaultwarden.env` 的外部 HTTPS `DOMAIN`、所需邮件设置、两个目标的在线凭据及仓库密码。为两个目标分别准备 `credentials/maintenance/<目标名>-maintenance.env`，使用同一仓库地址、区域和容器内密码路径，填入独立的维护访问密钥。填写后设置服务需要读取的文件权限。

```sh
chown -R 1000:1000 -- rules config credentials/backup credentials/restic
chmod -R u=rwX,go= -- rules config credentials/backup credentials/restic
chmod 0600 -- credentials/maintenance/*.env
```

Vaultwarden 可写 `data/`。backup 只读源库并可写 `cache/`、`state/`，proxy 只挂载规则。TLS 入口向 `127.0.0.1:8080` 转发并覆盖客户端传来的转发头。Docker 重启后原本运行的服务自动启动，手工停止的服务保持停机。

## 接管已有 SQLite

已有实例先停写，保留原数据和配置。令 `SOURCE_DATA` 指向已停止的数据库目录。若来源是可信的一致归档，先在私有目录提取同一批数据库文件。以下示例中，从 `tar -tf "$ARCHIVE"` 中找到主库成员名，设为 `DB_MEMBER`，例如 `export DB_MEMBER=data/db.sqlite3`。

<!-- native-archive-example -->
```sh
set -euo pipefail
umask 077
unpack=$(mktemp -d)
tar -tf "$ARCHIVE" > "$unpack/members"
member=${DB_MEMBER:?}
members=("$member")
for suffix in -wal -shm; do
  if grep -Fxq -- "$member$suffix" "$unpack/members"; then members+=("$member$suffix"); fi
done
tar -xf "$ARCHIVE" -C "$unpack" --no-same-owner --no-same-permissions -- "${members[@]}"
export SOURCE_DATA="$unpack/$(dirname -- "$member")"
```

将 `IMPORT_STAGE` 设为私有工作区里尚未创建的绝对路径，`DEPLOY_DIR/data` 是新部署的空目录。下面的命令使用 SQLite CLI，将 WAL 中已提交的事务纳入可独立打开的数据库。

<!-- native-import-example -->
```sh
set -euo pipefail
umask 077
: "${SOURCE_DATA:?}" "${IMPORT_STAGE:?}" "${DEPLOY_DIR:?}"
[[ $SOURCE_DATA = /* && $IMPORT_STAGE = /* && $DEPLOY_DIR = /* ]]
test -d "$DEPLOY_DIR/data" && test ! -L "$DEPLOY_DIR/data" || exit 1
contents=$(find "$DEPLOY_DIR/data" -mindepth 1 -maxdepth 1 -print -quit)
test -z "$contents"
mkdir -m 0700 -- "$IMPORT_STAGE"
cd "$IMPORT_STAGE"
for file in db.sqlite3 db.sqlite3-wal db.sqlite3-shm; do
  src="$SOURCE_DATA/$file"
  if [[ $file != db.sqlite3 && ! -e $src && ! -L $src ]]; then continue; fi
  test -f "$src" && test ! -L "$src" || exit 1
  cp -- "$src" "$file"
done
test -s db.sqlite3
sqlite3 -batch -bail db.sqlite3 '.backup standalone.sqlite3'
test "$(sqlite3 -batch -bail standalone.sqlite3 'PRAGMA integrity_check;')" = ok
install -m 0600 -- standalone.sqlite3 "$DEPLOY_DIR/data/db.sqlite3"
chown 1000:1000 -- "$DEPLOY_DIR/data/db.sqlite3"
```

核对 `sqlite3 -readonly standalone.sqlite3 'SELECT count(*) FROM organizations;'`。已有共享条目按所有者需要转为个人条目或另行备份。需要保留的附件和文件型 Send 连同原数据目录一起保管。在隔离地址启动工作副本，用官方客户端解密已知条目，再从两个仓库分别恢复和核对，最后切换正式入口。接管正在使用的本项目部署时，保留两个仓库的地址、密码以及已停机的 `state/` 工作副本，为通知记录设置 `1000:1000` 和 `0600`。

## 初始化与验收

在部署目录定义以下函数。模板的两个示例直接使用环境变量，若配置额外的 restic 后端选项，手工命令也要传入对应的 `-o key=value`。

```sh
dc() { docker compose --env-file "$DEPLOY_DIR/compose.env" -f "$DEPLOY_DIR/compose.yml" "$@"; }
dc config --quiet
dc run --rm --no-deps backup validate
RESTIC_IMAGE=$(dc config --images | grep '^vaultwarden-less-backup:')
export RESTIC_IMAGE
native() {
  docker run --rm --read-only --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
    --env-file "$RESTIC_ENV_FILE" \
    --mount "type=bind,source=$DEPLOY_DIR/credentials/restic,target=/run/credentials/restic,readonly" \
    --entrypoint /bin/restic "$RESTIC_IMAGE" --no-cache "$@"
}
```

逐仓核对地址。对全新的空前缀运行一次 `native init`。已有仓库直接使用原密码执行 `native cat config`。例如，初始化两个全新仓库时执行以下命令。

```sh
for target in aws-s3 backblaze-b2; do
  export RESTIC_ENV_FILE="$DEPLOY_DIR/credentials/maintenance/$target-maintenance.env"
  native init
  native cat config
done
dc up -d
dc exec -T backup /bin/vaultwarden-less-backup status
```

从两次 `native cat config` 的输出记录两个不同的仓库 ID。首次上传期间，`status` 会显示 `checking`。再次运行 `dc exec -T backup /bin/vaultwarden-less-backup status`，等两个目标都显示 `protected` 后，分别记录完整快照 ID。

## 开通个人账号

开户期间，仅向需要注册的人开放入口。在部署目录的 `config/vaultwarden.env` 中临时设置 `SIGNUPS_ALLOWED=true`，执行 `dc up -d --force-recreate vaultwarden`。每人用自己的邮箱注册并设置主密码。注册完成后，将 `SIGNUPS_ALLOWED` 设回 `false`，保持 `SIGNUPS_DOMAINS_WHITELIST` 为空，再执行同一条命令。以后开通账号可以重复此流程。

每人在自己的 vault 中创建一个能识别的条目。等两个仓库各有一份包含此次写入的快照，再按[恢复步骤](operations.md)逐仓核对。
