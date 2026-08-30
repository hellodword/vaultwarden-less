# vaultwarden-less

`vaultwarden-less` is a Linux/amd64 backup, restore, drill, and explicit-upgrade
system for one audited Vaultwarden release. A transparent loopback proxy turns
successful writes and logins into backup demand. The worker creates a verified
SQLite recovery point, includes only database-referenced attachments and file
Sends, and writes it to one or more Restic repositories.

Compatibility and support claims apply only to the exact clean project commit,
immutable release, and upstream component set locked and audited by this
repository. Substituting or independently upgrading Vaultwarden, Restic,
Bitwarden CLI, or another audited component is outside that scope. Two project
releases with the same actual Vaultwarden version can fully verify the upgrade
orchestrator without leaving a cross-version support gap. See the normative
[supported component set](docs/contracts.md#supported-component-set).

The deployment host needs only Git, Docker Engine, the Docker Compose plugin,
gzip/checksum tools, and a release directory containing `images.tar.gz` and
`SHA256SUMS`. It does not need Python, Nix, Just, Go, Restic, or a registry.

## Ten-minute local start

Start from the exact clean Git commit named by the release producer. Do not
mix a release archive with another commit, and never run `git clean -x` or
`git clean -X` in a deployment clone: the ignored `.env` is deployment input.
All real credentials and Vaultwarden state live under a separate explicit
`DEPLOYMENT_ROOT`, not in the source checkout.

```sh
git clone <repository-url> vaultwarden-less
cd vaultwarden-less
git checkout --detach <full-clean-commit>
test -z "$(git status --porcelain=v1 --untracked-files=all)"
```

Create private deployment inputs. Run these commands as the account whose UID
and GID will run the containers, or change ownership to that identity before
validation.

```sh
vwless_root=/srv/vaultwarden-less
install -d -m 0700 "$vwless_root"
install -d -m 0700 "$vwless_root/config" "$vwless_root/credentials" "$vwless_root/credentials/restic"
install -d -m 0700 "$vwless_root/data" "$vwless_root/import" "$vwless_root/staging" "$vwless_root/cache" "$vwless_root/restore" "$vwless_root/state"
install -m 0600 .env.example .env
install -m 0600 config/backup.example.yaml "$vwless_root/config/backup.yaml"
install -m 0600 config/vaultwarden.example.env "$vwless_root/config/vaultwarden.env"
install -m 0600 credentials/backup.example.yaml "$vwless_root/credentials/backup.yaml"
install -m 0600 credentials/maintenance.example.yaml "$vwless_root/credentials/maintenance.yaml"
install -m 0600 credentials/notifiers.example.yaml "$vwless_root/credentials/notifiers.yaml"
install -m 0600 credentials/restic/primary.example "$vwless_root/credentials/restic/primary"
```

Edit the private copies. In particular:

- `.env` must contain exactly the five documented assignments. Set
  `DEPLOYMENT_ROOT` to the absolute root created above, set `RUNTIME_UID` and
  `RUNTIME_GID` to the owner of every private path, and choose two different
  unprivileged loopback ports.
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

Verify and load the exact two-file release with ordinary host tools:

```sh
(cd <release-directory> && sha256sum --check SHA256SUMS)
gzip --decompress --stdout <release-directory>/images.tar.gz | docker image load
```

Validate both isolated credential roles before any repository or application
operation:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file .env --file compose.yml --profile deployment run --rm --no-deps deployment-admin deployment init
```

Initialize each new repository once, then start the three production services:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo init --target primary
docker compose --env-file .env --file compose.yml up --detach vaultwarden backup proxy
docker compose --env-file .env --file compose.yml ps
docker compose --env-file .env --file compose.yml exec --no-TTY backup /bin/vaultwarden-less-backup status
```

Only the proxy is published, and only on loopback. Put a trusted TLS edge in
front of it, remove untrusted forwarding headers there, and forward root-path
HTTP and WebSocket traffic. A healthy process is not proof of a completed
backup; confirm durable snapshots and complete a restore drill before relying
on the deployment. The exact procedure is in [Operations](docs/operations.md).

## Build and verification host

Development, release creation, and automated acceptance use the pinned Nix
environment. Do not install project tools globally.

```sh
nix develop --command just --list
nix develop --command just check
nix develop --command just check-release <A-full-commit>
nix develop --command just soak
nix develop --command just release <ignored-output-root>
```

`check` is the daily format, ordinary-test, vet, lint, and flake gate; it does
not run full-tree race or real container scenarios. `check-release` requires a
distinct clean A commit and clean HEAD B, then runs the full-tree race,
vulnerability/product/integration checks, lean backup, fast multi-target,
initial-import, and both A/B upgrade lifecycles. `soak` is the separate real
30-minute multi-target degradation gate. Cold-host and same-commit release
reproducibility remain conditional acceptance procedures rather than daily
commands.

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
