# Operations

These runbooks assume an ordinary Linux/amd64 host with Docker Engine, the
Docker Compose plugin, GNU file utilities, and checksum/copy tools. Nix, Git,
Go, Python, Just, and the source tree are build-host concerns, not deployment
dependencies.

Examples use task-specific variables:

```sh
vwless_release_dir=/opt/vaultwarden-less/releases/git-<full-commit>
vwless_root=/srv/vaultwarden-less
vwless_env="$vwless_root/compose.env"
vwless_compose="$vwless_release_dir/compose.yml"
```

Never edit the release directory. Never delete, overwrite, or exchange real
data merely to make a check pass. Any production recovery or release
transition requires explicit authorization for the exact instance, verified
recovery material, and a rehearsed reversal.

## Release build, transfer, and image load

On a clean committed build checkout:

```sh
just check
just release
test -L result
test -d result
```

`just release` is exactly `nix build .#release`; it creates the standard
`result` link and does not run or consume tests. Copy the bundle into a
previously absent, immutable version directory. Never overlay an older release
and never use a deletion option against the deployment root:

```sh
test ! -e <new-release-directory>
mkdir -m 0755 -- <new-release-directory>
rsync --archive --checksum -- result/ <new-release-directory>/
```

On the deployment host, verify the complete bundle before loading anything:

```sh
(cd "$vwless_release_dir" && sha256sum --check SHA256SUMS)
```

The file set must be exactly the allowlist in
[Product contracts](contracts.md#release-identity-and-bundle). Load all three
archives with the standard Docker loader:

```sh
docker image load --input "$vwless_release_dir/images/backup.tar.gz"
docker image load --input "$vwless_release_dir/images/proxy.tar.gz"
docker image load --input "$vwless_release_dir/images/vaultwarden.tar.gz"
```

Given the reviewed full commit, verify the tag and OCI labels:

```sh
vwless_commit=<40-lowercase-hex-commit>
vwless_tag="git-$vwless_commit"
for vwless_product in backup proxy vaultwarden; do
  vwless_image="vaultwarden-less-$vwless_product:$vwless_tag"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_image")" = "$vwless_commit"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_image")" = "$vwless_tag"
done
```

Loading proves only transferred bytes and labels. `SHA256SUMS` is not an
artifact signature, and the bundle is not an SBOM or corresponding-source
archive. Transport authentication, publication, retention, and redistribution
compliance remain operator policy.

Create the private tree and edit private copies as described in
[Configuration](configuration.md). Render the final Compose model and validate
both role mounts before starting services:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" config --quiet

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  run --rm --no-deps backup config validate --role backup

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  config validate --role maintenance
```

## Targetless first start and first account

Targetless operation is the only supported start without a repository. It is
an explicit unprotected state, not a partial configuration. Before validation:

1. write exactly `targets: []` and `notifiers: []` to
   `config/backup.yaml`;
2. write exactly `targets: {}` to both role credential files;
3. leave `credentials/restic` genuinely empty; and
4. set `SIGNUPS_ALLOWED=true` and `INVITATIONS_ALLOWED=false` in
   `config/vaultwarden.env`.

Run both validation commands above, then start the three production services:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  up --detach vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  ps vaultwarden backup proxy
```

Require all three health checks to become healthy. Confirm the backup process
emits exactly one targetless warning for that startup:

```sh
test "$(docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  logs backup | grep -c '"warning_code":"no_backup_targets"')" -eq 1
```

Status must be idle with an empty target array and null last/next checks, and no
staging point may appear:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  exec --no-TTY backup /bin/vaultwarden-less-backup status
test ! -e "$vwless_root/staging/point"
```

The following command must exit nonzero with error code `not_ready`:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  exec --no-TTY backup /bin/vaultwarden-less-backup request-backup
```

Do not expose the loopback port directly. Configure a trusted HTTPS reverse
proxy first. In an official Bitwarden desktop, browser-extension, or mobile
client, select the exact HTTPS server origin, create the first account, log in,
sync, and create/read a representative item. Web Vault assets are not included.

After successful client verification, atomically disable further signup. The
following example accepts only the expected single source line and publishes a
`0600` replacement in the same directory:

```sh
vwless_signup_file="$vwless_root/config/vaultwarden.env"
test "$(grep -c '^SIGNUPS_ALLOWED=true$' "$vwless_signup_file")" -eq 1
test "$(grep -c '^SIGNUPS_ALLOWED=false$' "$vwless_signup_file")" -eq 0
umask 077
vwless_signup_tmp="$(mktemp --tmpdir="$vwless_root/config" .vaultwarden.env.XXXXXX)"
sed 's/^SIGNUPS_ALLOWED=true$/SIGNUPS_ALLOWED=false/' "$vwless_signup_file" > "$vwless_signup_tmp"
chmod 0600 -- "$vwless_signup_tmp"
mv -T -- "$vwless_signup_tmp" "$vwless_signup_file"
```

Force-recreate only Vaultwarden and prove its container identity changed:

```sh
vwless_old_container="$(docker compose --env-file "$vwless_env" --file "$vwless_compose" ps --quiet vaultwarden)"
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  up --detach --force-recreate vaultwarden
vwless_new_container="$(docker compose --env-file "$vwless_env" --file "$vwless_compose" ps --quiet vaultwarden)"
test -n "$vwless_old_container"
test -n "$vwless_new_container"
test "$vwless_old_container" != "$vwless_new_container"
```

Use a fresh official-client login to read the first account's item, and verify
that creating another account is rejected. The data tree and RSA digest must be
unchanged by recreation. Status must remain targetless and no staging point may
be created. Configure and prove a real target promptly; until then, all
Vaultwarden data has no repository recovery point.

## Normal target-backed startup

Fill every repository, both independent role files, and every Restic password,
then run both configuration validations. Initialize each dedicated, known-empty
repository once:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo init --target primary

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo init --target b2
```

Do not initialize an unknown or nonempty prefix. Start production and wait for
all services to be healthy:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  up --detach vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  ps vaultwarden backup proxy
```

Create and read representative data through an official client, request a
backup, and confirm each target independently before relying on it.

## Existing SQLite data adoption

Existing-data adoption is an offline operator file operation. There is no
project import command or state machine. Only a stopped SQLite Vaultwarden is
supported.

1. Stop the source Vaultwarden completely and retain the original directory as
   the recovery copy.
2. Confirm destination `data` is absent or empty. Never merge trees.
3. Inventory the complete source, including `db.sqlite3`, any WAL/SHM files,
   RSA files, attachments, Sends, and `config.json` when present.
4. Reject every symlink and special file. Perform the copy as the configured
   runtime identity so the original source remains untouched.

A GNU-tooling preflight and copy for an already stopped source is:

```sh
vwless_source=/absolute/path/to/stopped-vaultwarden-data
vwless_destination="$vwless_root/data"
test "$vwless_source" != "$vwless_destination"
test -d "$vwless_source"
test -d "$vwless_destination"
test -z "$(find -P "$vwless_destination" -mindepth 1 -print -quit)"
test -z "$(find -P "$vwless_source" -mindepth 1 ! -type d ! -type f -print -quit)"
test -z "$(find -P "$vwless_source" -type f -links +1 -print -quit)"

vwless_source_rsa="$(sha256sum "$vwless_source/rsa_key.pem" | cut -d' ' -f1)"
cp -R --preserve=timestamps --no-preserve=mode,ownership -- \
  "$vwless_source/." "$vwless_destination/"
find -P "$vwless_destination" -type d -exec chmod 0700 -- {} +
find -P "$vwless_destination" -type f -exec chmod 0600 -- {} +
test "$(sha256sum "$vwless_destination/rsa_key.pem" | cut -d' ' -f1)" = "$vwless_source_rsa"
```

Generate and compare a filename-safe, sorted SHA-256 inventory of both trees,
including every regular file and the directory set. Keep that evidence outside
the source and destination. Confirm the source inventory did not change.

Use the current release's `compose.env`, run both configuration validations,
then start Vaultwarden. Vaultwarden itself performs any database migration.
After all three services are healthy, use a fresh official-client login to read
representative items, attachments, and file Sends. The RSA digest must remain
the source digest. Continuity of an old session is not an acceptance
requirement.

## Status, backup demand, and target catch-up

Read the resident process state and request one debounced check:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  exec --no-TTY backup /bin/vaultwarden-less-backup status

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  exec --no-TTY backup /bin/vaultwarden-less-backup request-backup
```

Acceptance means only that demand entered the in-memory queue. The worker uses
a 30-second quiet window, a two-minute maximum quiet window, a five-minute
minimum interval between check starts, a five-minute target retry, a 12-hour
fallback check, and a 30-minute continuous-failure degradation threshold.
Process status and notifications are not durable evidence; repository
snapshots are.

With multiple targets, `partial` means at least one target succeeded and at
least one failed. Repair only the failed endpoint or credential, make no new
Vaultwarden write, request another check, and require the failed target to
catch up to the same `content_id` without replacing an already valid snapshot
on the healthy target. Run check and restore against each target separately.

## Repository maintenance

Use the maintenance profile and its separate credentials. List bounded newest
snapshots and check repository metadata:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo snapshots --target primary --limit 100

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo check --target primary
```

Unlock only after proving no backup, restore, prune, or other Restic process is
active:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo unlock --target primary
```

Prune defaults to a dry run. Review every full snapshot ID before explicitly
applying the fixed retention policy, then check again:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo prune --target primary

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo prune --target primary --apply

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  repo check --target primary
```

Provider lifecycle rules must not expire live Restic objects independently.
Object lock can make unlock or prune fail. Repository-format migration is not a
project command.

## Verified restore

Restore latest or one explicit full snapshot ID into a previously absent name
under `restore/`:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  restore --target primary --snapshot latest --output recovery-primary

docker compose --env-file "$vwless_env" --file "$vwless_compose" \
  --profile maintenance run --rm --no-deps maintenance \
  restore --target primary --snapshot <64-lowercase-hex-id> --output recovery-earlier
```

Restore verifies repository identity tags, the manifest, exact file closure,
hashes, SQLite integrity and schema, database references, and content/archive
identities before an atomic no-replace publication. It never falls back from a
failed latest point or writes production `data`. After failure, the requested
output remains absent; preserve the semantic error, run `repo check`, and use a
different output name for any separately authorized attempt.

## Isolated restore exercise

Never let Vaultwarden modify the verified restore output used as evidence.
Create a disposable deployment root with a different loopback port, copy the
verified recovery tree into its empty `data`, and remove `manifest.json` only
from that disposable copy. Populate the other inputs from the same release as
an explicit targetless deployment and keep signup disabled.

Use a unique Compose project and the one shipped `compose.yml`:

```sh
vwless_drill_root=/srv/vaultwarden-less-drills/<unique-id>
vwless_drill_env="$vwless_drill_root/compose.env"
docker compose --project-name vaultwarden-less-drill-<unique-id> \
  --env-file "$vwless_drill_env" --file "$vwless_compose" \
  up --detach vaultwarden backup proxy
```

Route a temporary trusted HTTPS origin to that drill's distinct loopback port.
An old authenticated session should fail because recovery points exclude RSA.
Use a fresh official-client login and verify representative folders, items,
attachments, and file Sends. Record the newly generated RSA identity and
service health.

Remove only the unique disposable project, then verify it has no remaining
containers, networks, or volumes before deleting its exact root:

```sh
docker compose --project-name vaultwarden-less-drill-<unique-id> \
  --env-file "$vwless_drill_env" --file "$vwless_compose" down
```

The original verified restore output remains unchanged.

## Authorized production recovery

This is a destructive real-data operation and is never implied by a restore
command. Proceed only with authorization for the exact incident after the
selected restore passed the isolated exercise and the current data tree has a
separate verified recovery copy.

1. Record the active image identities, selected snapshot/manifest identities,
   current data inventory, and reversal paths in a protected incident record.
2. Stop new writes, then the worker, then Vaultwarden:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop backup
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop vaultwarden
test -z "$(docker compose --env-file "$vwless_env" --file "$vwless_compose" ps --status running --quiet vaultwarden backup proxy)"
```

3. On one filesystem, atomically rename current `data` to a previously absent
   incident path. With GNU `mv --no-clobber`, verify both postconditions so a
   racing destination cannot become a silent success:

```sh
vwless_previous="$vwless_root/data.pre-recovery-<unique-id>"
test ! -e "$vwless_previous"
test "$(stat -c %d "$vwless_root/data")" = "$(stat -c %d "$vwless_root")"
mv --no-clobber -T -- "$vwless_root/data" "$vwless_previous"
test ! -e "$vwless_root/data"
test -d "$vwless_previous"
```

4. Create a new `0700` working `data`, copy the verified restore into it without
   modifying the source, and remove only the copied `manifest.json`. Normalize
   copied directories to `0700` and files to `0600` under the runtime identity.
5. Start the same release. Require three healthy services, a newly generated
   RSA identity, failure of old sessions, and fresh official-client reads.
6. Immediately create, list, check, restore, and exercise a new recovery point
   before accepting the incident result.

If startup or client verification fails, stop all three services, atomically
rename failed working data to a new absent `data.failed-<unique-id>` path, then
rename the untouched previous tree back to absent `data`. Verify both
postconditions after each no-clobber rename. Never overwrite either tree and do
not remove incident material as part of recovery.

## Working-copy release transition and rollback

A release transition is an operator-owned offline working-copy procedure, not
project state and not a release gate. Retain both release directories, the
complete original A data, A's `compose.env`, and a tested rollback path.

Before stopping A:

1. transfer, checksum, and load B;
2. prepare a private B `compose.env` containing B's exact `git-<full-commit>`
   tag and the same reviewed root, runtime IDs, and port;
3. render B Compose and run both B configuration checks; and
4. record A/B full identities and exact absent destination names outside the
   deployment tree.

Then:

```sh
# Stop A in writer-to-reader order.
docker compose --env-file "$vwless_env" --file <A-release>/compose.yml stop proxy
docker compose --env-file "$vwless_env" --file <A-release>/compose.yml stop backup
docker compose --env-file "$vwless_env" --file <A-release>/compose.yml stop vaultwarden

vwless_original="$vwless_root/data.pre-<full-B-commit>"
test ! -e "$vwless_original"
test "$(stat -c %d "$vwless_root/data")" = "$(stat -c %d "$vwless_root")"
mv --no-clobber -T -- "$vwless_root/data" "$vwless_original"
test ! -e "$vwless_root/data"
test -d "$vwless_original"

# Copy the unmigrated A tree into a new B working directory.
install -d -m 0700 -- "$vwless_root/data"
cp -R --preserve=timestamps --no-preserve=mode,ownership -- \
  "$vwless_original/." "$vwless_root/data/"
find -P "$vwless_root/data" -type d -exec chmod 0700 -- {} +
find -P "$vwless_root/data" -type f -exec chmod 0600 -- {} +
```

Atomically install the already reviewed B `compose.env`. Start only B
Vaultwarden first and wait for its health check; only that working copy may be
opened or migrated by B. Then start B backup and proxy, route the trusted HTTPS
edge normally, and use a fresh official-client login to inspect representative
data. Targetless B is permitted but must emit its warning.

Repository checks, a B restore exercise, and broader compatibility testing are
useful evidence but are not automatic release prerequisites. Accept only after
the operator's own A/B criteria pass. Keep A and `data.pre-<full-B-commit>`.

To roll back, stop B in the same order. Rename B working data to a previously
absent `data.failed-<unique-id>` path, verify `data` is absent, then atomically
rename the untouched A directory back to `data` without replacement. Restore
A's recorded `compose.env` atomically and start only images from A's retained
release. If any destination exists, stop and investigate; never delete it to
continue.

## Shutdown and incident boundary

Normal shutdown removes only this Compose project's containers and networks;
bind-mounted data and private inputs remain:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" down
```

The product creates verified restore directories but never promotes them into
production. It does not administer provider data, delete old working copies,
or decide when incident or transition evidence may be destroyed. Those are
separate, explicitly authorized operator actions with their own recovery path.
