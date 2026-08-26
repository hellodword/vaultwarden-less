# Machine-readable contracts

Version 0.1.0 intentionally starts new schema, CLI, Restic, and runtime
contracts. Previous configuration, manifests, control state, and Restic tags
are neither read nor migrated.

## CLI behavior

The backup binary exposes:

```text
serve
request-backup
status
config validate
repo init
repo snapshots
repo check
repo unlock
repo prune [--apply]
restore
healthcheck
--version
```

The proxy exposes `serve`, `healthcheck`, and `--version`. A successful data
command writes exactly one JSON document plus a newline to stdout. Runtime
failure writes no stdout, a safe diagnostic to stderr, and exits 1. Argument
or usage failure similarly writes only stderr and exits 2. Help and version
are text. Long-running services emit JSON logs only to stderr.

`request-backup` returns:

```json
{"schema_version":1,"accepted":true}
```

`repo snapshots` returns newest first, using exactly one Restic metadata query:

```json
{"schema_version":1,"target":"primary","snapshots":[{"id":"64-lowercase-hex","time":"2026-08-25T12:00:00Z","content_id":"sha256:64-lowercase-hex"}]}
```

`repo prune` reports Restic's one forget operation. Dry-run is the default:

```json
{"schema_version":1,"operation":"prune","target":"primary","applied":false,"groups":[{"host":"vaultwarden-less","paths":["/staging/point"],"keep_snapshot_ids":[],"remove_snapshot_ids":[]}]}
```

A successful restore returns:

```json
{"schema_version":1,"target":"primary","snapshot_id":"64-lowercase-hex","content_id":"sha256:64-lowercase-hex","output":"/restore/drill"}
```

## Worker status and HTTP

Status is a process-local snapshot. Arrays are never null; unknown times and
identifiers are JSON `null`. On restart, `last_check` and historical target
fields remain null until the startup check supplies new information.

```json
{
  "schema_version": 1,
  "state": "idle",
  "started_at": "2026-08-25T12:00:00Z",
  "next_check_at": null,
  "last_check": {
    "started_at": "2026-08-25T12:00:30Z",
    "finished_at": "2026-08-25T12:00:40Z",
    "result": "unchanged",
    "content_id": "sha256:64-lowercase-hex",
    "error_code": null
  },
  "targets": [{
    "name": "primary",
    "state": "healthy",
    "last_attempt_at": "2026-08-25T12:00:35Z",
    "last_success_at": "2026-08-25T12:00:39Z",
    "snapshot_id": "64-lowercase-hex",
    "content_id": "sha256:64-lowercase-hex",
    "error_code": null
  }]
}
```

Worker state is `idle`, `debouncing`, or `checking`; target state is
`unknown`, `healthy`, `retrying`, or `degraded`; check result is `unchanged`,
`created`, `partial`, or `failed`.

The private worker HTTP interface is:

- `GET /healthz` returns 204;
- `POST /v1/events` accepts exactly `{"schema_version":1}` and returns 204;
- `POST /v1/check` accepts the same body and returns 202 with the acceptance
  JSON above; and
- `GET /v1/status` returns status JSON.

JSON content type, bounded body size, method, shape, and unknown fields are
strictly enforced with explicit 4xx responses. These endpoints are never
published to the host.

## Recovery manifest

`/staging/point/manifest.json` is strict schema v1:

```json
{
  "schema_version": 1,
  "creator_version": "0.1.0",
  "vaultwarden": {
    "version": "1.37.2",
    "database_migration": "20260505120000",
    "image": "vaultwarden/server:1.37.2@sha256:5d326778c22f063d093d6b0c9c766a28249561632266776f2c93132ab0ad3a80"
  },
  "files": [{"path":"data/db.sqlite3","size":1234,"sha256":"64-lowercase-hex"}],
  "content_id": "sha256:64-lowercase-hex"
}
```

File entries are unique and strictly path-sorted. The content ID is SHA-256
over canonical JSON containing schema version, the exact supported
Vaultwarden tuple, and every path/size/SHA-256 tuple. It excludes time and
creator version. Readers reject unknown fields and accept only the explicit
tuple above; generic SemVer or arbitrary 14-digit migrations are not accepted.

## Restic identity and retention

New recovery snapshots have host `vaultwarden-less`, path
`/staging/point`, product tag `vaultwarden-less-recovery-v1`, and exactly one
`content-id=<64-lowercase-hex>` identity tag. Old product tags are out of scope.

Retention is one Restic `forget --prune`, grouped by `host,paths`, restricted
to the new host/path/product tag, with:

```text
keep-within 24h
keep-hourly 48
keep-daily 14
keep-weekly 8
keep-monthly 12
keep-yearly 3
```

## Notifications

Event types are `recovery_point_created`, `source_degraded`,
`source_recovered`, `target_degraded`, and `target_recovered`. Every payload
has schema version 1 and a non-null `targets` array. A recovery event lists all
configured target outcomes as `created`, `synced`, or `failed`; it is emitted
once per process when that content ID first creates at least one snapshot.
Payloads never contain repository credentials, Restic passwords, webhook
URLs, APNs keys, or Vaultwarden user data.
