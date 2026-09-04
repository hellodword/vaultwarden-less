# Product contracts

This document is the sole normative product contract. The Compose file,
configuration, binaries, tests, release bundle, and operator documentation must
agree with it. It does not authorize publication, deployment to a real service,
or mutation of real data.

This is a direct breaking redesign of an early project. The former CLI,
configuration, Compose layout, and state documents have no compatibility layer
or project-supplied migrator. The recovery manifest, Restic tags, content and
archive identities, and Restic repository format remain compatible.

## Product boundary

The project is responsible only for:

- running Vaultwarden, a resident backup worker, and a write-aware proxy;
- constructing a consistent recovery point from a running SQLite Vaultwarden;
- synchronizing that point to zero or more Restic targets; and
- repository maintenance, status, manual backup requests, and verified restore
  commands.

The project does not initialize or activate deployments, run a stateful import,
coordinate release transitions or rollback, replace production `data`, provide
TLS or public access control, manage provider IAM or object lifecycles, publish
artifacts, sign releases, produce an SBOM, or discharge redistribution
obligations for an operator.

## Runtime and Compose

One `compose.yml` is the complete runtime definition. It contains exactly these
services:

| Service | Responsibility | Mounts | Networks |
| --- | --- | --- | --- |
| `vaultwarden` | Run the selected upstream Vaultwarden directly | writable `data`; `vaultwarden.env` | project `default` |
| `backup` | Resident worker and internal HTTP API | backup configuration and credentials; Restic passwords; read-only `data`; writable `staging` and `cache` | project `default` |
| `proxy` | Transparently forward traffic and asynchronously report writes | none | project `default`; loopback port only |
| `maintenance` | One-shot config, repository, and restore commands | maintenance credentials; Restic passwords; writable `cache` and `restore` | project `default`; profile-gated |

Every active service joins only the one project-scoped `default` bridge that
Compose creates implicitly. The network is not internal and is not a security
boundary between these four trusted services. There are no custom networks,
named data volumes, secondary Compose files, dedicated config checker, or
special-purpose drill or data-adoption networks.

Every service has an explicit non-root UID/GID, a read-only root filesystem,
`tmpfs` at `/tmp`, `no-new-privileges`, all capabilities dropped, bounded log
rotation, and a health check. Every bind uses long syntax with
`bind.create_host_path: false`. No service mounts a container-engine socket.

Compose reads exactly these operator variables:

```dotenv
DEPLOYMENT_ROOT=/srv/vaultwarden-less
VWLESS_IMAGE_TAG=git-0123456789abcdef0123456789abcdef01234567
RUNTIME_UID=1000
RUNTIME_GID=1000
LISTEN_PORT=8080
```

The image tag is `git-` followed by one complete 40-character lowercase Git
commit. Runtime IDs are canonical decimal values from 1 through 4294967294.
`LISTEN_PORT` is a canonical decimal value from 1024 through 65535. Compose,
not project code, owns dotenv parsing and interpolation.

The deployment root has exactly this project-managed shape:

```text
DEPLOYMENT_ROOT/
  compose.env
  config/
    backup.yaml
    vaultwarden.env
  credentials/
    backup.yaml
    maintenance.yaml
    restic/
      <target-name>
  data/
  staging/
  cache/
  restore/
```

The root and each private directory are real, non-symlink directories owned by
the configured runtime UID/GID with mode `0700`. Each private file is a real,
non-symlink regular file owned by that identity with mode `0600`. Validation
fails without repairing permissions or ownership. Project code never discovers
or replaces production `data` implicitly.

## Vaultwarden

The Vaultwarden image runs `/bin/vaultwarden` directly. The image or Compose
fixes these values:

```dotenv
ROCKET_ADDRESS=0.0.0.0
ROCKET_PORT=80
DATA_FOLDER=/data
DATABASE_URL=sqlite:///data/db.sqlite3
RSA_KEY_FILENAME=/data/rsa_key
ATTACHMENTS_FOLDER=/data/attachments
SENDS_FOLDER=/data/sends
WEB_VAULT_ENABLED=false
DISABLE_ICON_DOWNLOAD=true
ICON_CACHE_TTL=0
```

The supplied runtime template contains only:

```dotenv
SIGNUPS_ALLOWED=false
INVITATIONS_ALLOWED=false
```

The project neither sets nor validates a public domain. Operators may add other
upstream Vaultwarden environment settings; the project does not translate or
validate them. Web Vault assets are absent. End users access the service with an
official Bitwarden desktop, browser-extension, or mobile client.

The proxy publishes only `127.0.0.1:${LISTEN_PORT}`. A trusted external HTTPS
reverse proxy is a deployment prerequisite and remains operator-owned.

## Backup configuration

`config/backup.yaml` is a strict YAML document of at most 1 MiB. Its only
top-level fields are the required arrays `targets` and `notifiers`:

```yaml
targets:
  - name: primary
    repository: s3:s3.eu-central-1.amazonaws.com/example/vaultwarden
    options: []
  - name: b2
    repository: s3:https://s3.eu-central-003.backblazeb2.com/example/vaultwarden
    options:
      - name: s3.bucket-lookup
        value: path
notifiers:
  - name: operations
    type: wechat
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me
```

Unknown fields, duplicate keys at any depth, multiple documents, implicit YAML
types, merge keys, anchors, aliases, and tags are rejected. Mapping order is not
semantic. Target and notifier array order is semantic and is preserved.

`targets` contains 0 through 32 entries. Each unique target name matches
`[a-z][a-z0-9-]{0,62}`. `repository` is a nonempty string of at most 4096 bytes
without NUL, CR, or LF and is passed to Restic as one structured argument.
`options` is always explicit and contains at most 64 `{name, value}` objects.
Option names are unique within one target, 1 through 256 bytes, and contain no
NUL, CR, LF, or `=`. Values are strings of at most 4096 bytes without NUL, CR,
or LF. Each option becomes the two arguments `--option` and `name=value` in
configuration order.

`notifiers` contains 0 through 16 entries with unique names using the target
name syntax. A WeChat notifier has exactly these fields:

```yaml
- name: operations
  type: wechat
  webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me
```

The URL is absolute HTTPS, at most 4096 bytes, has a host and nonempty path, and
has no user info, fragment, NUL, CR, or LF. A Bark notifier has exactly these
fields, except that `sound` is optional:

```yaml
- name: operations
  type: bark
  device_token: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  auth_key: |
    -----BEGIN PRIVATE KEY-----
    replace-me
    -----END PRIVATE KEY-----
  key_id: KEY123
  team_id: TEAM123
  topic: me.example.app
  sound: "1107"
```

The device token is 64 lowercase hexadecimal characters. `auth_key` is one
PKCS#8 P-256 private key in PEM form, at most 16 KiB, with no NUL or trailing
non-whitespace data. Key ID, team ID, topic, and sound are at most 255 bytes and
contain no NUL, CR, or LF; required values are at least two bytes. An omitted or
empty sound uses `1107`. Each notifier type rejects fields belonging to the
other type. Errors and logs never disclose notifier values.

An explicit targetless configuration is:

```yaml
targets: []
notifiers: []
```

Missing or null arrays are invalid.

## Role credentials and Restic passwords

`credentials/backup.yaml` and `credentials/maintenance.yaml` are separate
strict documents with the same shape:

```yaml
targets:
  primary:
    environment:
      AWS_ACCESS_KEY_ID: replace-me
      AWS_SECRET_ACCESS_KEY: replace-me
  b2:
    environment:
      AWS_ACCESS_KEY_ID: replace-me
      AWS_SECRET_ACCESS_KEY: replace-me
```

The target key set in each role file exactly equals the configured target set.
Each environment mapping has at most 128 entries. Names match
`[A-Z_][A-Z0-9_]{0,127}`. Values are nonempty strings of at most 65536 bytes
without NUL. Names beginning with `RESTIC_`, plus `PATH`, `HOME`, `TMPDIR`,
`TMP`, `TEMP`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, and `GODEBUG`, are forbidden.
One role never falls back to the other.

`credentials/restic` contains exactly one ordinary private file for each target
and no other entry. A password is 1 through 65536 bytes, contains neither NUL
nor CR, and represents one nonempty line; one terminal LF is accepted. No
filename suffix is ignored.

For targetless operation, both role files contain exactly:

```yaml
targets: {}
```

and `credentials/restic` is genuinely empty.

Restic receives no ambient environment. It receives only the selected role's
target environment plus immutable project-controlled home, temporary, cache,
password, and trust-store values. Repository locators, credentials, passwords,
notifier values, and raw subprocess diagnostics are never logged or returned.

## Configuration validation

Validation uses the actual service mounts and never contacts a Restic
repository or constructs a repository runner:

```sh
docker compose --env-file "$DEPLOYMENT_ROOT/compose.env" -f RELEASE/compose.yml \
  run --rm --no-deps backup config validate --role backup

docker compose --env-file "$DEPLOYMENT_ROOT/compose.env" -f RELEASE/compose.yml \
  --profile maintenance run --rm --no-deps maintenance \
  config validate --role maintenance
```

The backup role validates the mounted backup configuration, backup credentials,
password directory, `data`, `staging`, and `cache`. The maintenance role
validates the mounted configuration, maintenance credentials, password
directory, `cache`, and `restore`. Validation does not mount or compare a whole
deployment root. Success returns semantic JSON containing `valid`, `role`, and
target names in configuration order.

## CLI and internal HTTP

The backup executable exposes only this command tree:

```text
config validate --role backup|maintenance
healthcheck
repo init|snapshots|check|unlock|prune
request-backup
restore
serve
status
version
```

`repo` and `restore` select one configured target by name. Restore accepts
`latest` or a complete 64-character lowercase snapshot ID. Successful results
and errors use semantic JSON. Errors use the envelope
`{"error":{"code":"stable_code","message":"Safe message"}}`; fields may be
added only where documented. JSON field order, help prose, command registration
order, and encoder byte choices are not contracts.

The resident worker has no host-published port. On the shared project network it
provides:

- `GET /healthz`, which reports process health;
- `GET /status`, which returns the current typed state;
- `POST /events`, which accepts a bounded write event and returns 204 when
  accepted or deliberately ignored; and
- `POST /checks`, which requests a backup and returns 202 when accepted.

Clients bound responses and verify status, content type, and JSON structure.
They do not revalidate every field produced by the same-version worker.

When `targets` is empty, a backup process emits exactly one WARN per process
startup with `warning_code=no_backup_targets`. It does not start scheduling,
create a staging point, construct a repository runner, or execute Restic. Its
status is semantically:

```json
{
  "state": "idle",
  "targets": [],
  "last_check": null,
  "next_check_at": null
}
```

`POST /events` returns 204. `POST /checks` returns 503 with `not_ready`.
`request-backup` exits nonzero and preserves that same error code.

## Proxy event boundary

The proxy transparently supports HTTP and WebSocket traffic. It emits an event
only for a Vaultwarden write route whose upstream response succeeds, and only
after the successful response body has been completely read. Event delivery is
asynchronous and can never change the response observed by the client. A
pending startup event and bounded failure retry remain supported.

The write-route set is maintained in code comments. An upstream version string,
test reason field, or a particular internal route-table representation is not a
contract.

## Snapshot and recovery format

Live SQLite snapshots use the SQLite Online Backup API; a plain database-file
copy under WAL is invalid. The recovery point contains `manifest.json`,
`db.sqlite3`, every database-referenced attachment, and every database-referenced
file Send. It excludes unreferenced files and the Vaultwarden RSA key.

The existing manifest schema and meanings of `project_commit`, `content_id`,
and `archive_id` remain unchanged. Canonical logical identity, archive identity,
projection validation, attachment/Send closure, deterministic ordering, and
golden encodings remain unchanged. SQLite schema and projection validation,
concurrent WAL consistency, available-space checks, and atomic publication are
required.

Recovery-point construction uses a process-owned `.build-*` directory with
mode `0700`. Cleanup may remove only a real directory whose name and ownership
prove it belongs to that convention. An unsafe same-prefix file, symlink,
special object, wrong mode, or wrong owner makes the build fail without removal.

Each Restic snapshot retains the three existing required identity tags. Each
required tag is present exactly once with a valid value. Additional unrelated
tags are accepted for forward compatibility. `repo prune` validates this rule
before applying the fixed retention policy:

```text
--keep-within 24h
--keep-hourly 48
--keep-daily 14
--keep-weekly 8
--keep-monthly 12
--keep-yearly 3
```

Repository format remains Restic format 2. `repo init` requests version 2;
there is no project command for a repository-format migration.

The Restic adapter uses structured arguments, a cleaned environment, bounded
stdout/stderr and JSON frames, forward-compatible parsing of unknown fields and
progress records, and process-group cancellation. Timeouts or cancellation stop
the whole subprocess group and wait for exit. One runner constructor owns
process execution, command formation, parsing, and repository behavior.

## Restore boundary

`restore` may select only a configured target and either `latest` or one full
snapshot ID. Its output is a valid, single path component below `/restore` that
does not already exist. It restores into an owned temporary directory, validates
the complete recovery point, and publishes atomically without replacement.
After any failure, the requested final path remains absent. It never writes to
or replaces production `data`.

Because Restic recovery points exclude RSA material, an operator who promotes a
restored tree lets Vaultwarden create a new RSA key. Existing sessions may no
longer authenticate; acceptance uses a fresh official-client login.

## Operator-controlled existing-data adoption

Adopting an existing instance is a documented offline file operation, not a
product command. Only SQLite Vaultwarden data is supported. The operator stops
the source instance, verifies that the destination `data` is absent or empty,
and copies the complete tree, including the database and WAL/SHM files, RSA
material, attachments, Sends, and `config.json`. Symlinks and special files are
rejected. The source tree is never modified. Vaultwarden performs its own
database migration at startup. Acceptance preserves the RSA digest and uses a
fresh official-client login; continuity of an old session is not required.

## Operator-controlled release transition and rollback

A release transition is an offline working-copy procedure, not product state or
a hard release gate:

1. Stop the current release A.
2. Atomically rename `data` to a previously absent
   `data.pre-<full-B-commit>` path.
3. Completely copy that unmigrated A tree to a new working `data`.
4. Allow only B Vaultwarden to open and migrate the working copy.
5. Retain A's release and original data directory.
6. To roll back, rename failed B data to a new `data.failed-*` name and rename
   the original A directory back to `data`, never replacing an existing path.

An isolated B exercise, repository check, or restore exercise is optional
evidence, not a transition or release prerequisite. Targetless operation does
not block the procedure and produces the documented warning.

## Release identity and bundle

The production identity is exactly `git-<40-lowercase-hex-commit>`. That same
value is used for all three production image tags, generated
`templates/compose.env`, the Go `version`, and the OCI version label. The OCI
revision label is the same full commit without `git-`.

`just release` is exactly:

```make
release:
    nix build .#release
```

The release builds only from a clean Git flake, creates the standard `result`
symlink, neither runs tests nor reads external evidence, and gives every
production derivation `doCheck = false`. It never reads `tmp/`, `dist/`,
`result`, or another ignored path.

The release directory contains exactly:

```text
SHA256SUMS
LICENSE
NOTICE.md
compose.yml
images/backup.tar.gz
images/proxy.tar.gz
images/vaultwarden.tar.gz
templates/compose.env
templates/config/backup.yaml
templates/config/vaultwarden.env
templates/credentials/backup.yaml
templates/credentials/maintenance.yaml
templates/credentials/restic/primary
templates/credentials/restic/b2
```

`SHA256SUMS` covers every other file and no absent file. The bundle contains no
source, tests, schemas, scripts, documentation beyond `NOTICE.md`, repository
service emulator, state, private configuration, release manifest, SBOM, or
signature. `NOTICE.md` states the operator's corresponding-source obligations
for the included AGPL Vaultwarden image.

The default template contains AWS S3 `primary` and Backblaze B2 `b2` targets,
but repositories, role credentials, and passwords are intentionally empty and
must fail validation until configured. An operator choosing targetless mode
must explicitly write both empty arrays, both empty role maps, and an actually
empty password directory.

## Formal release validation

The complete release-level acceptance command is:

```sh
nix build --no-link -L .#release-test
```

`just check-release` is exactly a convenience wrapper for that command. The
`release-test` package requires a clean `x86_64-linux` Git flake and KVM. It is
not part of `nix flake check`, the default package, or the release bundle.

Host-side Nix builds the current commit's release bundle, the test-only Moto
image, and the Go E2E executable. A disposable NixOS guest runs Docker and the
official Bitwarden CLI, loads the three production archives directly from that
release bundle, proves that the test process and Docker daemon share bind
paths, and runs exactly these scenarios:

```text
TestTargetlessDeploymentAndFullDataAdoption
TestTwoTargetBackupRestore
```

The scenarios retain their targetless, write-proxy, complete-data adoption,
registration closure, multi-target failure, repository check, restore,
fresh-login, and cleanup assertions. They use only synthetic temporary data.
A successful build is the formal VM acceptance result. A host without KVM must
report that missing capability and cannot substitute emulation, a remote Docker
daemon, or an ad hoc VM exercise. Distribution-specific Debian deployment and
SSH validation are outside this formal gate.

## Source documentation

The complete source documentation set is:

```text
README.md
docs/configuration.md
docs/operations.md
docs/contracts.md
docs/maintenance.md
```

These files respectively provide the entry point, configuration reference,
complete operator runbook, stable product contract, and upstream maintenance
procedure. No document or generated bundle silently expands the support scope.
