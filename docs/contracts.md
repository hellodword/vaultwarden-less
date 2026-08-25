# Stable v1 contracts

This document describes the machine-readable interfaces exposed by the v1
worker. Times are UTC with millisecond precision. Arrays are never null and
are sorted as stated.

## Manifest

```json
{
  "schema_version": 1,
  "vaultwarden_version": "1.37.2",
  "database_migration": "20260505120000",
  "status": "complete",
  "files": [],
  "missing": [],
  "content_id": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
```

The writer records its current Vaultwarden version and the actual highest
14-digit database migration. Readers accept any specification SemVer version
and any 14-digit migration, then require the restored database migration to
match the manifest. Files are limited to `data/db.sqlite3`,
`data/attachments/<uuid>/<path-safe-id>`, `data/rsa_key.pem`, and
`data/rsa_key.pub.pem`.

## Restic candidate identity

Each product candidate has host `vaultwarden-less-v1`, exactly one product tag,
exactly one `complete` or `incomplete` tag, and exactly one
`content-id=<64 lowercase hexadecimal>` tag. Other safe tags and paths are
allowed. The manifest is read with `restic dump <id> /manifest.json`; manifest
status and content ID must match the tags.

`repo snapshots` isolates invalid candidates:

```json
{
  "schema_version": 1,
  "target": "primary",
  "snapshots": [{"id": "...", "time": "...", "status": "complete", "content_id": "sha256:..."}],
  "anomalies": [{"snapshot_id": "...", "error_code": "snapshot_manifest_invalid"}]
}
```

Default restore chooses the latest valid complete candidate. Explicit selection
of an anomaly fails with `snapshot_candidate_invalid`. Any anomaly blocks
prune.

## Restore and prune results

Successful restore returns:

```json
{
  "schema_version": 1,
  "target": "primary",
  "snapshot_id": "...",
  "output": "/restore/prepared",
  "publish_state": "durable",
  "reused_existing_output": false,
  "manifest": {}
}
```

Prune returns its own stable report rather than Restic JSON:

```json
{
  "schema_version": 1,
  "operation": "prune",
  "target": "primary",
  "applied": false,
  "reports": [{
    "status": "complete",
    "kept_count": 10,
    "removed_count": 2,
    "kept_snapshot_ids": [],
    "removed_snapshot_ids": []
  }]
}
```

## Events

The proxy sends the strict body `{"schema_version":1}` with a JSON content
type to `/v1/events`. Notification events use schema version 1 and the event
types `recovery_point_available`, `source_degraded`, `source_recovered`,
`target_degraded`, and `target_recovered`.
