# Product contracts

This document is the normative machine contract for the versionless product.
Tracked examples, Compose files, binaries, tests, and operator prose must agree
with it; none can silently redefine it. The breaking change from the retired
schema-marked contract is explicitly authorized for the interfaces named here.
It does not authorize deployment, publication, deletion, or mutation of real
data.

There is no reader, migration, fallback, or converter for old project
configuration, state, manifests, hash streams, or Restic snapshots. A first
versionless deployment uses a fresh deployment root and fresh repository
prefix, and retains the complete legacy root, repositories, images, and commit
as the rollback path. Project code must reject old fields and bytes using the
stable classifications defined below rather than infer compatibility.

All instants are UTC RFC 3339 with exactly three fractional digits, for example
`2026-08-27T12:34:56.789Z`. A full commit is 40 lowercase hexadecimal
characters. A digest is `sha256:` followed by 64 lowercase hexadecimal
characters. The only allowed version markers are externally owned: Restic
repository format 2, OCI and Docker media types, Docker authentication URL
`/v1/`, Git porcelain v1, upstream versions, and Vaultwarden migrations.

## Supported component set

Compatibility and support claims are scoped to one exact clean project commit
and its immutable release. The supported component set is exactly the project
binaries, images, Compose files, and audited upstream packages selected by that
commit's `flake.nix`, tool-generated `flake.lock`, and `UPSTREAMS.toml`. This
includes the selected Vaultwarden and unmodified Restic packages and, for
development and acceptance, the selected Bitwarden CLI, Moto, and Go toolchain.

Replacing, rebuilding, relabelling, or independently upgrading any of those
components is outside the contract, even if its name, CLI, protocol, schema, or
version appears compatible. A candidate upstream version enters the supported
set only through a reviewed project commit, project-produced immutable release,
and the acceptance required by this contract. The project makes no support or
compatibility promise for arbitrary Vaultwarden, Restic, Bitwarden CLI, Moto,
Go, image, or wrapper versions outside that managed set.

Support evidence is exact-set and exact-pair evidence, not a general upstream
compatibility matrix. Two managed project releases that contain the same actual
Vaultwarden version can completely verify the A/B orchestration contract
without creating an unverified cross-version obligation. If two managed
releases intentionally contain different actual Vaultwarden versions,
cross-version compatibility may be claimed only for that exact pair after the
complete upgrade scenario passes.

## Source checkout and deployment root

The source checkout contains tracked Compose files, public templates, and
documentation. Its ignored `.env` selects a separate private deployment root;
the checkout is not the owner or default location of credentials, state, or
application data. Existing checkout-relative ignored paths remain protected by
`.gitignore` only to prevent legacy private material from entering Git. Product
code and tests never read them as fixtures.

The deployment `.env` is parsed by the locked Docker Compose implementation.
The project does not implement a second dotenv grammar. After Compose
interpolation these five values are required:

```dotenv
DEPLOYMENT_ROOT=/srv/vaultwarden-less
RUNTIME_UID=1000
RUNTIME_GID=1000
LISTEN_PORT=8080
DRILL_PORT=8081
```

`DEPLOYMENT_ROOT` resolves to an existing absolute directory and is not `/`.
Runtime IDs are canonical decimal `1..4294967294`; ports are canonical decimal
`1024..65535` and differ. The Compose-resolved values, not a product-owned
reparse of `.env`, are the inputs to configuration checks. The example uses
plain canonical assignments; quoting, escaping, comments, interpolation, and
precedence follow Docker Compose's official `.env` rules.

The private layout is exactly rooted below `DEPLOYMENT_ROOT`:

```text
<DEPLOYMENT_ROOT>/
  config/
    backup.yaml
    vaultwarden.env
  credentials/
    backup.yaml
    maintenance.yaml
    notifiers.yaml
    restic/<target>
  data/
  import/
  staging/
  cache/
  restore/
  state/
```

The root and every private directory are existing, real, non-symlink
directories owned by the runtime UID/GID with mode `0700`. Every private file
is a real, single-link, non-symlink regular file owned by that identity with
mode `0600`; every ancestor is inspected without following symlinks. Checks
report a stable error and never repair ownership or mode with `chmod` or
`chown`. The recommended root is outside the checkout. A root inside it is
permitted only when the operator has independently confirmed the whole root is
ignored.

`compose.yml` defines production, maintenance, restore, drill, and import
profiles. `compose.upgrade.yml` is only an upgrade/rollback overlay. Every
private bind uses `${DEPLOYMENT_ROOT}/...` long syntax with
`bind.create_host_path: false`; rendered Compose contains no
checkout-relative private bind. Both files use only local production aliases,
contain no `build:` key, set `pull_policy: never`, publish only intended
loopback/public endpoints, mount no container-engine socket, and pull no
registry image. Resident services use the configured non-root identity,
read-only roots, `no-new-privileges`, and no capabilities.

Before production start, both role configuration checks validate the resolved
deployment values, actual binds, private paths, target/password key sets,
role separation, notifier credentials, UID/GID, and ports without contacting a
repository. An empty deployment is initialized only after those checks.
Production operator procedures use Git, Docker Engine, Docker Compose, gzip,
and checksum tools; they do not require Python, Go, Nix, or Just. No product
command may overwrite live `data`.

## Active release identity and startup gates

`state/deployment` is a real `0700` directory owned by the runtime UID/GID.
Its sole durable document is a non-symlink, single-link `0600` regular file no
larger than 1 MiB, owned by that same identity:

```json
{"project_commit":"0123456789abcdef0123456789abcdef01234567","release":"RELEASE.2026-08-28T12-34-56Z-g0123456789ab","activated_at":"2026-08-28T12:34:56.789Z"}
```

The file is canonical one-line JSON followed by LF. Unknown or duplicate
fields, trailing values, wrong JSON types, a retired version field, a
non-lowercase/full commit, a release that does not contain the matching first
12 commit characters, and a non-UTC millisecond timestamp are invalid.
For a terminal workflow, immutable `decided_at` is chosen strictly after the
prior active instant. The resulting `activated_at` is also the publication
boundary for the completed data transition and must not predate that decision.

`deployment init` requires one clean immutable build, an existing empty
`data` directory, no active document, and no `state/upgrade/current` or
`state/import/current`. It atomically creates the active document without
replacement. Repeating the exact same identity returns `already_initialized`
without changing `activated_at`, including after Vaultwarden has populated
`data`; a different identity returns `conflict`. Missing, damaged, unsafe, or
mismatched active state is never reconstructed from data, image tags, or the
invoking binary.

The Vaultwarden image contains a CGO-free entry gate and invokes it with
exactly one Compose-owned mode: `normal`, `upgrade-candidate`, `rollback`,
`drill`, or `import`. The gate validates the clean build identity before
executing `/bin/vaultwarden`:

- `normal` requires active commit and release to match the image. It permits A
  during prepare before freeze; afterwards current must contain an immutable
  terminal decision for that same identity. Rollback proves live `data` has
  A's frozen tree ID before publishing the terminal active instant and before
  any normal process opens it;
- `upgrade-candidate` requires immutable B, active A, and a frozen current
  upgrade with valid rollback evidence and no terminal decision;
- `rollback` requires the immutable rollback decision and active A;
- `drill` requires the private recovery manifest commit to match the image;
  while an upgrade is current, its content and archive identities must also
  match B's verification evidence;
- `import` requires no active document and exact current import preparation
  for the image and candidate.

An unavailable or incomplete mode fails closed. Normal backup and proxy serve
paths independently require the exact active commit and release before they
open data or accept traffic. The data-opening Vaultwarden and backup roles also
validate the live data filesystem against accepted upgrade evidence. Proxy has
no data mount: its state-only gate still validates import state, the complete
upgrade decision/activation, verified candidate, and frozen rollback device and
tree identity. `version`, `healthcheck`, `config validate`, and the explicit
initialization workflow retain their narrower pre-active modes.

### Stable state-file ownership

`internal/statefile` is the only package that owns durable private JSON file
I/O. Its interface is:

```go
ReadJSON(path string, limit int64, destination any) error
WriteNewJSON(path string, limit int64, value any) error
ReplaceJSON(path string, limit int64, value any) error
IsOwnedTemporary(targetName string, candidateName string) bool
```

It enforces the private single-link `0600` file and UID/GID contract, rejects
symlinks and file substitution by comparing the opened object before and after
I/O, applies the explicit size limit, writes canonical JSON plus one LF, uses a
same-directory owned temporary file, fsyncs the file and parent, and implements
no-replace or replace exactly as selected. Cleanup recognizes only temporary
names owned by the target. Deployment, import, and upgrade retain ownership of
their fields, phases, identities, validation, and stable domain error codes.
Injected failure at any write boundary leaves either the complete prior file or
the complete new file, never a partial durable document.

Retired version fields are unknown fields. Old deployment, import, and upgrade
JSON map respectively to `deployment_state_invalid`, `import_state_invalid`,
and `upgrade_state_invalid`; no package removes the field and retries.

## YAML configuration and credentials

Each YAML file is at most 1 MiB, contains exactly one document, and is
decoded without implicit type conversion. Unknown fields, duplicate mapping
keys at any depth, merge keys, anchors, aliases, tags, and trailing documents
are rejected. Mapping order is not semantic. Target and notifier list order is
semantic and is preserved in output and execution.

### Backup configuration

`config/backup.yaml` has exactly this shape:

```yaml
upgrade_verification_targets:
  - primary
targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/vaultwarden
    options:
      - name: s3.bucket-lookup
        value: path
notifiers:
  - name: operations
    type: wechat
```

There are 1 to 32 targets. A target name matches
`[a-z][a-z0-9-]{0,62}` and is unique. `repository` is a string of 1 to 4096
bytes with no NUL, CR, or LF and is passed to Restic as one argument; the
product does not parse or branch on a provider. `options` is required and is an
array of at most 64 objects with exactly `name` and `value`. An option name is
1 to 256 bytes, unique within the target, and contains no NUL, CR, LF, or `=`.
Its value is a string of at most 4096 bytes with no NUL, CR, or LF. Each option
becomes the two arguments `--option` and `name=value`, in configured order.
Within the project-managed Restic package, Restic decides whether the
repository and options are supported. This delegation does not extend project
support to another Restic build or version.

`upgrade_verification_targets` contains 1 to 32 unique target names, preserves
order, and is a subset (including the possible full set) of `targets`.
Resident target synchronization uses at most two operations concurrently.
Targets are dispatched and all results and notifications are published in
configuration order; one failure does not cancel another target. Upgrade
verification targets remain serial. There are 0 to 16 notifiers;
notifier names use the target-name pattern and are unique. The only notifier
types are `wechat` and `bark`. This file never contains a secret.

### Role credentials and passwords

`credentials/backup.yaml` and `credentials/maintenance.yaml` have the same
strict shape but are separate files and security roles:

```yaml
targets:
  primary:
    environment:
      AWS_ACCESS_KEY_ID: replace-me
      AWS_SECRET_ACCESS_KEY: replace-me
```

The target key set equals the backup configuration target set exactly. Each
`environment` mapping contains at most 128 entries. A name matches
`[A-Z_][A-Z0-9_]{0,127}`. A value is a nonempty string of at most 65536 bytes
with no NUL. Names beginning `RESTIC_`, and the exact names `PATH`, `HOME`,
`TMPDIR`, `TMP`, `TEMP`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, and `GODEBUG`, are
forbidden. The backup role is available only to the resident worker. The
maintenance role is available only to one-shot repository, restore, and
upgrade commands. Neither role falls back to the other.

Each target has exactly one `credentials/restic/<target>` password file. It is
1 to 65536 bytes, contains no NUL or CR, and represents one nonempty line. One
terminal LF is allowed; no other LF is allowed. The password file set equals
the configured target set exactly; examples do not count as passwords.

Restic is invoked by its fixed image path `/bin/restic` with a structured argv,
`--repo <repository>`, `--password-file
/run/credentials/restic/<target>`, a target-specific cache directory beneath
`/cache`, and a product-owned temporary directory. It receives no ambient
environment. Its environment consists only of the selected role target map
plus immutable product-controlled home, temporary-directory, and trust-store
values required by the image. User configuration cannot override those fixed
values. Repository locators, password material, target environment values,
notifier values, and raw subprocess stderr are never logged or returned.

`repo init` always invokes Restic with `init --repository-version 2`. The real
integration must read repository config and observe integer version 2. No
product command exposes or invokes `restic migrate`; changing repository
format is an explicit, separately authorized maintenance event, never a
software-upgrade side effect.

Every Restic invocation owns a fresh parser and process context. JSON output is
read as command-specific frames with an 8 MiB maximum frame, 64 MiB maximum
stdout, 100,000-frame maximum, and 64 KiB captured-stderr maximum. A truncated
frame, invalid UTF-8 or JSON, wrong consumed-field type, duplicate required
semantic record, missing final summary, unexpected summary order, nonzero exit,
or output after the final summary fails the command. Fields actually consumed
for snapshot identity, prune decisions, or success are strict. Unknown fields
within a recognized record and unrecognized progress records are ignored for
forward compatibility. Parsers are not shared between commands. Timeout or
context cancellation terminates the process group, drains bounded pipes,
waits for exit, and returns `timeout` or the caller's stable error without
including raw diagnostics.

### Notifier credentials

`credentials/notifiers.yaml` has exactly:

```yaml
notifiers:
  operations:
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me
```

The notifier key set equals the configured notifier set exactly. A `wechat`
entry has only `webhook_url`: an absolute HTTPS URL of at most 4096 bytes, with
a host and nonempty path, and without user info, fragment, NUL, CR, or LF. A
`bark` entry has exactly `device_token`, `auth_key`, `key_id`, `team_id`,
`topic`, and optional `sound`. The token is 64 lowercase hexadecimal
characters. `auth_key` is one PKCS#8 P-256 private key in PEM form, at most 16
KiB, with no NUL or trailing non-whitespace data. Key ID, team ID, and topic are
2 to 255 bytes and contain no NUL, CR, or LF. Sound is 0 to 255 bytes with the
same exclusions; empty selects `1107`. Notification requests refuse redirects,
are bounded to 10 seconds, and never block or change backup success.

After a check that creates at least one target snapshot, notifications list
every configured target in configuration order as `created`, `synced`, or
`failed`. A catch-up can therefore notify again for an existing content ID.
Source and target degraded events are emitted once per continuous degraded
interval and once on recovery. A notifier has a bounded queue of 16; a full
queue drops only that notification and writes a safe error code. Delivery is
attempted immediately, after 1 minute, and after 4 additional minutes, then is
dropped. Messages contain only notifier name, safe target names, stable error
codes, and the first 12 digest hexadecimal characters; never repository,
credential, endpoint, password, raw stderr, or record data.

### Fixed scheduling and retention

The quiet window is 30 seconds, maximum quiet is 2 minutes, minimum check
interval is 5 minutes, fallback is 12 hours, consistency retry is 30 seconds,
ordinary failure retry is 5 minutes, continuous failure becomes degraded at 30
minutes, event retry is 30 seconds, one target is bounded to 2 hours, one
maintenance command to 24 hours, one notification to 10 seconds, and shutdown
to 2 minutes. These are constants and have no configuration override.

Prune uses exactly:

```text
--keep-within 24h
--keep-hourly 48
--keep-daily 14
--keep-weekly 8
--keep-monthly 12
--keep-yearly 3
```

## Product CLI and JSON

The backup executable has exactly these commands:

```text
serve
request-backup
status
config validate --role backup|maintenance
repo init --target <name>
repo snapshots --target <name> [--limit <1..1000>]
repo check --target <name>
repo unlock --target <name>
repo prune --target <name> [--apply]
restore --target <name> [--snapshot latest|<64-lowercase-hex>] --output <name>
deployment init
deployment import prepare
deployment import verify --confirm-client-login
deployment import accept
deployment import cancel
deployment import finalize
upgrade prepare --to-commit <full-commit> --to-release <immutable-release>
upgrade freeze
upgrade migrate-check
upgrade verify
upgrade accept
upgrade cleanup-drill
upgrade finalize
rollback apply
healthcheck
version
```

`--config <path>` is the only global option and defaults to
`/config/backup.yaml`. There are no environment-variable overrides. Snapshot
limit defaults to 100. Restore snapshot defaults to `latest`; output name
matches `[a-z][a-z0-9-]{0,62}` and resolves to `/restore/<name>`. Unknown
commands, flags, positional arguments, or omitted required flags are usage
errors. The proxy executable has only `serve`, `healthcheck`, and `version`.
Explicit `--help` at any valid command may write text and exit 0.

For every non-streaming command, success writes exactly one JSON document and
one LF to stdout, writes nothing to stderr, and exits 0. A runtime or validation
failure writes no stdout, exactly one JSON error document and LF to stderr, and
exits 1. A usage failure has the same stream rule and exits 2. `serve` writes
no stdout and writes bounded structured JSON Lines logs to stderr. Logs never
contain secrets or record data.

Every error uses this strict envelope:

```json
{"error":{"code":"invalid_config","message":"Safe message"}}
```

The exhaustive stable codes are `usage_error`, `invalid_config`,
`invalid_credentials`, `unsafe_path`, `not_ready`, `conflict`,
`source_inconsistent`, `repository_failure`,
`verification_failure`, `timeout`, `upgrade_state_invalid`,
`import_state_invalid`, `deployment_state_invalid`, and
`internal_error`. The message is safe human context and is not a stable parsing
surface. No error has extra fields.

Retired inputs fail without compatibility handling:

| Input boundary | Stable result |
| --- | --- |
| Backup YAML with a retired field | `invalid_config` |
| Credential YAML with a retired field | `invalid_credentials` |
| Versioned HTTP path or nonempty request object | HTTP 400 `usage_error` |
| Deployment, import, or upgrade state with a retired field | the matching `*_state_invalid` code |
| Manifest, hash stream, or snapshot carrying retired bytes/tags | `verification_failure` |

These classifications apply before any side effect. There is no automatic
rewrite, fallback reader, or converter.

A dirty or non-Git build reports development identity. Only `version`,
`healthcheck`, `config validate`, and help may run in that state. Both `serve`
commands and every command that reads or changes runtime, repository, restore,
or upgrade state fail `not_ready` before side effects.

The complete success documents are:

```json
{"project_commit":"0123456789abcdef0123456789abcdef01234567","release":"RELEASE.2026-08-27T12-34-56Z-g0123456789ab","clean":true}
{"project_commit":null,"release":"development","clean":false}
{"healthy":true}
{"accepted":true}
{"valid":true,"role":"backup","targets":["primary"]}
{"status":"initialized","project_commit":"0123456789abcdef0123456789abcdef01234567","release":"RELEASE.2026-08-27T12-34-56Z-g0123456789ab"}
{"status":"already_initialized","project_commit":"0123456789abcdef0123456789abcdef01234567","release":"RELEASE.2026-08-27T12-34-56Z-g0123456789ab"}
{"operation":"init","target":"primary","success":true}
```

`repo init`, `repo check`, and `repo unlock` use the last shape with their own
operation string. Config target order is configuration order. Configuration
validation has no `warnings` field.

Snapshots are newest first, and matching snapshots with equal timestamps are
ordered by ID descending:

```json
{
  "target": "primary",
  "limit": 100,
  "truncated": false,
  "snapshots": [{
    "id": "<64-lowercase-hex>",
    "time": "2026-08-27T12:34:56.789Z",
    "project_commit": "<full-commit>",
    "content_id": "sha256:<64-lowercase-hex>",
    "archive_id": "sha256:<64-lowercase-hex>"
  }]
}
```

Prune is a preview unless `--apply` is present. Group and ID order is Restic's
stable JSON order after validation:

```json
{
  "operation": "prune",
  "target": "primary",
  "applied": false,
  "groups": [{
    "host": "vaultwarden-less",
    "paths": ["/staging/point"],
    "keep_snapshot_ids": [],
    "remove_snapshot_ids": []
  }]
}
```

Restore returns:

```json
{"target":"primary","snapshot_id":"<64-lowercase-hex>","project_commit":"<full-commit>","content_id":"sha256:<64-lowercase-hex>","archive_id":"sha256:<64-lowercase-hex>","output":"/restore/drill"}
```

Upgrade and rollback commands use one common result. `targets` is present only
for `frozen` and `verified` and otherwise is an empty array:

```json
{
  "upgrade_id": "<A-full-commit>..<B-full-commit>",
  "phase": "verified",
  "from_commit": "<A-full-commit>",
  "to_commit": "<B-full-commit>",
  "targets": [{
    "target": "primary",
    "snapshot_id": "<64-lowercase-hex>",
    "content_id": "sha256:<64-lowercase-hex>",
    "archive_id": "sha256:<64-lowercase-hex>"
  }]
}
```

The phase is `prepared`, `frozen`, `migrated`, `verified`, `accepted`, or
`rolled_back`, matching the command that completed.

Status arrays are never null; unavailable instants and identifiers are JSON
null:

```json
{
  "state": "idle",
  "started_at": "2026-08-27T12:00:00.000Z",
  "next_check_at": null,
  "last_check": {
    "started_at": "2026-08-27T12:00:30.000Z",
    "finished_at": "2026-08-27T12:00:40.000Z",
    "result": "unchanged",
    "content_id": "sha256:<64-lowercase-hex>",
    "archive_id": "sha256:<64-lowercase-hex>",
    "error_code": null
  },
  "targets": [{
    "name": "primary",
    "state": "healthy",
    "last_attempt_at": "2026-08-27T12:00:35.000Z",
    "last_success_at": "2026-08-27T12:00:39.000Z",
    "snapshot_id": "<64-lowercase-hex>",
    "content_id": "sha256:<64-lowercase-hex>",
    "archive_id": "sha256:<64-lowercase-hex>",
    "error_code": null
  }]
}
```

Worker state is `idle`, `debouncing`, `checking`, `partial`, or `degraded`.
Target state is `unknown`, `healthy`, `retrying`, or `degraded`. Check result is
`unchanged`, `created`, `partial`, or `failed`. `last_check.archive_id` is the
source archive observed during that check even when unchanged logical content
causes no upload.

## Internal HTTP and proxy events

The private backup HTTP interface is:

- `GET /healthz` accepts no query or body and returns 204 with an empty body.
- `GET /status` accepts no query or body and returns the CLI status JSON.
- `POST /events` accepts the strict empty request and returns 204.
- `POST /checks` accepts the same request and returns 202 plus the
  `request-backup` JSON.

The only request object is `{}`. JSON whitespace is allowed, but the object
must be empty and no second JSON value may follow. Body size is at most 1024 bytes.
Content-Type is exactly `application/json` with no parameters. A query string,
wrong method, duplicate or unknown key, redirect, or extra document is
rejected. JSON HTTP errors use the CLI error envelope. Invalid input is 400,
wrong method is 405, conflict is 409, unsafe path is 422, not-ready is 503,
timeout is 504, repository failure is 502, and other runtime failures are 500.
Retired versioned paths and request fields are invalid input and return 400
`usage_error`; they are not aliases.

The proxy publishes `/.vaultwarden-less/healthz`; every other request and
response is transparently forwarded. A completed upstream response with a
2xx status triggers one event after `POST`, `PUT`, `PATCH`, or `DELETE`.
`POST /identity/connect/token` is explicitly included because login can change
KDF state. Failed writes, reads, health probes, and incomplete responses do not
trigger. Event delivery retries every 30 seconds and does not alter the client
response. Whether a triggered check uploads is decided only by `content_id`.

## Recovery identity and manifest

The recovery identity is the full clean project commit that created the
snapshot. That commit uniquely selects `flake.lock`; the manifest does not
repeat software versions, image digests, source hashes, or the lock hash.

`manifest.json` is strict versionless JSON:

```json
{
  "project_commit": "<40-lowercase-hex>",
  "content_id": "sha256:<64-lowercase-hex>",
  "archive_id": "sha256:<64-lowercase-hex>",
  "files": [
    {"path":"db.sqlite3","size":123,"sha256":"sha256:<64-lowercase-hex>"}
  ]
}
```

The JSON rejects duplicate or unknown fields, invalid UTF-8, trailing values,
more than 1,000,000 entries, or more than 64 MiB. `files` is nonempty, contains
`db.sqlite3`, excludes the manifest, is unique, and is sorted by raw UTF-8 path
bytes. Size is `0..9223372036854775807`. Every digest includes its `sha256:`
prefix.

Allowed paths are `db.sqlite3`,
`attachments/<cipher-uuid>/<attachment-id>`, and
`sends/<send-uuid>/<file-id>`. UUIDs use lowercase canonical hexadecimal with
hyphens, attachment IDs are 20 lowercase hexadecimal characters, and Send file
IDs are 64 lowercase hexadecimal characters. Attachment paths and sizes come
from `(cipher_uuid,id,file_size)` in `attachments`. File Send paths come from
the strict schema of `sends.data` for `atype=1`. Any invalid reference,
duplicate path, missing file, symlink, special file, path escape, orphan file,
size mismatch, or file change during copying fails the build without
publishing a partial point.

The staged database is made with the SQLite Online Backup API. Its journal is
normalized without copying WAL, SHM, journal, Vaultwarden-created backups, RSA
keys, icons, or cache files. Before publication it must return exactly `ok`
from `PRAGMA integrity_check`, no row from `PRAGMA foreign_key_check`, match the
complete tracked SQLite schema and audited latest Diesel migration, and match
the complete projection contract. An unknown table, column, schema object,
missing object, missing column, or projection mismatch fails closed.

### Projection contract

`internal/upstream/recovery-projection.json` is the human-reviewed projection.
It lists every known table, every schema-ordinal column, and the canonical row
key. Table disposition is `included`, `excluded`, or `validate_only`.
`excluded_columns` applies only to an included table. The fully excluded tables
are `auth_requests`, `devices`, `event`, `sso_auth`, `twofactor_duo_ctx`, and
`twofactor_incomplete`. `__diesel_schema_migrations` is validation-only.
`users` excludes `updated_at`, `last_verifying_at`, `login_verify_count`, and
`stamp_exception`; `twofactor` excludes `last_used`. Every other known column
is included. Primary-key order is the default row key;
`collections_groups` uses `(collections_uuid,groups_uuid)` and `groups_users`
uses `(groups_uuid,users_organizations_uuid)`.

The projection JSON is tracked because the running binary consumes it and a
change is a recovery-identity contract review. It is never generated from the
schema automatically. The schema JSON is generated deterministically from the
audited Vaultwarden migration and is tracked because offline build and runtime
validation consume it. Either change requires generator verification, a real
consumer test, a second generation with no diff, and the real lifecycle gates.

### Canonical logical stream

`content_id` is SHA-256 over this exact byte stream:

1. Raw magic `vaultwarden-less-logical\0`.
2. One length frame containing the 40-byte ASCII project commit.
3. A big-endian uint64 count of included tables.
4. For each included table by raw UTF-8 table-name bytes: a name frame, a
   big-endian uint64 included-column count, one name frame per included column
   in schema ordinal order, a big-endian uint64 row count, then every row.
5. A big-endian uint64 referenced-file count, then for every manifest file
   except `db.sqlite3`, in raw UTF-8 path order: path frame, big-endian uint64
   size, and the 32 raw SHA-256 bytes.

A length frame is an unsigned big-endian uint64 byte length followed by exactly
that many bytes. Rows are sorted by the declared row-key columns using SQLite
ascending storage-class order and `BINARY` collation for text. A row key must
be unique. Values follow schema ordinal and use: NULL tag byte 0; INTEGER tag 1
and big-endian two's-complement int64; REAL tag 2 and big-endian IEEE-754
binary64 bits; TEXT tag 3 and a length frame of the exact SQLite text bytes;
BLOB tag 4 and a length frame of the exact bytes. Other storage classes or a
value outside the fixed representation fail. No Unicode, case, whitespace,
timestamp, filename, or path normalization occurs.

`archive_id` is SHA-256 over raw magic
`vaultwarden-less-archive\0`, a big-endian uint64 manifest-file count, and,
for every manifest file in raw UTF-8 path order, a path frame followed by a
length frame containing the exact staged file bytes. It therefore changes when
the online-backup bytes change even if the logical projection does not.

### Restic identity, deduplication, and restore

Every recovery snapshot repeats these tags:

```text
project_commit=<full-commit>
content_id=sha256:<64-lowercase-hex>
archive_id=sha256:<64-lowercase-hex>
```

The fixed Restic host is `vaultwarden-less` and fixed path is
`/staging/point`. Selection and prune require the host, path, and all three
identity tags. A retired schema tag makes the snapshot invalid rather than
compatible. Explicit snapshot IDs are full 64-character IDs.

For each target, the newest matching snapshot's `content_id` decides upload.
Equal content skips that target even when the observed `archive_id` differs.
Targets that missed content are retried and catch up without a new source
mutation. A source consistency failure publishes no target. Partial target
success is retained and reported; it is not rolled back.

Restore uses a temporary real directory beneath the output parent, refuses an
existing output and every symlink ancestor, and verifies the strict manifest,
listed file set, file modes, sizes and hashes, SQLite integrity and foreign
keys, exact schema and migration, projection, database references, both IDs,
and snapshot tags. It then atomically renames without replacement to the
previously absent `/restore/<name>`. Failure removes only the task-owned
temporary tree and never exposes a partial output. RSA material is not in the
manifest, so first startup generates a new key and invalidates all prior
access, refresh, invitation, and session state.

### Production restore boundary

Production restore is an operator runbook, not an automatic promotion command.
The selected restore must pass full verification and a drill, and its
`project_commit` must equal the current active identity. After exact
incident-specific authorization, the operator stops proxy, backup, and
Vaultwarden in that order and confirms no process holds the database.

On the same deployment-root filesystem the operator creates a unique
`state/incidents/<incident-id>/`, saves the selected manifest, renames existing
`data` without replacement to `pre-restore-data`, removes the manifest from the
verified restore tree, and renames that tree without replacement to `data`.
Every failure preserves both available trees. Rollback while stopped moves a
failed restore without replacement to `failed-restored-data` and restores the
original tree without replacement. Active identity is unchanged. A snapshot
from another commit or from a legacy/versioned contract is rejected.

Because the recovery tree excludes RSA material, old sessions must fail and a
fresh login, backup, restore, and drill must all succeed. Incident material is
not deleted without separate authorization. Product commands never perform
these promotion renames or infer permission from a successful backup.

## Release, images, and Nix

There is no `VERSION` contract. For a clean commit, immutable release is:

```text
RELEASE.<committer timestamp UTC YYYY-MM-DDTHH-MM-SSZ>-g<first-12-commit-hex>
```

The timestamp is the commit's integer committer time rendered in UTC, not wall
clock time. A clean tree means `git status --porcelain=v1
--untracked-files=all` is empty; ignored private/runtime files do not affect
identity. The three production images each have a local and immutable tag:

```text
vaultwarden-less-backup:local
vaultwarden-less-backup:<immutable-release>
vaultwarden-less-proxy:local
vaultwarden-less-proxy:<immutable-release>
vaultwarden-less-vaultwarden:local
vaultwarden-less-vaultwarden:<immutable-release>
```

For each image, `org.opencontainers.image.revision` is the full commit,
`org.opencontainers.image.version` is the immutable release, and
`org.opencontainers.image.created` is that commit time in UTC RFC 3339 seconds.
Local and immutable tags for one product point to the same image ID. Dirty
images report development identity and cannot serve or persist.

A release directory is created in a same-parent temporary directory and
atomically renamed to a previously absent requested output. It contains only:

```text
images.tar.gz
SHA256SUMS
```

The archive loads directly with standard `docker image load`, contains exactly
the six production references, and excludes Moto. Gzip has an empty original
filename and timestamp zero. `SHA256SUMS` is exactly one lowercase SHA-256,
two spaces, `images.tar.gz`, and LF. Failure removes the temporary output and
never overwrites an existing path. Two builds from the same clean commit and
inputs must make both files byte-identical.

The flake has one locked `nixos-unstable` input; treefmt follows it. Public
outputs are the existing packages, formatter, one
`devShells.<system>.default`, and checks. It exposes no `apps`. `checks.unit`
executes `go test ./... -count=1` as a validation derivation distinct from a
product package. Ordinary Go and Python tests, vet, GolangCI-Lint, Ruff,
formatting, contracts, and toolchain checks all participate in
`nix flake check`. Vaultwarden is `pkgs.vaultwarden`; Restic is unmodified
`pkgs.restic`; Go, Bitwarden CLI, and Moto come from the same locked package
set. Nixpkgs `knownVulnerabilities` remains effective. Project vulnerability
checking is `govulncheck ./...`.

`UPSTREAMS.toml` contains only human-audited repository, tag, commit, audit
paths, Vaultwarden migration, schema path, and projection path. It does not
repeat image digests, vendor hashes, release archive hashes, or tool source
hashes already owned by nixpkgs and `flake.lock`. Checks compare actual package
versions and readable source revisions to the audit record.

There is no Restic patch or custom derivation, Trivy, Syft, CycloneDX, bundle
JSON, source archive, license archive, generated license index, or tracked
third-party license copy. The project remains MIT under `LICENSE`; `NOTICE.md`
accurately notes Vaultwarden's AGPL obligations. The project does not claim a
complete SBOM or license/source archive.

### Public Just interface

The single root `Justfile` has no modules, groups, aliases, private helpers, or
recipe dependencies. `just --list` exposes exactly:

```text
check
check-release <A-full-commit>
release <A-full-commit> <output-directory>
```

`just` is invoked directly, never through `nix develop`. `check` is one
`nix flake check` invocation. `check-release` enters the default development
shell once to run `scripts/check_release.py`; that script directly invokes its
fixed tools and never invokes Just. `release` similarly runs the tested
`scripts/release.py` build-machine workflow. Production operation and
maintenance use documented raw Compose commands. All retired recipes,
including `soak`, have no alias or compatibility fallback.

`check-release A` requires a full clean A distinct from clean HEAD B and, in
order, runs: `nix flake check`; full-tree race; full-tree coverage;
`govulncheck`; every binary and production/Moto image build; Moto/Restic
integration; expanded backup/restore/drill E2E; accelerated fake-clock
multi-target threshold integration; short production-image dual-repository
failure isolation/catch-up E2E; initial import E2E; A/B accept E2E; and A/B
rollback E2E. It then revalidates A, B, and the unchanged worktree.

On success it atomically creates
`tmp/release-evidence/<A>..<B>/candidate.json`. The directory is ignored and
`0700`; the canonical `0600` JSON contains exact A/B, UTC millisecond start and
finish instants, the ordered passed gate names, and `"status":"passed"`. It
contains no version field, secret, or raw stderr. Evidence has no wall-clock
expiry; any mismatched commit or byte content invalidates it.

```json
{"from_commit":"<A-full-commit>","to_commit":"<B-full-commit>","started_at":"2026-08-30T10:00:00.000Z","finished_at":"2026-08-30T10:30:00.000Z","gates":["nix-flake-check","go-race","go-coverage","govulncheck","products","moto-restic","backup-restore-drill","multitarget-threshold","dual-repository","initial-import","upgrade-accept","upgrade-rollback"],"status":"passed"}
```

`release A output-directory` requires an absent output, an existing real
parent, a clean B, and byte-valid candidate evidence for that exact A/B. If the
output is inside the Git tree it must be ignored. Two independent temporary
builds must produce byte-identical `images.tar.gz` and `SHA256SUMS`. Standard
image loading must prove exactly six references, three distinct image IDs,
matching local/immutable pairs, OCI identity, entrypoints, expected upstream
version, and absence of Moto. Before the final no-replace rename, canonical
`artifact.json` is atomically written beside candidate evidence with its hash,
A/B, immutable release, both output hashes, UTC millisecond completion, and
`"status":"verified"`. It attests verification, not external publication.
Failure removes only task-owned temporary paths and never replaces output.

```json
{"candidate_sha256":"sha256:<64-lowercase-hex>","from_commit":"<A-full-commit>","to_commit":"<B-full-commit>","release":"<B-immutable-release>","files":{"SHA256SUMS":"sha256:<64-lowercase-hex>","images.tar.gz":"sha256:<64-lowercase-hex>"},"verified_at":"2026-08-30T10:40:00.000Z","status":"verified"}
```

### First versionless release identities

`L` is the last complete legacy/schema-marked commit and release actually kept
for rollback. `C` is the first complete, fully green, unpublishing versionless
implementation commit. `B` is a later, distinct, fully green release-candidate
commit that preserves the same versionless runtime contract.

The first release requires two independent evidence paths. Normal
`check-release C` and `release C <output>` establish C→B A/B state-machine and
artifact evidence. Separately, an isolated L→B rehearsal starts with a fresh
deployment root, fresh Restic repository prefix, and B-owned configuration and
state. It never parses L project state or snapshots: a completely stopped copy
of L's raw Vaultwarden data is placed at B's `import/source` and processed by
B's ordinary external-import flow, including upstream migration, RSA rotation,
two-client data verification, new-repository backup, restore, and drill.

The complete L commit, images, root, and repository remain intact. Before edge
traffic switches, failure recovery is to stop B and restart complete L. After
an authorized switch, L remains until B backup, restore, drill, and a separately
defined observation period pass. This contract neither authorizes a production
switch nor deletion of L.

The ignored canonical `tmp/release-evidence/<L>..<B>/cutover.json` shape is:

```json
{"legacy_commit":"<L-full-commit>","to_commit":"<B-full-commit>","legacy_source_tree_id":"sha256:<64-lowercase-hex>","import":{"project_commit":"<B-full-commit>","active_release":"<B-immutable-release>","source_tree_id":"sha256:<64-lowercase-hex>","candidate_tree_id":"sha256:<64-lowercase-hex>"},"repository":{"prefix":"synthetic-fresh-prefix","snapshot_id":"<64-lowercase-hex>","content_id":"sha256:<64-lowercase-hex>","archive_id":"sha256:<64-lowercase-hex>"},"verification":{"two_users":true,"user_data":true,"old_sessions_rejected":true,"restore":true,"drill":true,"rollback":true},"status":"passed"}
```

Only synthetic identifiers may enter this evidence. A real endpoint, account,
credential, personal value, database row, or raw command output is forbidden.

## Upgrade and explicit rollback

Upgrade applies only between two different clean commits that both implement
this versionless contract. The upgrade ID is `<A-full-commit>..<B-full-commit>`.
A owns `upgrade prepare`, `upgrade freeze`, and `rollback apply`. B owns
`upgrade migrate-check`, `upgrade verify`, and `upgrade accept`. A never opens,
decodes, hashes, or makes a decision from `candidate.json`; moving that fixed
path as opaque evidence is allowed.

State is under:

```text
state/upgrade/current/
  prepare.json
  rollback-ticket.json
  candidate.json
  decision.json
  compose.env
  rollback-data/
state/upgrade/history/<upgrade-id>/
state/upgrade/quarantine/<upgrade-id>/
```

Every state directory is real, owned, and `0700`. Every JSON or env file is a
real `0600` file, written and fsynced in a same-directory temporary file and
atomically renamed without following symlinks. JSON rejects duplicate/unknown
fields and trailing values. An upgrade state file is at most 1 MiB. Recovery
data follows the normal private tree rules.

`prepare.json` is immutable after successful creation:

```json
{
  "upgrade_id": "<A>..<B>",
  "phase": "prepared",
  "from_commit": "<A>",
  "from_release": "<A-immutable-release>",
  "to_commit": "<B>",
  "to_release": "<B-immutable-release>",
  "prepared_at": "2026-08-27T12:34:56.789Z"
}
```

`compose.env` contains exactly these seven unique, unquoted assignments in
this order and no other content:

```dotenv
DEPLOYMENT_ROOT=<absolute-directory>
RUNTIME_UID=<decimal>
RUNTIME_GID=<decimal>
LISTEN_PORT=<decimal>
DRILL_PORT=<decimal>
A_RELEASE=<A-immutable-release>
B_RELEASE=<B-immutable-release>
```

It is passed explicitly to the tracked upgrade overlay. Both release values
are validated immutable tags and exactly match `prepare.json`.

`rollback-ticket.json` is created once by freeze and is then immutable:

```json
{
  "upgrade_id": "<A>..<B>",
  "phase": "frozen",
  "from_commit": "<A>",
  "from_release": "<A-immutable-release>",
  "to_commit": "<B>",
  "to_release": "<B-immutable-release>",
  "active_path": "data",
  "rollback_path": "state/upgrade/current/rollback-data",
  "active_device": 1,
  "rollback_device": 1,
  "rollback_tree_id": "sha256:<64-lowercase-hex>",
  "frozen_at": "2026-08-27T12:34:56.789Z",
  "targets": [{
    "target": "primary",
    "snapshot_id": "<64-lowercase-hex>",
    "content_id": "sha256:<64-lowercase-hex>",
    "archive_id": "sha256:<64-lowercase-hex>"
  }]
}
```

Device values are unsigned filesystem device IDs read after both paths exist;
they must be equal at freeze and again at rollback. Paths are the exact
relative literals shown. `rollback_tree_id` hashes raw magic
`vaultwarden-less-rollback\0`, an entry count, and every non-symlink regular
file by UTF-8 relative path: a path frame, big-endian uint64 mode, and a content
frame. Directories must be owned `0700`; files must be owned `0600`; special
files, hard-link aliases, and unexpected symlinks fail. The frozen database is
closed and has no live sidecar before this copy is accepted.

`candidate.json` is B-owned and atomically replaced at B phase transitions:

```json
{
  "upgrade_id": "<A>..<B>",
  "phase": "verified",
  "from_commit": "<A>",
  "to_commit": "<B>",
  "database_migration": "<audited-B-migration>",
  "database_schema_fingerprint": "sha256:<64-lowercase-hex>",
  "updated_at": "2026-08-27T12:34:56.789Z",
  "targets": [{
    "target": "primary",
    "snapshot_id": "<64-lowercase-hex>",
    "content_id": "sha256:<64-lowercase-hex>",
    "archive_id": "sha256:<64-lowercase-hex>"
  }]
}
```

Phase is only `migrated` or `verified`. `targets` is empty while migrated and
equals the configured verification-target order after verify. Acceptance never
rewrites this evidence document and the old `accepted`/`accepted_at` shape is
invalid state.

`decision.json` is created once and is immutable. It has exactly:

```json
{
  "upgrade_id": "<A>..<B>",
  "decision": "accept",
  "active_project_commit": "<B>",
  "active_release": "<B-immutable-release>",
  "decided_at": "2026-08-27T12:34:56.789Z"
}
```

`decision` is `accept` with B's exact identity or `rollback` with A's exact
identity. A conflicting decision can never replace it. Once it exists, only
the matching terminal command may continue.

The required production sequence is:

1. B migrates a disposable A restore, creates a B snapshot, restores it through
   real Restic, and starts an isolated B drill.
2. Production proxy stops first; then backup and Vaultwarden stop. No new write
   is admitted.
3. Before loading B, immutable A rechecks source consistency, creates final
   snapshots on configured verification targets, really restores them, makes
   and validates the same-filesystem rollback tree, and writes the immutable
   ticket.
4. B is loaded; local aliases move to B while every A immutable tag remains.
5. Only B Vaultwarden starts and migrates active data. Proxy and normal backup
   remain stopped.
6. B checks exact schema/migration, snapshots only verification targets,
   restores each through Restic, and starts an isolated B drill.
7. `upgrade accept` is explicit. It validates the healthy B drill, atomically
   creates the accept decision, and then idempotently changes active identity
   from A to B. An interruption after the decision can only resume accept and
   does not repeat the pre-decision drill authorization.
8. The documented raw Compose sequence starts B's three normal services,
   waits for health, proves every container image ID matches its immutable B
   tag and OCI identity, removes only the verified drill resources, and then
   invokes `upgrade finalize`. Finalize validates the immutable decision,
   active identity, data evidence, and recorded cleanup before moving current
   to `history/<upgrade-id>`; it does not infer container health. The rollback
   copy remains in history, while ordinary rollback is permanently rejected
   once accept is decided.

Evidence can be reused only when A snapshot ID, content ID, archive ID, project
commit, B commit, configured target order, and state-file bytes all match.
Prepare is idempotent only for the exact same identity. Freeze is idempotent
only after revalidating the immutable ticket and rollback tree. Every other
repeat, illegal phase, corrupt or missing state, commit mismatch, target
mismatch, cross-filesystem layout, or changed evidence returns `conflict` or
`upgrade_state_invalid` before side effects.

No pre-accept failure automatically rolls back. Proxy remains stopped. The
operator invokes immutable A `rollback apply`. A validates only its prepare,
ticket, exact active/rollback paths, device IDs, and frozen A tree; it treats a
safe `candidate.json` as opaque. It first atomically creates the rollback
decision, then uses Linux `renameat2(RENAME_EXCHANGE)` only when the tree IDs
show A is still in `rollback-data`. If `data` already has A's tree ID, a rerun
does not exchange again. The command finally idempotently confirms active A;
after verifying the exchange it refreshes A's `activated_at` no earlier than
the rollback decision. That refresh is the startup marker which lets all three
ordinary A services open the now-mutable tree without repeatedly hashing it;
an interruption before the refresh leaves every service gate closed. A rerun
preserves the refreshed instant. The command does not rewrite A data or remove
RSA state.

`rollback-a` is the immutable-A, networkless, non-root administrative role. It
has one writable deployment-root bind at `/deployment-root` because Linux
returns `EXDEV` when `data` and `rollback-data` are exposed as separate mount
points. The command derives only `.env`, `data`, `restore`, and `state` paths
under that fixed root; its container root filesystem is read-only, all
capabilities are dropped, `no-new-privileges` is set, and no engine socket is
mounted.

The documented raw Compose rollback sequence stops B and drill services,
invokes that resumable A command, verifies and retags only A's three immutable
images as local, starts and verifies the three ordinary A services, removes
only the verified drill resources, and invokes A `upgrade finalize`. Until
then current retains the decision, candidate, and exchanged B tree. Finalize
moves that complete directory to `quarantine/<upgrade-id>`. Any interruption
can repeat only the same recorded decision; neither data tree is deleted and
the opposite decision remains impossible.

Migration, repository verification, and drill failures are induced in tests by
real read-only permissions, an invalid disposable target, and corrupted
disposable restore data. Product binaries have no failure test hook. Kill and
restart after every phase must yield either a valid idempotent continuation or
a stable refusal, never an inferred transition.

Framework verification requires different A/B clean commits and immutable
tags and covers that exact managed release pair. When both releases contain the
same actual Vaultwarden version, this is complete framework evidence and not an
unverified compatibility obligation. Exact-pair cross-version verification is
an additional claim available only after the same scenario runs with two
managed versionless releases whose actual Vaultwarden versions differ. Neither
result promises compatibility for an upstream version or build that the
project does not manage.

## One-time external SQLite import

Import is available only before the first active identity exists. It accepts a
complete copy of a fully stopped external Vaultwarden data directory; it is not
an online copy, merge, repeat import, or restore-promotion interface. The
operator retains the original tree outside the deployment clone and copies it
into `import/source`. The external original is never mounted into a project
container. Project code treats `import/source` as immutable and never writes or
deletes it.

The fixed layout is:

```text
import/source/
import/candidate/
state/import/current/
  prepare.json
  verification.json
  decision.json
state/import/history/initial/
```

The deployment root, `data`, `import`, `import/source`, `state`, and all import
state directories are real, owned `0700` directories. Every source directory
is owned `0700`; every source file is an owned, single-link `0600` regular
file. Symlinks, hard-link aliases, sockets, devices, FIFOs, path escape,
cross-device entries, invalid UTF-8 paths, or a changing entry fail before a
candidate is published. `data`, `import/source`, and `import/candidate` must be
on the same nonzero filesystem device. Import state JSON follows the canonical,
single-value, duplicate/unknown-field-rejecting, 1 MiB, owned `0600` rules used
by active and upgrade state.

`deployment import prepare` requires no active identity, completely empty
`data`, no current upgrade/import, a root `db.sqlite3`, and a root
`rsa_key.pem`. It hashes the complete stopped source, copies ordinary files
into a private temporary candidate, and excludes the source database,
SQLite sidecars, and all three RSA files. It first stable-copies the source
database plus any `-wal`, `-shm`, or `-journal` sidecar into candidate-private
scratch, then uses the SQLite Online Backup API against that scratch to create
one closed `candidate/db.sqlite3`. Thus committed WAL content is absorbed
without opening or changing `import/source`; the candidate retains no SQLite
sidecar and no old RSA identity. Directories and files are normalized to
`0700`/`0600`. Before publishing state, prepare exchanges two disposable
sibling directories with Linux `renameat2(RENAME_EXCHANGE)` and rejects a
filesystem that cannot perform the required atomic promotion.

`source_tree_id` and `candidate_tree_id` use
`vaultwarden-less-import-tree\0`, a big-endian entry count, then sorted
UTF-8 slash-relative entries. Each entry contributes a length-framed path, a
`d` or `f` byte, and a big-endian uint64 permission mode; each file then
contributes its big-endian uint64 size and exact bytes. `source_rsa_key_id`
uses the same size-plus-bytes file digest. All IDs are lowercase SHA-256 with
the `sha256:` prefix.

Immutable `prepare.json` has exactly:

```json
{
  "import_id": "initial",
  "phase": "prepared",
  "project_commit": "<B-full-commit>",
  "release": "<B-immutable-release>",
  "source_path": "import/source",
  "candidate_path": "import/candidate",
  "data_path": "data",
  "filesystem_device": 1,
  "source_tree_id": "sha256:<64-lowercase-hex>",
  "candidate_tree_id": "sha256:<64-lowercase-hex>",
  "source_rsa_key_id": "sha256:<64-lowercase-hex>",
  "prepared_at": "2026-08-28T12:34:56.789Z"
}
```

Repeating prepare resumes only that exact immutable state and source. It may
recover a matching private temporary candidate or deterministically rebuild
one whose tree ID equals the preparation. A changed source, identity, device,
candidate, active document, or unrelated candidate path is a hard failure.

`import-vaultwarden` is the current production Vaultwarden image in the
explicit `import` gate mode. It mounts only `import/candidate` as writable
`/data`, mounts `import/source` and state read-only, disables registration,
push, icons, and the Web Vault, and publishes container port 80 only on
loopback `DRILL_PORT`. The operator routes the existing trusted HTTPS edge to
that loopback endpoint and logs in with a real Bitwarden client. Direct
plaintext HTTP probing is not client-login evidence. Vaultwarden owns its
upstream migration and generates a new RSA identity in the candidate.

After the candidate is stopped, `deployment import verify
--confirm-client-login` rehashes the unchanged source, normalizes the candidate,
requires its `rsa_key.pem` digest to differ from the source, closes/removes
SQLite sidecars, runs `integrity_check`, `foreign_key_check`, the exact audited
migration and schema fingerprint checks, and the ordinary snapshot closure for
database rows, attachments, and file Sends. The flag is mandatory usage input;
without a real successful HTTPS client login the operator must not supply it.
Immutable `verification.json` has exactly:

```json
{
  "import_id": "initial",
  "phase": "verified",
  "project_commit": "<B-full-commit>",
  "release": "<B-immutable-release>",
  "source_tree_id": "sha256:<64-lowercase-hex>",
  "candidate_tree_id": "sha256:<64-lowercase-hex>",
  "database_migration": "<audited-B-migration>",
  "database_schema_fingerprint": "sha256:<64-lowercase-hex>",
  "content_id": "sha256:<64-lowercase-hex>",
  "archive_id": "sha256:<64-lowercase-hex>",
  "client_login_confirmed": true,
  "verified_at": "2026-08-28T13:34:56.789Z"
}
```

`deployment import accept` first revalidates source, candidate, empty data,
device, verification, and absent active identity, then atomically creates:

```json
{
  "import_id": "initial",
  "decision": "accept",
  "active_project_commit": "<B-full-commit>",
  "active_release": "<B-immutable-release>",
  "decided_at": "2026-08-28T14:34:56.789Z"
}
```

After that immutable boundary, cancel is permanently rejected. Accept uses the
verified candidate tree ID and exact empty-tree state to decide whether
`RENAME_EXCHANGE(data, import/candidate)` has not happened or already happened;
it never blindly exchanges twice. It then verifies live `data`, verifies that
the exchanged candidate is exactly empty, and publishes the first active B
identity no earlier than `decided_at`. An interruption after decision can only
repeat accept.

The documented raw Compose accept sequence stops and removes the candidate
service, resumes the Go accept operation, starts and verifies the three normal
B services, and invokes `deployment import finalize`. Finalize accepts only
the exact active/decision ordering, verified data identity, and an empty
exchanged candidate, removes that exact empty directory, and atomically
archives current as `state/import/history/initial`. It records durable-state
consistency only; adjacent Compose `ps`, container `version`/health checks, and
image inspection own runtime proof. Repeating the terminal sequence can only
continue the same recorded decision. `deployment import cancel` is available
only before a decision and removes only the exact disposable candidate,
matching candidate temporaries, and current state; it never removes source or
data.

Every import command returns the ordinary one-line success/error contract.
The import success document has `import_id`, `phase`, exact project identity,
source/candidate tree IDs, and
`client_login_confirmed`. Phases exposed by those commands are `prepared`,
`verified`, `accepted`, `cancelled`, and `finalized`. Unsafe or malformed state
uses `import_state_invalid`; changed/unsafe source uses `source_inconsistent`;
tree, RSA, database, or closure failure uses `verification_failure`; durable
state conflicts use `conflict`.

## Development scripts

The only durable Python scripts are `capture_vaultwarden_schema.py`,
`check_release.py`, `release.py`, `upstream_diff.py`, and
`verify_go_test.py`; each uses only the standard library and has a focused
contract test. `test_justfile.py` owns the three-recipe allowlist and their
final dry-run argv. Production terminal workflows are documented Compose and
product-CLI steps, not a host Python interface. Schema capture, release, and
upstream diff may print one result path followed by LF. The release checker and
Go verifier print one strict result.

Their exact command surfaces are:

```text
capture_vaultwarden_schema.py --database <sqlite-file> [--upstreams UPSTREAMS.toml] [--output internal/upstream/vaultwarden-schema.json]
check_release.py <A-full-commit>
release.py <A-full-commit> <output-directory>
upstream_diff.py <vaultwarden|bitwarden-cli|restic|moto|go> <candidate-tag> [--upstreams UPSTREAMS.toml]
verify_go_test.py --require <package>:<test> [--require ...] -- <JSON-event-producing test command>
```

`check_release.py` requires the exact clean pair and gate order defined by the
public Just contract, stops on the first failure, and rechecks the pair before
atomically writing candidate evidence. It never emits raw child stderr into
evidence and does not invoke Just.

`verify_go_test.py` validates the child stream rather than requiring a literal
`-json` token in child argv. Every line must still be a valid Go test JSON
event, and each required test must have ordered run, test PASS, later package
PASS, no skip/fail event, and a zero child exit. Project recipes use
`go test -json`; repository-external runners may equivalently feed the locked
`go tool test2json` stream.

Schema capture refuses a symlink database or output/parent, opens SQLite
read-only with WAL-aware semantics, checks `quick_check`, exact audited latest
migration, and nonempty complete schema objects, and atomically replaces only
its explicit output after fsync. A corrupt database, changed migration, output
failure, or interruption leaves the prior output byte-identical and no
half-file. Object order is `(type,name)`, SQL is trimmed only at both ends, JSON
uses UTF-8, two-space indent, LF, and a deterministic framed fingerprint. An
unchanged database produces identical bytes even through a path containing
spaces, tabs, newlines, backslashes, or non-ASCII characters.

Release validates exact candidate evidence, independently builds twice, loads
and verifies each production image set, assigns exactly six production
references, validates labels and image-ID pairs, and writes deterministic gzip
and checksum output in task-owned same-parent temporary directories. It
refuses an existing final directory. Standard checksum and image-load tools,
not a script subcommand, perform external re-verification. A subprocess or
inspection failure cleans only task-owned temporary output.

Upstream diff resolves both tags to full commits in an ignored cache with an
exact expected origin. Changed paths come only from `git diff --name-status -z
--find-renames --find-copies`. It parses status and NUL fields as bytes before
filesystem decoding. Rename/copy old and new paths are each matched against the
audited globs. Every path and commit subject in Markdown is JSON-quoted with
ASCII escapes, so tabs, LF, backslashes, Unicode, backticks, and Markdown
control characters cannot alter report structure. The report is atomically
written below the component's ignored audit directory and its path is printed.

The verifier's required identity is `<package>:<test>`. It accepts a required
test only after seeing, in that package, test run then PASS, later package PASS,
and process exit 0. A skip, fail, missing package, wrong order, malformed JSON,
or same test name in a different package fails verification.
