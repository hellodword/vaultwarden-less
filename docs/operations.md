# Operations

Complete [deployment](deployment.md) first. Use your deployment's `compose.yml` and
[configuration tree](configuration.md).
Run examples with Bash as the non-root runtime user. Replace paths, image
tags, IDs and provider inputs with reviewed values before running them.
Do not enable shell tracing around credentials.

## Checks and status

In each new shell, set `DEPLOY_DIR` to the existing absolute deployment path
and define the Compose helper. This is an ordinary shell variable; do not
export it or add it to `compose.env`. The helper works from any working directory:

```sh
set -euo pipefail
DEPLOY_DIR=/srv/vaultwarden-less
compose() {
  docker compose --env-file "$DEPLOY_DIR/compose.env" \
    --file "$DEPLOY_DIR/compose.yml" "$@"
}
compose ps
compose exec --no-TTY backup /bin/vaultwarden-less-backup status
```

Run a backup or verification and wait for the actual job result:

```sh
compose exec --no-TTY backup /bin/vaultwarden-less-backup backup
compose exec --no-TTY backup /bin/vaultwarden-less-backup backup --force
compose exec --no-TTY backup /bin/vaultwarden-less-backup verify --target aws-s3
compose exec --no-TTY backup /bin/vaultwarden-less-backup verify --target aws-s3 --read-data
compose exec --no-TTY backup /bin/vaultwarden-less-backup status
```

Ordinary backup captures immediately and uploads only effective changes. Force
creates a recovery point even without change and grants each target one attempt
past backoff; it does not reset consecutive failure counts. Partial failure,
incomplete source or deferred necessary uploads exit 1 with per-target results.
Input errors exit 2. Ctrl+C exits 130 and only stops CLI waiting: the service
continues the accepted job. Status retains current and recent operation results.
A service restart loses these receipts, so the CLI reports completion unknown.

Read `last_check`, `last_result`, source completeness/issues and `last_change`
together. Old snapshot age is not lag if a recent source check confirmed no
change. Each target reports current coverage, success_id/success_at, the
separate complete_id/complete_at, upload errors, backoff and active operation.
Structure and read_data report last attempt/success, result and enabled next
period. Unscheduled verification still shows its last known result.

Source faults clear only after a complete capture; upload/coverage faults after
a confirmed upload; structure faults after successful structure/full verification;
full-read faults only after a successful full read through this service.
Uploads continue during verification failures. An external restic check does
not update this state. Generic errors are not a diagnosis of network failure
or proven corruption. No automatic repair, unlock or deletion is performed.

Health means process responsiveness. Configuration reloads on worker restart.
An empty target list is `unconfigured` and does not capture data. For source
issues, repair the source and request another backup. Incomplete snapshots keep
the database and available files without substituting older attachments.

## Reading notifications

Reports are English with local time and UTC offset. Only first/expanded faults,
recoveries, sustained-fault reminders and the daily summary generate delivery.
A healthy check, unchanged result, upload or verification updates status and
daily counts. The defaults are 21:00 daily and reminders every six hours.
Missed daily periods are combined into one catch-up report.

```text
Vaultwarden backup — Fault / Recovery / Reminder / Daily
Time: 2026-10-29 21:00:00 +08:00
Period: ...                         # daily only
Source: ...
Targets: ...
Verification: ...
Faults / Recoveries: ...
Next actions: ...
```

Fault details identify the object, category, first occurrence, safe code,
latest complete recovery point and next action. A recovery lists the recovered
item and retains other unresolved faults. An incident recovered before its
first successful delivery remains as “occurred then recovered”, with duration.
Daily reports include source checks/changes, per-target snapshots, recent
uploads/complete points, verification results and unresolved faults/retries.

Each channel persists its independent merged summary and three-attempt budget.
Retries wait one minute after the first failure and five after the second;
Retry-After and APNs server minimum delays also apply. Exhausted content waits
for a new fault, recovery, reminder or daily report to reopen a round. Ordinary
checks do not reopen it. Correct permanent authentication/configuration errors
and restart to revalidate. Pending delivery survives restarts. Platform acceptance
may be delivered twice if the process dies before recording success locally.
`Omitted details: N` means whole entries were omitted to meet platform limits.

## Local disk and shutdown

Keep `state/` across restarts. It holds deadlines, faults, backoff and notification
summaries. `cache/` is rebuildable; stop backup before clearing it. Loss of this
cache does not force a new snapshot because native tags restore comparison
baselines. Data-sized temporary content uses disk under `cache/work/`, not tmpfs.
After acquiring its instance lock the worker removes abandoned work directories.
Running uploads retain immutable versions; waiting versions merge to the latest.

Normal shutdown is gateway, worker, Vaultwarden (`compose stop` follows the
configured dependencies). The gateway drains for up to 120 seconds and flushes
for ten; Compose grants 150 seconds. `restart: "on-failure:5"` requires host
boot/startup and stopped-service supervision. Sudden termination is recovered
by startup and six-hour source captures.

## Native restic maintenance

Use a dedicated repository/prefix for this installation. Obtain `RESTIC_IMAGE`
from `compose config --images backup` using the helper above.

Prepare a maintenance env file on the host when needed; the release does not
ship maintenance credential templates. For example, create the private directory
`$DEPLOY_DIR/credentials/maintenance/` with mode 0700 and save
`aws-s3-maintenance.env` there with mode 0600, readable by the runtime user:

```dotenv
RESTIC_REPOSITORY=s3:https://s3.us-east-1.amazonaws.com/REPLACE_WITH_AWS_S3_BUCKET/vaultwarden-less
AWS_DEFAULT_REGION=us-east-1
RESTIC_PASSWORD_FILE=/run/credentials/restic/aws-s3-repository-password.txt
AWS_ACCESS_KEY_ID=REPLACE_WITH_AWS_S3_MAINTENANCE_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY=REPLACE_WITH_AWS_S3_MAINTENANCE_SECRET_ACCESS_KEY
```

Copy the actual repository URL, region and password path from the selected
target's backup env file. Use a separate provider principal authorized for the
intended maintenance operations, including deletions for `forget` and `prune`.
For temporary AWS credentials, also supply the matching `AWS_SESSION_TOKEN`.
The file uses literal Docker env-file values: do not quote, interpolate or
source it. Keep it outside the backup service's credential mount. Changes take
effect on the next maintenance invocation.

The following invokes the native restic binary with this env file, parsed by
Docker. It mounts only the password directory and works without a running
worker. Set `RESTIC_ENV_FILE` to the absolute host path for the selected file.

```sh
set -euo pipefail
: "${DEPLOY_DIR:?}" "${RESTIC_IMAGE:?}"
RESTIC_ENV_FILE="$DEPLOY_DIR/credentials/maintenance/aws-s3-maintenance.env"
restic_repository() {
  : "${RESTIC_ENV_FILE:?}"
  mkdir -p -- "$DEPLOY_DIR/cache/maintenance"
  docker run --rm --read-only --user "$(id -u):$(id -g)" \
    --mount "type=bind,source=$DEPLOY_DIR/cache/maintenance,target=/cache" \
    --env TMPDIR=/cache \
    --env-file "$RESTIC_ENV_FILE" \
    --env RESTIC_CACHE_DIR=/cache/restic \
    --mount "type=bind,source=$DEPLOY_DIR/credentials/restic,target=/run/credentials/restic,readonly" \
    --entrypoint /bin/restic "$RESTIC_IMAGE" "$@"
}
restic_repository snapshots --host vaultwarden-less --group-by host
restic_repository check
```

For R2 or B2, prepare a `cloudflare-r2-maintenance.env` or
`backblaze-b2-maintenance.env` in the same way, using that target's repository,
region and password path and the provider's maintenance credentials. Their S3
credential mappings are in [configuration](configuration.md#destinations).
Change `RESTIC_ENV_FILE` to the absolute path of the file you prepared.
If you added backend options in `backup.yaml`, pass the same native `--option`
arguments here and during restore. During first deployment, define the helper,
then run `restic_repository init` once for a new empty repository before the
`snapshots`/`check` examples above. Repeat with each destination's env file.
List and record explicit native snapshot IDs before recovery. Neither local
status nor a notification replaces verification of remote snapshots.

Select complete and incomplete snapshots separately and group by host. Never
include the changing fingerprint tag in retention groups: it would give every
version its own group. Review these dry-run deletion sets before applying them:

```sh
set -euo pipefail
restic_repository forget --host vaultwarden-less --tag complete --group-by host \
  --keep-within 24h --keep-hourly 48 --keep-daily 14 --keep-weekly 8 \
  --keep-monthly 12 --keep-yearly 3 --keep-last 1 --dry-run
restic_repository forget --host vaultwarden-less --tag incomplete --group-by host \
  --keep-within 7d --keep-last 3 --dry-run
restic_repository snapshots --host vaultwarden-less --tag vwless-pending
```

`vwless-pending` identifies an upload whose successful finalization was not
confirmed. It never establishes worker coverage. Inspect such native snapshots
before deciding whether to remove them; do not mistake them for complete points.

When you approve that deletion set, run the same command without `--dry-run`,
then `restic_repository prune` and `restic_repository check`. Keep maintenance
separate from backup credentials. Use `restic_repository unlock` only after
establishing that no relevant restic operation remains active. Object locks
and provider policy may prevent maintenance; do not bypass them automatically.

## Restore to a new directory

Select the source repository with `RESTIC_ENV_FILE` as above, then choose one
full snapshot ID from its native `snapshots` output. Set `RESTORE_DIR`
to a new absolute directory whose parent exists. The block fails before
invoking restic if that destination exists, including a dangling symlink.
A failed restore leaves its partial directory for inspection; retry with a
different new path.

<!-- native-restore-example -->
```sh
set -euo pipefail
: "${DEPLOY_DIR:?}" "${RESTIC_IMAGE:?}" "${RESTIC_ENV_FILE:?}" "${SNAPSHOT_ID:?}" "${RESTORE_DIR:?}"
[[ $SNAPSHOT_ID =~ ^[0-9a-f]{64}$ ]]
[[ $RESTORE_DIR = /* ]]
test "$(realpath -m -- "$RESTORE_DIR")" = "$RESTORE_DIR"
test ! -e "$RESTORE_DIR"
test ! -L "$RESTORE_DIR"
mkdir -m 0700 -- "$RESTORE_DIR"
mkdir -p -- "$DEPLOY_DIR/cache/maintenance"
docker run --rm --read-only --user "$(id -u):$(id -g)" \
  --mount "type=bind,source=$DEPLOY_DIR/cache/maintenance,target=/cache" \
  --env TMPDIR=/cache \
  --env-file "$RESTIC_ENV_FILE" \
  --env RESTIC_CACHE_DIR=/cache/restic \
  --mount "type=bind,source=$DEPLOY_DIR/credentials/restic,target=/run/credentials/restic,readonly" \
  --mount "type=bind,source=$RESTORE_DIR,target=/restore" \
  --entrypoint /bin/restic "$RESTIC_IMAGE" \
  restore "$SNAPSHOT_ID" --verify --target /restore
integrity=$(docker run --rm --read-only --user "$(id -u):$(id -g)" \
  --mount "type=bind,source=$RESTORE_DIR,target=/restore" \
  --entrypoint /bin/sqlite3 "$RESTIC_IMAGE" \
  '/restore/db.sqlite3' 'PRAGMA integrity_check;')
test "$integrity" = ok
printf '%s\n' "$integrity"
```

The integrity check opens this independent restored copy normally. The selected
SQLite skips CHECK-constraint validation on read-only connections; do not use
`immutable=1` as a shortcut for this verification. Never run these commands
against the live source directory.
Require the integrity result `ok`. The restored root contains `db.sqlite3`
and the native tree, including RSA files. Preserve that restored evidence and
start an isolated Vaultwarden against a separate copy, with its own port.
Use an official client to read representative items, attachments and Sends.
Restoring an incomplete tag does not repair missing data.

## Upgrade with a preserved original

The deployment owns its `compose.yml`. Compare it with the new bundle's file
and merge the required changes, preserving your runtime UID/GID and host port.
Select the new bundle's `VWLESS_IMAGE_TAG` in the new deployment's `compose.env`.
Keep the previous Compose file, image tag and other configuration for rollback;
do not overwrite your local settings with the new templates wholesale.

Prepare and load the new bundle before stopping service. Disable incoming
writes, stop the proxy, run `backup --force` and wait until the selected
destination reports success with no pending upload. Verify the corresponding
native snapshot ID and its completeness. Keep the old release, its
configuration and the original directory available for rollback.

Then stop backup and Vaultwarden. Verify they have stopped. Choose
`UPGRADE_DATA` as a **new** absolute working directory for the new version,
outside the original `data/` tree. The following block copies only a stopped
tree and never overwrites an existing working directory:

<!-- upgrade-copy-example -->
```sh
set -euo pipefail
: "${DEPLOY_DIR:?}" "${UPGRADE_DATA:?}"
[[ $UPGRADE_DATA = /* ]]
test "$(realpath -m -- "$UPGRADE_DATA")" = "$UPGRADE_DATA"
case "$UPGRADE_DATA/" in "$DEPLOY_DIR/data/"*) exit 1 ;; esac
test -d "$DEPLOY_DIR/data"
test ! -e "$UPGRADE_DATA"
test ! -L "$UPGRADE_DATA"
mkdir -m 0700 -- "$UPGRADE_DATA"
cp -a -- "$DEPLOY_DIR/data/." "$UPGRADE_DATA/"
```

Point an explicitly reviewed new deployment at a copy of this working tree,
leaving the original data path stopped and untouched. Start only the new
Vaultwarden first and let upstream perform migrations. Verify health and
client reads before starting its backup and proxy. Do not run two writers
against one directory.

If validation fails, stop the new deployment, retain its failed working copy,
and restart the previous release against its untouched original data.
Do not run an older Vaultwarden against a database already migrated by the
new version. Switching production traffic or replacing a live data directory
is an operator decision; the backup program performs neither action.

Normal `compose down` removes this project's containers and network while
leaving bind-mounted data, configuration and recovery material in place.
