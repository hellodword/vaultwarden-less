# Configuration

This reference describes private deployment inputs. Exact limits and rejection
rules are normative in [Product contracts](contracts.md). The release templates
are intentionally unusable until their empty repository, credential, and
password placeholders are replaced, or until the operator explicitly selects
targetless operation.

## Deployment tree and permissions

Keep the checksummed release directory read-only and separate from mutable
state. Create the deployment tree as the exact UID/GID that will run the
containers:

```sh
vwless_root=/srv/vaultwarden-less
vwless_release_dir=/opt/vaultwarden-less/releases/git-<full-commit>

install -d -m 0700 -- "$vwless_root"
install -d -m 0700 -- \
  "$vwless_root/config" \
  "$vwless_root/credentials" \
  "$vwless_root/credentials/restic" \
  "$vwless_root/data" \
  "$vwless_root/staging" \
  "$vwless_root/cache" \
  "$vwless_root/restore"

install -m 0600 -- "$vwless_release_dir/templates/compose.env" "$vwless_root/compose.env"
install -m 0600 -- "$vwless_release_dir/templates/config/backup.yaml" "$vwless_root/config/backup.yaml"
install -m 0600 -- "$vwless_release_dir/templates/config/vaultwarden.env" "$vwless_root/config/vaultwarden.env"
install -m 0600 -- "$vwless_release_dir/templates/credentials/backup.yaml" "$vwless_root/credentials/backup.yaml"
install -m 0600 -- "$vwless_release_dir/templates/credentials/maintenance.yaml" "$vwless_root/credentials/maintenance.yaml"
install -m 0600 -- "$vwless_release_dir/templates/credentials/restic/primary" "$vwless_root/credentials/restic/primary"
install -m 0600 -- "$vwless_release_dir/templates/credentials/restic/b2" "$vwless_root/credentials/restic/b2"
```

The resulting project-managed shape is:

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

Every directory is a real, non-symlink `0700` directory. Every private file is
a real, non-symlink `0600` regular file. All are owned by the configured
runtime identity. Validation reports a mismatch and never repairs it. If an
administrator must establish ownership for a separate runtime account, scope
that change to these exact newly created paths; never recursively change an
unrelated parent or existing data tree.

## Compose environment

`compose.env` contains exactly five unquoted assignments:

```dotenv
DEPLOYMENT_ROOT=/srv/vaultwarden-less
VWLESS_IMAGE_TAG=git-0123456789abcdef0123456789abcdef01234567
RUNTIME_UID=1000
RUNTIME_GID=1000
LISTEN_PORT=8080
```

- `DEPLOYMENT_ROOT` is the clean absolute non-root path above.
- `VWLESS_IMAGE_TAG` comes from the release template and remains the exact
  `git-<40-lowercase-hex-commit>` identity loaded from that bundle.
- `RUNTIME_UID` and `RUNTIME_GID` are canonical decimal values from 1 through
  4294967294 and match private-path ownership.
- `LISTEN_PORT` is a canonical decimal value from 1024 through 65535. Compose
  publishes only `127.0.0.1:${LISTEN_PORT}`.

Compose owns dotenv parsing. Do not source this file as shell code. A trusted
HTTPS reverse proxy must forward root-path HTTP and WebSocket traffic to the
loopback port and must apply the operator's public access and forwarding-header
policy.

All four Compose services share the one non-internal project `default` bridge;
there are no custom networks. Treat every service on that network as trusted:
the shared bridge is connectivity, not a security boundary. The resident
worker has no published port, and only proxy port `8080` is published as the
configured `127.0.0.1:${LISTEN_PORT}` host endpoint.

## Backup YAML

`config/backup.yaml` has only two required top-level arrays: ordered `targets`
and ordered `notifiers`. A two-target AWS S3 and Backblaze B2 configuration has
this shape:

```yaml
targets:
  - name: primary
    repository: s3:s3.ap-southeast-1.amazonaws.com/example-bucket/vaultwarden
    options: []
  - name: b2
    repository: s3:https://s3.us-west-004.backblazeb2.com/example-bucket/vaultwarden
    options:
      - name: s3.bucket-lookup
        value: path
notifiers: []
```

Targets use unique names matching `[a-z][a-z0-9-]{0,62}`. `options` is always
an explicit array, even when empty. Each option becomes the structured Restic
arguments `--option` and `name=value`; no value is interpreted by a shell.
Target order is preserved in status and notification results, while at most two
target operations run concurrently.

For AWS S3, choose the endpoint, region, bucket, and prefix assigned to this
deployment. For B2's S3-compatible API, use its HTTPS regional endpoint and
path-style bucket lookup. Both are consumed by unmodified Restic. Provider IAM,
object retention, object lock, lifecycle rules, cost, durability, and regional
behavior are outside the product: create dedicated least-privilege identities,
prevent lifecycle deletion of active Restic objects, and prove the actual
account with init, backup, check, restore, and a client exercise.

An intentional targetless configuration is exactly:

```yaml
targets: []
notifiers: []
```

Missing arrays and YAML nulls are invalid. Targetless keeps the application
available while explicitly providing no recovery point.

The parser rejects unknown or duplicate fields, multiple YAML documents,
implicit scalar types, anchors, aliases, tags, and merge keys. Repository and
option values are bounded and must not contain control-line separators. See
[Product contracts](contracts.md#backup-configuration) for the complete
limits.

## Role credentials and Restic passwords

`credentials/backup.yaml` is mounted only by the resident worker.
`credentials/maintenance.yaml` is mounted only by the one-shot maintenance
service. They have the same schema but should contain separate provider keys:

```yaml
targets:
  primary:
    environment:
      AWS_ACCESS_KEY_ID: replace-with-backup-key
      AWS_SECRET_ACCESS_KEY: replace-with-backup-secret
  b2:
    environment:
      AWS_ACCESS_KEY_ID: replace-with-b2-backup-key
      AWS_SECRET_ACCESS_KEY: replace-with-b2-backup-secret
```

The target key set in each role file exactly equals the configured target set.
Environment names are uppercase identifiers. Ambient environment is not
inherited; `RESTIC_*`, path/home/temp variables, loader injection, and runtime
debug variables are reserved. Values must be explicit nonempty strings.

Each target has one separate `0600` password file named exactly after the
target:

```text
credentials/restic/primary
credentials/restic/b2
```

Each contains one nonempty line, with one optional final LF. There are no
ignored suffixes or extra entries. Use independent high-entropy passwords and
retain protected recovery copies outside this deployment. Losing a password
makes that encrypted repository unusable.

For targetless operation, both role files contain exactly:

```yaml
targets: {}
```

and `credentials/restic` is actually empty. Remove the two empty release
template copies from the private password directory; do not leave placeholder
filenames behind.

## Inline notifiers

Notifier values live directly in private `config/backup.yaml`; there is no
separate notifier credential file. WeChat uses:

```yaml
notifiers:
  - name: operations
    type: wechat
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me
```

Bark/APNs uses:

```yaml
notifiers:
  - name: operations
    type: bark
    device_token: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    auth_key: |
      -----BEGIN PRIVATE KEY-----
      replace-with-one-P-256-PKCS8-key
      -----END PRIVATE KEY-----
    key_id: replace-me
    team_id: replace-me
    topic: me.example.app
    sound: "1107"
```

`sound` may be omitted and defaults to `1107`. Each notifier rejects fields
from the other type. Delivery is bounded and best effort; it is not durable
backup evidence. Treat webhook URLs, device tokens, and private keys as
secrets.

## Vaultwarden environment

The supplied `config/vaultwarden.env` intentionally contains only:

```dotenv
SIGNUPS_ALLOWED=false
INVITATIONS_ALLOWED=false
```

Compose fixes the listen address, SQLite/data paths, RSA prefix, attachment and
Send paths, disabled Web Vault, and disabled icon downloads. Do not duplicate
or override those values. The project neither requires nor validates a public
domain. Operators may add other settings supported by the pinned Vaultwarden
release, but the project does not translate or validate them.

For a new empty deployment, temporarily set `SIGNUPS_ALLOWED=true`, create the
first account through an official Bitwarden client over the trusted HTTPS edge,
then atomically restore `false` and force-recreate only Vaultwarden. The exact
procedure is in [Operations](operations.md#targetless-first-start-and-first-account).

## Validation

After loading the release images, validate each actual role mount without
contacting a repository:

```sh
docker compose --env-file "$vwless_root/compose.env" --file "$vwless_release_dir/compose.yml" \
  run --rm --no-deps backup config validate --role backup

docker compose --env-file "$vwless_root/compose.env" --file "$vwless_release_dir/compose.yml" \
  --profile maintenance run --rm --no-deps maintenance \
  config validate --role maintenance
```

Each success document semantically contains `valid: true`, the selected role,
and target names in configuration order. Validation checks YAML, role keys,
password filenames and contents, private paths, ownership, modes, and symlink
containment. It does not connect to Restic or construct a repository runner.

After both checks pass, initialize each known-empty dedicated target exactly
once. Skip initialization when targetless:

```sh
docker compose --env-file "$vwless_root/compose.env" --file "$vwless_release_dir/compose.yml" \
  --profile maintenance run --rm --no-deps maintenance \
  repo init --target primary
```

Never run `repo init` against an unreviewed nonempty prefix. A successful
configuration check is not evidence that provider access or recovery works.
