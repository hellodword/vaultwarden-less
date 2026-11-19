# 从快照恢复

在另一台 Linux amd64 主机上准备 Docker、Docker Compose、与快照兼容的发布包，以及异地保管的**一个**仓库的地址、密码和对象存储访问密钥。整个过程只访问选定仓库。恢复的是全实例在同一时点的数据库，客户端解锁仍使用各自的账号和主密码。

## 找到要恢复的快照

把发布包放在 `/srv/releases/vaultwarden-less`。下面用有 Docker 和设置属主权限的账号操作，把 `/secure/offsite/primary.env` 换成取回的仓库凭据文件。如果分开保管了地址、密码和访问密钥，用发布包里的 `templates/credentials/backup/s3.env.example` 将四项填回一份私有文件。直接填写字面值，引号也会成为值的一部分。

```sh
(
  set -eu
  umask 077
  bundle=/srv/releases/vaultwarden-less
  mkdir -m 0700 /srv/vault-recovery
  cd /srv/vault-recovery
  install -m 0600 /secure/offsite/primary.env primary.env
  docker load -i "$bundle/images/backup.tar.gz"
  docker load -i "$bundle/images/vaultwarden.tar.gz"
  docker load -i "$bundle/images/proxy.tar.gz"
)
cd /srv/vault-recovery
```

先核对 `cat config` 的仓库 ID，再按时间从快照列表选取完整的 `id`。如果原目标另设了后端选项，给下文各条 restic 命令带上相同的 `-o key=value`。

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache cat config
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./primary.env \
  --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache snapshots --json
```

## 还原数据库

将下面的 `snapshot` 替换成选定的 ID。`ls` 应列出 `/db.sqlite3`。`restore --verify` 在新建的 `recovered/` 中还原并复核文件，然后用镜像里的 SQLite 检查数据库，最后应输出 `ok`。

<!-- sqlite-restore -->
```sh
(
  set -eu
  snapshot=替换为完整快照ID
  docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
    --env-file ./primary.env \
    --entrypoint /bin/restic vaultwarden-less-backup:development --no-cache ls "$snapshot"
  mkdir -m 0700 recovered
  chown 1000:1000 recovered
  docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
    --env-file ./primary.env --mount "type=bind,source=$PWD/recovered,target=/restore" \
    --entrypoint /bin/restic vaultwarden-less-backup:development \
    --no-cache restore "$snapshot" --verify --target /restore
  chmod 0600 recovered/db.sqlite3
  check=$(docker run --rm --pull=never --read-only \
    --mount "type=bind,source=$PWD/recovered,target=/restore,readonly" \
    --entrypoint /bin/sqlite3 vaultwarden-less-backup:development \
    -batch -bail -readonly 'file:/restore/db.sqlite3?immutable=1' 'PRAGMA integrity_check;')
  echo "$check"
  test "$check" = ok
)
```

## 用客户端验收

在这台独立主机的 `/srv/vault-recovery` 目录，给测试实例准备一份数据库副本。保留 `recovered/db.sqlite3` 供复查。使用空的 `data/`，Vaultwarden 会自行生成服务端 RSA 密钥。

```sh
(
  set -eu
  bundle=/srv/releases/vaultwarden-less
  mkdir -m 0700 data
  chown 1000:1000 data
  install -o 1000 -g 1000 -m 0600 recovered/db.sqlite3 data/db.sqlite3
  install -m 0600 "$bundle/compose.yml" .
  install -d -o 1000 -g 1000 -m 0700 rules config credentials/backup state cache
  install -o 1000 -g 1000 -m 0600 "$bundle"/rules/* rules/
  install -o 1000 -g 1000 -m 0600 "$bundle"/templates/config/* config/
)
```

按需在 `config/vaultwarden.env` 填写恢复实例的 HTTPS 地址和账号所需的邮件等设置。保留模板的 `targets: []`，将隔离的 HTTPS 入口转发到这台主机的 `127.0.0.1:8080`。

```sh
docker compose up -d --wait
```

用全新配置的官方客户端连接恢复实例，登录、解锁并同步。每个人核对一个备份前已知的条目、其中的密码以及所在文件夹或收藏。需要验证第二个仓库时，单独从它选取快照，在新的私有目录重复恢复。若要投入使用，先核对快照之后有无需要另行保留的写入，再切换正式入口并接入仓库。[接入一个仓库](deployment.md#接入一个仓库)
