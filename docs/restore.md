# Isolated restore and manual cutover

The restore command never writes online `/data`. The Compose restore profile
does not mount it. Restic downloads into a private sibling temporary directory;
the tool verifies Restic's restored bytes, the v1 manifest/content ID, every
file size and SHA-256, SQLite integrity/foreign keys/schema, and all database
attachment/Send references. Only then is the temporary directory renamed to
the requested prepared path.

The output must be a clean absolute path that is absent or an empty real
directory, with a pre-existing non-symlink parent. It must not overlap
configured `data_dir` in either direction.

## Offline recovery inventory

Keep an independently protected, tested copy of all items below. They are not
inside Restic snapshots:

- repository address and backend type;
- Restic repository password;
- backend recovery/maintenance credentials and any private CA;
- `backup.json`, `proxy.json`, `.env.vaultwarden`, Compose/front-proxy/TLS
  configuration, and the intended `DOMAIN`;
- project, Vaultwarden, and Restic versions;
- host UID/GID and filesystem ownership expectations;
- notification configuration and credentials if alerts are needed during
  recovery.

Vaultwarden end-to-end encryption cannot recover a lost Restic password or
backend account.

## Prepare the newest complete point

Ensure `./restore` is mode `0700` and `./restore/prepared` is absent or empty:

```sh
docker compose --profile restore run --rm restore \
  restore --target primary --output /restore/prepared \
  --config /config/backup.json
```

Without `--snapshot`, only v1 snapshots tagged `complete` are considered and
the newest is selected. The successful JSON result records the target, exact
Restic snapshot ID, output, and verified manifest.

To select an exact full 64-character snapshot ID:

```sh
docker compose --profile restore run --rm restore \
  restore --target primary --snapshot SNAPSHOT_ID \
  --output /restore/prepared --config /config/backup.json
```

An exact incomplete point is rejected unless the operator also supplies
`--allow-incomplete`. Its manifest lists what is absent. Treat it as forensic
partial recovery, not a substitute for the most recent complete history.

## Quarterly rescue-copy drill

Do not replace production data during a drill.

1. Prepare and verify a complete point as above.
2. Copy `restore/prepared/data` into an isolated disposable directory or host.
3. Run the same Vaultwarden version (`1.37.2` for this format) with a copy of the
   independently managed configuration and with networking restricted.
4. Verify health, login, full client sync, an attachment download, and any
   expected local template behavior using a designated synthetic/test account
   and file. Do not use real exports as development fixtures.
5. Destroy the disposable environment according to local data-handling policy,
   while retaining the non-sensitive drill result and exact snapshot ID.

## Manual production cutover

Only perform this after explicit change approval, adequate free space, and a
successful isolated drill.

1. Stop Vaultwarden, proxy, and backup so no process can write `/data`:

   ```sh
   docker compose down --timeout 130
   ```

2. Create a separate rescue copy or recoverable rename of the current `data`
   directory on the same protected filesystem. Verify that recovery path before
   proceeding. Never overwrite the only live copy.
3. Create a new `data` directory from `restore/prepared/data`, preserving its
   `0700` directories and `0600` files. Do not merge it over the old tree;
   merging can retain excluded cache, temporary files, or stale database
   sidecars.
4. Confirm the newly prepared data contains no `db.sqlite3-wal` or
   `db.sqlite3-shm`. If a cutover procedure created or carried those files,
   remove only those two sidecars from the stopped, newly prepared copy—not
   from a running database and not from the rescue copy.
5. Restore ownership to the exact `PUID:PGID` recorded in `.env`. Verify the
   root is `0700` and recovery files are `0600` before starting.
6. Start Vaultwarden and proxy, leaving backup stopped during application
   validation:

   ```sh
   docker compose up -d vaultwarden proxy
   ```

7. Verify health, login, sync, attachment download, and final registration,
   Web Vault, WebSocket, Push, Sends, and admin settings.
8. Start backup, require a new complete point on every target, and perform a
   second isolated restore before retiring the rescue copy under the approved
   retention policy.

The tool intentionally provides no `--overwrite-data`, `--apply`, or automatic
cutover command.

## Rollback after a failed cutover

Stop all services again. Preserve the failed restored tree for diagnosis,
return the verified rescue copy to `data`, recheck ownership and WAL/SHM rules,
then start the exact application version compatible with that rescue database.

If the failed cutover followed a Vaultwarden upgrade, restore the complete
pre-upgrade snapshot and its compatible Vaultwarden version. Never run an old
Vaultwarden binary against a database already migrated by a newer version in
an attempt to downgrade it in place.
