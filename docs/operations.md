# Routine operations

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
  status --config /config/backup.json
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
  run --config /config/backup.json
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
