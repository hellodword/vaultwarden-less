# vaultwarden-less

Run and backup vaultwarden rootless, distroless and CVE-less.

## features

- [ ] protect vaultwarden with proxies
  - [ ] Nginx hardening
  - [x] obscurity
- [x] trigger backup via Nginx access log
- [ ] hardening docker images
  - [ ] service:vaultwarden
    - [ ] distroless
    - [x] nonroot
    - [ ] healthcheck
    - [ ] CVE-less
  - [ ] service:nginx
    - [x] distroless
    - [x] nonroot
    - [ ] healthcheck
    - [ ] CVE-less
  - [x] service:trigger
    - [x] distroless
    - [x] nonroot
    - [ ] healthcheck
    - [ ] CVE-less

## how it works

Bitwarden applies all changes to the vaultwarden database, so I prefer to backup on each change. I used `inotifywatch`, it works, but not graceful, and sometimes buggy.

In `vaultwarden-less`, I created a [trigger](./cmd/trigger/main.go) as a reverse proxy between Nginx and vaultwarden. So all the requests that change the database will trigger [scripts/backup](./scripts/backup), and report results via [scripts/notify](./scripts/notify)

I use git, [restic](https://github.com/restic/restic) and [bark](https://github.com/Finb/bark) in the scripts, but you can replace them to anything, and make sure they'll be working with [distroless-trigger](./docker/distroless-trigger.Dockerfile).

The [scripts/backup](./scripts/backup) receives no arguments and the [scripts/notify](./scripts/notify) receives one argument.

> [!CAUTION]
> Currently it's for personal usage, there's a lock in [trigger](./cmd/trigger/main.go), so it won't work well with too much concurrent changes.

## how to use

<details>
<summary><b>
Click this if you're running this on an IPv6-only EC2
</b></summary>

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

   trigger:
+     networks:
+       - wan
+     sysctls:
+       - net.ipv6.conf.all.disable_ipv6=0
     hostname: trigger
     logging:
       driver: "local"
```

</details>

```sh
# clone repo
git clone --depth=1 https://github.com/hellodword/vaultwarden-less && cd vaultwarden-less

# prepare directories and chown for distroless nonroot
# https://github.com/GoogleContainerTools/distroless/blob/64ac73c84c72528d574413fb246161e4d7d32248/common/variables.bzl#L18
mkdir -p data git-backup restic-cache
sudo chown -R 65532:65532 git-backup data restic-cache

cp restic.json.template restic.json
vim restic.conf

cp .env.template .env
vim .env

# ignore this if you don't know what this is
vim obscurity/obscurity.conf

docker compose up --build --pull always -d
```

## ref

- distroless: https://github.com/hellodword/distroless-all
- Nginx hardening
  - https://github.com/trimstray/nginx-admins-handbook/blob/master/doc/RULES.md
- CVE-less
  - https://github.com/aquasecurity/trivy
  - https://docs.docker.com/scout/
  - https://www.chainguard.dev/unchained/migrating-a-node-js-application-to-chainguard-images
  - https://www.chainguard.dev/unchained/reducing-vulnerabilities-in-backstage-with-chainguards-wolfi
  - https://www.chainguard.dev/unchained/zero-cves-and-just-as-fast-chainguards-python-go-images
- httputil.ReverseProxy
  - https://blog.joshsoftware.com/2021/05/25/simple-and-powerful-reverseproxy-in-go/
