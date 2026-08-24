# Routine operations

## Parallel fresh-instance rehearsal before migration

Keep the old instance running. Prepare a different deployment directory, a
different Compose project name, an empty `data` directory, a separate trial
`DOMAIN`, and an unused loopback `LISTEN_PORT` such as `18080`. Never point the
trial at the old `data`, state, cache, credentials, or production repository.

1. Complete every filesystem, YAML, and credential check in
   [configuration](configuration.md). Use a new disposable S3/B2 repository
   prefix and initialize it with the trial maintenance role.
2. Keep the trial front-proxy route private. Set `SIGNUPS_ALLOWED=true` only in
   the trial `.env.vaultwarden`, then start it with an explicit project name:

   ```sh
   docker compose -p vaultwarden-less-trial up --build -d
   ```

3. Connect an official client through the trial TLS name, register one
   designated test account, sign in on a second client, and verify bidirectional
   sync. Add a synthetic vault item and a synthetic attachment; do not copy
   production secrets into the trial.
4. Return `SIGNUPS_ALLOWED=false`, recreate Vaultwarden, and verify the running
   value as described below. Confirm another registration is rejected.
5. Wait for every configured trial target to show a `complete` content ID.
   Exercise a deliberate second mutation and confirm a newer point appears.
6. Run `repo check`, prepare an isolated restore, and start a separate rescue
   copy from the restored data. Verify health, login, sync, and attachment
   download against that rescue copy.
7. Record the image versions, snapshot ID, content ID, test results, and all
   configuration corrections. Retire the synthetic account/repository only
   according to the test-data retention policy.

Only after this clean path works should you rehearse migration using a copy of
the old instance. The v1 tool deliberately has no legacy SQL-import command.
A pure SQL dump is not a complete Vaultwarden recovery inventory: preserve the
old instance's `rsa_key*` and any local `config.json`/templates independently,
even when there are no attachments or file Sends. Never copy a live SQLite main
file without its database-aware backup procedure.

## Rehearse a legacy SQL migration without stopping the old instance

This is an operator procedure, not a supported v1 input format. Keep every
source and output separate from both live deployments.

1. Record the old Vaultwarden/image version and its exact environment. Produce
   a new database-aware SQL export using the old deployment's known-good
   procedure. Copy its stable `rsa_key*`, local `config.json`, templates, and
   any attachment/Send directories into a protected migration inventory. Do
   not treat “currently unused” as permission to omit RSA or configuration.
2. Create a new empty `0700` rehearsal data directory. Import the SQL into a
   newly created SQLite database there using the old deployment's documented
   restore toolchain. Do not import over a database Vaultwarden has already
   opened, and do not point any command at live `/data`.
3. Populate only the allowlisted static assets in that rehearsal tree, set
   directories/files to `0700`/`0600`, and ensure copied database WAL/SHM files
   are absent. Keep the original export and old live data unchanged.
4. Start Vaultwarden `1.37.2` against this copy under a third isolated Compose
   project and TLS name. Its forward migration is allowed only on the copy.
   Verify health, old-account login, full sync, organization access, and all
   expected attachment downloads. Compare account/organization/item counts
   with the old instance using application-level observations that do not
   expose secrets.
5. Enable the new v1 backup worker for the migrated rehearsal, require a
   complete point on every target, and restore it into another isolated rescue
   copy. Repeat login/sync/attachment checks against that restored copy.
6. Write down every command, version, duration, ownership correction, and
   validation result. A final cutover should repeat this proven sequence with a
   new final export during a short old-instance write freeze; it should not
   improvise against the only copy.

At cutover, close old ingress and stop old writers, take and verify the final
export/rescue copy, repeat the staged import, start the new stack, and validate
before changing the front-proxy route. Keep the old tree and compatible old
image recoverable until the new stack has produced and independently restored
a complete v1 point on every target. If rollback is required, stop the new
writers and return to the untouched old database; never run the old binary
against a copy already migrated by Vaultwarden 1.37.2.

## First account without leaving registration open

The tracked Vaultwarden template has registration disabled, Web Vault
disabled, and the project port bound to host loopback. Use this one-time flow:

1. Keep the public front-proxy/firewall route closed. Reach the host loopback
   endpoint through an authenticated administrative channel such as an SSH
   tunnel and a local TLS front proxy whose URL matches `DOMAIN`.
2. Change only `SIGNUPS_ALLOWED=false` to `SIGNUPS_ALLOWED=true` in
   `.env.vaultwarden`.
3. Recreate Vaultwarden so the environment actually changes:

   ```sh
   docker compose up -d --force-recreate vaultwarden
   ```

4. Create exactly the intended first account with an official client that is
   configured for this self-hosted server.
5. Immediately restore `SIGNUPS_ALLOWED=false` and recreate Vaultwarden again.
6. Verify the running value without printing the rest of the container
   environment:

   ```sh
   docker inspect "$(docker compose ps -q vaultwarden)" \
     --format '{{range .Config.Env}}{{println .}}{{end}}' \
     | grep '^SIGNUPS_ALLOWED=false$'
   ```

7. Test login and sync, then reopen the normal public TLS route if required.

Do not use an obscurity-only path as an authentication control. Never set
`DISABLE_ADMIN_TOKEN=true`; with no `ADMIN_TOKEN`, the admin page remains
disabled.

## Observe health and backup state

Container process health:

```sh
docker compose ps
```

Stable target and scheduling state:

```sh
docker compose exec backup vaultwarden-less-backup \
  status --config /config/backup.yaml
```

Useful fields are:

- `dirty`: another sample is required;
- `backup_in_progress`: local construction or target work is active;
- `pending_recovery_point`: the newest verified local set retained for a
  lagging target;
- target `state`, `content_id`, `last_success_at`, `next_retry_at`, and safe
  `error_code`.

Remote repository failure is intentionally not a container-health failure.
Restart loops do not repair credentials, permissions, capacity, TLS, or a
backend outage and can obscure the persisted retry state.

## Scheduling semantics

The proxy emits one coalesced event after successful mutating responses. The
worker debounces 30 seconds, limits continuous change waiting to two minutes
once the five-minute minimum snapshot interval permits, and samples every 15
minutes even if no event arrives. Startup samples immediately.

An event during a backup asks for one further sample. It does not retain every
HTTP request. The 15-minute interval bounds local detection/start, not remote
upload completion.

To request an operator-controlled sample immediately:

```sh
docker compose exec backup vaultwarden-less-backup \
  run --config /config/backup.yaml
```

`run` uses the same single-instance lock and fails while `serve` owns it. Stop
the long-running backup service first if an isolated one-shot run is required.

## Partial target failure

Targets are independent and execute concurrently. A healthy repository can
accept the new point while another target enters exponential retry. Only the
latest verified point is retained locally for catch-up. The retained old point
is not displaced until the new set has passed database, file, manifest, and
space checks.

When the next local sample becomes due, an unfinished retryable upload is
terminated and retried later against the new latest verified point. This keeps
a slow remote from blocking the 15-minute local sampling boundary; it does not
promise that the remote upload itself completes within 15 minutes.

When any target accepts a content ID for the first time, every configured
notifier receives one persisted summary. A target's first degradation and its
later catch-up are separate replaceable state notifications. Notifier outages
never alter Restic success and never stop later samples.

## Local capacity

The worker checks staging free space before database and file work. A degraded
target can require one full pending recovery set in `./staging`; Restic caches
consume `./cache` independently. Monitor both filesystems. A capacity error
does not delete the previous verified set or online data.

After every target catches up, the worker atomically retires and deletes the
local staged set. It never deletes old `git-backup`, legacy SQL files, or old
Restic snapshots.

## Regular operator checks

- Daily: inspect `status` and run the repository structure check described in
  [maintenance](maintenance.md).
- After material Vaultwarden use: exercise login/sync and download a known
  synthetic/test attachment where operational policy permits.
- Before maintenance or upgrades: require all targets to show the same newest
  `complete` content ID and complete an isolated restore.
- Quarterly: perform the full cutover rehearsal in [restore](restore.md) on a
  disposable rescue copy.

Logs use target/notifier names and stable error codes. Do not increase logging
by wrapping Restic in a shell that prints its environment or full repository
URL.
