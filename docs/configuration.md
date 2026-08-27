# Configuration

Deployment configuration uses strict schema 1. Product and credential
documents use strict schema 2. Unknown fields, duplicate keys, additional YAML
documents, and implicit credential fallback are rejected. Every private file
must be a non-symlink regular file with mode 0600 under real mode-0700
directories owned by the configured runtime identity.

Create the layout with explicit numeric ownership:

```sh
nix run .#just -- init <runtime-uid> <runtime-gid>
```

The command exclusively creates missing content, validates anything already
present, and never replaces a secret or changes an established identity. It is
safe to repeat with the same UID/GID. UID or GID 0 is supported only when an
operator deliberately chooses root; there is no hidden default identity. The
invoking account must be allowed to `chown` new files when the selected numeric
identity differs from its own.

## Deployment layout

```text
config/
  deployment.yaml
  backup/backup.yaml
  vaultwarden/vaultwarden.env
credentials/
  backup/credentials.yaml
  maintenance/credentials.yaml
  restic/<target-name>
  notifiers/credentials.yaml
data/
staging/
cache/
restore/
```

Runtime directories are ignored by Git. `deploy/` contains only public
templates. There is no root `.env`, image registry setting, or project-private
environment-variable protocol.

## Deployment configuration

`config/deployment.yaml` has exactly these fields:

```yaml
schema_version: 1
runtime_uid: 1000
runtime_gid: 1000
listen_port: 8080
drill_port: 8081
```

- `schema_version` is the integer 1, not a quoted value or boolean.
- UID/GID are integers from 0 through 4294967294.
- ports are integers from 0 through 65535; 0 asks Docker for an ephemeral
  loopback port.
- equal nonzero production and drill ports are rejected.

The renderer reads product/image identity from `VERSION` and `UPSTREAMS.toml`,
not from this file. It accepts only the declared `@@...@@` template tokens and
rejects unresolved tokens or Compose `${...}` interpolation. Compose is always
called with `/dev/null` as its environment file, an explicit project name,
the deployment root as project directory, and rendered YAML on stdin.
The production and drill project names are fixed. Docker subprocesses inherit
the caller's ordinary process environment and use an isolated credential-free
client configuration.

## Backup configuration

`config/backup/backup.yaml` declares targets in serial execution order and
contains notifier references, never secrets:

```yaml
schema_version: 2

targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/vaultwarden
    restic_options:
      s3.bucket-lookup: path
      s3.region: ap-singapore

notifiers:
  - name: operations
    type: wechat
```

At least one target is required. Names are unique lowercase identifiers. A
repository is a credential-free `s3:http[s]://host/bucket/path` locator.
User information, query strings, fragments, empty/traversal components, and
unknown Restic options are rejected. Plain HTTP is test-only and warns.

Only `s3.bucket-lookup` (`auto`, `dns`, or `path`) and a constrained
`s3.region` are configurable. Scheduling, retries, retention, paths, Restic
identity, and supported upstream tuples remain product constants.

## Role credentials and Restic passwords

The worker reads `credentials/backup/credentials.yaml`; one-shot maintenance
and restore read `credentials/maintenance/credentials.yaml`:

```yaml
schema_version: 2
targets:
  primary:
    AWS_ACCESS_KEY_ID: replace-me
    AWS_SECRET_ACCESS_KEY: replace-me
```

The target key set must exactly match `backup.yaml`. Each entry accepts exactly
the two shown keys. Restic receives exactly those two variables plus
`TMPDIR=/tmp`; ambient AWS, Restic, HOME, PATH, and session variables are not
forwarded.

Each target also has a distinct 0600 password file, for example
`credentials/restic/primary`. There is no shared-password or environment
fallback. Back up credential and password files separately from repositories.

## Notifiers and Vaultwarden

`credentials/notifiers/credentials.yaml` has exactly one entry per notifier
reference and no extras. WeChat accepts one HTTPS `webhook_url`. Bark requires
its device token, PKCS#8 P-256 key, key/team IDs, topic, and optional sound.
Notifier credentials are mounted only into the resident worker.

`config/vaultwarden/vaultwarden.env` starts from
`deploy/vaultwarden.env.template` and is passed only to Vaultwarden. The
template disables signup, invitation, admin, web-vault, push, icon download,
and outbound features. It and regenerated RSA state are outside recovery.

## Validation

```sh
nix run .#just -- config-validate
```

This validates product configuration, target passwords, role key sets, and
notifier fields without contacting repositories. Maintenance commands validate
the maintenance role again before invoking Restic.

Relative Compose binds are intentionally rooted at the source/deployment tree.
