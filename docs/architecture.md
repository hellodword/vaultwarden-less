# Architecture and threat model

## Process and network boundaries

```text
local Nginx/Caddy
       |
       v
proxy --app network--> vaultwarden --rw--> /data
  |
  +--events network--> backup --ro-------> /data
                           |--rw----------> state/staging/cache/local repositories
                           +--outbound----> Restic targets and notifiers
```

Only the proxy port is published, and Compose binds it to host loopback by
default. `app` and `events` are internal Docker networks. The backup worker has
a separate outbound network. Vaultwarden and the proxy do not receive Restic,
backend, APNs, WeChat, or webhook credentials.

The one-shot maintenance and restore containers join only the outbound
network. Maintenance has cache, configuration, and maintenance secrets but no
`/data`. Restore additionally has `./restore:/restore:rw`; online data is not
mounted writable or read-only.

## Proxy behavior

The public proxy is a static Go binary with no data mount. Before forwarding,
it validates the Host and trusted front-proxy fields, removes all forwarding
headers, and constructs new `Forwarded`, `X-Real-IP`, and `X-Forwarded-*`
values. Upgrade/WebSocket support is disabled by the Vaultwarden template.

Any method other than `GET`, `HEAD`, `OPTIONS`, or `TRACE` whose upstream
response is 2xx or 3xx produces an idempotent dirty event. This includes PATCH
and future mutation routes without maintaining a route-regex exclusion list.
The event sender has one coalescing slot and one worker goroutine. A down worker
causes a safe error log, never delays the client response, and is covered by
the worker's next 15-minute sample.

HTTP boundaries are explicit: 600 MiB request bodies, 10-second header reads,
30-minute request reads/writes, two-minute idle connections, five-minute
upstream response headers, and two-minute graceful shutdown.

## Worker scheduling and failure isolation

The worker obtains a non-blocking file lock for its state directory. State is
versioned JSON, mode `0600`, and replaced through a synced temporary file plus
atomic rename. Startup reconciles a verified staged recovery point and samples
immediately.

- Changes debounce for 30 seconds.
- Continuous changes force eligibility after two minutes.
- Completed recovery points are separated by at least five minutes.
- Sampling occurs at least every 15 minutes even without an event.
- An event received during a backup increments one dirty generation; it asks
  for one additional sample rather than queueing every request.

The 15-minute statement is a bound on detecting/sampling local changes and
starting a recovery-point attempt. It is not an upload completion guarantee
when a repository, network, or backend is unavailable.

Each target has its own cache, credential environment, status, snapshot ID,
and retry deadline. Due targets run concurrently, bounded by the configured
maximum of 32. Failure uses persisted exponential retry from one through 15
minutes with jitter. Healthy targets continue while another target is down.
Only the latest verified local recovery point is retained for catch-up; the
old point is not replaced until the new point has passed all local checks.
If another sample becomes eligible while a target upload is still running, the
worker terminates that retryable upload, preserves its prior failure state, and
lets it catch up directly to the newly verified latest point. A slow remote
therefore cannot extend the local sampling bound to `target_timeout`.

On shutdown, the event server stops, retryable Restic processes receive
termination through their context, and the pending recovery point and retry
state remain persisted. Repository health is visible in `status`; a remote
failure does not intentionally restart the container or falsify process
health.

## Data consistency boundary

The source `/data/db.sqlite3` is treated as live real data. The worker opens it
read-only and uses the SQLite Online Backup API; it never copies the live main
file or its WAL/SHM. The resulting independent database is switched to DELETE
journal mode, checked with `integrity_check` and `foreign_key_check`, and
required to match Vaultwarden 1.37.2's migration and referenced columns.

Only after that database is fixed does the worker derive attachment and file
Send paths. File copies use validated IDs, containment checks, non-symlink
opens, regular-file checks, expected size, pre/post identity and metadata, and
SHA-256. A concurrent add/delete becomes a retry or an explicitly incomplete
point. Unknown directories, icon cache, temporary uploads, old built-in
backups, deployment environment, and WAL/SHM are outside the set.

## Notification isolation

Every notifier owns a persistent bounded queue and delivery goroutine. Normal
success events are limited to 100 per notifier; overflow is counted on the
newest pending entry while replacing it with the latest success. An incomplete recovery point occupies one priority,
replaceable source-integrity slot; a target failure/recovery occupies one
priority replaceable slot per target until delivered or superseded.
The next complete recovery summary atomically supersedes a still-pending
source-integrity slot so a stale incomplete state cannot remain ahead of it.
Notification I/O has explicit timeouts, redirects are forbidden, and response
bodies are bounded.

The first successful target for a content ID persists one summary event.
Target degradation and catch-up persist state-change events. Notification
persistence, secret initialization, or delivery errors are logged with safe
codes and never turn a successful Restic upload into a failed one, prevent
worker startup, or prevent the next sample. `config --check-secrets` remains
the explicit deployment-time check.

## Threat model and residual risk

The design reduces the impact of a public proxy compromise: that process has
neither Vaultwarden files nor backup/notification credentials. It does not
make the host, Docker daemon, kernel, local root, Vaultwarden process, backup
worker, or maintenance credentials mutually distrustful. Anyone controlling
the host or backup worker can read encrypted Vaultwarden database contents,
Restic credentials, and availability-critical material.

Vaultwarden's end-to-end encryption protects vault item plaintext from the
server under its own model. It does not protect deployment configuration,
Restic or notification credentials, traffic metadata, availability, rollback,
or a malicious client. `/data/config.json` admin overrides are included in the
recovery set; Compose files, `.env`, `.env.vaultwarden`, proxy/backup JSON,
TLS/front-proxy configuration, and every secret file are not. Keep those in an
independent protected configuration backup.

Version tags without digests permit registry-side artifact changes. Minimal
images and prompt patch releases reduce attack surface but do not prove a
vulnerability count of zero. Scanner results require reachability and
component attribution; unpatched base-image, dependency, configuration,
availability, and operator errors remain risks.
