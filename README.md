# vaultwarden-less

Run and backup vaultwarden rootless, distroless and CVE-less.

## features

- [x] protect vaultwarden with proxies
  - [x] Nginx hardening
  - [x] obscurity
- [x] trigger backup via Nginx access log
  - [x] filtering
  - [x] configurable
- [ ] hardening docker images
  - [ ] service:vaultwarden
    - [ ] distroless
    - [x] nonroot
  - [x] service:nginx
    - [x] distroless
    - [x] nonroot
  - [x] service:syslog-parser
    - [x] distroless
    - [x] nonroot

## how it works

Bitwarden applies all changes to the vaultwarden database, so I prefer to backup on each change. I used `inotifywatch`, it works, but not graceful, and sometimes buggy.

In `vaultwarden-less`, I created a [syslog-parser](./cmd/syslog-parser/main.go) to receive access_log (`status`, `request_method`, `request_uri`) from Nginx[^1], cool!

All the requests that change the database will trigger 3 backup stages:

1. [backup-to-local](./scripts/backup-to-local) dumps the database to a SQL file (BTW I don't use `sends` or `attachments` and I don't care about the `rsa_key` or `config.json`) and commit it to a local git repo[^2]
2. [backup-to-remote](./scripts/backup-to-remote) syncs the local git repo to remote S3 storages via `rclone`
3. [notify](./scripts/notify) sends notifications to me, I use [bark](https://github.com/Finb/bark)

You can replace them to anything, but make sure they'll be working with [distroless-syslog-parser](./docker/distroless-syslog-parser.Dockerfile).

> [!CAUTION]
> Currently it's for personal usage, there's a lock in [syslog-parser](./cmd/syslog-parser/main.go), so it won't work well with too much concurrent changes.

## how to use

<details><summary>Click to see configurations for IPv6-only VPS</summary>

```sh
# enable IPv6 support of docker
# https://docs.docker.com/config/daemon/ipv6/
sudo vim /etc/docker/daemon.json
# {
#   "ipv6": true,
#   "fixed-cidr-v6": "2001:db8:1::/64",
#   "experimental": true,
#   "ip6tables": true
# }
sudo systemctl restart docker

# enable GitHub/ghcr.io IPv6 proxy (shame on you GitHub!)
# https://danwin1210.de/github-ipv6-proxy.php
vim /etc/hosts
# 2a01:4f8:c010:d56::2 github.com
# 2a01:4f8:c010:d56::3 api.github.com
# 2a01:4f8:c010:d56::4 codeload.github.com
# 2a01:4f8:c010:d56::5 objects.githubusercontent.com
# 2a01:4f8:c010:d56::6 ghcr.io
# 2a01:4f8:c010:d56::7 pkg.github.com npm.pkg.github.com maven.pkg.github.com nuget.pkg.github.com rubygems.pkg.github.com
```

Edit the `docker-compose.yml`

```diff
+ networks:
+   wan:
+     enable_ipv6: true
+     driver: bridge
+     ipam:
+       config:
+         - subnet: 192.168.234.0/24
+         - subnet: fd5f:c26e:7746:f664::/64


   vaultwarden:
+     networks:
+       - wan
+     sysctls:
+       - net.ipv6.conf.all.disable_ipv6=1
     hostname: vaultwarden
     logging:
       driver: "local"


   nginx:
+     networks:
+       - wan
+     sysctls:
+       - net.ipv6.conf.all.disable_ipv6=1
     logging:
       driver: "local"
       options:

   syslog-parser:
+     networks:
+       - wan
+     sysctls:
+       - net.ipv6.conf.all.disable_ipv6=0
     hostname: syslog-parser
     logging:
       driver: "local"
```

</details>

```sh
# clone repo
git clone --depth=1 https://github.com/hellodword/vaultwarden-less && cd vaultwarden-less

# prepare directories and chown for distroless nonroot
# https://github.com/GoogleContainerTools/distroless/blob/64ac73c84c72528d574413fb246161e4d7d32248/common/variables.bzl#L18
mkdir -p data git-backup
sudo chown -R 65532:65532 git-backup data

cp rclone.conf.template rclone.conf
vim rclone.conf

cp .env.template .env
vim .env

# ignore this if you don't know what this is
vim obscurity/obscurity.conf

docker compose up --build --pull always -d
```

## ref

- rclone alias
  - https://github.com/rclone/rclone/issues/4862
  - https://forum.rclone.org/t/specify-bucket-or-bucket-and-sub-directory-for-s3-in-config-file/29888/3
- librclone
  - https://github.com/rclone/rclone/issues/361#issuecomment-1611890274
- distroless
  - https://github.com/TheProjectAurora/distroless-nginx/blob/cd36b3fb754dd31e20303dbe9ddd45afb7091fbf/Dockerfile
  - https://github.com/kyos0109/nginx-distroless/blob/4fa36b8c066303f34e490aad7b407d447ade4b7d/Dockerfile
- Nginx hardening
  - https://github.com/trimstray/nginx-admins-handbook/blob/master/doc/RULES.md

[^1]: https://docs.nginx.com/nginx/admin-guide/monitoring/logging/#logging-to-syslog
[^2]: https://news.ycombinator.com/item?id=38110286
