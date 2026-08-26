# Architecture

## Runtime flow

```text
trusted Nginx/CDN boundary
          │ root-path HTTP/WebSocket
          ▼
transparent proxy ── coalesced in-memory signal ──► worker scheduler
          │                                          │
          ▼                                          │ SQLite online backup
Vaultwarden 1.37.2                                   ▼
                                             /staging/point
                                                   │ serial Restic backup
                                                   ▼
                                             ordered S3 targets
```

The proxy preserves the upstream response status, headers, body, streaming,
and WebSocket behavior. After a 2xx response, a non-read method requests a
check unless its exact external route is in the audited Vaultwarden 1.37.2
read-like POST/PUT table. Query strings are irrelevant, and path parameters
match one non-empty segment. GET, HEAD, OPTIONS, TRACE, and all 3xx/4xx/5xx
responses do not signal.

Event delivery is independent of the Vaultwarden request goroutine. One
pending bit is set at proxy startup and by every qualifying response. Failed
worker delivery retries after 30 seconds. A signal received during a send is
retained for the next round. Event failures can neither alter proxy responses
nor make proxy liveness fail.

The proxy accepts and forwards the deployment's `Forwarded`, `X-Real-IP`, and
`X-Forwarded-*` values. It does not infer trust from private peer addresses.
The preceding Nginx/CDN is therefore the mandatory header-cleaning boundary.
Only root-path deployment is supported; an external prefix such as `/vault`
must be stripped before proxying.

The exact Vaultwarden 1.37.2 ignore table is:

- `POST /identity/connect/token`
- `POST /identity/accounts/prelogin`
- `POST /identity/accounts/prelogin/password`
- `POST /api/accounts/prelogin`
- `POST /api/two-factor/send-email-login`
- `POST /api/accounts/request-otp`
- `POST /api/accounts/verify-otp`
- `POST|PUT /api/devices/identifier/{device_id}/token`
- `POST|PUT /api/devices/identifier/{device_id}/clear-token`
- `POST /api/auth-requests`
- `PUT /api/auth-requests/{auth_request_id}`
- `POST /events/collect`
- `POST /api/sends/access`
- `POST /api/sends/access/{access_id}`
- `POST /api/sends/access/file/{file_id}`
- `POST /api/sends/{send_id}/access/file/{file_id}`
- `POST /api/organizations/domain/sso/verified`
- `POST /api/organizations/{org_id}/users/public-keys`

Each implementation entry records its 1.37.2 source location and reason.
Unlisted read-like POSTs conservatively request an extra check; content IDs
prevent duplicate snapshots.

## In-memory scheduler

The worker has one serial scheduling loop and no control database:

- startup, proxy events, and `request-backup` create the same one-bit demand;
- the first demand starts a 30-second quiet window, reset by later demands but
  capped at two minutes from the first demand;
- check start times are at least five minutes apart;
- demand received during a check is retained;
- after a successful check, a no-demand fallback is due in 12 hours;
- missing or changing referenced files retry in 30 seconds;
- other source or target failures retry in five minutes; and
- a continuous source or target failure becomes degraded at 30 minutes and
  emits one transition notification. Recovery emits one further notification.

Targets run serially in configuration order. A recovered target receives only
the newest point at that time; intermediate history is not replayed. Restart
loses dirty, retry, transition, and notification memory. The startup and
12-hour checks are the explicit recovery mechanisms.

## Recovery point transaction

The source `/data` and staging directory must be real, non-symlink, mode 0700
directories owned by the service UID. A build occurs under
`/staging/.build-*`:

1. SQLite online backup copies `db.sqlite3`; the copy is normalized to DELETE
   journal mode and must pass `integrity_check` and `foreign_key_check`.
2. Required tables, columns, and the exact supported migration are checked.
3. Attachment and file Send references are queried and sorted. UUIDs, IDs,
   sizes, and derived paths are strictly validated.
4. Each source payload must remain the same non-symlink regular file across
   open and copy, including device, inode, size, and modification time.
5. A strict manifest is written and the complete tree is re-derived from its
   database. Paths, sizes, hashes, and the absence of extra files are checked.
6. Only the verified build is renamed to `/staging/point`. A failed build is
   removed and never published.

Staging is reproducible scratch state, not a crash-durability protocol. A
process restart rebuilds it. Unknown staging entries are never automatically
deleted.

## Restic and notifications

For each target, the worker performs one latest-metadata query scoped to the
fixed host, path, and product tag. Matching content means no upload. Otherwise
one `restic backup --json` creates the snapshot. Restic's returned full
snapshot ID is the success evidence; stderr text is not parsed into product
decisions.

Notifications are best effort. A bounded in-memory queue tries each notifier
immediately, after one minute, and after five minutes. Queue overflow and send
failure are safely logged and cannot change backup success. Pending events are
cancelled at shutdown and are not replayed after restart.

## Compose isolation

- `app`: internal Vaultwarden-to-proxy data plane.
- `events`: internal proxy-to-worker control plane.
- `proxy-ingress`: loopback publication without IP masquerading.
- `backup-outbound`: resident worker repository/notification egress.
- `maintenance-outbound`: one-shot maintenance and restore egress.
- drill networks: a private restored-data plane plus a separate loopback
  publication bridge; neither joins production proxy or event networks.

Maintenance and restore are not members of `events`. Only proxy and drill
publish host ports, both bound to loopback.
