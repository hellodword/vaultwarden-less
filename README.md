# vaultwarden-less

Run and backup vaultwarden rootless, distroless and CVE-less.

## features

- [x] trigger backup on change
- [ ] hardening docker images
  - [ ] service:vaultwarden
    - [ ] distroless
    - [x] nonroot
    - [x] healthcheck
    - [ ] CVE-less
  - [x] service:trigger
    - [x] ~~distroless~~ (maybe, because I use bash scripts in it, but I distroless it for fun)
    - [x] nonroot
    - [x] healthcheck
    - [ ] CVE-less (hard, becase I use prebuilt restic in it, I can compile it with the latest Go version, but the dependencies are always vulnerable)

## how it works

Bitwarden applies all changes to the vaultwarden database, making it possible to backup on each change. I used `inotifywatch`, it works, but it's not graceful and can be buggy at times.

In `vaultwarden-less`, I created a [trigger](./cmd/trigger/main.go) that acts as a reverse proxy before vaultwarden. This way, all requests that change the database trigger the [scripts/backup](./scripts/backup), and report results via the [scripts/notify](./scripts/notify)

> [!CAUTION]
> Currently, this setup is for personal usage. There is a lock in the [trigger](./cmd/trigger/main.go), so it doesn't handle too many concurrent changes well.

## how to use

<details>
<summary><b>
Click if you're running this on an IPv6-only EC2
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

```

</details>

<details>
<summary><b>
Click if you don't trust ghcr.io and want to build the images by yourself
</b></summary>

Edit the `docker-compose.yml`:

```diff
           memory: 128M
-    image: ghcr.io/hellodword/vaultwarden-less-trigger:latest
-    # build:
-    #   context: .
-    #   dockerfile: ./docker/distroless-trigger.Dockerfile
+    # image: ghcr.io/hellodword/vaultwarden-less-trigger:latest
+    build:
+      context: .
+      dockerfile: ./docker/distroless-trigger.Dockerfile
     env_file:
```

</details>

---

1. clone repo

```sh
git clone --depth=1 https://github.com/hellodword/vaultwarden-less

cd vaultwarden-less
```

2. prepare directories and chown for nonroot distroless container

```sh
mkdir -p data git-backup restic-cache
sudo chown -R 65532:65532 git-backup data restic-cache
```

3. replace the [scripts/backup](./scripts/backup) and [scripts/notify](./scripts/notify) with your own scripts or executables

I use git, [restic](https://github.com/restic/restic) and [bark](https://github.com/Finb/bark) in my scripts, but you can replace them to anything, and make sure they'll be working with [distroless-trigger](./docker/distroless-trigger.Dockerfile).

The [scripts/backup](./scripts/backup) receives no arguments and should be secure (DoS not considered). The [scripts/notify](./scripts/notify) receives one argument, which is the notification message, although I format the URIs in the source code, **but you should still be cautious**.

4. customize the vaultwarden features

> > See https://github.com/dani-garcia/vaultwarden/blob/main/.env.template

```sh
vim .env.vaultwarden
```

5. customize the trigger configuration

> > edit the `exclude_path`, see the regexp syntax https://pkg.go.dev/regexp/syntax

```sh
vim config/trigger.json
```

6. expose the trigger (`127.0.0.1:8080`) to the world

I'm using Nginx and Cloudflare, but you can use any tools and services you prefer.

7. start

```sh
docker compose down -t 360

docker compose up --build --pull always -d
```

## ref

- distroless: https://github.com/hellodword/distroless-all
- CVE-less
  - https://github.com/aquasecurity/trivy
  - https://docs.docker.com/scout/
  - https://www.chainguard.dev/unchained/migrating-a-node-js-application-to-chainguard-images
  - https://www.chainguard.dev/unchained/reducing-vulnerabilities-in-backstage-with-chainguards-wolfi
  - https://www.chainguard.dev/unchained/zero-cves-and-just-as-fast-chainguards-python-go-images
- httputil.ReverseProxy
  - https://blog.joshsoftware.com/2021/05/25/simple-and-powerful-reverseproxy-in-go/
