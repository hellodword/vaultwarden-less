# Configuration

For the preparation sequence, use [deployment](deployment.md). This page is
the reference for the files in [templates](../templates/README.md); their inline
comments explain required values and when to restart or recreate a service.

The supplied [Compose file](../compose.yml) runs `vaultwarden`, `proxy` and
`backup` with a non-root UID/GID, read-only root filesystems, disk-backed worker temporary storage, dropped capabilities and bounded logs. Only Vaultwarden writes the
source data. The proxy has no data or credential mounts. The backup service
reads `data/` and writes `state/` and `cache/`; its internal port 9090 is not published.

Create the following tree under an operator-owned absolute deployment path.
Use private directory permissions such as 0700 and file permissions such as
0600, with ownership matching the runtime UID/GID. The services do not repair
host permissions or create missing bind mount sources.

```text
deployment/
  compose.yml
  compose.env
  config/
    vaultwarden.env
    vaultwarden-runtime.json    # read-only {}
    backup.yaml
  credentials/
    backup/
      aws-s3-backup.env
      cloudflare-r2-backup.env
      backblaze-b2-backup.env
      bark-apns-auth-key.p8      # operator-supplied, only when using Bark
    restic/
      aws-s3-repository-password.txt
      cloudflare-r2-repository-password.txt
      backblaze-b2-repository-password.txt
  data/
  state/
  cache/
  restore/
```

Copy the bundle's `compose.yml` and the files from `templates/` into this
directory, keep the destinations and notifications you need, and replace their
placeholders. Remove unused YAML entries and their credential files. The APNs
private key is not included in the templates. Host bind sources and the service
`env_file` use paths relative to this Compose file, so moving the complete,
stopped deployment tree does not require changing those paths.

Edit `x-service-defaults.user` in `compose.yml` to select the non-root numeric
runtime UID:GID (default `1000:1000`). Edit the proxy's `ports` entry to select
its host loopback port (default `127.0.0.1:8080:8080`); container port 8080 is
fixed. Recreate the affected containers after changing these settings.

`compose.env` contains only `VWLESS_IMAGE_TAG`, selecting the same release tag
for all three product images. Compose owns interpolation of this file. The
`DEPLOY_DIR` variable in documentation is only a shell convenience for host
commands; it is not read by Compose or the applications and does not belong in
`compose.env`. A clean Git build uses the `git-<commit>` image
tag; a dirty working-tree build uses `development`. All three product images
record the full commit hash in the `org.opencontainers.image.revision` label,
with `-dirty` appended for uncommitted tracked changes. The backup and proxy
`version` commands expose the same value as `project_commit`. Sources without
Git revision metadata omit the revision label and `project_commit`.

## Vaultwarden profile and precedence

The supplied minimal-feature profile sets:

```dotenv
WEB_VAULT_ENABLED=false
ADMIN_TOKEN=
DISABLE_ADMIN_TOKEN=false
SIGNUPS_ALLOWED=false
SIGNUPS_DOMAINS_WHITELIST=
INVITATIONS_ALLOWED=false
DISABLE_ICON_DOWNLOAD=true
HTTP_REQUEST_BLOCK_NON_GLOBAL_IPS=true
PASSWORD_HINTS_ALLOWED=false
SHOW_PASSWORD_HINT=false
SENDS_ALLOWED=false
EMERGENCY_ACCESS_ALLOWED=false
ORG_CREATION_USERS=none
```

An empty admin token with `DISABLE_ADMIN_TOKEN=false` disables the admin panel.
The empty signup domain whitelist prevents an exception to closed registration.
The HTTP destination block applies to Vaultwarden HTTP clients that enforce it;
it is neither a general firewall nor a setting for backup/restic/notifier
connections. Icon downloads are separately disabled. These settings do not
alter inbound listeners or the external reverse proxy.

The feature switches do not erase existing data. `SENDS_ALLOWED=false` blocks
new or modified Sends; existing Sends can still be accessed and deleted.
`ORG_CREATION_USERS=none` prevents new organizations and retains existing ones.
Emergency access is disabled. Re-enable individual features deliberately when
your installation needs them. Request and login limits retain upstream defaults;
the template does not configure separate limit values.

Compose fixes `CONFIG_FILE=/config/vaultwarden-runtime.json` to the shipped
read-only `{}` file. Deployment environment settings determine runtime behavior.
Imported `data/config.json` remains a native backed-up file and is never loaded
as active configuration. Compose also fixes the database, RSA, attachment and
Send paths and disables the admin panel. Do not change the runtime JSON to
import an old instance's environment overrides.

`compose.env` and `vaultwarden.env` use
[Compose's interpolation and quoting rules](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation/).
An exported `VWLESS_IMAGE_TAG` takes precedence over `compose.env`; unset it
when selecting the tag through that file. Single-quote values
containing literal `$`, such as an Argon2 admin
token hash. Restic credential env files use the different literal rules below.
Never source any of these files. After changing a Compose env file, run
`compose up --detach` to recreate affected containers; `restart` alone does
not reload their environment. Restart backup for changes to its mounted YAML,
target credential files or APNs key. Use 0600 permissions for secrets.

## Destinations

`backup.yaml` has only `targets` and `notifications`. Unknown fields are rejected.
An invalid target or notifier is disabled with a safe error code; unrelated
valid entries continue. A malformed root document prevents startup.
Configuration is read at process startup; restart backup after changes.

```yaml
targets:
  - name: aws-s3
    env_file: /run/credentials/backup/aws-s3-backup.env
    options: []
  - name: cloudflare-r2
    env_file: /run/credentials/backup/cloudflare-r2-backup.env
    options: []
  - name: backblaze-b2
    env_file: /run/credentials/backup/backblaze-b2-backup.env
    options: []
```

`name` identifies a destination in status and notifications. `env_file` is a
path inside the backup container. All retained destinations operate independently;
their order does not define a priority or failover sequence. Each `options`
string becomes one restic `--option` value. Repository, authentication, passwords
and backend settings use restic's environment directly. For example,
`credentials/backup/aws-s3-backup.env` contains:

```dotenv
RESTIC_REPOSITORY=s3:https://s3.us-east-1.amazonaws.com/REPLACE_WITH_AWS_S3_BUCKET/vaultwarden-less
AWS_DEFAULT_REGION=us-east-1
RESTIC_PASSWORD_FILE=/run/credentials/restic/aws-s3-repository-password.txt
AWS_ACCESS_KEY_ID=REPLACE_WITH_AWS_S3_BACKUP_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY=REPLACE_WITH_AWS_S3_BACKUP_SECRET_ACCESS_KEY
```

The three templates use [restic's S3 backend](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#amazon-s3)
with these provider-specific inputs:

| Destination | S3 endpoint in the example | `AWS_DEFAULT_REGION` | Credential pair |
| --- | --- | --- | --- |
| AWS S3 | `s3.us-east-1.amazonaws.com` | `us-east-1`; match the bucket | Access key ID and secret access key; add `AWS_SESSION_TOKEN` for temporary credentials |
| Cloudflare R2 | `<ACCOUNT_ID>.r2.cloudflarestorage.com` | `auto` | R2 S3 Access Key ID and Secret Access Key |
| Backblaze B2 | `s3.us-west-002.backblazeb2.com` | `us-west-002`; match the bucket | S3-compatible application key's keyID and applicationKey |

Use the actual bucket endpoint and region. R2's S3 credentials and signing region
are described in its [token guide](https://developers.cloudflare.com/r2/api/tokens/)
and [S3 API reference](https://developers.cloudflare.com/r2/api/s3/api/).
B2 uses the S3-compatible API in these examples; see its
[restic guide](https://www.backblaze.com/docs/cloud-storage-integrate-restic-with-backblaze-b2)
and [credential mapping](https://www.backblaze.com/docs/cloud-storage-use-the-aws-cli-with-backblaze-b2).
When preparing a maintenance env file, copy the selected target's repository,
region and password path. Setting the region in each env file also applies it to
operator-run restic commands. The examples use restic's automatic bucket lookup;
add an option such as `s3.bucket-lookup=path` only when your backend needs it,
and use the same option during maintenance and restore.

The worker uses Docker CLI's environment-file parser, with literal values and
no shell expansion, interpolation, or fallback to its process environment.
Do not quote values as shell strings or `source` this file. Put the restic
password in the referenced private file. Backend-specific settings remain
native restic inputs; there is no fixed set of backend names.

Give each installation its own repository or object-store prefix. Prepare
maintenance credentials separately when needed, following
[native restic maintenance](operations.md#native-restic-maintenance); these files
are not included in `templates/`. Keep them outside the backup mount, for example
in an operator-created `credentials/maintenance/` directory. Use provider
permissions appropriate for each role, including the operations needed for
restic's lock objects and native snapshot tag finalization (which replaces the
pending snapshot object). The worker does not execute retention or prune.
Test those permissions against
the actual provider; Moto acceptance does not prove IAM restrictions. Avoid
provider lifecycle rules that independently expire live restic objects.

An empty `targets: []` is supported. No source capture or repository operation
is performed, and status clearly reports that no valid destination is
configured. A check still produces an unconfigured result; health only reports
that the process responds.

## Per-target verification

Add `verify` only for the schedules you want:

```yaml
targets:
  - name: repository-a
    env_file: /run/credentials/backup/a.env
    options: []
    verify:
      structure_interval: 24h
      read_data_interval: 720h
      timeout: 2h
  - name: repository-b
    env_file: /run/credentials/backup/b.env
    verify:
      structure_interval: 24h
  - name: repository-c
    env_file: /run/credentials/backup/c.env
```

A runs both checks; B only structure; C has no automatic verification. These
intervals are examples, not inherited defaults. Durations use Go duration syntax
(e.g. 24h, not 1d), must be positive, and are read at startup. Timeout alone
never enables a schedule. Timeout defaults to 2h; adjust it for repository size.
Manual `verify` is available regardless of configured schedules. Plans and
last-known results live in `state/`, independently of caches.

## Notifications

The template defaults to no channels. It includes commented examples for all
four adapters. Enable only your own endpoints and restart backup:

```yaml
notifications:
  timezone: Asia/Shanghai
  daily_at: "21:00"
  reminder_interval: 6h
  channels:
    - name: wecom-operations
      type: wecom
      webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=REPLACE_WITH_KEY
    - name: operations-feishu
      type: feishu
      webhook_url: https://open.feishu.cn/open-apis/bot/v2/hook/REPLACE_WITH_TOKEN
      secret: REPLACE_WITH_SECRET
    - name: operations-dingtalk
      type: dingtalk
      webhook_url: https://oapi.dingtalk.com/robot/send?access_token=REPLACE_WITH_TOKEN
      secret: REPLACE_WITH_SECRET
    - name: bark-apns-operations
      type: bark
      device_token: REPLACE_WITH_APNS_DEVICE_TOKEN
      auth_key_file: /run/credentials/backup/bark-apns-auth-key.p8
      key_id: REPLACE_WITH_APNS_KEY_ID
      team_id: REPLACE_WITH_APPLE_TEAM_ID
      topic: me.fin.bark
      sound: "1107"
```

The timezone is an IANA zone, daily_at is local HH:MM, and reminder_interval is a
positive duration. Defaults are Asia/Shanghai, 21:00 and 6h. Reports are English
with an explicit UTC offset. API times remain UTC RFC3339. Empty/omitted channels
disable delivery. These schedule settings must be valid even with no channels.

Webhook URLs require HTTPS. Feishu and DingTalk secrets are optional. WeCom and
DingTalk use Markdown; Feishu uses an interactive card with Markdown. Bark sends
directly to production APNs over HTTP/2, using a PKCS#8 P-256 key and ES256 token.
The private key must authorize the device token and topic; an unrelated Apple
key cannot push to the official Bark app. Supply an APNs device token rather
than a Bark HTTP API key. The key is never shipped. Omitting sound uses 1107;
explicit empty sound is preserved. Bark carries top-level Markdown and a plain
alert fallback; no BarkServer is required.

Only first/expanded faults, recoveries, six-hour reminders and daily reports
trigger delivery. Pending summaries, failures and attempt budgets persist in
`state/`. A recovered but undelivered incident remains in the summary. Healthy
single checks/results enter status and daily counts. See [contracts](contracts.md#notifications)
for byte limits and retry semantics. Credentials, private paths, database values
and subprocess diagnostics never enter these reports.
