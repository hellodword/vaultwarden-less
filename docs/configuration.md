# Configuration

All product configuration and credential files are strict schema v1 YAML.
Unknown fields, extra YAML documents, unsupported notifier-specific fields,
and implicit environment credentials are rejected. Each file must be a
non-symlink regular file with mode 0600 beneath a real mode 0700 directory.

## Backup configuration

`config/backup.yaml` contains targets in their serial execution order and
optional notifiers:

```yaml
schema_version: 1

targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/vaultwarden
    restic_options:
      s3.bucket-lookup: path
      s3.region: ap-singapore

notifiers:
  - name: operations
    type: wechat
    webhook_url: https://example.invalid/secret
```

`serve` requires at least one target. Names are unique and retain declaration
order. Repositories must be credential-free `s3:https://...` locators. An
`s3:http://...` locator is permitted for local/Moto use but produces the
`plaintext_s3_endpoint` warning. Query strings, fragments, user information,
path traversal, and empty path components are rejected.

Only these Restic options are supported:

- `s3.bucket-lookup`: `auto`, `dns`, or `path`;
- `s3.region`: a non-empty region identifier.

Scheduling, retry intervals, timeouts, retention, filesystem paths, Restic
identity, and supported Vaultwarden versions are fixed product behavior, not
configuration fields.

## Role credentials

`credentials/backup/credentials.yaml` and
`credentials/maintenance/credentials.yaml` have the same strict schema:

```yaml
schema_version: 1
targets:
  primary:
    AWS_ACCESS_KEY_ID: "..."
    AWS_SECRET_ACCESS_KEY: "..."
```

Their target key sets must exactly equal the keys in `backup.yaml`. Each
target accepts exactly the two shown values; session tokens, extra keys, and
environment fallback are unsupported. The resident worker uses the backup
file. Repository maintenance and restore share the maintenance file. Compose
mounts only `credentials/backup` into the resident worker and only
`credentials/maintenance` into one-shot maintenance/restore services; both
receive the separate shared-password directory.

`credentials/common/restic-password` is one shared 0600 Restic password used
for all targets. Its compromise therefore affects every configured repository.

## Notifiers

Notifier secrets remain in private `backup.yaml`. This means maintenance and
restore jobs can read those secrets; that is an accepted deployment boundary.

WeChat accepts only `name`, `type: wechat`, and an HTTPS `webhook_url`. Bark
uses direct APNs delivery and accepts only:

```yaml
- name: phone
  type: bark
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

The APNs key must be a PKCS#8 P-256 private key. `sound` is optional.

## Initialization and ownership

Run `nix run .#just -- init`. The initializer uses exclusive creation and
never replaces existing content. It creates `data`, `staging`, `cache`,
`restore`, `config`, and `credentials` as mode 0700; writes private templates,
a random Restic password, and ignored `.env` files as mode 0600; and rejects
symlinks, wrong permissions, or wrong ownership.

`.env` records `DEPLOY_UID`, `DEPLOY_GID`, absolute bind paths, ports, and the
product image. Every service that touches a bind mount runs with this UID/GID.
When Docker runs remotely, these values must identify the remote deployment
directory owner, not the local caller.

Vaultwarden application settings live only in `.env.vaultwarden`. The file is
not part of the recovery set and must be backed up separately.
