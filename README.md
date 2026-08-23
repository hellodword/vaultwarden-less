# vaultwarden-less 1.0

`vaultwarden-less` is a small, recovery-first deployment for general
self-hosted Vaultwarden users. Version 1.0 separates the public reverse proxy
from the private backup worker so that the public process has neither `/data`
nor Restic or notification credentials.

This is a new v1 contract. It does not read the old `trigger.json`, SQL/Git
backup history, backup scripts, or CLI. Upgrading the repository does not
delete those files, existing Vaultwarden data, or old Restic snapshots.

## Supported scope

- Vaultwarden `1.37.2`, SQLite, and files stored locally under `/data`.
- Local attachments, file Sends, `rsa_key*`, `/data/config.json`, and custom
  templates. Vaultwarden PostgreSQL/MySQL and Vaultwarden S3/MinIO attachment
  or Send storage are not supported.
- Restic `0.19.1` native local, REST, S3/MinIO/COS, B2, Azure, GCS, and Swift
  backends. SFTP and rclone are intentionally absent from the image.
- `linux/amd64` project images, starting at tag `1.0.0`. Other architectures
  are outside the current support and release-validation scope.

The image references use version tags rather than immutable digests. That is
an accepted availability and supply-chain risk; read [the threat
model](docs/architecture.md) before deployment.

The tracked Vaultwarden environment fixes `DATA_FOLDER`, `DATABASE_URL`, and
every recoverable asset folder to local `/data` paths. Do not override them with
database servers or `s3://` storage; those deployments are outside this
recovery contract.

## Services

| Service | Public reachability | Data and credentials |
| --- | --- | --- |
| `vaultwarden` | only through `proxy` | read/write `/data`; no backup credentials |
| `proxy` | `127.0.0.1:8080` by default | no data, state, cache, or secrets |
| `backup` | private event network plus outbound network | read-only `/data`; private state, staging, cache, optional local repositories, and secrets |

Maintenance and restore are one-shot Compose profiles. Maintenance never
mounts `/data`; restore writes only beneath `./restore`.

## Start a new deployment

1. Initialize private paths. The script records the current host UID/GID and
   never overwrites an existing file:

   ```sh
   scripts/init
   ```

2. Set the externally visible HTTPS `DOMAIN` in `.env`. Populate every
   required file under `secrets/`, keep it mode `0600`, and adjust
   [backup.json](config/backup.json) for the selected repository. Store the
   repository address, Restic password, and backend credentials in those
   files—not in JSON or command arguments.

3. Validate configuration and mounted secrets:

   ```sh
   docker compose --profile maintenance run --rm maintenance \
     config --check-secrets --config /config/backup.json
   ```

4. Explicitly initialize a new repository once. The long-running worker never
   runs `init`, `unlock`, `forget`, `prune`, or `check`:

   ```sh
   docker compose --profile maintenance run --rm maintenance \
     repo init --target primary --config /config/backup.json
   ```

5. Start the three long-running services:

   ```sh
   docker compose up --build -d
   ```

6. Point a local Nginx or Caddy listener at `http://127.0.0.1:8080`. The front
   proxy must overwrite `X-Real-IP` and `X-Forwarded-Proto`; configure its
   actual source CIDR in [proxy.json](config/proxy.json).

For the one-time first-account flow, routine checks, and failure semantics,
read [operations](docs/operations.md). Registration must be returned to
`false` immediately after the first account is created.

## Recovery and maintenance

The worker creates a new binary SQLite database with the SQLite Online Backup
API, validates it, then derives the file set from that snapshot database. A
versioned deterministic `manifest.json` binds every copied file to its size and
SHA-256. Missing referenced files are retried twice at 30-second intervals;
the third attempt becomes a separately tagged `incomplete` recovery point and
alerts immediately after any target accepts it.

- [Configuration](docs/configuration.md)
- [Architecture and threat model](docs/architecture.md)
- [1.0.0 security validation record](docs/security-validation.md)
- [HTTP and JSON contracts](docs/api.md)
- [Backup format](docs/backup-format.md)
- [Routine operations](docs/operations.md)
- [Repository maintenance](docs/maintenance.md)
- [Isolated restore and cutover](docs/restore.md)
- [Upgrade policy](docs/upgrade.md)

`restore` defaults to the newest `complete` v1 snapshot, verifies Restic,
manifest hashes, SQLite, and database file references, and publishes an
isolated prepared directory atomically. It never writes the online `/data`.

## Development validation

The required local checks are:

```sh
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
shellcheck scripts/init
docker compose --env-file .env.template config --quiet
```

Image builds use Go `1.26.7`, `go-sqlite3 v1.14.50`, Cobra `v1.10.2`, and copy
the Restic binary from `ghcr.io/restic/restic:0.19.1`. Scanner findings must be
attributed to reachable code, base-image packages, or accepted residual risk;
this project makes no “zero CVE” promise.
