# v1 HTTP and JSON contracts

All machine instants are UTC RFC 3339 with exactly millisecond precision, for
example `2026-08-23T12:34:56.789Z`. Structured errors use:

```json
{"error":{"code":"stable_code","message":"Safe message"}}
```

Errors and logs never include repository URLs, passwords, tokens, secret-file
contents, command environments, or raw Restic output.

## Internal event endpoint

`POST /v1/events` exists only on the Compose `events` network. There is no
identity assertion in the payload and no per-request queue; the endpoint
rate-limits a small body and marks the worker state dirty idempotently.

Request:

```http
Content-Type: application/json

{"schema_version":1}
```

Success is `204 No Content`. Unknown fields, multiple JSON values, wrong
versions, non-JSON media types, and oversized bodies are rejected. Relevant
errors include `invalid_event`, `event_too_large`, `unsupported_media_type`,
`rate_limited`, and `state_unavailable`.

## Health endpoints

Both project binaries reserve `GET`/`HEAD
/.vaultwarden-less/healthz` and return `204 No Content` when their local HTTP
process is serving. The checks execute a real request every time; they are not
latched in `/tmp`.

Repository failures do not make this process health endpoint fail. They appear
in status and notifications so that container restarts do not hide a remote
outage.

## Status JSON

`vaultwarden-less-backup status` prints only JSON on stdout. Diagnostics use
stderr. Example:

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
      "content_id": "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
      "snapshot_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "last_attempt_at": "2026-08-23T12:30:00.000Z",
      "last_success_at": "2026-08-23T12:15:00.000Z",
      "next_retry_at": "2026-08-23T12:31:05.000Z",
      "consecutive_failures": 1,
      "error_code": "backup_failed"
    }
  ]
}
```

Optional timestamp, snapshot, pending, and error fields are absent when no
value exists. Target order is lexical by configured name. Target state is one
of `unknown`, `catching_up`, `healthy`, or `degraded`.

## Generic webhook JSON

The generic webhook receives versioned JSON with `Content-Type:
application/json`. Redirects are not followed. A recovery-point event is:

```json
{
  "schema_version": 1,
  "id": "recovery:sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "type": "recovery_point_available",
  "occurred_at": "2026-08-23T12:30:00.000Z",
  "recovery_point": {
    "content_id": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "status": "complete"
  },
  "targets": [
    {"name":"primary","state":"healthy"},
    {"name":"secondary","state":"degraded","error_code":"backup_failed"}
  ],
  "merged_successes": 3
}
```

`merged_successes` is omitted unless bounded-queue overflow was folded into
this delivery. Target changes use `type` `target_degraded` or
`target_recovered` and a single `target` object instead of `recovery_point`
and `targets`.

Enterprise WeChat deliberately uses its required `msgtype=text` shape rather
than this generic envelope. Bark translates the same logical event into an
APNs alert.

## CLI exit and stream contract

The public command tree is:

```text
vaultwarden-less-backup serve
vaultwarden-less-backup run
vaultwarden-less-backup status
vaultwarden-less-backup config [--check-secrets]
vaultwarden-less-backup repo snapshots|init|check|unlock|prune
vaultwarden-less-backup restore
```

Primary structured results go to stdout. Logs and human diagnostics go to
stderr. Exit `0` means success, `2` means a usage/input error, and `1` means a
runtime, data, repository, or environment failure.
