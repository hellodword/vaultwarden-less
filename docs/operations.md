# Operations

Complete [deployment](deployment.md) first. Use the bundle's `compose.yml` and
[configuration tree](configuration.md).
Run examples with Bash as the non-root runtime user. Replace paths, image
tags, IDs and provider inputs with reviewed values before running them.
Do not enable shell tracing around credentials.

## Checks and status

Set `RELEASE_DIR` and `DEPLOYMENT_ROOT` to the existing release and deployment.
In each new shell, define the Compose helper:

```sh
set -euo pipefail
compose() {
  docker compose --env-file "$DEPLOYMENT_ROOT/compose.env" \
    --file "$RELEASE_DIR/compose.yml" "$@"
}
compose ps
compose exec --no-TTY backup /bin/vaultwarden-less-backup status
```

Request a check and then inspect completion separately:

```sh
compose exec --no-TTY backup /bin/vaultwarden-less-backup check
compose exec --no-TTY backup /bin/vaultwarden-less-backup check --full
compose exec --no-TTY backup /bin/vaultwarden-less-backup status
```

An accepted check is asynchronous. `last_result`, `source`, `issues`,
target `success_id`, `success_at`, `waiting`, `uploading`, `error` and `retry_at`
explain progress. Timestamps are absent until a success, check or schedule
exists; cleared retry times are absent too. This also applies to notifier
`last_success` and `next_attempt`. Health checks only prove process responsiveness. Restart backup to
reload configuration or credentials. Client traffic continues during backup
failure. Empty targets keep the service usable but do not protect data.

An incomplete snapshot is useful recovery material, but its missing files
remain missing. Repair source problems and require a later complete snapshot.
For database or cache failures, retain the diagnostics and repair the cause;
the worker retries without rewriting source data.

## Reading notifications

All channels show the same English report. Titles use the following priority,
so an uploaded incomplete capture remains an **Incomplete capture** report.

| Title | Meaning |
| --- | --- |
| Backup not configured | No valid backup target is configured. |
| Capture failed | The source could not be captured; target states describe the previous capture. |
| Incomplete capture | The native database and available files were kept, but source issues remain; retry is pending. |
| Backup target error | At least one target has invalid configuration or a repository failure. |
| Backup warning | Capture comparison, reference diagnostics or local baseline caching reported a warning. |
| Backup pending | Capture or target work remains, including a newer capture waiting after an older upload. |
| Backup updated | At least one target uploaded the current capture, with no higher-priority condition. |
| No changes | All targets are up to date with the current capture. |

For example, a check followed by two target results can produce:

```markdown
**vaultwarden-less · Backup target error**

**Source:** Complete

**Targets:** 1 error · 1 up to date

**Target details**

> primary: Up to date

> secondary: Upload failed; retry pending

**Triggers**

> Scheduled check: 1

> Target result: 2

**Request activity**

> Unknown GET: started 1, finished 1

**Summary:** 1 check · 3 reports
```

Triggers distinguish startup, manual, scheduled ordinary/full, request activity,
source retry and target results. A trigger count describes merged trigger
reports; request counts appear separately as `started` and `finished`. A request
that crosses two checks can have its start and finish in different reports.

After notification delivery recovers, `undelivered reports` counts the pending
reports included in the recovery summary. Source diagnostics and target states
describe the latest observation, rather than a history of every intermediate
failure. `Omitted details: N` means whole detail entries were left out to fit the
channel's byte limit; the source, target totals and report counts remain.

## Native restic maintenance

Use a dedicated repository/prefix for this installation. Obtain `RESTIC_IMAGE`
from `compose config --images backup` using the helper above. The following
invokes the native restic binary and uses the maintenance environment file, parsed by Docker.
It mounts only the password directory. It works without a running worker.

```sh
set -euo pipefail
: "${DEPLOYMENT_ROOT:?}" "${RESTIC_IMAGE:?}"
restic_primary() {
  docker run --rm --read-only --user "$(id -u):$(id -g)" \
    --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
    --env-file "$DEPLOYMENT_ROOT/credentials/maintenance/primary.env" \
    --env RESTIC_CACHE_DIR=/tmp/restic \
    --mount "type=bind,source=$DEPLOYMENT_ROOT/credentials/restic,target=/run/credentials/restic,readonly" \
    --entrypoint /bin/restic "$RESTIC_IMAGE" \
    --option s3.bucket-lookup=path --option s3.region=us-east-1 "$@"
}
restic_primary snapshots --host vaultwarden-less --group-by host
restic_primary check
```

Adjust backend options to match the destination. During first deployment,
define the helper, then run `restic_primary init` once for a new empty repository
before the `snapshots`/`check` examples above. Use separate credentials and
invocations for each destination.
List and record explicit native snapshot IDs before recovery. Neither local
status nor a notification replaces verification of remote snapshots.

Retention groups by host **and tags**, so incomplete snapshots cannot crowd
complete recovery material out of the same retention group. Start with this
dry run and review its selected snapshots:

```sh
set -euo pipefail
restic_primary forget --host vaultwarden-less --group-by host,tags \
  --keep-within 24h --keep-hourly 48 --keep-daily 14 --keep-weekly 8 \
  --keep-monthly 12 --keep-yearly 3 --keep-last 1 --dry-run
```

When you approve that deletion set, run the same command without `--dry-run`,
then `restic_primary prune` and `restic_primary check`. Keep maintenance
separate from backup credentials. Use `restic_primary unlock` only after
establishing that no relevant restic operation remains active. Object locks
and provider policy may prevent maintenance; do not bypass them automatically.

## Restore to a new directory

Choose one full snapshot ID from native `snapshots` output. Set `RESTORE_DIR`
to a new absolute directory whose parent exists. The block fails before
invoking restic if that destination exists, including a dangling symlink.
A failed restore leaves its partial directory for inspection; retry with a
different new path.

<!-- native-restore-example -->
```sh
set -euo pipefail
: "${DEPLOYMENT_ROOT:?}" "${RESTIC_IMAGE:?}" "${SNAPSHOT_ID:?}" "${RESTORE_DIR:?}"
[[ $SNAPSHOT_ID =~ ^[0-9a-f]{64}$ ]]
[[ $RESTORE_DIR = /* ]]
test "$(realpath -m -- "$RESTORE_DIR")" = "$RESTORE_DIR"
test ! -e "$RESTORE_DIR"
test ! -L "$RESTORE_DIR"
mkdir -m 0700 -- "$RESTORE_DIR"
docker run --rm --read-only --user "$(id -u):$(id -g)" \
  --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
  --env-file "$DEPLOYMENT_ROOT/credentials/maintenance/primary.env" \
  --env RESTIC_CACHE_DIR=/tmp/restic \
  --mount "type=bind,source=$DEPLOYMENT_ROOT/credentials/restic,target=/run/credentials/restic,readonly" \
  --mount "type=bind,source=$RESTORE_DIR,target=/restore" \
  --entrypoint /bin/restic "$RESTIC_IMAGE" \
  --option s3.bucket-lookup=path --option s3.region=us-east-1 \
  restore "$SNAPSHOT_ID" --verify --target /restore
integrity=$(docker run --rm --read-only --user "$(id -u):$(id -g)" \
  --mount "type=bind,source=$RESTORE_DIR,target=/restore,readonly" \
  --entrypoint /bin/sqlite3 "$RESTIC_IMAGE" \
  -readonly 'file:/restore/db.sqlite3?immutable=1' 'PRAGMA integrity_check;')
test "$integrity" = ok
printf '%s\n' "$integrity"
```

The `immutable=1` connection is for this completed, stopped restore only; it
allows checking a WAL-mode database without creating sidecar files on the
read-only mount. Never use it against the live source database.
Require the integrity result `ok`. The restored root contains `db.sqlite3`
and the native tree, including RSA files. Preserve that restored evidence and
start an isolated Vaultwarden against a separate copy, with its own port.
Use an official client to read representative items, attachments and Sends.
Restoring an incomplete tag does not repair missing data.

## Upgrade with a preserved original

Prepare and load the new bundle before stopping service. Disable incoming
writes, stop the proxy, request a full check and wait until the selected
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
: "${DEPLOYMENT_ROOT:?}" "${UPGRADE_DATA:?}"
[[ $UPGRADE_DATA = /* ]]
test "$(realpath -m -- "$UPGRADE_DATA")" = "$UPGRADE_DATA"
case "$UPGRADE_DATA/" in "$DEPLOYMENT_ROOT/data/"*) exit 1 ;; esac
test -d "$DEPLOYMENT_ROOT/data"
test ! -e "$UPGRADE_DATA"
test ! -L "$UPGRADE_DATA"
mkdir -m 0700 -- "$UPGRADE_DATA"
cp -a -- "$DEPLOYMENT_ROOT/data/." "$UPGRADE_DATA/"
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
