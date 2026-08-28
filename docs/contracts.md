# Schema 1 contracts

This document is the normative machine contract for the implemented schema 1
product. Tracked examples, Compose files, binaries, tests, and operator prose
must agree with it; none can silently redefine it.

Schema 1 deliberately has no migration from older product schemas or runtime
data. To read old configuration, snapshots, or state, check out the exact clean
commit that created it and use that commit's complete toolchain. No product
command authorizes destroying or overwriting real data, publishing, deploying,
or changing another durable contract.

All `schema_version` values in this document are the JSON or YAML integer `1`,
never a string or boolean. All instants are UTC RFC 3339 with exactly three
fractional digits, for example `2026-08-27T12:34:56.789Z`. A full commit is 40
lowercase hexadecimal characters. A digest is `sha256:` followed by 64
lowercase hexadecimal characters.

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

## Deployment clone

The deployment directory is an ordinary Git clone. There is no generated
deployment tree:

```text
compose.yml
compose.upgrade.yml
.env.example
.env
config/
  backup.example.yaml
  backup.yaml
  vaultwarden.example.env
  vaultwarden.env
credentials/
  backup.example.yaml
  backup.yaml
  maintenance.example.yaml
  maintenance.yaml
  notifiers.example.yaml
  notifiers.yaml
  restic/
    primary.example
    primary
data/
staging/
cache/
restore/
state/
dist/
tmp/
```

The non-example private files and all runtime directories are ignored. The
examples remain tracked. Never run `git clean -x` or `git clean -X` in a
deployment clone: either command can remove credentials and application data.
Normal operation consists of `git pull --ff-only`, checking and loading an
image archive, validating configuration, and `docker compose up -d`. Artifact
transport is outside the project contract and is not documented by this
repository.

`.env` is a non-symlink regular file owned by the runtime UID/GID with mode
`0600`. It contains each of these five assignments exactly once and contains no
other line:

```dotenv
DEPLOYMENT_SCHEMA_VERSION=1
RUNTIME_UID=1000
RUNTIME_GID=1000
LISTEN_PORT=8080
DRILL_PORT=8081
```

Assignments are ASCII `NAME=value` with no leading or trailing whitespace,
quotes, comments, `export`, interpolation, continuation, blank lines, or CR.
The last line may end at EOF or with one LF. Assignment order is not semantic.
`DEPLOYMENT_SCHEMA_VERSION` is exactly `1`; each runtime ID is canonical
decimal `1..4294967294`; each port is canonical decimal `1024..65535`; the two
ports differ. A leading plus, sign, leading zero, exponent, or surrounding
whitespace is invalid.

`config`, `credentials`, `credentials/restic`, `data`, `staging`, `cache`,
`restore`, and `state`, including every required runtime subdirectory, are real
non-symlink directories owned by the runtime UID/GID with mode `0700`. Every
private configuration, credential, password, manifest, and upgrade-state file
is a real non-symlink regular file owned by that identity with mode `0600`.
Every ancestor is inspected without following a symlink. Examples are public
templates and are not subject to private-file ownership checks until copied to
their private names.

`compose.yml` is the production topology and defines the `maintenance`,
`restore`, and `drill` profiles. `compose.upgrade.yml` is used only as an
overlay during an upgrade or rollback. Both files are normative. They use only
the three `vaultwarden-less-*:local` production aliases in normal operation,
contain no `build:` key, set `pull_policy: never`, publish only the proxy and
the isolated drill on loopback, never mount a container engine socket, and do
not pull from a registry. Every resident service uses the configured non-root
identity, a read-only root filesystem, `no-new-privileges`, and an empty
capability set. A bounded one-shot copy helper may run as root with only the
capabilities declared in `compose.yml`.

Before production `up`, both `config-check-backup` and
`config-check-maintenance` must succeed. Together they validate `.env`, all
private paths and modes, target/password key sets, role isolation, notifier
credentials, and both role configurations without contacting a repository.
An operator must not use `docker compose build` for this project.

## YAML configuration and credentials

Each schema 1 YAML file is at most 1 MiB, contains exactly one document, and is
decoded without implicit type conversion. Unknown fields, duplicate mapping
keys at any depth, merge keys, anchors, aliases, tags, and trailing documents
are rejected. Mapping order is not semantic. Target and notifier list order is
semantic and is preserved in output and execution.

### Backup configuration

`config/backup.yaml` has exactly this shape:

```yaml
schema_version: 1
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
Targets execute serially in configuration order. There are 0 to 16 notifiers;
notifier names use the target-name pattern and are unique. The only notifier
types are `wechat` and `bark`. This file never contains a secret.

### Role credentials and passwords

`credentials/backup.yaml` and `credentials/maintenance.yaml` have the same
strict shape but are separate files and security roles:

```yaml
schema_version: 1
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
schema_version: 1
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
upgrade prepare --to-commit <full-commit> --to-release <immutable-release>
upgrade freeze
upgrade migrate-check
upgrade verify
upgrade accept
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

Every error is strict schema 1:

```json
{"schema_version":1,"error":{"code":"invalid_config","message":"Safe message"}}
```

The exhaustive stable codes are `usage_error`, `invalid_config`,
`invalid_credentials`, `unsafe_path`, `not_ready`, `conflict`,
`source_inconsistent`, `unsupported_schema`, `repository_failure`,
`verification_failure`, `timeout`, `upgrade_state_invalid`, and
`internal_error`. The message is safe human context and is not a stable parsing
surface. No error has extra fields.

A dirty or non-Git build reports development identity. Only `version`,
`healthcheck`, `config validate`, and help may run in that state. Both `serve`
commands and every command that reads or changes runtime, repository, restore,
or upgrade state fail `not_ready` before side effects.

The complete success documents are:

```json
{"schema_version":1,"project_commit":"0123456789abcdef0123456789abcdef01234567","release":"RELEASE.2026-08-27T12-34-56Z-g0123456789ab","clean":true}
{"schema_version":1,"project_commit":null,"release":"development","clean":false}
{"schema_version":1,"healthy":true}
{"schema_version":1,"accepted":true}
{"schema_version":1,"valid":true,"role":"backup","targets":["primary"],"warnings":[]}
{"schema_version":1,"operation":"init","target":"primary","success":true}
```

`repo init`, `repo check`, and `repo unlock` use the last shape with their own
operation string. Config target order is configuration order. Schema 1 defines
no configuration warnings, so `warnings` is an empty array; retaining the field
prevents a later warning from changing the envelope.

Snapshots are newest first, and matching snapshots with equal timestamps are
ordered by ID descending:

```json
{
  "schema_version": 1,
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
  "schema_version": 1,
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
{"schema_version":1,"target":"primary","snapshot_id":"<64-lowercase-hex>","project_commit":"<full-commit>","content_id":"sha256:<64-lowercase-hex>","archive_id":"sha256:<64-lowercase-hex>","output":"/restore/drill"}
```

Upgrade and rollback commands use one common result. `targets` is present only
for `frozen` and `verified` and otherwise is an empty array:

```json
{
  "schema_version": 1,
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
  "schema_version": 1,
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

The private backup HTTP interface is schema 1:

- `GET /healthz` accepts no query or body and returns 204 with an empty body.
- `GET /v1/status` accepts no query or body and returns the CLI status JSON.
- `POST /v1/events` accepts the strict schema request and returns 204.
- `POST /v1/checks` accepts the same request and returns 202 plus the
  `request-backup` JSON.

The only request object is `{"schema_version":1}`. JSON whitespace is allowed,
but the object must have exactly one occurrence of that key, its value must be
integer 1, and no second JSON value may follow. Body size is at most 1024 bytes.
Content-Type is exactly `application/json` with no parameters. A query string,
wrong method, duplicate or unknown key, redirect, or extra document is
rejected. JSON HTTP errors use the CLI error envelope. Invalid input is 400,
wrong method is 405, conflict is 409, unsafe path is 422, not-ready is 503,
timeout is 504, repository failure is 502, and other runtime failures are 500.

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

`manifest.json` is strict schema 1:

```json
{
  "schema_version": 1,
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

1. Raw magic `vaultwarden-less-logical-v1\0`.
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
`vaultwarden-less-archive-v1\0`, a big-endian uint64 manifest-file count, and,
for every manifest file in raw UTF-8 path order, a path frame followed by a
length frame containing the exact staged file bytes. It therefore changes when
the online-backup bytes change even if the logical projection does not.

### Restic identity, deduplication, and restore

Every recovery snapshot repeats these tags:

```text
schema=1
project_commit=<full-commit>
content_id=sha256:<64-lowercase-hex>
archive_id=sha256:<64-lowercase-hex>
```

The fixed Restic host is `vaultwarden-less` and fixed path is
`/staging/point`. Selection and prune require the host, path, schema tag, and
all three identity tags. Explicit snapshot IDs are full 64-character IDs.

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
outputs include backup and proxy binaries, backup/proxy/Vaultwarden production
images, the Moto test image, formatter, checks, the default development shell,
and apps `.#just`, `.#image-load`, and `.#release`. Vaultwarden is
`pkgs.vaultwarden`; Restic is unmodified `pkgs.restic`; Go, Bitwarden CLI, and
Moto come from the same locked package set. Nixpkgs `knownVulnerabilities`
remains effective. Project vulnerability checking is only `govulncheck ./...`.

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

## Upgrade and explicit rollback

Upgrade applies only between two different clean commits that both implement
this schema 1 contract. The upgrade ID is `<A-full-commit>..<B-full-commit>`.
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
  "schema_version": 1,
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
DEPLOYMENT_SCHEMA_VERSION=1
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
  "schema_version": 1,
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
`vaultwarden-less-rollback-v1\0`, an entry count, and every non-symlink regular
file by UTF-8 relative path: a path frame, big-endian uint64 mode, and a content
frame. Directories must be owned `0700`; files must be owned `0600`; special
files, hard-link aliases, and unexpected symlinks fail. The frozen database is
closed and has no live sidecar before this copy is accepted.

`candidate.json` is B-owned and atomically replaced at B phase transitions:

```json
{
  "schema_version": 1,
  "upgrade_id": "<A>..<B>",
  "phase": "verified",
  "from_commit": "<A>",
  "to_commit": "<B>",
  "database_migration": "<audited-B-migration>",
  "database_schema_fingerprint": "sha256:<64-lowercase-hex>",
  "updated_at": "2026-08-27T12:34:56.789Z",
  "accepted_at": null,
  "targets": [{
    "target": "primary",
    "snapshot_id": "<64-lowercase-hex>",
    "content_id": "sha256:<64-lowercase-hex>",
    "archive_id": "sha256:<64-lowercase-hex>"
  }]
}
```

Phase is `migrated`, `verified`, or `accepted`. `targets` is empty while
migrated and equals the configured verification-target order after verify.
`accepted_at` is null before accept and is the acceptance instant after it.
All other fields are always present.

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
7. `upgrade accept` is explicit. Before it, B proxy and resident backup refuse
   to serve. A successful accept is the only state transition that permits the
   tracked orchestration to start them.
8. Accept moves the complete current directory to
   `history/<upgrade-id>` with accepted candidate evidence. The rollback copy
   is retained as history, but ordinary rollback is permanently rejected
   because external writes may now exist.

Evidence can be reused only when A snapshot ID, content ID, archive ID, project
commit, B commit, configured target order, and state-file bytes all match.
Prepare is idempotent only for the exact same identity. Freeze is idempotent
only after revalidating the immutable ticket and rollback tree. Every other
repeat, illegal phase, corrupt or missing state, commit mismatch, target
mismatch, cross-filesystem layout, or changed evidence returns `conflict` or
`upgrade_state_invalid` before side effects.

No pre-accept failure automatically rolls back. Proxy remains stopped. The
operator invokes immutable A `rollback apply`. A validates only its prepare and
ticket, the exact active and rollback paths, device IDs, and rollback tree. It
uses Linux `renameat2(RENAME_EXCHANGE)` to exchange active B data with A
rollback data atomically, moves the exchanged B tree to
`quarantine/<upgrade-id>/data`, moves `candidate.json` there without reading
it, records immutable rolled-back evidence in `history/<upgrade-id>`, and
removes RSA state from active A data. Tracked orchestration then starts only
the immutable A rollback services. Any failure leaves proxy stopped and does
not delete either data tree. After accept, current A ticket is absent and A
rollback returns `conflict` without reading history candidate data.

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
managed schema 1 releases whose actual Vaultwarden versions differ. Neither
result promises compatibility for an upstream version or build that the
project does not manage.

## Development scripts

The only durable Python scripts are
`capture_vaultwarden_schema.py`, `release.py`, `upstream_diff.py`, and
`verify_go_test.py`; each uses only the standard library and has a same-named
contract test. They are development tools, not the product CLI. Schema capture,
release, and upstream diff may print one result path followed by LF. The Go
verifier relays `go test -json` evidence and prints its single verification
result. They do not use the product JSON envelope.

Their exact command surfaces are:

```text
capture_vaultwarden_schema.py --database <sqlite-file> [--upstreams UPSTREAMS.toml] [--output internal/upstream/vaultwarden-schema.json]
release.py create --output-root <ignored-directory>
release.py load <release-directory>
upstream_diff.py <vaultwarden|bitwarden-cli|restic|moto|go> <candidate-tag> [--upstreams UPSTREAMS.toml]
verify_go_test.py --require <package>:<test> [--require ...] -- <go test -json command>
```

Schema capture refuses a symlink database or output/parent, opens SQLite
read-only with WAL-aware semantics, checks `quick_check`, exact audited latest
migration, and nonempty complete schema objects, and atomically replaces only
its explicit output after fsync. A corrupt database, changed migration, output
failure, or interruption leaves the prior output byte-identical and no
half-file. Object order is `(type,name)`, SQL is trimmed only at both ends, JSON
uses UTF-8, two-space indent, LF, and a deterministic framed fingerprint. An
unchanged database produces identical bytes even through a path containing
spaces, tabs, newlines, backslashes, or non-ASCII characters.

Release `create` enforces the clean identity and release/image contract above,
loads the three production Nix image outputs, assigns exactly six production references,
validates labels and image-ID pairs, runs one structured image save, writes
deterministic gzip and checksum output in a same-parent temporary directory,
and refuses an existing final directory. `load` verifies the two-file directory
and checksum before standard image load, then proves the six references point
to three exact image IDs and have the expected labels. A subprocess or inspect
failure cleans only task-owned temporary output.

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
