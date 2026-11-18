# 维护与升级

## 在构建机准备发布包

```sh
just check
just release
just check-release
```

`just check` 运行锁定工具链的格式检查、Go 测试、竞态检测、vet 和运维示例测试。`just release` 的 `result/` 包含三个可由 Docker 加载的镜像、Compose、规则、配置模板和文档。`just check-release` 在具备本地 Docker socket 的非 root 构建机上运行。验收使用合成账号、官方 Bitwarden CLI、两个独立 S3 测试端和本地通知接收端，覆盖设备登录、个人条目写入、单仓故障追平、冷启动及逐仓解密恢复。凭据和运行目录只在部署端或临时测试路径出现。

## 更新上游

1. **Vaultwarden**。选定官方版本，查发布说明、数据库迁移、`src/db/schema.rs`、个人数据模型、`src/api/core` 和 `src/api/identity.rs`。逐条审阅 `rules/routes.txt` 跳过的成功请求是否仍只读，以及 `rules/personal-vault.sql` 是否需要覆盖新字段、表或关联。用普通 `git diff 旧标签 新标签 -- src/api src/db migrations` 即可。在干净的合成库上验证新增、编辑、删除、登录与恢复。[Vaultwarden 发布](https://github.com/dani-garcia/vaultwarden/releases)
2. **官方镜像**。在 `nix/packages.nix` 更新 `finalImageTag`、Linux amd64 manifest digest 和 Nix 归档哈希，在 `compose.yml` 更新官方标签。用 `docker buildx imagetools inspect vaultwarden/server:版本` 查对应平台 manifest，再用 `docker buildx imagetools inspect --raw vaultwarden/server@sha256:平台摘要` 读取其 `config.digest`。先以 `lib.fakeHash` 构建 `nix build .#vaultwarden-image`，用 Nix 报出的归档哈希替换后重建。运行 `just release` 后，将官方配置摘要去掉 `sha256:` 前缀赋给 `CONFIG_SHA`，执行 `tar -xOzf result/images/vaultwarden.tar.gz "$CONFIG_SHA.json" | sha256sum`，结果应与 `CONFIG_SHA` 一致。同步部署手册中的版本，并在两份规则上记录最后审核的版本。
3. **restic 和 SQLite**。版本由 `flake.lock` 固定的 nixpkgs 决定。选择更新后使用 `nix flake update nixpkgs` 生成新锁，记录实际版本，审阅 restic 的快照 JSON、dump、stdin 读取、仓库格式和 restore 变动，以及 SQLite 的在线备份和查询行为。对旧快照执行新版本的读取、比较和恢复。仓库格式迁移属于单独的手工维护操作，先在专用副本上验证。[restic 发布说明](https://github.com/restic/restic/releases)
4. **工具与规则**。Go 模块由 `go.mod` 和 `go.sum` 固定，使用 Go 工具更新，再以 Nix 实际计算的 `vendorHash` 构建。验收客户端的兼容版本在 `nix/upstreams.nix`。仅编辑运行规则时，更新部署目录的规则文件，分别重启 proxy 或 backup 即可。SQL 变更后，旧快照也会按新规则重新比较。

## 切换与回退

先确认两仓各有一份可恢复快照，保留旧镜像、Compose、规则、配置和访问凭据。停止旧实例写入，在新目录准备可写工作副本。以下 Bash 示例要求 `DEPLOY_DIR` 和 `UPGRADE_DATA` 是已核对的绝对路径。

<!-- upgrade-copy-example -->
```sh
set -euo pipefail
: "${DEPLOY_DIR:?}" "${UPGRADE_DATA:?}"
[[ $UPGRADE_DATA = /* ]]
test "$(realpath -m -- "$UPGRADE_DATA")" = "$UPGRADE_DATA"
case "$UPGRADE_DATA/" in "$DEPLOY_DIR/data/"*) exit 1 ;; esac
test -d "$DEPLOY_DIR/data"
test ! -e "$UPGRADE_DATA"
test ! -L "$UPGRADE_DATA"
mkdir -m 0700 -- "$UPGRADE_DATA"
cp -a -- "$DEPLOY_DIR/data/." "$UPGRADE_DATA/"
```

让新 Vaultwarden 在工作副本上运行自己的迁移，先从隔离地址用官方客户端读出预期条目。再启动使用新规则的备份进程，与现有原生快照比较。如果上一版本的快照与新 SQL 模型不兼容，保留旧快照，在确认新版本可解密后按[手工建立恢复点](operations.md#手工建立原生恢复点)把新库分别写入两个仓库。启动自动备份，确认状态并逐仓恢复。维护窗口内只启用一个正式写入口。

回退使用保留的旧镜像、旧配置和升级前未写入的原始数据目录。已经在新版产生的写入须先单独保存并核对，再决定如何带回。切换正式入口前验证客户端能解密，两个仓库仍可独立读出数据。
