# v1 HTTP, JSON, and CLI contracts

Machine instants are UTC RFC 3339 with exactly millisecond precision, for
example `2026-08-23T12:34:56.789Z`. Logs and errors do not include repository
URLs, passwords, tokens, YAML values, command environments, or raw Restic
output.

## Internal event endpoint

`POST /v1/events` exists only on the Compose `events` network. The request has
no identity claim and no per-request queue; a rate-limited bounded request
merely marks persisted state dirty idempotently.

```http
Content-Type: application/json

{"schema_version":1}
```

Success is `204 No Content`. Unknown fields, multiple values, wrong versions,
wrong media types, and oversized bodies fail. Relevant safe codes include
`invalid_event`, `event_too_large`, `unsupported_media_type`, `rate_limited`,
and `state_unavailable`.

## Health endpoints

Both binaries answer `GET`/`HEAD /.vaultwarden-less/healthz` with `204` while
their local HTTP process is serving. Each container check performs a real
request; no result is latched in `/tmp`.

Repository failure is status, not process health. Restarting a worker cannot
repair backend credentials, capacity, or an outage and would hide its persisted
retry schedule.

## Status JSON

`vaultwarden-less-backup status` writes stable v1 JSON to stdout and
diagnostics to stderr:

```json
{
  "schema_version": 1,
  "dirty": false,
  "backup_in_progress": false,
  "last_sample_at": "2026-08-23T12:30:00.000Z",
  "last_snapshot_at": "2026-08-23T12:30:00.000Z",
  "last_content_id": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "pending_recovery_point": {
    "content_id": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "status": "complete"
  },
  "targets": [
    {
      "name": "primary",
      "state": "degraded",
      "last_attempt_at": "2026-08-23T12:30:00.000Z",
      "next_retry_at": "2026-08-23T12:31:05.000Z",
      "consecutive_failures": 1,
      "error_code": "backup_failed"
    }
  ]
}
```

Optional timestamps, IDs, pending points, and errors are absent when empty.
Targets are sorted by configured name; state is `unknown`, `catching_up`,
`healthy`, or `degraded`. With zero targets, `targets` is an empty array and no
sampling/upload is performed.

## Notification protocols

Enterprise WeChat receives JSON containing `msgtype: "text"` and
`text.content`. Bark receives the equivalent logical message as an APNs alert.
There is no public generic-webhook JSON contract.

Ordinary successes are bounded to 100 persisted items per notifier; overflow
is folded into a merge count. Each target fault/recovery and source-integrity
fault is a replaceable state item retained until delivered or superseded.

## CLI

```text
vaultwarden-less-backup serve
vaultwarden-less-backup run
vaultwarden-less-backup status
vaultwarden-less-backup config [--check-credentials backup|maintenance]
vaultwarden-less-backup repo snapshots|init|check|unlock|prune
vaultwarden-less-backup restore
```

Every public command accepts `--config`; its default is
`/config/backup.yaml`. Primary structured results use stdout. Logs and human
diagnostics use stderr. Exit `0` is success, `2` is command/argument usage
failure, and `1` is configuration, filesystem, data, process, or repository
failure.

`config` validates strict main YAML and emits a secret-free summary:

```json
{
  "schema_version": 1,
  "product_version": "1.0.0",
  "vaultwarden_version": "1.37.2",
  "restic_version": "0.19.1",
  "credentials_checked": "backup",
  "targets": [{"name":"primary","backend":"s3"}],
  "notifiers": [{"name":"operations","type":"wechat"}]
}
```

The credential check reads only the role mounted at the fixed
`/run/credentials/role/credentials.yaml` and the fixed common password. It
does not contact or initialize a repository.

`repo snapshots` accepts `--status all|complete|incomplete`. `repo prune` is a
Restic dry-run unless `--apply` is explicit. `repo check` accepts an optional
`--read-data-subset n/12`. Every repo command requires an exact configured
`--target`.

`restore` requires `--target` and an absolute `--output`. It selects the newest
complete point by default. `--snapshot` must be the full 64-character
lowercase ID; an incomplete point additionally requires
`--allow-incomplete`. Output must be absent or an empty safe directory and can
never be online `/data`.
