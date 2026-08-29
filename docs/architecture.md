# Architecture

This document explains the implemented system shape. Exact fields, limits,
paths, JSON, and failure codes are normative only in
[the schema 1 contracts](contracts.md).

## Dataflow

```text
trusted TLS and header-cleaning edge
                  |
                  | loopback-published HTTP/WebSocket
                  v
              proxy :8080 ----------------------+
                  |                              |
                  | unchanged upstream request  | authenticated write/login event
                  v                              v
             Vaultwarden                     backup worker
                  |                              |
                  | live /data                   | SQLite online backup
                  |                              | referenced-file stable copies
                  |                              v
                  +------------------------> /staging/point
                                                 |
                                                 | verified manifest + identities
                                                 v
                                      Restic target 1, then target 2, ...
```

The proxy is transparent: it forwards root-path traffic and emits one bounded
event only after a qualifying upstream response is completely received and is
successful. The event is demand, not proof that a recovery snapshot exists.
The worker also performs startup, periodic, and fallback checks, so losing its
in-memory event queue does not lose the durable state of the repositories.

The worker mounts live Vaultwarden data read-only. It uses SQLite's online
backup interface, validates the complete audited schema and migration, closes
the reference set from the copied database, and stable-copies only referenced
attachments and file-Send payloads. A changing, missing, unsafe, or unexpected
file prevents publication. Staging is disposable; Restic snapshots are the
durable recovery record.

Targets execute serially in configuration order. A target can be degraded
while another succeeds. When a failed target returns, it catches up to the
newest verified recovery point; the product does not invent intermediate
points that were never stored there.

## Network boundaries

| Network | Members | Property |
| --- | --- | --- |
| `app` | proxy, Vaultwarden | Internal; carries only proxied application traffic. |
| `events` | proxy, backup | Internal; carries only the worker event/status API. |
| `proxy-ingress` | proxy | Non-internal bridge used solely for the loopback publication. |
| `backup-outbound` | backup | Repository and notifier egress for the resident worker. |
| `maintenance-outbound` | one-shot maintenance/restore/upgrade services | Repository egress without joining application networks. |
| `drill` | drill Vaultwarden and upgrade verifier | Internal recovery-drill traffic. |
| `drill-ingress` | drill Vaultwarden | Separate loopback publication for a disposable drill. |
| `import` | initial-import Vaultwarden candidate | Internal candidate traffic, isolated from the active application. |
| `import-ingress` | initial-import Vaultwarden candidate | Separate loopback publication for trusted HTTPS client verification. |

No production container receives the container-engine socket. Normal Compose
uses only local image aliases and `pull_policy: never`; it cannot build or pull
an image. Only the proxy, drill, and initial-import candidate endpoints are
published, all on loopback and never simultaneously as ordinary serving paths.

## Filesystem and identity boundaries

The deployment directory is both the ordinary Git clone and the root of
ignored private/runtime state:

```text
.env                         exact deployment identity and ports
config/                      private product and Vaultwarden configuration
credentials/                 isolated backup/maintenance/notifier/password inputs
data/                        live Vaultwarden data
import/                      immutable import copy and disposable candidate
staging/                     disposable recovery-point construction
cache/                       disposable Restic cache
restore/                     verified, operator-named restore outputs
state/                       active deployment identity and workflow evidence
dist/                        ignored release output
tmp/                         ignored task output
```

Private directories are mode `0700`; private files are mode `0600`; both are
owned by the exact runtime UID/GID in `.env`. Every ancestor is checked without
following symlinks. Resident services use that non-root identity, a read-only
root filesystem, `no-new-privileges`, and no Linux capabilities.

The sole root container is the bounded, networkless drill-copy helper. It gets
only `CHOWN`, `DAC_OVERRIDE`, and `FOWNER`, copies a previously verified restore
into a new disposable volume, and assigns that volume to the configured runtime
identity. It does not alter the restore source or production data.

Every production image carries one immutable project commit and release. The
private `state/deployment/active.json` is the only serving authority: normal
Vaultwarden, backup, and proxy independently reject a missing, damaged, or
mismatched identity. The Vaultwarden gate runs before `/bin/vaultwarden`, so a
rejected image cannot open or migrate `/data/db.sqlite3` or create SQLite/WAL
or RSA state. Proxy remains data-blind and validates terminal import/upgrade
state plus frozen rollback evidence without gaining a `/data` mount; roles that
open data additionally validate the live data filesystem device. Upgrade,
rollback, drill, and import use explicit Compose argv
and their narrower durable workflow evidence; no mode is inferred from tags or
the presence of a database. The host-side Python orchestrator owns only
structured Docker argv, exact image/runtime verification, drill cleanup, and
resumption; Go remains the sole owner of decisions and data-state transitions.

Role credentials are intentionally duplicated and checked independently:

- `credentials/backup.yaml` is available only to the resident worker.
- `credentials/maintenance.yaml` is available only to one-shot repository,
  restore, and upgrade commands.
- each target has one separate Restic password file;
- notifier secrets are available only to the resident worker.

Restic receives a fixed executable, temp directory, password file, repository
argument, declared target environment, and controlled options. It does not
inherit ambient host credentials, `HOME`, `PATH`, proxy variables, or debug
settings.

## Recovery identity

Each point has two independent identities:

- `content_id` is a canonical logical encoding of the accepted SQLite schema
  and rows plus the byte contents of referenced payloads. It is stable across
  SQLite page layout, insertion order, `VACUUM`, and archive metadata changes.
- `archive_id` covers the exact recovery-tree entries and bytes that Restic
  stores.

The manifest records both IDs, the full clean product commit, and the creation
instant. Restic snapshots repeat schema, project, content, and archive identity
as tags. Backup can reuse an existing snapshot only after all of those facts
agree; restore recomputes and checks them all. This separates semantic equality
from exact archive equality and prevents a tag alone from authorizing recovery.

The recovery closure contains the copied database and only the attachment and
file-Send payloads referenced by it. It excludes RSA keys, SQLite sidecars,
configuration, caches, templates, old Vaultwarden backups, and orphaned files.
Restored Vaultwarden therefore creates a new RSA identity. Existing access,
refresh, invite, attachment, and download tokens are expected to become
invalid; users establish fresh sessions after recovery.

## Restore and drill boundary

A restore selects `latest` or one full snapshot ID from one named target,
downloads into a task-owned temporary sibling, and performs complete schema,
manifest, tag, identity, path, and file verification. Publication is an atomic
no-replace rename to a previously absent `/restore/<name>`. No command chooses
an earlier snapshot after a failed latest restore, overwrites an output, or
deletes production data.

A drill is a separate Compose project and volume. It copies one operator-chosen
`restore/drill` tree, starts the same immutable Vaultwarden image without the
production proxy or worker, and publishes a different loopback port. Removing
the drill project and volume cannot remove the verified restore.

## Release and deployment split

Nix owns builds, tests, and release creation. A clean commit produces three
images, each tagged with both `local` and one deterministic immutable release
name. OCI revision, version, and creation labels derive from that commit.

The release directory contains only:

```text
images.tar.gz
SHA256SUMS
```

The archive loads directly through standard `docker image load`, contains six
references for three image IDs, and excludes the test-only Moto image. The
ordinary deployment host verifies and loads these bytes; it does not compile,
contact a registry, or need Nix.

## Upgrade ownership

An upgrade is an explicit A/B state machine, not `docker compose pull`:

- immutable A owns prepare, freeze, rollback evidence, and rollback apply;
- immutable B owns migration checking, repository verification, drill
  verification, and accept;
- B cannot expose its proxy or start its resident worker before accept;
- A never interprets B candidate evidence when rolling back;
- immutable `decision.json` makes accept and rollback mutually exclusive;
- active identity, service startup and health, verified drill cleanup, and
  final archive publication are resumable parts of the chosen terminal path.

Freeze stops writes, records same-filesystem rollback data owned by A, and
binds it to immutable release identities. B must migrate, create and restore
evidence, verify configured repositories, and pass an isolated drill before an
operator can accept. On any pre-accept failure the operator invokes explicit A
rollback; there is no automatic data exchange. Accept archives the complete
current record to history only after healthy B services and drill cleanup.
Rollback uses the frozen tree ID to resume a single same-filesystem exchange,
starts ordinary A services, and then archives the complete current record with
exchanged B data to quarantine. See [Upgrades](upgrades.md).

## Initial import ownership

Initial import is a separate pre-active state machine. The operator keeps the
stopped external original outside the clone and supplies only a private copy at
`import/source`. Go owns source/type/permission/device validation, stable tree
identities, SQLite Online Backup, RSA exclusion/rotation evidence, schema and
snapshot-closure verification, the immutable accept decision, the single
same-filesystem directory exchange, active publication, cancel, and finalize.

The candidate uses the existing production Vaultwarden image in explicit
`import` gate mode and mounts only `import/candidate` as writable `/data`. The
source and state mounts are read-only. It has separate internal/ingress
networks and only the drill port is published on loopback, so the operator can
route the trusted HTTPS edge for an official-client login without exposing the
candidate as the normal service. The old RSA files never enter the candidate;
Vaultwarden generates the new identity.

The networkless non-root deployment-admin sees the deployment root because
Linux can atomically exchange `data` and `import/candidate` only through one
common mount. Its code treats source as immutable, validates it again across
every boundary, and has no container-engine socket. The host Python
orchestrator owns only candidate stop/removal, structured Compose argv, exact
normal image/health verification, and finalize resumption. A decision is made
only in Go. Before decision cancel removes only disposable state; after
decision the only legal direction is accept/start/finalize. The empty exchanged
candidate is the only data directory finalize may remove.

## Failure semantics

- Health proves only that a process can answer its local health check.
- Worker status is bounded process state; repository snapshots are durable
  evidence.
- A missing or changing referenced payload fails closed and preserves the last
  good point.
- A target failure does not convert partial success into complete success.
- Notification queues are bounded, best effort, and non-durable.
- Restore, drill, import, upgrade, and rollback validate exact owners before side
  effects and never infer permission to overwrite a real destination.
- Moto acceptance validates the Restic/S3 protocol boundary, not a cloud
  provider's production behavior.
- Framework A/B acceptance completely covers its exact project-managed release
  pair. Same-version pairs create no pending compatibility obligation;
  cross-version claims require two different managed Vaultwarden versions and
  apply only to that exact pair.
