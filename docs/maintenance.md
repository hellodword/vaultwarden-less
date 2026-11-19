# 维护与升级

## 准备升级

留好当前发布包、Compose、规则、配置和凭据。先从每个仓库选一份近期快照，在独立主机解锁一个已知条目。确认有足够磁盘同时保留原数据、工作副本和 SQLite 临时文件。维护窗口内只开放一个正式写入口。[从快照恢复](recovery.md)

先在隔离主机准备新发布包，核对 Vaultwarden、restic 和规则的版本。在原部署目录用 `docker compose ls` 确认现有项目名。Compose 文件的默认项目名是 `vaultwarden-less`。原来使用其他名字的部署，将原名写入部署目录 `.env` 的 `COMPOSE_PROJECT_NAME`。已有 `.env` 时编辑对应条目。这样直接执行 `docker compose` 仍会管理原来的容器。

## 保留停写时的数据

在当前部署目录关停服务，旧版部署沿用该版本的 Compose 参数。验收期间保持正式入口关闭。

```sh
docker compose stop proxy backup vaultwarden
```

在同级私有目录 `../vaultwarden-next` 准备工作副本。下面的命令要求该目录是新建的。将 `data/` 整体复制，WAL、服务器密钥及原有文件都会随它保留。配置、在线凭据和通知记录单独复制，缓存可重新建立。升级和回退期间保管好两套目录。

<!-- upgrade-copy -->
```sh
(
  set -eu
  umask 077
  mkdir -m 0700 ../vaultwarden-next
  cp -a -- data config credentials state ../vaultwarden-next/
  install -d -o 1000 -g 1000 -m 0700 ../vaultwarden-next/cache
  if test -f .env; then cp -p -- .env ../vaultwarden-next/; fi
)
```

将新版 `compose.yml` 和配套规则放进新目录，并加载三个镜像。`/srv/releases/vaultwarden-next` 是新版发布包。用模板对照工作副本中的配置，保留账号设置、仓库地址、密码和渠道。有本地规则或端口调整时逐项核对。如果原目标使用单独的密码文件，在新目录的备份和维护 env 中把 `RESTIC_PASSWORD_FILE` 一行改为 `RESTIC_PASSWORD=原仓库密码`，原文件留在旧部署供回退使用。

```sh
(
  set -eu
  bundle=/srv/releases/vaultwarden-next
  cd ../vaultwarden-next
  install -m 0600 "$bundle/compose.yml" .
  install -d -o 1000 -g 1000 -m 0700 rules
  install -o 1000 -g 1000 -m 0600 "$bundle"/rules/* rules/
  docker load -i "$bundle/images/vaultwarden.tar.gz"
  docker load -i "$bundle/images/backup.tar.gz"
  docker load -i "$bundle/images/proxy.tar.gz"
  docker compose config --quiet
  docker compose run --rm --no-deps backup validate
)
```

## 切换与回退

在新目录运行以下命令，让新版 Vaultwarden 在数据副本上完成数据库迁移。`--force-recreate` 确保 Compose 使用当前目录的挂载。测试用的 HTTPS 入口指向新实例。在同一主机使用默认端口时，旧 proxy 须已停机。

```sh
cd ../vaultwarden-next
docker compose up -d --force-recreate --wait vaultwarden proxy
```

用官方客户端在隔离入口登录、解锁并核对已知条目。确认内容后，启动使用配套 SQL 规则的 backup。

```sh
docker compose up -d --no-deps --force-recreate --wait backup
docker compose exec -T backup /bin/vaultwarden-less-backup status
```

等已配置的仓库逐个显示 `protected`，抽取新快照或确认沿用的原快照能解密，再开放正式入口。若新 SQL 无法读取旧快照的数据模型，保留旧快照，在已验收的新库上按下节为每个仓库建立新的比较起点。零仓库部署可先完成客户端验收，再按部署手册接入第一处存储。[接入仓库](deployment.md#接入一个仓库)

回退时先在新版部署目录停写，保留新版数据目录。已开放正式入口时，核对并另存升级后新增的个人条目。回到旧目录，使用原发布包的镜像、旧 Compose、规则、凭据和停写时保留的数据库启动。客户端解锁并确认预期内容后，再恢复正式流量。

```sh
docker compose stop proxy backup vaultwarden
cd ../vaultwarden-less
docker compose up -d --force-recreate --wait
```

## 手工建立恢复点

升级前要保存停写时刻，或升级后需要给新模型建立首份快照时，在**产生这份数据的部署目录**操作，用与该部署配套的手册和镜像。在部署目录停止服务，按下面的命令准备一份校验过的数据库。`cache/manual/` 须是新目录，工作副本可写，SQLite 能读到与主库一同复制的 WAL。

```sh
(
  set -eu
  docker compose stop proxy backup vaultwarden
  mkdir -m 0700 cache/manual
  chown 1000:1000 cache/manual
  install -o 1000 -g 1000 -m 0600 data/db.sqlite3 cache/manual/
  for file in db.sqlite3-wal db.sqlite3-shm; do
    if test -f "data/$file"; then install -o 1000 -g 1000 -m 0600 "data/$file" cache/manual/; fi
  done
  docker run --rm --pull=never --read-only \
    --mount "type=bind,source=$PWD/cache/manual,target=/work" \
    --entrypoint /bin/sqlite3 vaultwarden-less-backup:development \
    -batch -bail /work/db.sqlite3 '.backup /work/standalone.sqlite3'
  chmod 0600 cache/manual/standalone.sqlite3
  check=$(docker run --rm --pull=never --read-only \
    --mount "type=bind,source=$PWD/cache/manual,target=/work,readonly" \
    --entrypoint /bin/sqlite3 vaultwarden-less-backup:development \
    -batch -bail -readonly 'file:/work/standalone.sqlite3?immutable=1' 'PRAGMA integrity_check;')
  echo "$check"
  test "$check" = ok
)
```

确认输出为 `ok` 后，每个仓库各执行一次下面的命令，替换为对应的 env 文件，带上该目标使用的后端选项。应得到可独立恢复的 `/db.sqlite3` 快照。

```sh
docker run --rm --pull=never --read-only --tmpfs /tmp:mode=1777 \
  --env-file ./credentials/backup/primary.env \
  --mount "type=bind,source=$PWD/cache/manual,target=/work,readonly" \
  --entrypoint /bin/restic vaultwarden-less-backup:development \
  --no-cache backup --host vaultwarden-less --stdin-filename db.sqlite3 \
  --stdin-from-command -- cat /work/standalone.sqlite3
```

逐仓记下快照 ID 并验证恢复。维护结束时，在选定的正式部署目录执行 `docker compose up -d --wait`。验收后可清理本次 `cache/manual/` 工作副本。

## 构建和更新上游

在构建机上检查源码、生成发布包，再用 Docker 验收。

```sh
just check
just release
just check-release
```

`just check` 检查格式，运行 Go 测试、竞态检测、vet 和脚本测试。`just release` 的 `result/` 包含三个可离线加载的镜像、Compose、两份规则、配置模板及配套手册。干净提交的构建使用 `git-<完整提交号>` 作为项目镜像标签，工作区构建使用 `development`。`just check-release` 需要本地 Docker socket，运行合成账号、官方 Bitwarden CLI、独立 S3 测试端和本地通知接收端，结果写入 `results.log`。

- **Vaultwarden**　升级时按发布说明审阅数据库迁移、`src/db/schema.rs`、个人数据模型、`src/api/core` 和 `src/api/identity.rs`。用 `git diff 旧标签 新标签 -- src/api src/db migrations` 对照 `rules/routes.txt` 跳过的成功请求和 `rules/personal-vault.sql` 覆盖的数据。用合成账号验证登录、条目新增、编辑、删除和恢复，在两份规则中记录审核的版本。[Vaultwarden 发布说明](https://github.com/dani-garcia/vaultwarden/releases) · [规则说明](contracts.md)
- **官方镜像**　在 `nix/packages.nix` 更新 `finalImageTag`、Linux amd64 的 manifest digest 和 Nix 归档哈希，在 `compose.yml` 同步标签。用 `docker buildx imagetools inspect vaultwarden/server:版本` 查平台摘要，再用 `docker buildx imagetools inspect --raw vaultwarden/server@sha256:平台摘要` 读取 `config.digest`。用 `lib.fakeHash` 构建 `nix build .#vaultwarden-image`，填回 Nix 报出的归档哈希并重建。将官方配置摘要去掉 `sha256:` 前缀赋给 `CONFIG_SHA`，运行 `tar -xOzf result/images/vaultwarden.tar.gz "$CONFIG_SHA.json" | sha256sum`，核对输出与 `CONFIG_SHA` 一致。
- **restic、SQLite 和验收客户端**　版本由 `flake.lock` 锁定。使用 `nix flake update nixpkgs` 更新，核对实际版本和 restic 对快照 JSON、dump、stdin、restore 及仓库格式的变动。用新版读取并恢复旧快照，确认 SQLite 在线备份和内容比较仍有效。仓库格式需要迁移时，先在仓库副本上演练。[restic 发布说明](https://github.com/restic/restic/releases)
- **Go 和规则**　使用 Go 工具更新 `go.mod`、`go.sum`，按 Nix 实际计算的 `vendorHash` 构建。单独调整运行规则时，更新部署目录的对应文件，重启 proxy 或 backup。SQL 规则生效后，旧快照也使用新规则比较。
