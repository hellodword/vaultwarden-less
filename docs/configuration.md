# Configuration

This document shows how to create deployment inputs. Exact validators and
limits are normative in [the product contracts](contracts.md). Storage-specific
endpoints and permissions are in [Storage providers](providers.md).

## Private layout and ownership

The Git clone contains tracked Compose files and public `*.example*` templates.
Its ignored `.env` points at an existing absolute `DEPLOYMENT_ROOT`; private
counterparts and runtime directories live under that separate root. Never put
real credentials in an example and never run `git clean -x` or `git clean -X`
in a deployment clone.

Create the layout as the account whose identity will run the containers:

```sh
vwless_root=/srv/vaultwarden-less
install -d -m 0700 "$vwless_root"
install -d -m 0700 "$vwless_root/config" "$vwless_root/credentials" "$vwless_root/credentials/restic"
install -d -m 0700 "$vwless_root/data" "$vwless_root/import" "$vwless_root/staging" "$vwless_root/cache" "$vwless_root/restore" "$vwless_root/state"
install -m 0600 .env.example .env
install -m 0600 config/backup.example.yaml "$vwless_root/config/backup.yaml"
install -m 0600 config/vaultwarden.example.env "$vwless_root/config/vaultwarden.env"
install -m 0600 credentials/backup.example.yaml "$vwless_root/credentials/backup.yaml"
install -m 0600 credentials/maintenance.example.yaml "$vwless_root/credentials/maintenance.yaml"
install -m 0600 credentials/notifiers.example.yaml "$vwless_root/credentials/notifiers.yaml"
install -m 0600 credentials/restic/primary.example "$vwless_root/credentials/restic/primary"
```

Every private directory must be a real, non-symlink directory with mode
`0700`. Every private file must be a real, non-symlink regular file with mode
`0600`. They and their required subdirectories must be owned by the exact
runtime UID/GID. Validation walks ancestors without following symlinks and
rejects group/world permissions rather than silently repairing them.

If the runtime identity differs from the invoking account, create or chown only
the explicit deployment paths with an administrative account. Do not apply a
recursive ownership change to a parent directory or an existing machine-wide
path.

## Deployment environment

The tracked `.env.example` shows five canonical unquoted assignments. Docker
Compose owns actual `.env` quoting, escaping, and interpolation; the product
validates only the five resolved values passed to its containers:

```dotenv
DEPLOYMENT_ROOT=/srv/vaultwarden-less
RUNTIME_UID=1000
RUNTIME_GID=1000
LISTEN_PORT=8080
DRILL_PORT=8081
```

- `DEPLOYMENT_ROOT` resolves to an existing absolute directory other than `/`.
- IDs are canonical decimal `1..4294967294` and must match private-path
  ownership.
- ports are canonical decimal `1024..65535`, must differ, and are published
  only on loopback by tracked Compose.
- resolved IDs and ports reject leading zeros, signs, or whitespace. Private
  roots, files, and directories reject symlinks.

The normal and drill ports may be chosen freely inside those bounds. Do not
publish either port directly on an untrusted interface.

## Backup configuration

`config/backup.yaml` defines ordered targets, the subset required during an
upgrade, and optional notifiers:

```yaml
schema_version: 1
upgrade_verification_targets:
  - primary
targets:
  - name: primary
    repository: s3:https://objects.example.invalid/vaultwarden-primary
    options:
      - name: s3.bucket-lookup
        value: path
notifiers:
  - name: operations
    type: wechat
```

Target names use lowercase letters, digits, and hyphens and begin with a
letter. Target order is semantic: backup and maintenance run serially in that
order. Each option is a separate name/value pair so arguments are passed to
Restic without shell interpretation. Repository and option values are never
logged.

For two repositories, add the second target and its independent password and
role credentials:

```yaml
schema_version: 1
upgrade_verification_targets:
  - primary
  - secondary
targets:
  - name: primary
    repository: s3:https://objects-a.example.invalid/vaultwarden
    options:
      - name: s3.bucket-lookup
        value: path
  - name: secondary
    repository: s3:https://objects-b.example.invalid/vaultwarden
    options:
      - name: s3.bucket-lookup
        value: path
notifiers: []
```

`upgrade_verification_targets` must contain configured target names and should
include every repository that must be proven before accepting B. Normal backup
still attempts every configured target regardless of this subset.

## Isolated role credentials

`credentials/backup.yaml` and `credentials/maintenance.yaml` have the same
schema and must contain exactly the configured target keys:

```yaml
schema_version: 1
targets:
  primary:
    environment:
      AWS_ACCESS_KEY_ID: replace-me
      AWS_SECRET_ACCESS_KEY: replace-me
```

Create separate provider keys for the two files. The backup key belongs only
to the resident worker. The maintenance key belongs only to one-shot init,
check, prune, restore, and upgrade commands. This separation limits a resident
service compromise and permits independent rotation.

Environment names match `[A-Z_][A-Z0-9_]{0,127}`. Empty values, NUL, and
oversized values are rejected. `RESTIC_*`, `PATH`, `HOME`, temp-directory
variables, loader variables, and debug/runtime injection variables are
reserved. Ambient host environment is not inherited.

Each target also has a separate password file at
`credentials/restic/<target>`:

```text
replace-with-a-long-random-restic-password
```

The password file is raw text with one optional final LF. Do not reuse a cloud
secret, Vaultwarden password, or another target's Restic password. Losing this
password makes the encrypted repository unrecoverable; store a protected copy
outside the deployment and test it through a restore.

The key sets in backup credentials, maintenance credentials, target passwords,
and `config/backup.yaml` must agree exactly. Adding or removing a target is one
coordinated configuration change.

## Notifiers

The only supported notifier type is WeCom/WeChat Work. For a configured
notifier, `credentials/notifiers.yaml` contains the matching key exactly:

```yaml
schema_version: 1
notifiers:
  operations:
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me
```

To disable notifications, use both:

```yaml
# config/backup.yaml
notifiers: []
```

```yaml
# credentials/notifiers.yaml
schema_version: 1
notifiers: {}
```

Notifier delivery is bounded and best effort. It reports safe target names,
stable codes, and shortened digests; it is not durable backup evidence. Protect
the webhook as a secret and rotate it through the provider if disclosed.

## Vaultwarden environment

Start from `config/vaultwarden.example.env` and review every value. Required
storage paths are fixed to the mounted recovery boundary:

```dotenv
DATA_FOLDER=/data
DATABASE_URL=sqlite:///data/db.sqlite3
RSA_KEY_FILENAME=/data/rsa_key
ATTACHMENTS_FOLDER=/data/attachments
SENDS_FOLDER=/data/sends
```

Set `DOMAIN` to the exact public HTTPS origin presented by the trusted edge.
The tracked example disables signups, invitations, the web vault, outbound
icon download, push, SMTP-like features, and non-global HTTP requests. These
are security defaults, not a claim that every deployment should have identical
business policy. Changes must be deliberate and reviewed against the pinned
Vaultwarden release.

The RSA key is deliberately excluded from recovery. A restored or drilled
instance creates a new key and invalidates old token-bearing sessions. This is
a recovery property, not a configuration defect.

## Validation

Load the exact release images before validation. Then run both role checks:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-maintenance
```

Each command must print one versionless JSON success document and exit zero.
Together they check the five resolved values, exact YAML, actual Compose bind
identity, ownership, modes, symlink containment, target/password key sets,
both role files, notifier credentials, and required runtime directories. They
intentionally do not contact a repository.

After validation, initialize every newly created repository with the
maintenance role:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo init --target primary
```

Do not run `repo init` on an unknown nonempty location. Confirm the bucket,
prefix, account, region, role, and independent Restic password first. A real
`repo check`, backup, restore, and drill are still required before accepting
the deployment.
