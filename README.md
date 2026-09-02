# vaultwarden-less

`vaultwarden-less` is a Linux/amd64 backup, restore, drill, and explicit-upgrade
system for one audited Vaultwarden release. A transparent loopback proxy turns
successful writes and logins into backup demand. The worker creates a verified
SQLite recovery point, includes only database-referenced attachments and file
Sends, and writes it to one or more Restic repositories.

Build and runtime claims apply only to the exact clean project commit,
immutable release, and upstream component set locked and audited by this
repository. Substituting or independently upgrading Vaultwarden, Restic,
Bitwarden CLI, or another audited component is outside that scope. Compatibility
between a deployed A and a candidate B is an operator-owned acceptance decision,
not a property of the B release gate. See the normative [supported component
set](docs/contracts.md#supported-component-set).

The deployment host needs only Docker Engine, the Docker Compose plugin,
checksum tools, and one complete release bundle. It does not need the project
source tree, Git, Python, Nix, Just, Go, Restic, or a registry.

## Ten-minute local start

Copy the exact Nix-built release bundle into a new versioned directory, verify
all of its files, and keep it read-only. All real credentials and Vaultwarden
state live under a separate explicit `DEPLOYMENT_ROOT`, not in the release.

```sh
vwless_release_dir=/opt/vaultwarden-less/releases/<immutable-release>
(cd "$vwless_release_dir" && sha256sum --check SHA256SUMS)
docker image load --input "$vwless_release_dir/images/backup.tar.gz"
docker image load --input "$vwless_release_dir/images/proxy.tar.gz"
docker image load --input "$vwless_release_dir/images/vaultwarden.tar.gz"
```

Create private deployment inputs. Run these commands as the account whose UID
and GID will run the containers, or change ownership to that identity before
validation.

```sh
vwless_root=/srv/vaultwarden-less
install -d -m 0700 "$vwless_root"
install -d -m 0700 "$vwless_root/config" "$vwless_root/credentials" "$vwless_root/credentials/restic"
install -d -m 0700 "$vwless_root/data" "$vwless_root/import" "$vwless_root/staging" "$vwless_root/cache" "$vwless_root/restore" "$vwless_root/state"
install -m 0600 "$vwless_release_dir/.env.example" "$vwless_root/.env"
install -m 0600 "$vwless_release_dir/config/backup.example.yaml" "$vwless_root/config/backup.yaml"
install -m 0600 "$vwless_release_dir/config/vaultwarden.example.env" "$vwless_root/config/vaultwarden.env"
install -m 0600 "$vwless_release_dir/credentials/backup.example.yaml" "$vwless_root/credentials/backup.yaml"
install -m 0600 "$vwless_release_dir/credentials/maintenance.example.yaml" "$vwless_root/credentials/maintenance.yaml"
install -m 0600 "$vwless_release_dir/credentials/notifiers.example.yaml" "$vwless_root/credentials/notifiers.yaml"
install -m 0600 "$vwless_release_dir/credentials/restic/primary.example" "$vwless_root/credentials/restic/primary"
```

Edit the private copies. In particular:

- `.env` must contain exactly the six documented assignments. Keep the
  release-provided immutable `VWLESS_IMAGE_TAG`, set `DEPLOYMENT_ROOT` to the
  absolute root created above, set `RUNTIME_UID` and `RUNTIME_GID` to the owner
  of every private path, and choose two different unprivileged loopback ports.
- Set `DOMAIN` in `$vwless_root/config/vaultwarden.env` to the public HTTPS origin. Review
  every security-sensitive Vaultwarden setting rather than enabling examples
  blindly.
- Set the repository and provider options in `$vwless_root/config/backup.yaml`, put the same
  target keys in both role credential files, and replace each Restic password.
- Configure the notifier credentials, or set `notifiers: []` in
  `config/backup.yaml` and use `notifiers: {}` in
  `credentials/notifiers.yaml`.

See [Configuration](docs/configuration.md) and [provider recipes](docs/providers.md)
before inserting cloud credentials.

Verify the loaded images against `release.json`. Production Compose uses their
immutable tag directly; it never requires a mutable `local` alias:

```sh
vwless_commit='<full-clean-commit>'
vwless_release='<immutable-release>'
for vwless_product in backup proxy vaultwarden; do
  vwless_immutable="vaultwarden-less-$vwless_product:$vwless_release"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_immutable")" = "$vwless_commit"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_immutable")" = "$vwless_release"
done
```

Validate both isolated credential roles before any repository or application
operation:

```sh
docker compose --env-file "$vwless_root/.env" --file "$vwless_release_dir/compose.yml" --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file "$vwless_root/.env" --file "$vwless_release_dir/compose.yml" --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file "$vwless_root/.env" --file "$vwless_release_dir/compose.yml" --profile deployment run --rm --no-deps deployment-admin deployment init
```

Initialize each new repository once, then start the three production services:

```sh
docker compose --env-file "$vwless_root/.env" --file "$vwless_release_dir/compose.yml" --profile maintenance run --rm --no-deps maintenance repo init --target primary
docker compose --env-file "$vwless_root/.env" --file "$vwless_release_dir/compose.yml" up --detach vaultwarden backup proxy
docker compose --env-file "$vwless_root/.env" --file "$vwless_release_dir/compose.yml" ps
docker compose --env-file "$vwless_root/.env" --file "$vwless_release_dir/compose.yml" exec --no-TTY backup /bin/vaultwarden-less-backup status
```

Only the proxy is published, and only on loopback. Put a trusted TLS edge in
front of it, remove untrusted forwarding headers there, and forward root-path
HTTP and WebSocket traffic. A healthy process is not proof of a completed
backup; confirm durable snapshots and complete a restore drill before relying
on the deployment. The exact procedure is in [Operations](docs/operations.md).

The path above is intentionally backup-first. If no repository is available,
the only supported exception is an explicit `targets: []` bootstrap. It keeps
Vaultwarden and the proxy usable while warning that the data has no recovery
point; manual backup and upgrade preparation remain unavailable. Use the
complete [targetless bootstrap and first-account runbook](docs/operations.md#explicit-targetless-bootstrap-and-first-account)
and its exact [empty configuration](docs/configuration.md#backup-configuration).

## Build and verification host

Development, release creation, and automated acceptance use the pinned Nix
environment. Do not install project tools globally.

```sh
just --list
just check
just check-release
just release
```

`check` is the daily flake gate for formatting, ordinary tests, vet, lint,
scripts, contracts, and pinned toolchain wiring. `check-release` requires clean
HEAD B, then runs full-tree race, vulnerability and Nix product checks, the
targetless deployment, real Moto/Restic and recovery lifecycles, the accelerated
worker threshold, two repositories, and initial import. It is optional and
stateless: success is not consumed by `release`. `release` directly builds the
pure Nix `.#release` package and prints its store path without creating a result
link. Test-log retention, artifact signing, transfer, and A/B compatibility
acceptance remain operator responsibilities.

## Documentation map

- [Architecture](docs/architecture.md): dataflow, networks, recovery identity,
  and ownership boundaries.
- [Product contracts](docs/contracts.md): normative machine behavior.
- [Configuration](docs/configuration.md): private files, modes, identities, and
  examples.
- [Storage providers](docs/providers.md): six S3-compatible provider recipes.
- [Operations](docs/operations.md): backup, status, maintenance, restore, and
  drills.
- [Upgrades and rollback](docs/upgrades.md): prepare, freeze, verify, accept,
  and explicit rollback.
- [Verification](docs/verification.md): what each gate proves and does not
  prove.
- [Upstream maintenance](docs/upstream-maintenance.md): one-component review
  checklists.
- [Licensing and provenance](NOTICE.md): project and Vaultwarden obligations.

The authoritative pins are [`UPSTREAMS.toml`](UPSTREAMS.toml), the complete
accepted SQLite schema is
[`internal/upstream/vaultwarden-schema.json`](internal/upstream/vaultwarden-schema.json),
and [`Justfile`](Justfile) is the exact command index.
