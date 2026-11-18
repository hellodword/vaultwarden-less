# 部署与首次备份

## 选择三个存放位置

保留 VPS 上的工作库，在两家提供 SLA 的对象存储分别开通 bucket，选取不同地理区域、独立账号或访问主体，为每处生成独立的 restic 仓库密码。模板给出 AWS S3 和 Backblaze B2 的 S3 接口。按所选区域、存储类别和账户条款核对 SLA。供应商的可用性承诺与能恢复出哪一个时间点是两件事，恢复时间点以各仓库实际快照为准。[Amazon S3 SLA](https://aws.amazon.com/s3/sla/) · [Backblaze SLA](https://www.backblaze.com/company/policy/sla)

每个仓库独占一个空前缀。在线备份主体需要列举、读取、写入仓库对象并创建和删除 restic 锁。按供应商能力限制删除快照和数据包的权限。维护主体单独保管，可执行保留和清理。管理员在 VPS 之外保管 bucket、区域、端点、仓库密码和维护访问凭据。每位使用者自行保管主密码及已启用的账号验证恢复手段。

## 离线安装

在构建机运行 `just release`，把 `result/` 的实际内容复制到 VPS 上的只读发布目录。VPS 需要 Linux amd64、Docker 和 Compose v2。以下命令在 Bash 中运行。示例由能够访问 Docker 的部署用户执行，受限目录中的文件须可由对应运行身份读取。

```sh
export BUNDLE=/srv/releases/vaultwarden-less
export DEPLOY_DIR=/srv/vaultwarden-less
umask 077
mkdir -m 0700 "$DEPLOY_DIR"
cd "$DEPLOY_DIR"
cp "$BUNDLE/compose.yml" "$BUNDLE/templates/compose.env" .
cp -R "$BUNDLE/templates/config" "$BUNDLE/templates/credentials" "$BUNDLE/rules" .
chmod -R u+rwX,go-rwx config credentials rules
chmod u+rw compose.yml compose.env
mkdir -m 0700 data cache state credentials/maintenance
for image in vaultwarden backup proxy; do docker load -i "$BUNDLE/images/$image.tar.gz"; done
docker image inspect --format '{{.Os}}/{{.Architecture}} {{index .Config.Labels "org.opencontainers.image.version"}}' vaultwarden/server:1.37.5
```

最后一条命令应输出 `linux/amd64 1.37.5`。构建时使用的官方平台 manifest 摘要固定在 `nix/packages.nix`。[升级手册](maintenance.md#更新上游)说明如何用上游配置摘要复核发布包。

Compose 默认以 `65532:65532` 运行。可以在 `compose.yml` 的共享 `user` 中填部署用户的非 root UID:GID，使上面创建的文件直接可用。保留 65532 时，将新部署的 `data`、`cache`、`state`、`rules`、`config`、在线凭据和仓库密码文件的所有权设给该身份。宿主上的维护凭据由实际执行维护命令的管理员读取。

填写 `config/vaultwarden.env` 的 `DOMAIN=https://你的域名`、所需邮件设置、两个目标的凭据及原始仓库密码。TLS 入口只向 `127.0.0.1:8080` 转发，覆盖来自客户端的转发头。Vaultwarden 和备份进程的端口只在 Compose 网络内可见。配置说明见[配置](configuration.md)。

## 初始化与验收

在部署目录定义以下函数。维护凭据文件使用对应模板的仓库地址、区域、容器内密码路径和单独的维护访问密钥。模板的两个示例直接使用环境变量，若配置额外的 restic 后端选项，手工命令也要传入对应的 `-o key=value`。

```sh
dc() { docker compose --env-file "$DEPLOY_DIR/compose.env" -f "$DEPLOY_DIR/compose.yml" "$@"; }
dc config --quiet
dc run --rm --no-deps backup validate
export RESTIC_IMAGE="$(dc config --images backup)"
native() {
  docker run --rm --read-only --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
    --user "$(id -u):$(id -g)" --env-file "$RESTIC_ENV_FILE" \
    --mount "type=bind,source=$DEPLOY_DIR/credentials/restic,target=/run/credentials/restic,readonly" \
    --entrypoint /bin/restic "$RESTIC_IMAGE" --no-cache "$@"
}
for target in aws-s3 backblaze-b2; do
  export RESTIC_ENV_FILE="$DEPLOY_DIR/credentials/maintenance/$target-maintenance.env"
  native init
  native cat config
done
dc up -d
dc exec -T backup /bin/vaultwarden-less-backup status
```

只对已核对为空的新前缀执行 `init`。记录两个不同的原生仓库 ID，重复运行日常启动命令时直接使用原仓库。两个目标应分别显示 `protected` 和自己的完整快照 ID。

## 开通个人账号

开户期间，仅向需要注册的人开放入口。在部署目录的 `config/vaultwarden.env` 中临时设置 `SIGNUPS_ALLOWED=true`，执行 `dc up -d --force-recreate vaultwarden`。每人用自己的邮箱注册并设置主密码。注册完成后，将 `SIGNUPS_ALLOWED` 设回 `false`，保持 `SIGNUPS_DOMAINS_WHITELIST` 为空，再执行同一条命令。以后开通账号可以重复此流程。

每人在自己的 vault 中创建一个能识别的条目。等两个仓库各有一份包含此次写入的快照，再按[恢复步骤](operations.md)逐仓核对。
