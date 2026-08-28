# Upgrades and explicit rollback

An upgrade is an audited A/B state machine. It is not an image-tag change,
`docker compose pull`, automatic migration, or best-effort restart. Immutable A
owns preparation, freeze evidence, and rollback. Immutable B owns migration
checking, repository evidence, drill verification, and acceptance. Until
acceptance, B's proxy and resident backup are fail-closed.

The exact persisted state and JSON schemas are normative in
[the schema 1 contracts](contracts.md). Do not edit `state/upgrade`, A/B image
labels, or `compose.env` by hand.

## Preconditions

Before `prepare`:

- A is an exact clean schema-1 release and its three production services are
  healthy.
- A has recent verified snapshots on every configured upgrade-verification
  target, and a latest restore plus drill has passed.
- B is an exact clean schema-1 commit with a separately generated and checked
  two-file release.
- A and B immutable image references are available locally or their release
  archives are available for standard loading.
- `.env`, private configuration, credentials, directories, ownership, and
  modes pass both role checks.
- `data`, `state`, and the rollback parent are on the same filesystem with
  enough capacity for the frozen rollback tree.
- the operator has recorded the full A/B commits, immutable release names,
  repository evidence, expected Vaultwarden versions, maintenance window, and
  the decision owner for accept versus rollback.

Framework acceptance needs different clean product commits and immutable tags.
It is complete orchestration evidence for that exact managed release pair even
when both releases contain the same actual Vaultwarden version; that outcome
does not leave a compatibility gap. Exact-pair cross-version compatibility is
a separate claim available only when the actual Vaultwarden versions differ
and the complete scenario runs with both. Neither claim covers substituted or
independently upgraded components outside the project-managed set.

## Supported orchestrator

On a source/deployment host with the pinned Nix environment, use the recipes in
`Justfile`:

```sh
nix develop --command just upgrade-prepare <B-full-commit> <B-immutable-release>
nix develop --command just upgrade-freeze
nix develop --command just upgrade-start-b
nix develop --command just upgrade-migrate-check
nix develop --command just upgrade-verify
nix develop --command just upgrade-accept
```

If any pre-accept phase fails and the failure cannot be corrected by the same
idempotent phase, invoke only:

```sh
nix develop --command just upgrade-rollback
```

The recipes are thin, reviewable Compose orchestration. The following sections
state their order and give the equivalent archive-only commands for an ordinary
deployment host without Nix.

## 1. Prepare while A is serving

Run both role checks, then ask immutable A to create the one-time upgrade
identity. `B_COMMIT` is 40 lowercase hexadecimal characters and `B_RELEASE` is
the deterministic immutable release label supplied with B.

```sh
B_COMMIT=<B-full-commit>
B_RELEASE=<B-immutable-release>
test -n "$B_COMMIT"
test -n "$B_RELEASE"
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file .env --file compose.yml --profile maintenance run --rm --no-deps maintenance upgrade prepare --to-commit "$B_COMMIT" --to-release "$B_RELEASE"
```

Prepare binds the current full A commit/release, requested B commit/release,
deployment identity, exact canonical Compose environment, and upgrade ID. It
must happen **before** loading B, because the release archive updates the local
aliases. Running A containers continue to use their existing A image IDs, and
the generated state preserves A's immutable tags.

Now verify and standard-load B's exact release:

```sh
(cd <B-release-directory> && sha256sum --check SHA256SUMS)
gzip --decompress --stdout <B-release-directory>/images.tar.gz | docker image load
```

Do not continue if B's OCI revision/version labels do not match the requested
full commit and immutable release.

## 2. Freeze A

Freeze stops new ingress first, then the worker and Vaultwarden, and finally
runs the freeze command from A's immutable backup image:

```sh
docker compose --env-file .env --file compose.yml stop proxy
docker compose --env-file .env --file compose.yml stop backup vaultwarden
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile upgrade run --rm --no-deps upgrade-a upgrade freeze
```

Freeze verifies that writes are stopped, creates A-owned repository evidence,
stable-copies the rollback tree on the same filesystem, computes its framed
identity, and writes an immutable rollback ticket. It is safe to retry only
when the command recognizes the exact same completed phase. Do not start any A
or B proxy, worker, or Vaultwarden manually after this point.

## 3. Start only B Vaultwarden and audit migration

Validate B with both roles before starting it. Then start only B's Vaultwarden
and wait for health:

```sh
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml up --detach --no-deps --wait --wait-timeout 180 vaultwarden
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile upgrade run --rm --no-deps upgrade-b upgrade migrate-check
```

B migrates the stopped tree. `migrate-check` captures and compares the exact
audited migration and complete SQLite schema; a healthy HTTP process alone is
insufficient. No B proxy or resident backup may serve yet.

## 4. Verify B repositories and recovery

Stop active B, ask immutable B to create/restore its evidence, and start only
the isolated B drill:

```sh
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml stop proxy backup vaultwarden
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile upgrade run --rm --no-deps upgrade-b upgrade verify
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile drill up --detach --wait --wait-timeout 180 drill-vaultwarden
```

Verification must succeed on every configured upgrade target. B creates a
schema-1 recovery point, checks its immutable identities, performs real Restic
restores, validates the rollback evidence, and constructs the drill from B
data. With a normal Bitwarden client and fresh sessions, verify representative
items, organization/collection data, attachments, Sends, and expected session
invalidation. Record the full evidence before deciding.

If migration, target verification, restore, or drill fails, do not expose B.
Fix only a clearly idempotent external cause and rerun the same phase, or take
the explicit rollback path below.

## 5A. Accept B

Acceptance is an explicit terminal decision. It requires healthy B drill
evidence and changes serving ownership to B. On an archive-only host, mirror
the recipe exactly:

```sh
A_RELEASE="$(sed -n 's/^A_RELEASE=//p' state/upgrade/current/compose.env)"
B_RELEASE="$(sed -n 's/^B_RELEASE=//p' state/upgrade/current/compose.env)"
test -n "$A_RELEASE"
test -n "$B_RELEASE"
export A_RELEASE B_RELEASE
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill run --rm --no-deps upgrade-b upgrade accept
docker compose --env-file .env --file compose.yml --file compose.upgrade.yml up --detach vaultwarden backup proxy
docker compose --env-file .env --file compose.yml --file compose.upgrade.yml --profile drill stop drill-vaultwarden
docker compose --env-file .env --file compose.yml --file compose.upgrade.yml --profile drill rm --force drill-vaultwarden drill-copy
docker volume rm vaultwarden-less_drill-data
```

After accept, the immutable evidence moves to history, B services may serve,
and ordinary rollback for that upgrade is permanently rejected. To return to
an older product after acceptance, plan a new audited forward upgrade; do not
rewrite history or exchange data manually.

Immediately check B status, repository snapshots, and representative client
operations. Retain A's immutable images and accepted evidence according to
local incident policy; removing them is not part of acceptance.

## 5B. Explicit rollback before accept

Rollback is never automatic. It is owned by immutable A and may run only before
accept. It stops and removes B services, validates A's prepare/ticket and exact
rollback-tree identity without interpreting B's candidate document, atomically
exchanges the tree, quarantines B state, records rolled-back evidence, and
starts only A rollback services.

```sh
A_RELEASE="$(sed -n 's/^A_RELEASE=//p' state/upgrade/current/compose.env)"
B_RELEASE="$(sed -n 's/^B_RELEASE=//p' state/upgrade/current/compose.env)"
test -n "$A_RELEASE"
test -n "$B_RELEASE"
export A_RELEASE B_RELEASE
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile drill stop proxy backup vaultwarden drill-vaultwarden
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile drill rm --force proxy backup vaultwarden drill-vaultwarden drill-copy
if docker volume inspect vaultwarden-less_drill-data >/dev/null 2>&1; then docker volume rm vaultwarden-less_drill-data; fi
docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml --profile upgrade run --rm --no-deps rollback-a rollback apply
docker compose --env-file .env --file compose.yml --file compose.upgrade.yml --profile rollback up --detach rollback-vaultwarden-a rollback-backup-a rollback-proxy-a
```

After rollback, verify A through the rollback proxy, read worker status, list
snapshots, and complete a fresh restore/drill. Preserve quarantine and history
for review. A second rollback attempt returns a stable conflict; never delete
history to force it.

## Failure and restart semantics

- `prepare` and `freeze` allow only exact recognized idempotent continuation;
  conflicting inputs fail before side effects.
- B migration, verification, and accept reject missing, duplicate, unknown,
  reordered, truncated, or tampered evidence.
- A rollback validates only A-owned canonical state and the framed rollback
  tree; future B schema cannot teach A how to authorize an exchange.
- A crash or host restart does not infer the next phase. Inspect the strict
  phase result and rerun only the matching supported command.
- A failure leaves ingress stopped unless the named phase itself completed.
  Never start a proxy merely because Vaultwarden's healthcheck is green.
- Accept and rollback are mutually exclusive. There is no `--force`, hidden
  failure hook, automatic compatibility fallback, or legacy state path.

## A/B acceptance evidence

For every product upgrade, record:

1. full A/B commits, immutable release names, image IDs, OCI labels, and actual
   Vaultwarden versions;
2. checksum and standard image-load results for both releases;
3. prepare/freeze/migrate/verify/accept or rollback JSON and phase ordering;
4. migration and complete schema evidence;
5. repository snapshot, content, and archive identities for every verification
   target;
6. latest and explicit restore plus isolated drill results;
7. three forced pre-accept failures: migration permission, invalid target, and
   corrupted drill, each followed by explicit rollback;
8. restart/idempotency behavior at every phase;
9. post-terminal service, repository, history/quarantine, and resource state.

Only claim exact-pair Vaultwarden cross-version compatibility when items 1–9
ran with two genuinely different versions managed by the corresponding project
releases. Two commits that package the same Vaultwarden version completely
prove the orchestration framework for their exact pair without creating a
pending cross-version promise. Neither outcome supports arbitrary Vaultwarden,
Restic, Bitwarden CLI, or other component builds outside the project-managed
set.
