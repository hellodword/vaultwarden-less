# Architecture

## Design rules

The repository optimizes for a future maintainer who must first understand the
current audited boundary and only then change it:

1. One file owns each pin or contract.
2. Generated Go constants are derived from reviewed metadata, never edited.
3. Upstream source trees are not vendored.
4. Runtime decisions do not parse localized command stderr.
5. Source data is mounted read-only to backup code.
6. Publication is atomic and never overwrites an unknown destination.
7. Process-local state is explicitly disposable; Restic is the durable record.

The supported product and upstream revisions are defined by
[`VERSION`](../VERSION) and [`UPSTREAMS.toml`](../UPSTREAMS.toml).

## Runtime flow

```text
trusted TLS/header-cleaning edge
             │ root-path HTTP/WebSocket
             ▼
       transparent proxy ── one-bit event demand ──► backup worker
             │                                      │
             ▼                                      │ SQLite online backup
        Vaultwarden                                 ▼
                                             /staging/point
                                                   │ serial Restic backup
                                                   ▼
                                              ordered S3 targets
```

The proxy preserves upstream status, headers, streaming, bodies, and WebSocket
behavior. A successful non-read method requests a backup check unless its exact
route is in the audited read-like table in
[`internal/proxy/policy.go`](../internal/proxy/policy.go). Each exception
contains its upstream source location and reason. File-Send access endpoints
are deliberately treated as writes because access limits mutate durable state.
An unrecognized read-like POST causes an extra check; content identity prevents
an unnecessary snapshot.

The proxy forwards `Forwarded`, `X-Real-IP`, and `X-Forwarded-*` exactly as
received. The preceding edge is therefore the trust boundary: it must discard
untrusted copies and set authoritative values. Only root-path deployment is
supported.

## Scheduler and failure state

The worker has one serial scheduling loop and no scheduler database:

- startup, proxy events, and `request-backup` set the same coalescing demand;
- demand is quieted for 30 seconds, capped at two minutes from the first event;
- check starts are at least five minutes apart;
- demand arriving during a check remains pending;
- a successful check schedules a no-demand check 12 hours later;
- a changing or temporarily missing referenced file retries after 30 seconds;
- other source and target failures retry after five minutes; and
- a continuous failure becomes degraded after 30 minutes, with one degraded
  and one recovery transition event.

Targets execute in configuration order. A failed target later catches up to the
newest recovery point; intermediate points are not replayed. Whenever at least
one target creates a snapshot, the check emits a recovery event, including when
a previously failed target catches up to content already held by another
target. Restart intentionally loses status, retry timers, transition state, and
queued notifications.

## Recovery-point transaction

Both `/data` and `/staging` must be real, non-symlink, exact-mode 0700
directories owned by the effective service UID. A build occurs under a
recognized `/staging/.build-*` name:

1. A free-space check accounts for the live database, WAL estimate, and reserve.
2. SQLite's online backup API copies the database and normalizes the copy to
   DELETE journal mode.
3. `integrity_check`, `foreign_key_check`, the exact complete SQLite schema,
   its fingerprint, and the pinned latest migration are verified.
4. Attachment and file-Send references are derived from the copied database.
   UUIDs, IDs, sizes, JSON shapes, and relative paths are strictly validated.
5. A second free-space check counts only referenced files still to be copied;
   the already-created database is not counted twice.
6. Each source payload must remain the same non-symlink regular file across
   inspection, open, copy, and final inspection.
7. The strict manifest is written, synced, and used to re-derive and verify the
   complete tree. Missing, changed, or extra files and directories fail.
8. First publication uses Linux `renameat2(RENAME_NOREPLACE)`. Replacement
   uses `RENAME_EXCHANGE`; there is no check-then-rename fallback. Parent
   directory entries are synced.

A failed build never replaces the previous point. An unsupported kernel or
filesystem fails safe instead of weakening atomicity. Staging is reproducible
scratch state: only recognized `.build-*` entries are cleaned automatically.

The schema contract is a tracked generated artifact. It is captured from a
fresh database created by the exact audited Vaultwarden image, then reviewed
and regenerated with `scripts/capture_vaultwarden_schema.py`. Product code
embeds the complete object list rather than accepting a migration number alone.

## Restic boundary

[`internal/restic`](../internal/restic) is a thin, typed adapter around the
locally built patched Restic executable:

- every target has a distinct password file and credential entry;
- Restic receives exactly `TMPDIR=/tmp` and the selected target's two AWS
  credential variables; no ambient process environment is inherited;
- repository, password, and cache values are structured process arguments;
- snapshots are restricted by fixed host, path, and product-v2 tag;
- a content-ID tag is the equality boundary;
- full 64-character snapshot IDs are required for selection;
- listing is bounded and reports truncation; and
- command output is size-limited and parsed as strict JSON.

Structured Restic logs contain only operation, target, duration, exit code, and
safe snapshot/content identifiers. Arguments, repository locators, stderr, and
credentials are never logged.

Notifications are best effort. Each configured notifier has a bounded
process-local queue and attempts delivery immediately, after one minute, and
after five minutes. Queue overflow and delivery failure cannot change backup
success.

## Restore and drill

Restore resolves exactly one snapshot, restores into a private temporary
sibling, verifies the manifest, complete schema, complete tree, and content tag,
then publishes with `RENAME_NOREPLACE` and syncs the parent. It never falls
back from an invalid latest snapshot and never reuses or overwrites an output.

The drill reads a verified restore read-only, copies its `data` directory into
a newly reset disposable named volume, and starts Vaultwarden on isolated
networks. The verified restore itself remains unchanged. Drill teardown deletes
only the disposable copy volume.

## Compose isolation

`scripts/deploy.py` validates `config/deployment.yaml`, derives local image
tags from the same metadata used by bundle/generation tools, and renders the
single production template. The rendered document is not persisted. There is
no `.env` merge or private environment-variable vocabulary to remember.

- `app`: private proxy-to-Vaultwarden data plane.
- `events`: private proxy-to-worker control plane.
- `proxy-ingress`: loopback-only production publication.
- `backup-outbound`: resident repository and notifier egress.
- `maintenance-outbound`: one-shot maintenance and restore egress.
- `drill` and `drill-ingress`: isolated copied data and loopback drill
  publication.

Maintenance and restore do not join `events`. The worker receives only its
backup-role credentials; maintenance and restore receive only the maintenance
role. Notifier credentials are mounted only into the resident worker.

## Repository map

| Path | Responsibility |
| --- | --- |
| `cmd/` | Two small process entry points |
| `internal/cli` | Typed CLI/JSON contract and dependency wiring |
| `internal/config` | Strict schema-v2 configuration and credentials |
| `internal/proxy` | Transparent forwarding and audited event policy |
| `internal/snapshot` | SQLite copy, referenced-file capture, manifest, verification |
| `internal/restic` | Restic process boundary and parsers |
| `internal/restore` | Exact snapshot selection and atomic restore publication |
| `internal/worker` | In-memory scheduling, status, and private HTTP API |
| `internal/notify` | Safe message rendering and bounded delivery workers |
| `internal/privatefs` | Private-file checks and Linux atomic rename primitives |
| `internal/upstream` | Generated pins and embedded Vaultwarden schema |
| `nix/` | Toolchain, products, local images, checks, and the Restic patch |
| `deploy/` | Production Compose and Vaultwarden public templates |
| `tests/e2e/` | Real Vaultwarden/Bitwarden/Restic/Moto lifecycle |
| `scripts/` | Deployment, verification, generation, bundle, and audit tools |

There are no checked-out upstream trees, release scripts, hosted CI workflows,
or registry-specific files.

## Build and artifact ownership

Nix builds the product binaries and images, patched Restic, and the test-only
Moto image. The official Vaultwarden image is accepted only at the digest in
`UPSTREAMS.toml` and receives a local tag containing its digest prefix.
Nothing is pushed.

`internal/upstream/generated.go` and the Vaultwarden schema JSON are tracked
because they are small review artifacts consumed at compile time. They have
deterministic generators and checks. Build outputs, upstream checkouts, audit
reports, bundles, databases, screenshots, and runtime state are ignored.
