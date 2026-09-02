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

- A is an exact clean managed release and its three production services are
  healthy; `state/deployment/active.json` exactly names A.
- No initial import is current; a prepared, verified, decided, or incomplete
  `state/import/current` must be cancelled before decision or completed before
  any upgrade preparation.
- A has recent verified snapshots on every configured upgrade-verification
  target, and a latest restore plus drill has passed.
- At least one backup target is configured. Targetless deployments must add and
  verify a target before `upgrade prepare`; the command otherwise returns
  `not_ready` before creating upgrade state.
- B is an exact clean managed commit with a separately generated, checksummed
  Nix release bundle.
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

The project release gate validates B without selecting or rebuilding A. The
operator owns exact-pair acceptance: retain A's immutable images, verify the
actual Vaultwarden and Restic versions, rehearse the required accept or rollback
path on disposable deployment copies, and record the evidence below. The
product does not read that external evidence or infer compatibility from a
successful B release.

## Production command surface

The production interface is the raw Docker Compose and product-CLI sequence
below. The host needs no Git, Python, Nix, Just, Go, or source checkout. Copy B's
complete release bundle to an immutable absolute directory, keep mutable and
private files below the separate deployment root, and set these shell values
without sourcing `.env`:

```sh
vwless_b_release_dir='<absolute B release directory>'
vwless_root='<exact DEPLOYMENT_ROOT>'
vwless_env="$vwless_root/.env"
vwless_compose="$vwless_b_release_dir/compose.yml"
vwless_upgrade_compose="$vwless_b_release_dir/compose.upgrade.yml"
vwless_upgrade_env="$vwless_root/state/upgrade/current/compose.env"
test -f "$vwless_env"
test -f "$vwless_compose"
test -f "$vwless_upgrade_compose"
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
B_COMMIT='<B-full-commit>'
B_RELEASE='<B-immutable-release>'
test -n "$B_COMMIT"
test -n "$B_RELEASE"
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file "$vwless_env" --file "$vwless_compose" --profile maintenance run --rm --no-deps maintenance upgrade prepare --to-commit "$B_COMMIT" --to-release "$B_RELEASE"
test -f "$vwless_upgrade_env"
```

Prepare binds the current full A commit/release, requested B commit/release,
deployment identity, exact canonical Compose environment, and upgrade ID. Run
it **before** loading B so the transition is anchored while only A is available.
The new release archives contain no local aliases, so loading them cannot
retarget the running A services. The generated state preserves A's immutable
tags.

Now verify and standard-load B's exact release:

```sh
(cd "$vwless_b_release_dir" && sha256sum --check SHA256SUMS)
docker image load --input "$vwless_b_release_dir/images/backup.tar.gz"
docker image load --input "$vwless_b_release_dir/images/proxy.tar.gz"
docker image load --input "$vwless_b_release_dir/images/vaultwarden.tar.gz"
for vwless_product in backup proxy vaultwarden; do
  vwless_image="vaultwarden-less-$vwless_product:$B_RELEASE"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_image")" = "$B_COMMIT"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_image")" = "$B_RELEASE"
done
```

Do not create mutable aliases or continue if B's OCI revision/version labels do
not match the requested full commit and immutable release.

## 2. Freeze A

Freeze stops new ingress first, then the worker and Vaultwarden, and finally
runs the freeze command from A's immutable backup image:

```sh
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" stop backup vaultwarden
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade run --rm --no-deps upgrade-a upgrade freeze
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
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile maintenance run --rm --no-deps config-check-backup
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile maintenance run --rm --no-deps config-check-maintenance
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade up --detach --no-deps --wait --wait-timeout 180 upgrade-vaultwarden-b
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade run --rm --no-deps upgrade-b upgrade migrate-check
```

B migrates the stopped tree. `migrate-check` captures and compares the exact
audited migration and complete SQLite schema; a healthy HTTP process alone is
insufficient. No B proxy or resident backup may serve yet.

## 4. Verify B repositories and recovery

Stop active B, ask immutable B to create/restore its evidence, and start only
the isolated B drill:

```sh
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade stop proxy backup vaultwarden upgrade-vaultwarden-b
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade run --rm --no-deps upgrade-b upgrade verify
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile drill up --detach --wait --wait-timeout 180 drill-vaultwarden
```

Verification must succeed on every configured upgrade target. B creates a
recovery point, checks its immutable identities, performs real Restic
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
vwless_a_release='<A-immutable-release>'
vwless_b_commit='<B-full-commit>'
vwless_b_release='<B-immutable-release>'
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill run --rm --no-deps upgrade-b upgrade accept

test "$(grep -c '^VWLESS_IMAGE_TAG=' "$vwless_env")" -eq 1
vwless_current_tag="$(sed -n 's/^VWLESS_IMAGE_TAG=//p' "$vwless_env")"
case "$vwless_current_tag" in
  "$vwless_a_release")
    umask 077
    vwless_env_tmp="$(mktemp "$vwless_root/.env.XXXXXX")"
    trap 'rm -f -- "$vwless_env_tmp"' EXIT HUP INT TERM
    sed "s/^VWLESS_IMAGE_TAG=.*/VWLESS_IMAGE_TAG=$vwless_b_release/" "$vwless_env" > "$vwless_env_tmp"
    chown --reference="$vwless_env" "$vwless_env_tmp"
    chmod 0600 "$vwless_env_tmp"
    sync "$vwless_env_tmp"
    mv -T -- "$vwless_env_tmp" "$vwless_env"
    sync -f "$vwless_root"
    trap - EXIT HUP INT TERM
    ;;
  "$vwless_b_release") ;;
  *) false ;;
esac
grep -Fx "VWLESS_IMAGE_TAG=$vwless_b_release" "$vwless_env"

docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach --wait --wait-timeout 180 vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" ps vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup version
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY proxy /bin/vaultwarden-less-proxy version
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY vaultwarden /bin/vaultwarden --version
for vwless_product in backup proxy vaultwarden; do
  vwless_image="vaultwarden-less-$vwless_product:$vwless_b_release"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_image")" = "$vwless_b_commit"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_image")" = "$vwless_b_release"
done
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile drill stop drill-vaultwarden
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile drill rm --force --stop drill-vaultwarden drill-copy
vwless_drill_volume="$(docker volume ls --quiet --filter label=com.docker.compose.project=vaultwarden-less --filter label=com.docker.compose.volume=drill-data)"
test -z "$vwless_drill_volume" || test "$vwless_drill_volume" = vaultwarden-less_drill-data
if test -n "$vwless_drill_volume"; then
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$vwless_drill_volume")" = vaultwarden-less
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.volume"}}' "$vwless_drill_volume")" = drill-data
  docker volume rm -- "$vwless_drill_volume"
fi
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill run --rm --no-deps upgrade-b upgrade cleanup-drill
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill run --rm --no-deps upgrade-b upgrade finalize
```

The first command creates or resumes the immutable accept decision and active B
identity. The operator-owned edit then atomically changes the ordinary
deployment environment from A's immutable tag to B's. It accepts only the
reviewed A or already-selected B value, preserves the rest of `.env`, and is
idempotent across an interruption. Run it as the owner of the private
deployment files. Before cleanup, require all three `ps` entries to be healthy,
require the three in-container versions and immutable OCI labels to name the
reviewed B release, and complete a normal HTTPS client check. If interrupted
before finalize, repeat the sequence from its first line: the Go command can
continue only accept and cannot exchange or decide twice. Finalize validates
only durable decision, active, frozen-data, and cleanup evidence before
archiving current state to history; it does not infer runtime health. After
finalize the current `compose.env` is intentionally gone, so there is no host-level
`already_complete` command. Once accept is decided, rollback is permanently
rejected. Returning to an older product requires a new audited forward upgrade;
never rewrite history or exchange data manually.

If the host fails after `upgrade accept` but before the `.env` replacement,
ordinary A startup fails closed because active identity already names B. Repeat
the accept sequence; it resumes the recorded decision and completes the same
A-to-B environment change.

Immediately check B status, repository snapshots, and representative client
operations. Retain A's immutable images and accepted evidence according to
local incident policy; removing them is not part of acceptance.

## 5B. Explicit rollback before accept

Rollback is never automatic. It is owned by immutable A and may run only before
accept. Set the expected A identity from the reviewed prepare document, then
stop transient B/drill services, create or resume the rollback decision and
tree-ID-driven exchange, verify A's three immutable images, confirm the ordinary
deployment environment still selects A, start ordinary A, clean the exact drill
resources, and finalize:
The one-shot A role is networkless, non-root, capability-free, and uses one
deployment-root bind so both exchange directories remain beneath the same
mount; split `data`/`state` binds would make Linux reject the atomic exchange.

```sh
vwless_a_commit='<A-full-commit>'
vwless_a_release='<A-immutable-release>'
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill stop proxy backup vaultwarden upgrade-vaultwarden-b drill-vaultwarden
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill rm --force --stop proxy backup vaultwarden upgrade-vaultwarden-b drill-vaultwarden drill-copy
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill run --rm --no-deps rollback-a rollback apply
for vwless_product in backup proxy vaultwarden; do
  vwless_immutable="vaultwarden-less-$vwless_product:$vwless_a_release"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$vwless_immutable")" = "$vwless_a_commit"
  test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$vwless_immutable")" = "$vwless_a_release"
done
test "$(grep -c '^VWLESS_IMAGE_TAG=' "$vwless_env")" -eq 1
grep -Fx "VWLESS_IMAGE_TAG=$vwless_a_release" "$vwless_env"
docker compose --env-file "$vwless_env" --file "$vwless_compose" up --detach --wait --wait-timeout 180 vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" ps vaultwarden backup proxy
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY backup /bin/vaultwarden-less-backup version
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY proxy /bin/vaultwarden-less-proxy version
docker compose --env-file "$vwless_env" --file "$vwless_compose" exec --no-TTY vaultwarden /bin/vaultwarden --version
vwless_drill_volume="$(docker volume ls --quiet --filter label=com.docker.compose.project=vaultwarden-less --filter label=com.docker.compose.volume=drill-data)"
test -z "$vwless_drill_volume" || test "$vwless_drill_volume" = vaultwarden-less_drill-data
if test -n "$vwless_drill_volume"; then
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$vwless_drill_volume")" = vaultwarden-less
  test "$(docker volume inspect --format '{{index .Labels "com.docker.compose.volume"}}' "$vwless_drill_volume")" = drill-data
  docker volume rm -- "$vwless_drill_volume"
fi
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill run --rm --no-deps rollback-a upgrade cleanup-drill
docker compose --env-file "$vwless_upgrade_env" --file "$vwless_compose" --file "$vwless_upgrade_compose" --profile upgrade --profile drill run --rm --no-deps rollback-a upgrade finalize
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
  failure hook, automatic compatibility fallback, or alternate state path.

## A/B acceptance evidence

This evidence is owned and retained by the deployment operator; it is not part
of `check-release`, is not consumed by the product, and does not block creation
of B's release bundle. To claim both terminal paths, run accept and rollback on
separate disposable copies derived from the same verified A recovery point.

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
