# Contracts

This release uses product-v2 configuration, manifest, worker HTTP, and Restic
contracts. Each durable format is versioned independently.

A future breaking change must increment the specific contract it changes. Do
not reuse one global version constant for unrelated formats.

## Deployment surface

The supported outer interface is `nix run .#just -- <recipe>`. Nix selects all
Go, Python, formatting, Docker, Bitwarden CLI, and Restic tools. Direct host
tooling and alternate release scripts are not supported.

Production and drill use fixed, separate Compose project names. The supported
deployment cardinality is one vaultwarden-less instance per Docker daemon.

`config/deployment.yaml` is strict schema 1 with exactly `runtime_uid`,
`runtime_gid`, `listen_port`, and `drill_port` in addition to its schema field.
The renderer accepts no unknown/duplicate keys, booleans as integers, extra
documents, undeclared template tokens, or Compose environment interpolation.

Restore and drill public names match
`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` and map beneath `/restore`; callers do not
provide arbitrary container paths. Restore never overwrites an existing name.

## CLI

The backup binary exposes:

```text
serve
request-backup
status
config validate
repo init
repo snapshots [--limit 1..1000]
repo check
repo unlock
repo prune [--apply]
restore
healthcheck
--version
```

The proxy exposes `serve`, `healthcheck`, and `--version`.

A successful data command writes one JSON document plus a newline to stdout.
Runtime failure writes no stdout, a safe diagnostic to stderr, and exits 1.
Argument, unknown-command, and flag failure write only stderr and exit 2. Help
and version are text. Services and Restic audit records are JSON logs on
stderr.

`request-backup` returns:

```json
{"schema_version":2,"accepted":true}
```

`repo snapshots --limit 100` performs one bounded metadata query and returns
newest first. Times are normalized to UTC. `truncated` is true when more
matching snapshots exist:

```json
{"schema_version":2,"target":"primary","limit":100,"truncated":false,"snapshots":[{"id":"<64-lowercase-hex>","time":"2026-08-25T12:00:00Z","content_id":"sha256:<64-lowercase-hex>"}]}
```

`repo prune` describes one Restic forget operation; dry-run is the default:

```json
{"schema_version":2,"operation":"prune","target":"primary","applied":false,"groups":[{"host":"vaultwarden-less","paths":["/staging/point"],"keep_snapshot_ids":[],"remove_snapshot_ids":[]}]}
```

A successful restore returns:

```json
{"schema_version":2,"target":"primary","snapshot_id":"<64-lowercase-hex>","content_id":"sha256:<64-lowercase-hex>","output":"/restore/prepared"}
```

## Worker status and HTTP

Status is process-local. Arrays are never null; unavailable times and
identifiers are JSON `null`:

```json
{
  "schema_version": 2,
  "state": "idle",
  "started_at": "2026-08-25T12:00:00Z",
  "next_check_at": null,
  "last_check": {
    "started_at": "2026-08-25T12:00:30Z",
    "finished_at": "2026-08-25T12:00:40Z",
    "result": "unchanged",
    "content_id": "sha256:<64-lowercase-hex>",
    "error_code": null
  },
  "targets": [{
    "name": "primary",
    "state": "healthy",
    "last_attempt_at": "2026-08-25T12:00:35Z",
    "last_success_at": "2026-08-25T12:00:39Z",
    "snapshot_id": "<64-lowercase-hex>",
    "content_id": "sha256:<64-lowercase-hex>",
    "error_code": null
  }]
}
```

Worker state is `idle`, `debouncing`, or `checking`. Target state is
`unknown`, `healthy`, `retrying`, or `degraded`. Check result is
`unchanged`, `created`, `partial`, or `failed`.

The private interface is:

- `GET /healthz`: empty body and query only; returns 204.
- `GET /v1/status`: empty body and query only; returns status JSON.
- `POST /v1/events`: exact `Content-Type: application/json` and exact body
  `{"schema_version":2}`; returns 204.
- `POST /v1/check`: the same request contract; returns 202 and the acceptance
  JSON shown above.

Methods, query strings, media-type parameters, unknown fields, trailing JSON,
and bodies over 1024 bytes receive explicit 4xx responses. The endpoints are
never published to the host.

## Recovery manifest

`/staging/point/manifest.json` is strict schema 2:

```json
{
  "schema_version": 2,
  "creator_version": "<product-version>",
  "vaultwarden": {
    "version": "<audited-version>",
    "database_migration": "<audited-latest-migration>",
    "database_schema_fingerprint": "sha256:<64-lowercase-hex>",
    "image": "<audited-repository>@sha256:<64-lowercase-hex>"
  },
  "files": [
    {
      "path": "data/db.sqlite3",
      "size": 1234,
      "sha256": "<64-lowercase-hex>"
    }
  ],
  "content_id": "sha256:<64-lowercase-hex>"
}
```

The exact accepted tuple comes from generated constants derived from
`UPSTREAMS.toml` and the complete schema contract. File entries are unique and
strictly path-sorted. The content ID is SHA-256 over canonical JSON containing
manifest schema, the complete Vaultwarden tuple, and every
path/size/SHA-256 tuple. It excludes creation time and creator version.

Readers reject unknown fields, extra JSON, noncanonical paths, missing database,
and any tuple not explicitly compiled into the product.

## Restic identity and retention

Recovery snapshots have:

```text
host: vaultwarden-less
path: /staging/point
tag: vaultwarden-less-recovery-v2
identity tag: content-id=<64-lowercase-hex>
```

Selection, retention, and deletion always require the fixed host, path, and
product tag. Explicit snapshot selection requires the full 64-character ID.

Retention is one `forget --prune`, grouped by `host,paths`, with:

```text
keep-within 24h
keep-hourly 48
keep-daily 14
keep-weekly 8
keep-monthly 12
keep-yearly 3
```

## Notifications

Enterprise WeChat and Bark notifications are rendered directly from in-memory
backup outcomes; there is no notification JSON or persisted event format. A
recovery message is sent after any check in which at least one target creates a
snapshot and lists every configured target as `created`, `synced`, or `failed`.
A later target catch-up can therefore produce another message for an existing
content ID. Source and target degraded/recovered messages are emitted once per
continuous in-process degraded interval and once on recovery.

Messages contain only safe target names, error codes, and shortened content
identifiers. They contain no repository locator or credential, Restic password,
notifier endpoint/key, stderr, or Vaultwarden record data.

## Offline bundle

The deployment bundle uses its independent `bundle_schema_version: 1`. A bundle
directory contains:

```text
bundle.json
images.tar
images.tar.sha256
source.tar
licenses.tar
licenses.json
sbom/backup.cdx.json
sbom/proxy.cdx.json
sbom/vaultwarden.cdx.json
```

Creation requires a clean Git commit and Linux/amd64. `bundle.json` records
the product commit, flake-lock hash, platform, a complete `UPSTREAMS.toml`
snapshot, exact local image tags, image IDs, and the original Vaultwarden
digest. Loading first verifies the SHA-256, uses `docker load`, and then
compares every loaded image ID with metadata.

The bundle contains images and review evidence only. It never contains
deployment configuration, Vaultwarden settings, role credentials, Restic
passwords, notifier credentials, volumes, databases, restore outputs, or user
data.
