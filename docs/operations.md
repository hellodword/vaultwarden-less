# Operations and recovery

These procedures run on an ordinary Linux/amd64 deployment host with Git,
Docker Engine, and the Docker Compose plugin. Nix is needed only to build or
verify source and create releases. Run commands from the exact deployment
clone; Compose bind paths are relative to it.

Never run `git clean -x` or `git clean -X` in a deployment clone. Do not delete
or overwrite `data`, `restore`, `state`, credentials, or a repository merely to
make a command pass.

## Release transfer and startup

A release directory must contain exactly `images.tar.gz` and `SHA256SUMS` from
one clean commit. Verify and load it before changing the running stack:

```sh
(cd <release-directory> && sha256sum --check SHA256SUMS)
gzip --decompress --stdout <release-directory>/images.tar.gz | docker image load
```

The archive contains local and immutable references for the three production
images and no test image. A successful load proves transferred image bytes and
labels, not configuration or repository access.

Validate both isolated roles:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-maintenance
```

For one new deployment only, while `data` is completely empty and no import
or upgrade is current, bind it to the exact loaded immutable release:

```sh
docker compose --env-file .env --file compose.yml --profile deployment run --rm --no-deps deployment-admin deployment init
```

The result is `initialized` or, for the exact same already-recorded identity,
`already_initialized`. Any other existing, damaged, or mismatched active state
is a hard stop; do not synthesize or edit `state/deployment/active.json`.

Initialize only a known-empty, dedicated repository target, once:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo init --target primary
```

Start the exact production services after both checks succeed:

```sh
docker compose --env-file .env --file compose.yml up --detach vaultwarden backup proxy
docker compose --env-file .env --file compose.yml ps
docker compose --env-file .env --file compose.yml logs --tail 200 vaultwarden backup proxy
```

Only the proxy is bound to loopback. A trusted TLS edge must present the
configured public `DOMAIN`, remove untrusted forwarding headers, and send
root-path HTTP and WebSocket traffic to the proxy. The worker API and direct
Vaultwarden endpoint remain on internal networks.

`down` stops and removes only the normal Compose project; bind-mounted data and
private inputs remain:

```sh
docker compose --env-file .env --file compose.yml down
```

## Status and backup demand

Read resident process status:

```sh
docker compose --env-file .env --file compose.yml exec --no-TTY backup /bin/vaultwarden-less-backup status
```

Request one debounced check:

```sh
docker compose --env-file .env --file compose.yml exec --no-TTY backup /bin/vaultwarden-less-backup request-backup
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
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo snapshots --target primary --limit 100
```

Use the full 64-character snapshot ID returned here for an explicit earlier
restore. Do not parse log messages as a snapshot contract.

## Repository maintenance

Run maintenance only after both role validation commands pass. The one-shot
maintenance container has repository egress but does not join application
networks.

List snapshots and check repository metadata:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo snapshots --target primary --limit 100
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo check --target primary
```

Unlock is for a confirmed stale Restic lock after proving no backup, restore,
prune, or other client is active:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo unlock --target primary
```

Prune always begins with a preview:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo prune --target primary
```

Review every retained and removed full snapshot ID. Only then apply the exact
fixed policy:

```sh
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo prune --target primary --apply
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo check --target primary
```

The fixed retention is within 24 hours, 48 hourly, 14 daily, 8 weekly, 12
monthly, and 3 yearly snapshots. Selection is limited to this product's exact
host, path, schema, project, content, and archive identity. Provider lifecycle
rules must not independently expire active Restic objects. Object lock may make
unlock or prune fail; see [Storage providers](providers.md).

## Verified restore

Restore latest to one previously absent output name:

```sh
docker compose --env-file .env --file compose.yml --profile restore run --rm --no-deps restore restore --target primary --snapshot latest --output drill
```

Restore an explicit earlier full snapshot ID to a different absent name:

```sh
docker compose --env-file .env --file compose.yml --profile restore run --rm --no-deps restore restore --target primary --snapshot <full-64-character-id> --output earlier
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

## Isolated recovery drill

The drill profile consumes exactly `restore/drill`, so create that verified
output first. Start the separate project and wait for health:

```sh
docker compose --project-name vaultwarden-less-drill --env-file .env --file compose.yml --profile drill up --detach --wait --wait-timeout 180 drill-vaultwarden
docker compose --project-name vaultwarden-less-drill --env-file .env --file compose.yml --profile drill ps
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
docker compose --project-name vaultwarden-less-drill --env-file .env --file compose.yml --profile drill down --volumes
```

The verified `restore/drill` tree remains. The production stack is a different
Compose project and is not changed by drill cleanup.

## Restart durability check

After the first successful point and after material deployment changes, prove
that durable repository state survives a complete service restart:

```sh
docker compose --env-file .env --file compose.yml stop proxy backup vaultwarden
docker compose --env-file .env --file compose.yml up --detach vaultwarden backup proxy
docker compose --env-file .env --file compose.yml exec --no-TTY backup /bin/vaultwarden-less-backup request-backup
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo snapshots --target primary --limit 100
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance repo check --target primary
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
4. Prove the primary succeeds, the secondary retries, and after 30 minutes the
   worker reports partial/degraded rather than complete success.
5. Restore secondary access without changing source data. The worker must
   catch that target up to the same latest `content_id`.
6. Run `repo check`, latest restore, and drill from each target. Do not accept
   equality based only on snapshot count or timestamp.

Targets execute serially, so a slow first target delays later targets. Catch-up
stores the newest verified point; it does not synthesize every missed
intermediate state.

## Cold deployment acceptance

Before relying on a new deployment, use an ordinary isolated Linux/amd64 host
with only Git, Docker Engine, and Compose for deployment operations:

1. clone the exact clean commit and verify the working tree;
2. create private copies and modes exactly as in the README;
3. checksum and standard-load the two-file archive;
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
