# Configuration

For the preparation sequence, use [deployment](deployment.md). This page is
the reference for the files in [templates](../templates/README.md); their inline
comments explain required values and when to restart or recreate a service.

The supplied [Compose file](../compose.yml) runs `vaultwarden`, `proxy` and
`backup` with a non-root UID/GID, read-only root filesystems, a temporary
`/tmp`, dropped capabilities and bounded logs. Only Vaultwarden writes the
source data. The proxy has no data or credential mounts. The backup service
reads `data/` and writes `cache/`; its internal port 9090 is not published.

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

Existing `data/config.json` retains
[Vaultwarden's precedence](https://github.com/dani-garcia/vaultwarden/wiki/Configuration-overview)
over editable environment settings and is included in backups. Review it before
relying on template defaults. Compose's explicit `environment` entries override
the service `env_file` and fix the SQLite database, RSA, attachment and Send
paths under `/data`. Other fields use the selected Vaultwarden's upstream
settings; consult its `.env.template` when changing them.

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

`backup.yaml` has only `targets` and `notifiers`. Unknown fields are rejected.
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
restic's lock objects. Test those permissions against
the actual provider; Moto acceptance does not prove IAM restrictions. Avoid
provider lifecycle rules that independently expire live restic objects.

An empty `targets: []` is supported. No source capture or repository operation
is performed, and status clearly reports that no valid destination is
configured. A check still produces an unconfigured result; health only reports
that the process responds.

## Notifications

The template includes Enterprise WeChat and Bark APNs entries. Keep only the
channels you use and replace their placeholders; `notifiers: []` disables all
notifications. Feishu and DingTalk are also supported, as shown below. Webhooks
require HTTPS. Keep URLs, signing secrets and the Apple private key private.

```yaml
notifiers:
  - name: wecom-operations
    type: wecom
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=REPLACE_WITH_WECOM_WEBHOOK_KEY
  - name: operations-feishu
    type: feishu
    webhook_url: https://open.feishu.cn/open-apis/bot/v2/hook/replace-me
    secret: replace-me
  - name: operations-dingtalk
    type: dingtalk
    webhook_url: https://oapi.dingtalk.com/robot/send?access_token=replace-me
    secret: replace-me
  - name: bark-apns-operations
    type: bark
    device_token: REPLACE_WITH_APNS_DEVICE_TOKEN
    auth_key_file: /run/credentials/backup/bark-apns-auth-key.p8
    key_id: REPLACE_WITH_APNS_KEY_ID
    team_id: REPLACE_WITH_APPLE_TEAM_ID
    topic: me.fin.bark
    sound: "1107"
```

Feishu and DingTalk `secret` are optional when the robot does not use signing.
Bark sends directly to Apple's production APNs endpoint over HTTP/2 using a
PKCS#8 P-256 key and ES256 provider token. Supply the private key at the configured
path; templates do not include one, and local `validate` rejects a retained Bark
entry without a readable valid key. The key, device token and topic must be
authorized for the same application. An unrelated Apple developer key cannot
push to the official Bark app. Use an APNs device token, not a Bark HTTP API
device key. Device tokens have no fixed length restriction. Omitting `sound`
uses `1107`; explicit empty or other sound values are preserved.

All four channels use English Markdown reports. WeCom and DingTalk use their
native `markdown` messages; Feishu uses a schema 2.0 interactive card with a
Markdown element. Bark still connects directly to Apple, with the report in
the top-level `markdown` field and a short plain-text `aps.alert.body` fallback.
Bark's notification extension converts Markdown to plain banner text, while
its archived message displays the formatted report. The fallback can therefore
be replaced by the extension; it is not a guaranteed short banner. This follows
[Bark's parameters](https://bark.day.app/#/params) and
[APNs payload assembly](https://github.com/Finb/bark-server/blob/master/apns/apns.go).

Notifications summarize checks, trigger templates or unknown methods, source
completeness and per-target results. Request activity keeps `started` and
`finished` counts separate: one request can contribute one of each, including
across checks. These are phase counts, not a total obtained by adding both.
Reports do not include request bodies, raw
unknown paths, repository credentials or subprocess output. Channel status is
in memory. Pending or failed notifications do not survive a process restart.
