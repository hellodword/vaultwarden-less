# vaultwarden-less 1.0

`vaultwarden-less` is a recovery-first Vaultwarden deployment for a single
self-hosted server. The public proxy can only forward HTTP and coalesce change
events. The resident backup worker alone receives read-only `/data` and the
backup backend role; backup-side one-shot services use a separate maintenance
role and never mount online data.

This is a clean v1 contract. It does not read the old `trigger.json`, SQL/Git
backup scripts, JSON configuration, or old CLI. It also never deletes those
files, an existing `git-backup`, live Vaultwarden data, or old Restic snapshots.

## Supported scope

- Vaultwarden `1.37.2`, SQLite, and attachments, file Sends, RSA files,
  `config.json`, and templates stored locally below `/data`.
- Restic `0.19.1` with an S3-compatible repository (Amazon S3, MinIO, or COS)
  or Restic's native Backblaze B2 backend.
- Enterprise WeChat text notifications and Bark notifications sent directly
  to production APNs.
- `linux/amd64` only. Project images start at `1.0.0`, never use `latest`, and
  are deliberately version-tagged rather than digest-pinned.

PostgreSQL/MySQL, Vaultwarden-managed S3 attachment/Send storage, local and
REST Restic repositories, Azure, GCS, Swift, SFTP, rclone, custom CAs, session
credentials, and generic webhooks are outside v1.

Vaultwarden end-to-end encryption does not protect deployment configuration,
Restic/notification credentials, availability, or rollback. Read the
[architecture and threat model](docs/architecture.md) before deployment.

## Services

| Service | Reachability | Mounts and authority |
| --- | --- | --- |
| `vaultwarden` | internal; reached through `proxy` | read/write `/data`; no backup credentials |
| `proxy` | host `127.0.0.1:8080` by default | no configuration, data, state, cache, or credentials |
| `backup` | private event network plus outbound network | read-only `/data`; backup role; state/staging/cache |
| `maintenance` | one-shot profile, outbound only | private main config, common password, maintenance role, and cache; no `/data` |
| `restore` | one-shot profile, outbound only | private main config, common password, maintenance role, cache, and `./restore`; no online `/data` |

## Prepare a deployment

There is no initialization shell script or duplicated preflight subsystem.
Compose refuses to create missing bind sources, and each process validates the
exact file or directory when it consumes it. It never creates a host mount root
or changes its permissions.

1. Create `.env` from [.env.template](.env.template), and create
   `.env.vaultwarden` from
   [.env.vaultwarden.template](.env.vaultwarden.template). Do not overwrite an
   existing file. Set `PUID`/`PGID` to the intended non-root host owner, set the
   externally visible HTTPS `DOMAIN`, and make both files `0600`.

2. Create these host directories as real, non-symlink directories owned by
   `PUID:PGID` and mode `0700`:

   ```text
   data/  state/  staging/  cache/  restore/
   credentials/common/
   credentials/backup/
   credentials/maintenance/
   ```

3. Create `config/backup.yaml` from
   [config/backup.template.yaml](config/backup.template.yaml), without
   overwriting any existing file, and make it `0600`. Zero targets is valid and
   useful for validating a fresh Vaultwarden instance, but it creates no
   backups.

4. Create the credential files described in
   [configuration](docs/configuration.md). Each must be a real, non-symlink
   `0400` or `0600` file. The three credential directories remain `0700`.

5. Validate the main YAML and each role independently:

   ```sh
   docker compose run --rm --no-deps backup \
     config --config /config/backup.yaml
   docker compose run --rm --no-deps backup \
     config --check-credentials backup --config /config/backup.yaml
   docker compose --profile maintenance run --rm --no-deps maintenance \
     config --check-credentials maintenance --config /config/backup.yaml
   ```

   Output is a secret-free v1 JSON summary. Missing paths and unsafe modes are
   reported against fixed container paths; use the host/container mapping in
   [configuration](docs/configuration.md) to correct the corresponding bind
   source.

6. For each confirmed new, empty repository, initialize it exactly once with
   the maintenance role. The worker never initializes or cleans a repository:

   ```sh
   docker compose --profile maintenance run --rm maintenance \
     repo init --target primary --config /config/backup.yaml
   ```

7. Start the deployment and point local Nginx or Caddy at the loopback port:

   ```sh
   docker compose up --build -d
   ```

   The front proxy must overwrite `X-Real-IP` and `X-Forwarded-Proto`.

Before migrating an existing instance, first exercise registration, login,
sync, a synthetic attachment, backup, and isolated restore on a separate fresh
instance. The parallel-validation and registration procedure is in
[routine operations](docs/operations.md); recovery and manual cutover are in
[restore](docs/restore.md).

## Recovery behavior

The worker uses SQLite's Online Backup API to create a new binary database,
checks it, and derives attachment/Send paths from that fixed database. A
deterministic `manifest.json` records the status, schema, sorted paths, sizes,
SHA-256 values, missing entries, and content ID. Missing referenced files are
retried twice at 30-second intervals; the third set is uploaded as separately
tagged `incomplete` data and alerts immediately after a target accepts it.

`restore` defaults to the newest complete v1 point, verifies Restic output,
manifest hashes, SQLite, and referenced files, and atomically publishes an
isolated prepared directory. It never writes online `/data`.

- [Configuration and credentials](docs/configuration.md)
- [Architecture and threat model](docs/architecture.md)
- [HTTP, JSON, and CLI contracts](docs/api.md)
- [Backup format](docs/backup-format.md)
- [Routine operations](docs/operations.md)
- [Repository maintenance](docs/maintenance.md)
- [Isolated restore and cutover](docs/restore.md)
- [Upgrade policy](docs/upgrade.md)

## Development validation

```sh
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
docker compose --profile maintenance --profile restore config --quiet
```

Builds pin Go `1.26.7`, `go-sqlite3 v1.14.50`, Cobra `v1.10.2`,
goccy/go-yaml `v1.19.2`, and Restic `0.19.1`. Scanner findings must be
attributed to reachable code, image contents, or accepted residual risk; the
project makes no “zero CVE” promise.
