# 部署与首次备份

## 安装

准备一台 Linux amd64 主机，安装 Docker 和带 `--wait` 的 Docker Compose。把发布包的实际内容放在 `/srv/releases/vaultwarden-less`，保留其中的镜像、文档和模板。自行构建可在构建机运行 `just release`，复制 `result/` 指向的目录内容。

下面用有 Docker 和文件属主设置权限的账号操作。`/srv/vaultwarden-less` 是本次新建的部署目录，后续在有 `compose.yml` 的目录运行 `docker compose`。发布包里的 Compose 和手册命令已写入对应镜像的标签。

```sh
(
  set -eu
  umask 077
  bundle=/srv/releases/vaultwarden-less
  mkdir -m 0700 /srv/vaultwarden-less
  cd /srv/vaultwarden-less
  install -m 0600 "$bundle/compose.yml" .
  install -d -m 0700 credentials
  install -d -o 1000 -g 1000 -m 0700 config rules credentials/backup data cache state
  install -o 1000 -g 1000 -m 0600 "$bundle"/templates/config/* config/
  install -o 1000 -g 1000 -m 0600 "$bundle"/rules/* rules/
  docker load -i "$bundle/images/vaultwarden.tar.gz"
  docker load -i "$bundle/images/backup.tar.gz"
  docker load -i "$bundle/images/proxy.tar.gz"
)
cd /srv/vaultwarden-less
```

三项服务以 `1000:1000` 运行。Vaultwarden 写入 `data/`，backup 只读数据库，工作副本放在 `cache/`，通知记录放在 `state/`。外部反代把 HTTPS 请求交给 `127.0.0.1:8080`，并覆盖来自客户端的转发头。域名按需填写。[配置说明](configuration.md)

## 导入已有 SQLite

新建空 vault 可直接启动。导入已有 vault 时，先停止原实例的写入和 Vaultwarden 进程，保留它的数据目录和配置。下面假定原库在部署目录旁边的 `../previous/data/`。把 `source` 改成实际目录，确认新部署尚未启动，`data/` 为空。

SQLite 有时把已提交的修改留在 `db.sqlite3-wal`。把同一次停机留下的主库和旁文件复制到可写的临时目录，再用发布镜像里的 SQLite 合成一份独立数据库。看到 `ok` 后，命令才会把它放进新实例。源文件保持原样。

<!-- sqlite-import -->
```sh
(
  set -eu
  source=../previous/data
  test -s "$source/db.sqlite3"
  test -d data && test -z "$(ls -A data)" || { echo '请使用新部署的空 data/ 目录' >&2; exit 1; }
  work=$(mktemp -d "$PWD/cache/import.XXXXXXXX")
  chown 1000:1000 "$work"
  install -o 1000 -g 1000 -m 0600 "$source/db.sqlite3" "$work/"
  for file in db.sqlite3-wal db.sqlite3-shm; do
    if test -f "$source/$file"; then
      install -o 1000 -g 1000 -m 0600 "$source/$file" "$work/"
    fi
  done
  docker run --rm --pull=never --read-only \
    --mount "type=bind,source=$work,target=/work" \
    --entrypoint /bin/sqlite3 vaultwarden-less-backup:development \
    -batch -bail /work/db.sqlite3 '.backup /work/standalone.sqlite3'
  check=$(docker run --rm --pull=never --read-only \
    --mount "type=bind,source=$work,target=/work,readonly" \
    --entrypoint /bin/sqlite3 vaultwarden-less-backup:development \
    -batch -bail -readonly 'file:/work/standalone.sqlite3?immutable=1' 'PRAGMA integrity_check;')
  echo "$check"
  test "$check" = ok
  install -o 1000 -g 1000 -m 0600 "$work/standalone.sqlite3" data/db.sqlite3
  rm -r -- "$work"
)
```

组织共享条目、条目附件和文件型 Send 如需继续使用，请保留原数据并单独安排迁移。验收个人 vault 时，用官方客户端在隔离的 HTTPS 入口登录、解锁，核对一个已知条目。[备份范围](contracts.md#什么算个人-vault-变化)

## 启动与开通账号

默认 `targets: []`、`channels: []`，可以先验收个人 vault，再申请对象存储。在部署目录校验配置并启动服务。

```sh
docker compose config --quiet
docker compose run --rm --no-deps backup validate
docker compose up -d --wait
docker compose exec -T backup /bin/vaultwarden-less-backup status
```

零仓库时 `status` 显示 `source: "unconfigured"`。此时 vault 只在本机。已启用的通知渠道会报告 `no_usable_targets`。

已有账号可以直接使用。开通新账号时，先将 HTTPS 入口限制给准备注册的人，在 `config/vaultwarden.env` 中设 `SIGNUPS_ALLOWED=true`，再执行下面的命令。每人用自己的邮箱和主密码注册。注册完成后，设回 `SIGNUPS_ALLOWED=false`，保持 `SIGNUPS_DOMAINS_WHITELIST=` 为空，再执行一次同一命令。

```sh
docker compose up -d --no-deps --force-recreate --wait vaultwarden
```

## 接入一个仓库

每处存储单独准备 bucket 或专用前缀、访问密钥和 restic 仓库密码。按你购买的存储类别、地区和服务条款确认 SLA。把仓库地址、密码和可用于恢复的访问密钥在 VPS 之外保管一份。下面用 `primary` 举例。按 R2、B2、AWS S3 接入时，可以依次取名 `r2`、`b2`、`aws`，每次做完一个再做下一个。[S3 接口与凭据](configuration.md#对象存储凭据)

在部署目录为新目标复制一份模板，用编辑器填写其中四项。已有凭据文件直接沿用，接入已有仓库时填写原来的仓库密码。

```sh
test ! -e credentials/backup/primary.env && install -o 1000 -g 1000 -m 0600 \
  /srv/releases/vaultwarden-less/templates/credentials/backup/s3.env.example \
  credentials/backup/primary.env
```

将 `config/backup.yaml` 中的 `targets` 写成下面这样。接入下一处时，在同一列表追加一项，保留已有仓库和 `notifications`。

```yaml
targets:
  - name: primary
    env_file: /run/credentials/backup/primary.env
```

```sh
docker compose run --rm --no-deps backup validate
```

`validate` 检查本地配置能否读取。接下来用 restic 验证远端地址和密码。**只有确认是新仓库的空前缀时，才执行一次 `init`**。接入已有仓库直接从 `cat config` 开始。如果为目标配置了额外的 `options`，手工执行 restic 时也要逐个传入 `-o key=value`。

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/backup/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache init
```

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/backup/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache cat config
```

记下输出里的仓库 `id`。已有仓库与异地保存的记录核对，不同仓库应有不同的 ID。然后让 backup 读取新的目标列表。

```sh
docker compose up -d --no-deps --force-recreate --wait backup
docker compose exec -T backup /bin/vaultwarden-less-backup status
```

等 `source` 为 `ready`，新目标的 `state` 为 `protected`，再看仓库中的快照。第一条命令列出时间和 ID，第二条应列出 `/db.sqlite3`。

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/backup/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache snapshots
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/backup/primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache ls latest
```

用客户端保存一个能认出的个人条目，等新快照写入。从这个仓库恢复一次并解锁核对，再接入下一处。[从快照恢复](recovery.md)
