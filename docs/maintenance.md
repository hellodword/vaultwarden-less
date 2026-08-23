# Restic repository maintenance

The long-running worker intentionally runs only `restic backup`. Repository
creation, checking, unlocking, retention, pruning, and restoration are explicit
operator commands using `maintenance_credentials`.

Stop or schedule around normal backup activity before commands that need an
exclusive repository lock. Never expose maintenance credentials to the proxy,
Vaultwarden, or the long-running append-only backup identity.

## Inspect v1 snapshots

```sh
docker compose --profile maintenance run --rm maintenance \
  repo snapshots --target primary --status all \
  --config /config/backup.json
```

`--status` is `all`, `complete`, or `incomplete`. Results are filtered by v1
host, stable paths, and tag before being returned.

## Initialize once

Only for a confirmed new, empty repository:

```sh
docker compose --profile maintenance run --rm maintenance \
  repo init --target primary --config /config/backup.json
```

The worker never interprets “repository not found” as permission to initialize
it. Pointing a configuration at the wrong URL therefore fails rather than
silently creating a new history.

## Checks

Daily structure and index check:

```sh
docker compose --profile maintenance run --rm maintenance \
  repo check --target primary --config /config/backup.json
```

Once a month, rotate through one twelfth of repository data. Use `1/12` in
January, `2/12` in February, and so on, or maintain the same rotation independent
of calendar month:

```sh
docker compose --profile maintenance run --rm maintenance \
  repo check --target primary --read-data-subset 1/12 \
  --config /config/backup.json
```

Restic documents the distinction between repository structure checks and
reading repository data in [Working with
repositories](https://restic.readthedocs.io/en/stable/045_working_with_repos.html#checking-integrity-and-consistency).

## Unlock

Use only after proving no backup, maintenance, or restore process still owns
the repository lock:

```sh
docker compose --profile maintenance run --rm maintenance \
  repo unlock --target primary --config /config/backup.json
```

An ordinary network timeout is not proof of a stale lock.

## Retention and prune

The policy is applied independently to `complete` and `incomplete` v1 groups:

- every snapshot within the most recent 24 hours;
- 48 hourly, 14 daily, 8 weekly, 12 monthly, and 3 yearly points.

Every `forget` invocation filters:

- host `vaultwarden-less-v1`;
- both stable v1 snapshot paths;
- tags `vaultwarden-less-v1` and exactly one integrity status;
- retention grouping by host and paths after the integrity-tag filter.

Before either dry-run or apply, the CLI lists the candidate integrity group and
refuses maintenance if any matching snapshot has additional/malformed tags,
paths, host metadata, or IDs.

It cannot select an old SQL snapshot, a different host/path, or another
application's tag through this command.

Review the default dry-run every week:

```sh
docker compose --profile maintenance run --rm maintenance \
  repo prune --target primary --config /config/backup.json
```

The JSON output contains Restic's dry-run documents for both integrity groups.
Only after reviewing them and confirming a current isolated restore, apply:

```sh
docker compose --profile maintenance run --rm maintenance \
  repo prune --target primary --apply --config /config/backup.json
```

`--apply` is the only path in this CLI that turns the retention dry-run into
`forget --prune`. It is explicit authorization for that configured target and
v1 filter only.

## Recommended calendar

| Frequency | Action |
| --- | --- |
| Daily | structure `repo check`; inspect worker status |
| Weekly | review and, after approval, apply v1 retention/prune |
| Monthly | rotate `--read-data-subset n/12` |
| Quarterly | isolated restore and application-level rescue-copy drill |

Repository checks and retention are not restore tests. A quarterly drill must
continue through manifest, SQLite, referenced-file validation and a disposable
Vaultwarden login/sync/attachment check.

## Restic upgrades

Restic self-update is not included and must not be enabled. Before changing the
binary, use the current binary to check every repository and complete a restore.
Then test the candidate against the existing repositories and repeat restore.

A Restic repository-format migration requires a full check, recovery drill,
separate verified backup/rollback plan, and an explicit upstream maintenance
command. `vaultwarden-less` never migrates repository format automatically.
