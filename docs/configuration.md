# Configuration

All product configuration and credential documents use strict schema version 2.
Unknown fields, duplicate or additional YAML documents, unsupported fields, and
implicit environment credentials are rejected. Private files must be
non-symlink regular files with mode 0600 under a real mode-0700 directory owned
by the service UID.

Run `nix run .#just -- init` once to create the layout. The initializer uses
exclusive creation, validates anything already present, and never replaces
content.

## Deployment layout

```text
.env
config/
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

The directories and private files are ignored by Git. `deploy/` contains only
public templates and topology.

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
  - name: phone
    type: bark
```

At least one target is required. Target and notifier names are unique lowercase
identifiers. A repository must be a credential-free
`s3:http[s]://host/bucket/path` locator. User information, query strings,
fragments, empty path components, and traversal components are rejected.
Plaintext HTTP is intended only for isolated local testing and produces a
warning.

Only these Restic options are accepted:

- `s3.bucket-lookup`: `auto`, `dns`, or `path`;
- `s3.region`: a constrained non-empty region identifier.

Scheduling, retry intervals, timeouts, retention, filesystem locations, Restic
identity, and supported upstream tuples are product constants, not
configuration.

## Role credentials

The resident worker reads `credentials/backup/credentials.yaml`. One-shot
repository maintenance and restore read
`credentials/maintenance/credentials.yaml`. Both documents have the same
shape:

```yaml
schema_version: 2
targets:
  primary:
    AWS_ACCESS_KEY_ID: replace-me
    AWS_SECRET_ACCESS_KEY: replace-me
```

The target key set must exactly match `backup.yaml`. Each entry accepts exactly
the two keys shown. Session tokens, extra keys, and ambient AWS environment
variables are unsupported.

Compose never mounts the maintenance role into the resident worker and never
mounts the backup role into maintenance or restore.

## Restic passwords

Each target has a distinct 0600 password file named exactly after the target:

```text
credentials/restic/primary
credentials/restic/cold
```

The file contains one non-empty secret with an optional final line ending.
Target names cannot contain path separators, and the containing directory is
validated before use. There is no shared-password fallback and no
`RESTIC_PASSWORD` environment fallback.

A new repository must be initialized with the password that will remain
associated with that target. Back up these files separately from the Restic
repositories.

## Notifier credentials

`credentials/notifiers/credentials.yaml` has an exact key for every notifier
reference and no extra keys:

```yaml
schema_version: 2
notifiers:
  operations:
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me
  phone:
    device_token: 64-lowercase-hex
    auth_key: |
      -----BEGIN PRIVATE KEY-----
      ...
      -----END PRIVATE KEY-----
    key_id: ABC123
    team_id: TEAM123
    topic: me.fin.bark
    sound: "1107"
```

A WeChat notifier accepts only an HTTPS `webhook_url`. A Bark notifier uses
direct APNs delivery and requires the device token, PKCS#8 P-256 private key,
key ID, team ID, and topic; `sound` is optional.

Notifier credentials are mounted only into the resident worker. Maintenance and
restore cannot read them. When `notifiers: []`, no notifier credential entry
is required.

## Vaultwarden settings

`config/vaultwarden/vaultwarden.env` is copied from
`deploy/vaultwarden.env.template`. It is passed only to Vaultwarden. The
template disables signup, invitation, admin access, web-vault serving, push,
icon download, and outbound features by default; review every change against
the pinned Vaultwarden release.

This file and Vaultwarden's regenerated RSA material are outside the recovery
point. Preserve the application settings separately. Do not add a real secret
to the tracked template.

## Generated Compose environment

The ignored root `.env` contains:

- deployment UID/GID and absolute bind root;
- local image tags;
- the private Vaultwarden environment path;
- loopback ports; and
- the selected verified-restore path for a drill.

It is machine-specific configuration, not an image registry file. All runtime
services use the recorded UID/GID. With a remote Docker daemon, those IDs and
paths describe the Docker host, not the machine invoking Just.

## Validation

```sh
nix run .#just -- config-validate
```

This validates `backup.yaml`, every target password, the backup-role exact key
set, notifier credential resolution, and notifier protocol fields without
contacting a repository. Each maintenance or restore command separately
validates the maintenance role before invoking Restic.
