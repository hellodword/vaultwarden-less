# Configuration and credentials

The proxy has no configuration file. The worker has one strict private YAML
file and one fixed role-credential YAML file. There are no environment-variable
overrides for either contract.

## Fixed container paths

Hard-coded paths are container paths, not arbitrary host paths. Compose owns
the mapping:

| Host source | Container path | Consumer | Access |
| --- | --- | --- | --- |
| `./data` | `/data` | Vaultwarden / backup | read-write / read-only |
| `./state` | `/state` | backup | read-write |
| `./staging` | `/staging` | backup | read-write |
| `./cache` | `/cache` | backup, maintenance, restore | read-write |
| `./restore` | `/restore` | restore | read-write |
| `./config/backup.yaml` | `/config/backup.yaml` | backup, maintenance, restore | read-only |
| `./credentials/common` | `/run/credentials/common` | all Restic roles | read-only |
| role-specific directory | `/run/credentials/role` | one role only | read-only |

The backup service maps `./credentials/backup`; maintenance and restore map
`./credentials/maintenance`. The proxy has none of these mounts. Compose sets
`create_host_path: false`, so a misspelled or absent host source fails instead
of becoming a root-owned directory.

`data`, `state`, `staging`, `cache`, `restore`, and all credential directories
must already be real non-symlink directories owned by the configured non-root
identity and mode `0700`. The worker does create private children and state
files inside its writable roots; it does not create or chmod the roots.

## Proxy constants

The proxy binary fixes these values:

| Behavior | Value |
| --- | --- |
| listen | `0.0.0.0:8080` in the container; host loopback publication by Compose |
| Vaultwarden upstream | `http://vaultwarden:80` |
| event endpoint | `http://backup:9090/v1/events` |
| request body | 600 MiB maximum |
| read header | 10 seconds |
| read/write | 30 minutes |
| idle | 2 minutes |
| upstream response header | 5 minutes |
| event delivery | 2 seconds |
| graceful shutdown | 2 minutes |

The only trusted forwarding peers are loopback/private source addresses, which
are safe here because the published port defaults to host loopback and the
Compose networks are internal. The local Nginx/Caddy hop must overwrite
`X-Real-IP` with one IP and `X-Forwarded-Proto` with exactly `http` or `https`.
The proxy deletes and rebuilds every `Forwarded` and `X-Forwarded-*` field.

## backup.yaml

[backup.template.yaml](../config/backup.template.yaml) is a non-secret
starting point. The real ignored `config/backup.yaml` is private because
repository topology, WeChat URLs, Bark tokens, and an APNs private key may be
present.

Unknown fields, duplicate keys, multiple YAML documents, invalid types,
unsupported repositories, invalid names, and out-of-range durations stop the
command. Parser errors report a category and position without echoing YAML
values.

The complete human-authored top level is:

| Field | Contract |
| --- | --- |
| `schema_version` | exactly `1` |
| `target_timeout` | `1m` through `2h` |
| `maintenance_timeout` | `1m` through `24h` |
| `targets` | optional list, at most 32; omitted/empty means no backup |
| `notifiers` | optional list, at most 16 |

Scheduling and runtime paths are deliberately not configuration. v1 fixes a
30-second debounce, 2-minute maximum continuous wait, 5-minute minimum
snapshot interval, 15-minute sampling interval, 30-second missing-file retry,
and 2-minute shutdown timeout. The event server listens at `0.0.0.0:9090`,
limits bodies to 1024 bytes, and uses a 10-per-second, burst-20 limiter.

The companion ignored `.env.vaultwarden` must retain the local SQLite and
`/data` asset paths from [.env.vaultwarden.template](../.env.vaultwarden.template).
Do not override them with PostgreSQL, MySQL, or Vaultwarden object storage.

## Targets

An S3-compatible target:

```yaml
targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/vaultwarden
    restic_options:
      s3.bucket-lookup: path
      s3.region: ap-singapore
```

A native B2 target:

```yaml
targets:
  - name: cold-copy
    repository: b2:bucket-name:vaultwarden
```

Target names are unique lowercase ASCII names. An S3 URL may use HTTPS or,
for an explicitly private MinIO/COS endpoint, HTTP; user info, query, fragment,
control characters, and embedded credentials are rejected. Only
`s3.bucket-lookup`, `s3.region`, and `s3.list-objects-v1` are accepted as
structured Restic options. B2 uses Restic's native `b2:` locator.

Native B2 requires an explicit non-empty bucket and repository prefix in the
form `b2:bucket-name:path/to/repository`. Restic 0.19.1 itself recommends B2's
S3-compatible API over its native B2 library when that is available; v1 keeps
native B2 as a supported explicit option.

Local, REST, Azure, GCS, Swift, SFTP, rclone, and every other locator are
rejected. There is no custom-CA field, anonymous mode, session token, or
arbitrary Restic flag. The process also removes inherited Restic and cloud
credential variables before constructing the target-specific environment.

## Exact credentials layout

No credential template is tracked. Create only the real ignored files you
need:

```text
credentials/
├── common/
│   └── restic-password
├── backup/
│   └── credentials.yaml
└── maintenance/
    └── credentials.yaml
```

- `common/restic-password`: the repository encryption password as raw text.
  All configured targets currently share this one Restic password. Store an
  independent offline recovery copy; neither Vaultwarden nor the repository
  backend can reconstruct it.
- `backup/credentials.yaml`: backend credentials exposed only to the
  long-running append-only backup worker.
- `maintenance/credentials.yaml`: independently issued higher-authority
  credentials for init/check/unlock/prune/snapshot listing/restore.

Every file must be regular, non-symlink, owner-readable, inaccessible to group
and other users (`0400` or `0600`), and bounded in size. Credential directories
must be `0700`. The role files have this shape:

```yaml
schema_version: 1
targets:
  primary:
    AWS_ACCESS_KEY_ID: REPLACE_ME
    AWS_SECRET_ACCESS_KEY: REPLACE_ME
  cold-copy:
    B2_ACCOUNT_ID: REPLACE_ME
    B2_ACCOUNT_KEY: REPLACE_ME
```

Only the configured target and its two required keys are consumed. Unknown
target entries and unknown keys are ignored intentionally, so operators may
roll credentials or share a superset across staged configuration changes.
For a configured S3 target both AWS keys are mandatory; for native B2 both B2
keys are mandatory. Empty values fail. `AWS_SESSION_TOKEN`, profiles, region
environment variables, and every other backend variable are ignored.

The backup backend identity should have only append/create authority where the
provider supports it. Maintenance needs the read/list/delete/administrative
authority required by the explicit command. The common Restic password alone
does not grant backend access.

When `targets` is empty, no Restic password or backend key is consumed, but the
backup role `credentials.yaml` must still be a valid private document; use:

```yaml
schema_version: 1
targets: {}
```

## Notifiers

Notifier failures never alter a backup result. They have independent persisted
queues and run concurrently.

Enterprise WeChat uses its required `msgtype=text` shape:

```yaml
notifiers:
  - name: operations
    type: wechat
    timeout: 10s
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=REPLACE_ME
```

Bark sends directly to `https://api.push.apple.com` over HTTP/2:

```yaml
notifiers:
  - name: phone
    type: bark
    timeout: 10s
    device_token: REPLACE_WITH_64_LOWERCASE_HEX_CHARACTERS
    auth_key: |
      -----BEGIN PRIVATE KEY-----
      REPLACE_ME
      -----END PRIVATE KEY-----
    key_id: REPLACE_ME
    team_id: REPLACE_ME
    topic: me.fin.bark
    sound: "1107"
```

The APNs key must be a PKCS#8 P-256 key. The production APNs origin cannot be
overridden. Redirects are forbidden and responses are bounded. APNs requires
HTTP/2, status `200`, and a valid error `reason` on failure; WeChat requires
both HTTP 2xx and `errcode == 0`. Generic webhooks are not part of v1.

## Validation without preflight

Validate syntax and print only names/types/backend kinds:

```sh
docker compose run --rm --no-deps backup \
  config --config /config/backup.yaml
```

Validate the role actually mounted into a service, the common password, and
(for the backup role) notifier material:

```sh
docker compose run --rm --no-deps backup \
  config --check-credentials backup --config /config/backup.yaml
docker compose --profile maintenance run --rm --no-deps maintenance \
  config --check-credentials maintenance --config /config/backup.yaml
```

The `backup`/`maintenance` label is checked for valid CLI use and recorded in
the secret-free JSON result; authority comes from which host role directory
Compose mounts at `/run/credentials/role`.

This design keeps filesystem preparation in the deployment system rather than
maintaining a second host-layout model in application code. Failures stay at
the consuming boundary: Compose reports absent bind sources; YAML loading
reports private-file/type/schema errors; state/staging/cache report the fixed
container path and required private directory; Restic validation reports a
stable target error without logging credentials or the repository URL.
