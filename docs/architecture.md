# Architecture

## Data flow

```text
Vaultwarden /data
      │ SQLite online backup + stable attachment copy
      ▼
verified immutable local recovery point
      │ per-target Restic upload with independent timeout
      ▼
S3 repositories
```

The proxy accepts Vaultwarden mutation requests and sends a strict v1 event to
the resident worker. A successful 2xx/3xx mutation marks the local state dirty;
client errors and transport failures do not. The worker debounces events,
creates at most one immutable local point at a time, and attempts all due
targets independently.

## Local transaction boundaries

SQLite is copied through the SQLite backup API. The copied database is checked
with `integrity_check`, `foreign_key_check`, the required attachment columns,
and the actual highest Diesel migration. Attachments and the two exact RSA key
names are copied only after stable-file checks. A manifest is hashed, written,
strictly verified once, and the complete tree is synced before promotion.

Promotion uses `current`, `.previous`, `.build-*`, and `.delete-*` entries. A
verified build is renamed into `current` only after the old point is retained.
State and notification files use same-directory temporary files, file fsync,
rename, and parent-directory fsync. A rename followed by an uncertain parent
fsync is reported as published-but-unconfirmed and is never rolled back.

Runtime state and notification queues are operational metadata. A safe regular
file with invalid JSON is rebuilt and an explicit event is logged. Symlinks,
special files, oversized files, and insecure permissions are hard failures.
The manifest, local recovery tree, and Restic repository are durable data and
are never silently migrated.

## Network isolation

- `app` is an internal network for Vaultwarden and proxy.
- `events` is an internal network for proxy and the resident backup worker.
- `proxy-ingress` is the proxy's published bridge network; IP masquerading is
  disabled so publication does not turn the proxy into an outbound gateway.
- `backup-outbound` is available only to the resident backup worker.
- `maintenance-outbound` is available only to maintenance and restore jobs.

Maintenance and restore are not members of `events`; they cannot resolve or
connect to the worker event address. The proxy reconstructs trusted forwarding
headers from the peer address and limits request bodies to 600 MiB.

## Failure model

`/healthz` reports only that the local HTTP process is serving. SQLite,
notification, Restic, S3, and individual target failures are represented in
status and notification events while `serve` remains alive. Source retries use
the fixed 1/2/4/8/15 minute and then 6 hour schedule. Target retries use
1/2/4/8 minute values with bounded cryptographic jitter and a 15 minute cap.
