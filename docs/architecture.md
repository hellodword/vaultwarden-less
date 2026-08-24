# Architecture and threat model

## Process and network boundaries

```text
local Nginx/Caddy
       |
       v
proxy --app network--> vaultwarden --rw--> /data
  |
  +--events network--> backup --ro-------> /data
                           |--rw----------> state/staging/cache
                           +--outbound----> S3 or native B2 + notifiers
```

Only proxy port 8080 is published, on host loopback by default. `app` and
`events` are internal Docker networks. The proxy has no config or filesystem
mount and never receives Restic, backend, APNs, or WeChat material.

Backup receives the private main config, common Restic password, and only the
backup backend role. Maintenance and restore also mount the private main
config and common password, receive the maintenance backend role, and never
mount `/data`; restore alone writes below host `./restore`. Because notifier
material lives in `backup.yaml`, those operator-invoked one-shot services can
read it even though only the resident backup process consumes it. All images
and Compose services are currently `linux/amd64` only.

Every bind disables Docker's automatic host-path creation. Application code
validates existing roots when used but does not implement a deployment-tree
preflight, create host roots, or repair modes. This removes a duplicated model
of host layout: orchestration creates the explicit bind sources, while each
runtime boundary validates only what it consumes.

## Proxy behavior

The static Go proxy has fixed upstream/event URLs and timeouts. Because the
host publication is loopback-only and its Compose networks are internal, it
accepts front-proxy overrides only from loopback or private peers. It validates
Host, a single IP-valued `X-Real-IP`, and an exact `http`/`https`
`X-Forwarded-Proto`; other peers' values are ignored. It deletes and rebuilds
`Forwarded`, `X-Real-IP`, and all `X-Forwarded-*` headers before upstream use.

Any method except `GET`, `HEAD`, `OPTIONS`, or `TRACE` whose upstream response
is 2xx/3xx emits an idempotent dirty event. This automatically includes PATCH
and future mutation routes. A capacity-one signal and one sender goroutine
coalesce events. Worker failure logs a safe code and never delays the client;
periodic sampling covers a lost event.

Limits are 600 MiB per request, 10 seconds to read headers, 30 minutes to read
or write a request, 2 minutes idle, 5 minutes for upstream response headers,
2 seconds for event delivery, and 2 minutes for graceful shutdown.

## Worker scheduling and failure isolation

The worker takes a non-blocking state-directory lock. Versioned `0600` JSON is
written using a synced temporary file and atomic rename. With at least one
target, startup reconciles pending state and samples immediately.

- changes debounce for 30 seconds;
- continuous changes wait at most 2 minutes once the minimum interval permits;
- completed recovery points are at least 5 minutes apart;
- sampling occurs at least every 15 minutes even without an event;
- events during backup coalesce into one further generation.

Fifteen minutes bounds local detection and the start of recovery-point work,
not completion against a failed or slow remote.

Targets have independent caches, backend environments, progress, and retry
deadlines. They run concurrently. A failure persists exponential backoff from
1 through 15 minutes with jitter while healthy targets continue. Lagging
targets catch up only to the newest locally verified point. The old staged
point is not displaced before the replacement verifies. When a newer local
sample is due, a retryable slow upload is canceled and later catches up to the
new latest point.

Zero targets is valid: event/status services remain available but no local
snapshot or upload is attempted. This is suitable for application-only fresh
instance validation, not backup protection.

On shutdown, HTTP stops, Restic receives cancellation, and pending content and
retry state remain persisted. Repository failure appears in status and
notifications rather than forcing container restart.

## Data consistency boundary

The worker treats `/data/db.sqlite3` as live data and uses SQLite's Online
Backup API. It never copies the live main file or its WAL/SHM. The independent
database is moved to DELETE journal mode, then checked with
`integrity_check`, `foreign_key_check`, and the supported Vaultwarden schema.

Only the fixed snapshot database determines attachment and file Send paths.
Copies require safe identifiers and containment, regular non-symlink files,
expected sizes, stable pre/post identity and metadata, and SHA-256. Concurrent
absence/change triggers a full retry or an explicitly incomplete point.
Unknown directories, icons, temporary uploads, built-in backups, deployment
environment, and live SQLite sidecars are excluded.

## Credentials and notifications

The common Restic encryption password is read from a fixed private file. Each
role has a separate YAML map containing only backend credentials. Backup and
maintenance can therefore use different cloud identities even though they
open the same encrypted repository. Each Restic child starts from a scrubbed
environment and receives only the repository locator, password-file path,
required backend keys, and allowlisted structured S3 options.

The resident worker invokes only `restic backup`; it never auto-runs init,
unlock, check, forget, prune, self-update, or repository migration.

Each notifier owns a persistent bounded queue and delivery goroutine. Success
entries are limited to 100 per notifier with overflow counting; incomplete,
target-failed, and target-recovered states are replaceable priority entries.
The first successful target for a content ID creates one summary. Later target
failure/catch-up produces state transitions. Notification validation,
persistence, or delivery failure never changes Restic success or blocks future
sampling.

## Residual risk

The split limits a public proxy compromise, but it does not make the host,
Docker daemon, kernel, local root, Vaultwarden, backup worker, or maintenance
identity mutually distrustful. Host/worker control exposes encrypted server
data, deployment material, and availability-critical credentials.

Vaultwarden E2EE protects vault plaintext only within its model. It does not
protect configuration, Restic/APNs/WeChat credentials, metadata, availability,
rollback, or a malicious client. `/data/config.json` is backed up as a data
asset; `.env`, `.env.vaultwarden`, `backup.yaml`, Compose/front-proxy/TLS
configuration, and all credentials require an independent protected offline
inventory.

Version tags without immutable digests permit registry-side artifact changes;
this is an accepted project choice. Minimal images and security patching reduce
surface but cannot promise zero vulnerabilities. Findings require component
and reachability analysis; dependency, base-image, configuration, operator,
availability, and supply-chain risks remain.
