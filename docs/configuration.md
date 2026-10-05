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
  compose.env
  config/
    vaultwarden.env
    backup.yaml
  credentials/
    backup/primary.env
    maintenance/primary.env
    restic/primary
  data/
  cache/
  restore/
```

Copy the files from `templates/`, then replace their placeholders.
`compose.env` selects the absolute deployment root, the image tag supplied by
the bundle, a non-root runtime UID/GID and the loopback listener port. Compose
owns interpolation of this file. A build from a Git revision records that
revision as provenance; a working-tree build uses `development`.

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
Exported shell variables take precedence over `compose.env`, so keep any
exported deployment values consistent with the file. Single-quote values
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
  - name: primary
    env_file: /run/credentials/backup/primary.env
    options:
      - s3.bucket-lookup=path
      - s3.region=us-east-1
notifiers: []
```

`name` identifies a destination in status and notifications. `env_file` is a
path inside the backup container. Each `options` string becomes one restic
`--option` value. Repository, authentication, passwords and backend settings
use restic's environment directly:

```dotenv
RESTIC_REPOSITORY=s3:https://s3.example.net/bucket/vaultwarden
RESTIC_PASSWORD_FILE=/run/credentials/restic/primary
AWS_ACCESS_KEY_ID=replace-with-backup-key
AWS_SECRET_ACCESS_KEY=replace-with-backup-secret
```

The worker uses Docker CLI's environment-file parser, with literal values and
no shell expansion, interpolation, or fallback to its process environment.
Do not quote values as shell strings or `source` this file. Put the restic
password in the referenced private file. Backend-specific settings remain
native restic inputs; there is no fixed set of backend names.

Give each installation its own repository or object-store prefix. Keep
maintenance credentials in `credentials/maintenance/`, outside the backup
mount. Use provider permissions appropriate for each role, including the
operations needed for restic's lock objects. Test those permissions against
the actual provider; Moto acceptance does not prove IAM restrictions. Avoid
provider lifecycle rules that independently expire live restic objects.

An empty `targets: []` is supported. No source capture or repository operation
is performed, and status clearly reports that no valid destination is
configured. A check still produces an unconfigured result; health only reports
that the process responds.

## Notifications

These entries may be added to `notifiers`. Webhooks require HTTPS. Keep URLs,
signing secrets and the Apple private key private.

```yaml
notifiers:
  - name: operations-wecom
    type: wecom
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me
  - name: operations-feishu
    type: feishu
    webhook_url: https://open.feishu.cn/open-apis/bot/v2/hook/replace-me
    secret: replace-me
  - name: operations-dingtalk
    type: dingtalk
    webhook_url: https://oapi.dingtalk.com/robot/send?access_token=replace-me
    secret: replace-me
  - name: operations-bark
    type: bark
    device_token: replace-with-device-token
    auth_key_file: /run/credentials/backup/AuthKey.p8
    key_id: replace-me
    team_id: replace-me
    topic: me.fin.bark
    sound: "1107"
```

Feishu and DingTalk `secret` are optional when the robot does not use signing.
Bark sends directly to Apple's production APNs endpoint over HTTP/2 using a
PKCS#8 P-256 key and ES256 provider token. The topic must match the authorized
application. Device tokens have no fixed length restriction. Omitting `sound`
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
