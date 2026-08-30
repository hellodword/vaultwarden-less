# Upgrades and explicit rollback

An upgrade is an audited A/B state machine. It is not an image-tag change,
`docker compose pull`, automatic migration, or best-effort restart. Immutable A
owns preparation, freeze evidence, and rollback. Immutable B owns migration
checking, repository evidence, drill verification, and acceptance. Until
acceptance, B's proxy and resident backup are fail-closed.

The exact persisted state and JSON shapes are normative in
[the product contracts](contracts.md). Do not edit `state/upgrade`, A/B image
labels, or `compose.env` by hand.

## Preconditions

Before `prepare`:

- A is an exact clean versionless release and its three production services are
  healthy; `state/deployment/active.json` exactly names A.
- No initial import is current; a prepared, verified, decided, or incomplete
  `state/import/current` must be cancelled before decision or completed before
  any upgrade preparation.
- A has recent verified snapshots on every configured upgrade-verification
  target, and a latest restore plus drill has passed.
- B is an exact clean versionless commit with a separately generated and checked
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

The B Vaultwarden entry runs only in `upgrade-candidate` mode. Before it opens
the active database, the gate proves that the image is the B named by current
state, active identity still names A, and frozen rollback evidence exists.
Ordinary `normal` mode cannot be substituted for this transition.

Framework acceptance needs different clean product commits and immutable tags.
It is complete orchestration evidence for that exact managed release pair even
when both releases contain the same actual Vaultwarden version; that outcome
does not leave a compatibility gap. Exact-pair cross-version compatibility is
a separate claim available only when the actual Vaultwarden versions differ
and the complete scenario runs with both. Neither claim covers substituted or
independently upgraded components outside the project-managed set.

## Production command surface

The production interface is the raw Docker Compose and product-CLI sequence
below. The host needs no Python, Nix, Just, or Go. Run every command from the
exact clean B checkout, and set this shell value from the reviewed absolute
deployment root without sourcing `.env`:

```sh
vwless_upgrade_env='<exact DEPLOYMENT_ROOT>/state/upgrade/current/compose.env'
test -f "$vwless_upgrade_env"
```

Every pre-terminal phase is retried only by repeating that same product
command. Once `decision.json` exists, only the matching accept or rollback
sequence may continue. A host restart never authorizes or infers another
phase.

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
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade run --rm --no-deps upgrade-a upgrade freeze
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
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade up --detach --no-deps --wait --wait-timeout 180 upgrade-vaultwarden-b
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade run --rm --no-deps upgrade-b upgrade migrate-check
```

B migrates the stopped tree. `migrate-check` captures and compares the exact
audited migration and complete SQLite schema; a healthy HTTP process alone is
insufficient. No B proxy or resident backup may serve yet.

## 4. Verify B repositories and recovery

Stop active B, ask immutable B to create/restore its evidence, and start only
the isolated B drill:

```sh
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade stop proxy backup vaultwarden upgrade-vaultwarden-b
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade run --rm --no-deps upgrade-b upgrade verify
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile drill up --detach --wait --wait-timeout 180 drill-vaultwarden
```

Verification must succeed on every configured upgrade target. B creates a
versionless recovery point, checks its immutable identities, performs real Restic
restores, validates the rollback evidence, and constructs the drill from B
data. With a normal Bitwarden client and fresh sessions, verify representative
items, organization/collection data, attachments, Sends, and expected session
invalidation. Record the full evidence before deciding.

If migration, target verification, restore, or drill fails, do not expose B.
Fix only a clearly idempotent external cause and rerun the same phase, or take
the explicit rollback path below.

## 5A. Accept B

Acceptance is an explicit terminal decision. It requires healthy B drill
evidence and changes serving ownership to B. Set the expected identity from
the reviewed prepare document and release archive, then run this resumable
sequence:

```sh
vwless_b_commit='<B-full-commit>'
vwless_b_release='<B-immutable-release>'
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill run --rm --no-deps upgrade-b upgrade accept
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml up --detach --wait --wait-timeout 180 vaultwarden backup proxy
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml ps vaultwarden backup proxy
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml exec --no-TTY backup /bin/vaultwarden-less-backup version
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml exec --no-TTY proxy /bin/vaultwarden-less-proxy version
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml exec --no-TTY vaultwarden /bin/vaultwarden --version
for vwless_product in backup proxy vaultwarden; do
  vwless_image="vaultwarden-less-$vwless_product:$vwless_b_release"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_image")" = "$vwless_b_commit"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_image")" = "$vwless_b_release"
done
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile drill stop drill-vaultwarden
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile drill rm --force --stop drill-vaultwarden drill-copy
vwless_drill_volume="$(docker volume ls --quiet --filter label=com.docker.compose.project=vaultwarden-less --filter label=com.docker.compose.volume=drill-data)"
test -z "$vwless_drill_volume" || test "$vwless_drill_volume" = vaultwarden-less_drill-data
if test -n "$vwless_drill_volume"; then
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$vwless_drill_volume")" = vaultwarden-less
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.volume"}}' "$vwless_drill_volume")" = drill-data
  docker volume rm -- "$vwless_drill_volume"
fi
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill run --rm --no-deps upgrade-b upgrade cleanup-drill
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill run --rm --no-deps upgrade-b upgrade finalize
```

The first command creates or resumes the immutable accept decision and active B
identity. Before cleanup, require all three `ps` entries to be healthy, require
the three in-container versions and immutable OCI labels to name the reviewed B
release, and complete a normal HTTPS client check. If interrupted before
finalize, repeat the sequence from its first line: the Go command can continue
only accept and cannot exchange or decide twice. Finalize validates only
durable decision, active, frozen-data, and cleanup evidence before archiving
current state to history; it does not infer runtime health. After finalize the
current `compose.env` is intentionally gone, so there is no host-level
`already_complete` command. Once accept is decided, rollback is permanently
rejected. Returning to an older product requires a new audited forward upgrade;
never rewrite history or exchange data manually.

Immediately check B status, repository snapshots, and representative client
operations. Retain A's immutable images and accepted evidence according to
local incident policy; removing them is not part of acceptance.

## 5B. Explicit rollback before accept

Rollback is never automatic. It is owned by immutable A and may run only before
accept. Set the expected A identity from the reviewed prepare document, then
stop transient B/drill services, create or resume the rollback decision and
tree-ID-driven exchange, verify and retag only A's three immutable images,
start ordinary A, clean the exact drill resources, and finalize:
The one-shot A role is networkless, non-root, capability-free, and uses one
deployment-root bind so both exchange directories remain beneath the same
mount; split `data`/`state` binds would make Linux reject the atomic exchange.

```sh
vwless_a_commit='<A-full-commit>'
vwless_a_release='<A-immutable-release>'
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill stop proxy backup vaultwarden upgrade-vaultwarden-b drill-vaultwarden
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill rm --force --stop proxy backup vaultwarden upgrade-vaultwarden-b drill-vaultwarden drill-copy
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill run --rm --no-deps rollback-a rollback apply
for vwless_product in backup proxy vaultwarden; do
  vwless_immutable="vaultwarden-less-$vwless_product:$vwless_a_release"
  vwless_local="vaultwarden-less-$vwless_product:local"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_immutable")" = "$vwless_a_commit"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_immutable")" = "$vwless_a_release"
  docker image tag "$vwless_immutable" "$vwless_local"
  test "$(docker image inspect --format '{{.Id}}' "$vwless_local")" = "$(docker image inspect --format '{{.Id}}' "$vwless_immutable")"
done
docker compose --env-file .env --file compose.yml up --detach --wait --wait-timeout 180 vaultwarden backup proxy
docker compose --env-file .env --file compose.yml ps vaultwarden backup proxy
docker compose --env-file .env --file compose.yml exec --no-TTY backup /bin/vaultwarden-less-backup version
docker compose --env-file .env --file compose.yml exec --no-TTY proxy /bin/vaultwarden-less-proxy version
docker compose --env-file .env --file compose.yml exec --no-TTY vaultwarden /bin/vaultwarden --version
vwless_drill_volume="$(docker volume ls --quiet --filter label=com.docker.compose.project=vaultwarden-less --filter label=com.docker.compose.volume=drill-data)"
test -z "$vwless_drill_volume" || test "$vwless_drill_volume" = vaultwarden-less_drill-data
if test -n "$vwless_drill_volume"; then
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$vwless_drill_volume")" = vaultwarden-less
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.volume"}}' "$vwless_drill_volume")" = drill-data
  docker volume rm -- "$vwless_drill_volume"
fi
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill run --rm --no-deps rollback-a upgrade cleanup-drill
docker compose --env-file "$vwless_upgrade_env" --file compose.yml --file compose.upgrade.yml --profile upgrade --profile drill run --rm --no-deps rollback-a upgrade finalize
```

After rollback, verify A through the ordinary proxy, read worker status, list
snapshots, and complete a fresh restore/drill. Preserve quarantine for review.
If interrupted before finalize, repeat the same sequence; `rollback apply`
recognizes the frozen A tree on either side of the exchange and cannot create
an accept decision. After finalize, quarantine and the active A identity are
the durable result. Never delete quarantine to force another exchange.

## Failure and restart semantics

- `prepare` and `freeze` allow only exact recognized idempotent continuation;
  conflicting inputs fail before side effects.
- B migration, verification, and accept reject missing, duplicate, unknown,
  reordered, truncated, or tampered evidence.
- A rollback validates only A-owned canonical state and the framed rollback
  tree; future B schema cannot teach A how to authorize an exchange.
- A crash or host restart does not infer the next phase. Inspect the strict
  phase result and rerun only the matching supported command. A decision is
  immutable; active identity update, service startup, cleanup, and finalize are
  individually resumable after it.
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
7. the complete accept case interrupted immediately after its immutable
   decision, followed by the same raw-sequence resume, healthy B services,
   history, and retained immutable A images;
8. the deepest rollback case: B migration/evidence completes, the final
   drill/accept boundary is corrupted, exchange is interrupted before active A
   publication, then the same raw rollback sequence restores A
   data/RSA/service identity and quarantines B;
9. unit fault-matrix evidence for decision-before/after, active publication,
   exchange, service start, drill cleanup, finalize, conflict, tamper, and
   restart boundaries;
10. post-terminal repository, client data, history/quarantine, task-resource,
    and credential cleanup state.

Only claim exact-pair Vaultwarden cross-version compatibility when items 1–10
ran with two genuinely different versions reported by the corresponding
project images. If the versions are equal, record the distinct-version gate as
not applicable; the two cases still prove orchestration for that exact pair.
The same actual-version rule applies independently to Restic. A distinct
Restic pair additionally runs A format-2 init/write, B list/check/restore/write,
then A list/check/restore of B's point; an equal pair records that matrix as not
applicable. Neither outcome supports arbitrary Vaultwarden, Restic, Bitwarden
CLI, or other component builds outside the project-managed set, and no upgrade
path invokes repository-format migration.
