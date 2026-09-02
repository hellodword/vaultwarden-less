# Operations and recovery

These procedures run on an ordinary Linux/amd64 deployment host with Docker
Engine, the Docker Compose plugin, checksum tools, rsync, and GNU coreutils.
Git, Python, Go, Nix, and Just are not production dependencies. Nix is used
only on a build or verification host. Run Compose from an exact checksummed
release directory; every private bind and the private Compose environment live
beneath a separate `DEPLOYMENT_ROOT`.

The examples use these reviewed absolute paths throughout:

```sh
vwless_release_dir=/opt/vaultwarden-less/releases/<immutable-release>
vwless_root=/srv/vaultwarden-less
vwless_env="$vwless_root/.env"
vwless_compose="$vwless_release_dir/compose.yml"
```

Do not edit the release directory. Do not delete or overwrite `data`,
`restore`, `state`, credentials, or a repository merely to make a command pass.

## Operator command surface

The exact Docker Compose commands below are the production interface. Any
development-host Just recipes are conveniences only and do not own deployment
state or terminal sequencing. State operations use the networkless
`deployment-admin` service; repository operations use the separately
credentialed maintenance service.

There are deliberately no `down`, `ps`, `logs`, `drill-status`, or detailed
`repo-*` recipes. Use the documented raw commands, for example:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" down
docker compose --env-file "$vwless_env" --file "$vwless_compose" ps
docker compose --env-file "$vwless_env" --file "$vwless_compose" logs --tail 200 vaultwarden backup proxy
docker compose --project-name vaultwarden-less-drill --env-file "$vwless_env" --file "$vwless_compose" --profile drill ps
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo check --target primary
```

Repository format migration is not an operator command: initialization always
creates Restic format 2, and a future explicit `restic migrate` would be a
separately authorized maintenance event outside this interface.

## Release transfer and startup

On the build host, `just release` performs only the pure Nix build and prints
one store path. It creates no `result` link and consumes no prior test evidence.
Because that path is not a persistent garbage-collection root, copy it before
allowing Nix store collection:

```sh
release_path="$(just release)"
test -d "$release_path"
ssh -p <ssh-port> <user>@<host> 'test ! -e <new-release-directory>'
rsync --archive --checksum -- \
  "$release_path/" \
  <user>@<host>:<new-release-directory>/
```

Never overlay an existing release directory and never use `--delete` against
`DEPLOYMENT_ROOT`. On the deployment host, verify every shipped regular file,
load the three images, and compare their immutable identities with
`release.json` before changing the running stack:

```sh
(cd "$vwless_release_dir" && sha256sum --check SHA256SUMS)
docker image load --input "$vwless_release_dir/images/backup.tar.gz"
docker image load --input "$vwless_release_dir/images/proxy.tar.gz"
docker image load --input "$vwless_release_dir/images/vaultwarden.tar.gz"
vwless_commit='<full-clean-commit>'
vwless_release='<immutable-release>'
for vwless_product in backup proxy vaultwarden; do
  vwless_immutable="vaultwarden-less-$vwless_product:$vwless_release"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_immutable")" = "$vwless_commit"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_immutable")" = "$vwless_release"
done
```

Each archive contains one immutable production reference and no test image.
Production Compose selects that immutable release through
`VWLESS_IMAGE_TAG`; `local` remains a source-tree development and E2E value.
A successful load proves transferred image bytes and labels, not configuration
or repository access. `SHA256SUMS` detects changed transfer/storage bytes but
is not a signature; artifact authentication belongs to the transfer or signing
policy chosen by the operator.

Create the private layout and install public templates from the immutable
release as described in [Configuration](configuration.md). In particular,
`$vwless_env` is a mode-`0600` copy of the release `.env.example`, edited only
for the private root, runtime identity, and ports while retaining the exact
release-provided image tag.

Validate both isolated roles:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps config-check-maintenance
```

For one new deployment only, while `data` is completely empty and no import
or upgrade is current, bind it to the exact loaded immutable release:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile deployment run --rm --no-deps deployment-admin deployment init
```

The result is `initialized` or, for the exact same already-recorded identity,
`already_initialized`. Any other existing, damaged, or mismatched active state
is a hard stop; do not synthesize or edit `state/deployment/active.json`.

Initialize only a known-empty, dedicated repository target, once:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo init --target primary
```

Start the exact production services after both checks succeed:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" ps
docker compose --env-file "$vwless_env" --file "$vwless_compose" logs --tail 200 vaultwarden backup proxy
```

Only the proxy is bound to loopback. A trusted TLS edge must present the
configured public `DOMAIN`, remove untrusted forwarding headers, and send
root-path HTTP and WebSocket traffic to the proxy. The worker API and direct
Vaultwarden endpoint remain on internal networks.

### Explicit targetless bootstrap and first account

Use this procedure only when no Restic target is currently available. It
starts Vaultwarden, backup, and proxy, but it deliberately creates no recovery
point. Every byte written until a target is later configured and proven is
unprotected. A trusted external HTTPS/TLS edge and operator-only access must
already protect the proxy before registration is enabled; this project does
not terminate TLS.

Create a dedicated private root as the runtime account, separate from the
release directory, and install the release-provided targetless examples:

```sh
vwless_root=/srv/vaultwarden-less
umask 077
install -d -m 0700 "$vwless_root"
install -d -m 0700 "$vwless_root/config" "$vwless_root/credentials" "$vwless_root/credentials/restic"
install -d -m 0700 "$vwless_root/data" "$vwless_root/import" "$vwless_root/staging" "$vwless_root/cache" "$vwless_root/restore" "$vwless_root/state"
install -m 0600 "$vwless_release_dir/.env.example" "$vwless_env"
install -m 0600 "$vwless_release_dir/config/vaultwarden.example.env" "$vwless_root/config/vaultwarden.env"
install -m 0600 "$vwless_release_dir/examples/targetless/config/backup.yaml" "$vwless_root/config/backup.yaml"
install -m 0600 "$vwless_release_dir/examples/targetless/credentials/backup.yaml" "$vwless_root/credentials/backup.yaml"
install -m 0600 "$vwless_release_dir/examples/targetless/credentials/maintenance.yaml" "$vwless_root/credentials/maintenance.yaml"
install -m 0600 "$vwless_release_dir/examples/targetless/credentials/notifiers.yaml" "$vwless_root/credentials/notifiers.yaml"
```

Set the six `.env` values and the public HTTPS `DOMAIN` as described in
[Configuration](configuration.md). The installed YAML documents have exactly
these contents:

```yaml
# config/backup.yaml
upgrade_verification_targets: []
targets: []
notifiers: []
```

```yaml
# credentials/backup.yaml and credentials/maintenance.yaml
targets: {}
```

```yaml
# credentials/notifiers.yaml
notifiers: {}
```

All four files are owned by the runtime UID/GID and mode `0600`; the private
Restic password directory is mode `0700` and completely empty. Set
`SIGNUPS_ALLOWED=true` in the private Vaultwarden environment only for the
first-account window. Then validate both roles and initialize the deployment
identity. Both validation results must contain `"targets":[]` and no
`warnings` field:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile deployment run --rm --no-deps deployment-admin deployment init
```

Do not run `repo init`. Start the three production services and prove the
targetless boundary:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" ps vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" logs --tail 200 backup
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup status
test ! -e "$vwless_root/staging/point"
if docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup request-backup; then
  exit 1
fi
```

All three services must be healthy. Each backup-process start writes exactly
one WARN with `warning_code=no_backup_targets` and an explicit statement that
Vaultwarden data is not being backed up. Status remains `idle` with
`targets: []`, `last_check: null`, and `next_check_at: null`. The manual request
must fail nonzero with the ordinary `not_ready` error. Proxy events continue to
return success and must not block Vaultwarden writes.

Through the trusted public HTTPS origin, configure an official Bitwarden
desktop or mobile client for this server, create the first account, log in,
sync, and create then read a representative vault item. A health response or
direct plaintext request is not account verification.

Immediately close registration with a same-directory atomic replacement. Run
this as the owner of the private environment file:

```sh
vwless_vaultwarden_env="$vwless_root/config/vaultwarden.env"
test "$(grep -c '^SIGNUPS_ALLOWED=true$' "$vwless_vaultwarden_env")" = 1
test "$(grep -c '^SIGNUPS_ALLOWED=false$' "$vwless_vaultwarden_env" || true)" = 0
vwless_vaultwarden_env_tmp="$(mktemp "$vwless_root/config/.vaultwarden.env.XXXXXX")"
trap 'rm -f -- "$vwless_vaultwarden_env_tmp"' EXIT HUP INT TERM
chmod 0600 "$vwless_vaultwarden_env_tmp"
sed 's/^SIGNUPS_ALLOWED=true$/SIGNUPS_ALLOWED=false/' "$vwless_vaultwarden_env" > "$vwless_vaultwarden_env_tmp"
test "$(grep -c '^SIGNUPS_ALLOWED=false$' "$vwless_vaultwarden_env_tmp")" = 1
sync "$vwless_vaultwarden_env_tmp"
mv -T -- "$vwless_vaultwarden_env_tmp" "$vwless_vaultwarden_env"
sync -f "$vwless_root/config"
trap - EXIT HUP INT TERM
docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach --force-recreate vaultwarden
```

`restart` is insufficient because it does not reload `env_file`; the
`--force-recreate` command is required. After Vaultwarden is healthy again,
use a fresh official-client login to read the existing item and confirm that a
new account registration is rejected. Recheck the empty worker status and the
startup warning. The account is usable, but it still has no recovery point;
configure, initialize, back up, restore, and drill a real target before
claiming data protection or attempting an upgrade.

`down` stops and removes only the normal Compose project; bind-mounted data and
private inputs remain:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" down
```

## One-time external SQLite import

Use this workflow instead of `deployment init` when the first deployment takes
ownership of an existing Vaultwarden data directory. It is unavailable after
an active identity exists and never merges with nonempty `data`.

First stop the external Vaultwarden completely. Keep that original directory
outside this clone as the recovery copy. Create owned mode-`0700`
`import/source`, then use an offline copy mechanism to copy the complete data
tree into it, including `db.sqlite3`, any stopped WAL/SHM files, attachments,
Sends, and RSA files. The copied source must contain only owned `0700`
directories and owned, single-link `0600` regular files; do not repair a
rejected symlink, hard link, special file, owner, or permission blindly.
`data`, `import`, and `import/source` must be on the same filesystem.

With normal services stopped, `data` empty, no active identity, and no current
upgrade, prepare the candidate:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile deployment run --rm --no-deps deployment-admin deployment import prepare
```

Record the returned tree IDs. A repeated exact command resumes preparation;
do not delete state to force another result. Preparation absorbs committed WAL
through SQLite Online Backup, preserves `import/source` byte-for-byte, and
publishes a private candidate without the old RSA identity or SQLite sidecars.

Start only the candidate:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile import up --detach --wait --wait-timeout 180 import-vaultwarden
```

Temporarily route the existing trusted HTTPS edge to loopback `DRILL_PORT`.
Use the official Bitwarden client to log in again and verify representative
logins, attachments, and file Sends. Do not treat `/alive`, direct plaintext
HTTP, database inspection, or an old session as client-login confirmation.
Then stop the candidate and record verification:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile import stop import-vaultwarden
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile deployment run --rm --no-deps deployment-admin deployment import verify --confirm-client-login
```

Supply `--confirm-client-login` only after that real HTTPS login succeeds.
Verification checks the unchanged source, rotated RSA identity, exact audited
schema/migration, SQLite integrity/foreign keys, and the complete
database/attachment/Send snapshot closure. Without verification, accept fails.

Accept and finish with the following raw, resumable sequence:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile import stop import-vaultwarden
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile import rm --force --stop import-vaultwarden
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile deployment run --rm --no-deps deployment-admin deployment import accept
docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach --wait --wait-timeout 180 vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" ps vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup version
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY proxy /bin/vaultwarden-less-proxy version
docker image inspect --format '{{json .Config.Labels}}' "vaultwarden-less-vaultwarden:$vwless_release"
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile deployment run --rm --no-deps deployment-admin deployment import finalize
```

The Go accept command records the immutable decision before exchanging the
verified candidate with empty `data` and publishing the active release. Before
finalize, require all three `ps` entries to be healthy, require backup/proxy
version output and the Vaultwarden image labels to name the same exact clean
release, and verify the normal HTTPS path. Finalize only validates durable
state and archives it; it does not infer container health. If interrupted after
the decision, exchange, active write, service start, or before finalize, repeat
the sequence from its first line. Each Go boundary is idempotent for the same
recorded decision. The pre-import session must fail because RSA was rotated.
Retain the external original and `import/source` until the deployment and
backups have been independently accepted.

Before any accept decision, cancellation is explicit:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile deployment run --rm --no-deps deployment-admin deployment import cancel
```

Cancel removes only disposable candidate/current state and never source or
data. Once `decision.json` exists, cancel is permanently invalid; continue the
same raw accept/start/verify/finalize sequence. Never edit import JSON, exchange
directories manually, or replace a missing active document.

## Status and backup demand

Read resident process status:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup status
```

Request one debounced check:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup request-backup
```

The acknowledgement means only that demand was accepted. A check can wait for
the 30-second quiet period, extend quieting to two minutes, and wait for the
five-minute minimum check-start interval. A source inconsistency retries after
30 seconds; ordinary target failure retries after five minutes; 30 minutes of
continuous target failure becomes degraded. A 12-hour fallback check protects
against lost process-local events.

Worker state is `idle`, `debouncing`, `checking`, `partial`, or `degraded`.
Target state is `unknown`, `healthy`, `retrying`, or `degraded`. `partial`
means at least one configured repository has the point and another does not;
it must not be reported as complete success.

Status and notification queues reset on worker restart. Repository snapshots
are the durable fact. Confirm them independently:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo snapshots --target primary --limit 100
```

Use the full 64-character snapshot ID returned here for an explicit earlier
restore. Do not parse log messages as a snapshot contract.

## Repository maintenance

Run maintenance only after both role validation commands pass. The one-shot
maintenance container has repository egress but does not join application
networks.

List snapshots and check repository metadata:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo snapshots --target primary --limit 100
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo check --target primary
```

Unlock is for a confirmed stale Restic lock after proving no backup, restore,
prune, or other client is active:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo unlock --target primary
```

Prune always begins with a preview:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo prune --target primary
```

Review every retained and removed full snapshot ID. Only then apply the exact
fixed policy:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo prune --target primary --apply
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo check --target primary
```

The fixed retention is within 24 hours, 48 hourly, 14 daily, 8 weekly, 12
monthly, and 3 yearly snapshots. Selection is limited to this product's exact
host, path, project commit, content identity, and archive identity. Provider
lifecycle rules must not independently expire active Restic objects. Object
lock may make unlock or prune fail; see [Storage providers](providers.md).

## Verified restore

Restore latest to one previously absent output name:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile restore run --rm --no-deps restore restore --target primary --snapshot latest --output drill
```

Restore an explicit earlier full snapshot ID to a different absent name:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile restore run --rm --no-deps restore restore --target primary --snapshot <full-64-character-id> --output earlier
```

The output name resolves beneath `restore/`. Restore downloads into a private
temporary sibling, verifies exact tags, manifest, file set, sizes, hashes,
SQLite integrity and foreign keys, complete audited schema, database
references, canonical content identity, and archive identity, then atomically
publishes the output without replacement. It never falls back from a corrupt
latest point to an earlier point and never overwrites an existing output.

On failure, preserve the error JSON, run `repo check`, and select a known full
earlier ID explicitly if incident policy authorizes it. Use a new output name;
do not delete a verified restore merely to reuse its name.

### Authorized production restore runbook

This is a manual incident procedure, not a product command. Do not run it
without incident-specific authorization for the exact deployment and selected
restore. The verified restore must already have passed the complete drill. Its
manifest `project_commit` must exactly equal the current healthy backup
container's `version` project commit; a point from another A/B identity is
ineligible. Record both JSON documents and stop on any
mismatch.

Set the following values explicitly from the reviewed incident record; do not
source or reparse `.env` in shell code:

```sh
vwless_root='<exact Compose-resolved DEPLOYMENT_ROOT>'
vwless_restore_name='<verified-restore-output-name>'
vwless_incident_id='<unique-lowercase-incident-id>'
printf '%s\n' "$vwless_restore_name" | grep -Eq '^[a-z][a-z0-9-]{0,62}$'
printf '%s\n' "$vwless_incident_id" | grep -Eq '^[a-z0-9][a-z0-9-]{0,62}$'
vwless_restore="$vwless_root/restore/$vwless_restore_name"
vwless_incident="$vwless_root/state/incidents/$vwless_incident_id"
test "$vwless_root" != / && test -d "$vwless_root"
test -d "$vwless_restore" && test -f "$vwless_restore/manifest.json"
test "$(stat -c %d "$vwless_root/data")" = "$(stat -c %d "$vwless_restore")"
LC_ALL=C mv --help | grep -q -- '--no-copy'
LC_ALL=C mv --help | grep -q -- 'none-fail'
LC_ALL=C cp --help | grep -q -- 'none-fail'
umask 077
test -e "$vwless_root/state/incidents" || mkdir -m 0700 -- "$vwless_root/state/incidents"
mkdir -m 0700 -- "$vwless_incident"
test "$(stat -c '%a:%u:%g' "$vwless_root/state/incidents")" = "700:$(id -u):$(id -g)"
test "$(stat -c '%a:%u:%g' "$vwless_incident")" = "700:$(id -u):$(id -g)"
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup version > "$vwless_incident/active-version.json"
cp --update=none-fail --preserve=mode,timestamps -- "$vwless_restore/manifest.json" "$vwless_incident/reviewed-manifest.json"
```

Review those two saved JSON files and require the same exact full project
commit before continuing. Then stop writers in order and confirm that no
container or host process holds `db.sqlite3`:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop backup
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop vaultwarden
test -z "$(docker compose --env-file "$vwless_env" --file "$vwless_compose" ps --status running --quiet vaultwarden backup proxy)"
```

While stopped, move the manifest into the incident record and perform only
same-filesystem, no-copy, no-overwrite renames. Every destination below must be
absent; `none-fail` turns an existing destination into a hard failure.

```sh
mv --update=none-fail --no-copy -T -- "$vwless_restore/manifest.json" "$vwless_incident/selected-manifest.json"
mv --update=none-fail --no-copy -T -- "$vwless_root/data" "$vwless_incident/pre-restore-data"
mv --update=none-fail --no-copy -T -- "$vwless_restore" "$vwless_root/data"
```

The durable active identity does not change. Start the same current release,
require all three health checks, reject every old session, and establish a
fresh login. Then create a new backup, restore it to a new output, and complete
another drill. Keep the entire incident directory and both retained data trees
until separate deletion authorization is recorded.

If startup or validation fails after the restored tree reached `data`, keep
services stopped and reverse only with no-overwrite renames:

```sh
mv --update=none-fail --no-copy -T -- "$vwless_root/data" "$vwless_incident/failed-restored-data"
mv --update=none-fail --no-copy -T -- "$vwless_incident/pre-restore-data" "$vwless_root/data"
docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach --wait --wait-timeout 180 vaultwarden backup proxy
```

If failure occurred before the restored tree reached `data`, leave that tree
where it is and restore only the missing original `data` with the second rename.
Never overwrite either tree, change `active.json`, promote a cross-commit
snapshot, or remove incident material as part of this runbook.

## Isolated recovery drill

The drill profile consumes exactly `restore/drill`, so create that verified
output first. Start the separate project and wait for health:

```sh
docker compose --project-name vaultwarden-less-drill --env-file "$vwless_env" --file "$vwless_compose" --profile drill up --detach --wait --wait-timeout 180 drill-vaultwarden
docker compose --project-name vaultwarden-less-drill --env-file "$vwless_env" --file "$vwless_compose" --profile drill ps
```

Configure a normal Bitwarden client for the drill's loopback origin. Use fresh
sessions and verify, at minimum:

- both a personal and an organization item, including a value changed after an
  earlier recovery point;
- folders, organizations, collections, and membership;
- attachment bytes and ordinary file-Send bytes;
- the exhausted behavior of an access-limited Send when present;
- a second user and data owned by that user.

An authenticated session captured before recovery should fail against the
drill. Login again and establish fresh sessions. RSA state is intentionally
excluded, so prior access, refresh, invitation, attachment, and download tokens
are expected to be invalid.

Remove only the disposable drill project and volume:

```sh
docker compose --project-name vaultwarden-less-drill --env-file "$vwless_env" --file "$vwless_compose" --profile drill down --volumes
```

The verified `restore/drill` tree remains. The production stack is a different
Compose project and is not changed by drill cleanup.

## Restart durability check

After the first successful point and after material deployment changes, prove
that durable repository state survives a complete service restart:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop proxy backup vaultwarden
docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup request-backup
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo snapshots --target primary --limit 100
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance repo check --target primary
```

Wait for a completed worker check before interpreting the snapshot list.
Unchanged logical content may reuse the existing point even if the fresh
SQLite archive bytes differ. That is expected content-ID deduplication, not a
missed backup.

## Two-target degradation and catch-up

Test this only with controlled acceptance targets. Do not create a provider
outage or revoke production credentials without exact authorization.

1. Configure two independent repositories, passwords, and role keys; validate
   both roles and initialize both targets.
2. Create changed Vaultwarden data and wait until both targets report the same
   `content_id` with their own verified snapshots.
3. Make only the secondary target unavailable through the controlled test
   mechanism, create another change, and request a backup.
4. Prove the primary succeeds while the secondary reports retrying and the
   worker reports partial rather than complete success. Use the release gate's
   exact-boundary fake-clock test and accelerated real-adapter test as the
   degradation-threshold evidence; do not turn a wall-clock observation into a
   stronger timing claim.
5. Restore secondary access without changing source data. The worker must
   catch that target up to the same latest `content_id`.
6. Run `repo check`, latest restore, and drill from each target. Do not accept
   equality based only on snapshot count or timestamp.

The resident worker dispatches in configuration order with at most two target
operations in flight, then publishes results and notifications in configuration
order. A slow target occupies one slot but does not erase another target's
result. Catch-up stores the newest verified point; it does not synthesize every
missed intermediate state.

## Cold deployment acceptance

Before relying on a new deployment, use an ordinary isolated Linux/amd64 host
with only Docker Engine, Compose, checksum tools, and rsync for deployment
operations:

1. copy the exact release bundle into a new directory and verify every checksum;
2. create private copies beneath `DEPLOYMENT_ROOT` exactly as documented;
3. checksum and standard-load all three immutable image archives, validate
   their labels, and render Compose with the same immutable tag;
4. run both configuration checks, initialize the empty deployment identity,
   and initialize two real repositories;
5. start production, create real Vaultwarden data through a normal client, and
   obtain verified points on both targets;
6. stop and restart all three services and prove repository continuity;
7. restore latest and an earlier full ID, run a drill, verify data, and prove
   old-session invalidation plus fresh login;
8. run the controlled two-target degradation/catch-up sequence;
9. remove only acceptance-created containers, networks, volumes, and temporary
   directories, then prove no such resource remains.

The operator should not need to read Go/Nix source or install project tools to
perform those steps. A real provider must still be tested separately from the
Moto-based automated boundary.

## Incident boundary

The product creates verified restore trees and disposable drills; it does not
replace live `data`. Promotion of a restore into production is a destructive
real-data decision and requires exact authorization plus a verified recovery
copy of the current tree. Keep the failed source intact, record the selected
snapshot and identities, complete the drill, plan rollback, and only then use
a separately reviewed incident procedure.

For an audited A/B product change, do not improvise image replacement or data
promotion. Follow [Upgrades and explicit rollback](upgrades.md).
