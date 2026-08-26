# Operations

All supported project actions start with `nix run .#just --`. The Just recipe
selects either the full pinned development environment or the lightweight
deployment environment. Do not globally install project toolchains or maintain
a second set of release commands.

## Quality gates

```text
generate-check   verify generated upstream constants
fmt / fmt-check  format or check Go, Nix, and Python
test             run Go and initializer tests
race             run Go tests with the race detector
vet               run go vet
lint              run golangci-lint and Ruff
compose-check     render production plus E2E Compose
check             normal local gate plus nix flake check
security          run govulncheck
build             build binaries, patched Restic, and all local images
integration       real patched-Restic/Moto round trip
e2e               real Vaultwarden/Bitwarden backup and restore lifecycle
check-full         every gate above
```

There is no hosted CI or registry release path. A maintainer runs these gates
locally and keeps the resulting evidence with the review.

When the Docker API is remote but its published ports are not directly
reachable, reserve three unused ports on both machines and forward them in a
separate terminal:

```sh
ssh -N \
  -L 45201:127.0.0.1:45201 \
  -L 45202:127.0.0.1:45202 \
  -L 45203:127.0.0.1:45203 user@docker-host
```

Then keep the Docker API remote while the tests reach the forwarded ports via
local loopback:

```sh
DOCKER_HOST=tcp://docker-host:2375 nix run .#just -- integration 45201
DOCKER_HOST=tcp://docker-host:2375 nix run .#just -- e2e 45202 45203
```

With a local Docker daemon, omit the port arguments; Docker chooses ephemeral
loopback ports. The fixed ports are a test transport detail, never production
service exposure.

The E2E deliberately uses the production 30-second quiet window and five-minute
minimum check-start interval. Its four recovery points therefore take roughly
20–30 minutes on a healthy machine; a shorter run would not exercise the
shipped scheduler policy.

## Offline image transfer

Build images on a capable Linux/amd64 machine. Do not compile the product or
Restic on a slow deployment device.

On the build machine, from a clean committed worktree:

```sh
nix run .#just -- check-full
nix run .#just -- bundle
```

`bundle` builds the backup and proxy images with Nix, builds the patched
Restic executable into the backup image, pulls Vaultwarden by its exact audited
digest, applies only a local tag, and runs `docker save` for all three images.
It also records image IDs, checksums, source, license material, and CycloneDX
SBOMs. Nothing is pushed.

Copy the entire resulting directory using removable media or a transport such
as:

```sh
scp -r dist/vaultwarden-less-<version>-linux-amd64 deploy-host:/srv/
```

On the deployment device:

```sh
nix run .#just -- bundle-load /srv/vaultwarden-less-<version>-linux-amd64
```

That command checks `images.tar.sha256`, runs `docker load`, and verifies
every loaded image ID. It enters only `devShells.deploy`, which contains
Docker, Compose, and Python; it does not depend on the locally built product or
Restic derivations.

The portable mechanism underneath the recipe is deliberately ordinary Docker:

```sh
# Performed by bundle creation on the build machine:
docker save --output images.tar vaultwarden-less-backup:<version> \
  vaultwarden-less-proxy:<version> vaultwarden-less-vaultwarden:<audited-local-tag>

# Performed after checksum verification on the deployment device:
docker load --input images.tar
```

For an air-gapped target, make the lightweight Nix deployment-shell closure
available in advance. The same load boundary can be inspected directly with
`nix develop .#deploy --command docker load --input images.tar`, followed by
`bundle-verify`. The image archive avoids all product compilation; it does not
include Nix inputs or a Docker daemon.

A bundle never contains `.env`, application settings, credentials, passwords,
volumes, databases, staging, or restored data. Transfer and back up those
separately.

## Initial deployment

Use the source tree from the same commit recorded in `bundle.json` (the bundle
also contains `source.tar`). On the deployment host filesystem:

```sh
nix run .#just -- init
```

Edit:

```text
config/vaultwarden/vaultwarden.env
config/backup/backup.yaml
credentials/backup/credentials.yaml
credentials/maintenance/credentials.yaml
credentials/restic/<target>
credentials/notifiers/credentials.yaml
```

Then validate and start:

```sh
nix run .#just -- config-validate
nix run .#just -- repo-init primary
nix run .#just -- up
nix run .#just -- ps
nix run .#just -- logs
```

`init` is safe to repeat but intentionally refuses wrong ownership, modes,
symlinks, and existing non-private files. It does not update an old layout.

The Compose bind paths in `.env` refer to the Docker host. If a Docker client
uses `DOCKER_HOST` to reach another machine, image loading can be remote, but
`init` still changes the local filesystem. For a real bind-mounted deployment,
run initialization and operations on the deployment host (for example through
SSH) or otherwise provision the exact remote paths and ownership explicitly.

Only the proxy is published, at `127.0.0.1:8080` by default. Terminate TLS at a
trusted edge, remove any external path prefix, discard untrusted forwarding
headers, and forward root-path HTTP and WebSocket traffic. `down` stops the
stack without deleting source, staging, restore, or repository data.

## Backup health

```sh
nix run .#just -- request-backup
nix run .#just -- status
nix run .#just -- repo-snapshots primary 100
```

`request-backup` acknowledges an in-memory demand; it does not claim that a
snapshot exists. Health endpoints prove only that the relevant HTTP process is
serving. They do not test SQLite, S3, Restic, or notifiers.

The scheduler can delay a request for quieting and the minimum check-start
interval. Process-local status disappears on restart. `repo-snapshots` is the
durable repository view and is intentionally bounded; raise the limit up to
1000 when auditing older history.

Structured logs are safe metadata, not a data backup. Restic logs exclude
arguments, stderr, locators, and secrets.

## Repository maintenance

```sh
nix run .#just -- repo-snapshots primary 100
nix run .#just -- repo-check primary
nix run .#just -- repo-check primary 1/12
nix run .#just -- repo-unlock primary
nix run .#just -- repo-prune primary
nix run .#just -- repo-prune-apply primary
```

The first prune is a dry run. Only `repo-prune-apply` deletes matching
product-v2 snapshots according to fixed retention. It cannot select an older
product tag. Deleting any out-of-scope history is a separate, explicit
operator operation.

Repository targets run serially. A slow first target delays later targets.
After an outage, a target receives the newest point rather than every
intermediate point.

## Restore

Restore publishes only a verified absent output:

```sh
nix run .#just -- restore primary /restore/prepared
nix run .#just -- restore-snapshot <full-64-hex-id> primary /restore/older
```

The default resolves the latest product-v2 snapshot. If that snapshot is
corrupt or incompatible, restore fails; it never silently falls back. Select an
older full ID explicitly.

The output and every parent must satisfy the private-directory contract. Restic
writes a temporary sibling, complete verification runs, and an atomic
no-replace rename publishes it. A failure removes the temporary sibling and
leaves the requested output absent. The tool never overwrites or resets real
restore data.

## Recovery drill

Point `DRILL_RECOVERY_PATH` in `.env` at a verified restore directory and
run:

```sh
nix run .#just -- drill-up
nix run .#just -- drill-status
nix run .#just -- drill-down
```

Each `drill-up` resets only the separate drill Compose project's disposable
copy volume, copies verified `data` from the read-only restore, and starts the
same pinned Vaultwarden image on isolated networks and a loopback port.
`drill-down` deletes only that disposable volume. It does not change or delete
the verified restore.

Use fresh sessions to verify login, items, folders, attachments, file Sends and
their access limits, organizations, and collections. The automated E2E covers
the supported API/CLI paths, but an operator drill is still required after
initial deployment, every Vaultwarden upgrade, storage changes, and at a
locally chosen periodic interval.

Because RSA state is excluded, expect old access, refresh, invite, attachment,
and download tokens to fail after recovery.

## Failure boundaries

- The worker sees live `/data` read-only and never pauses or deletes production
  Vaultwarden data.
- A changing referenced payload prevents publication and preserves the previous
  recovery point.
- Staging can be rebuilt and is not scheduler state.
- Target histories can differ after outages.
- Notification queues are best effort and disappear at shutdown.
- Restore and drill never authorize deletion of a real destination.
- Image bundles prove transferred image bytes and IDs, not the correctness of
  deployment secrets or external S3 behavior.

For upstream changes, follow
[the one-component review procedure](upstream-maintenance.md) rather than
editing pins opportunistically.
