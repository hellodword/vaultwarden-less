# Verification evidence and acceptance

Verification is layered. A static check, a compiled opt-in test, a local S3
emulator, a release build, and a real provider drill prove different things.
Skipped or unrun work is `unverified`; a non-triggered conditional boundary is
`not applicable`. Neither may be reported as passed.

## Public command surface

Run development commands from the repository root with the pinned Nix tooling
available:

```sh
just --list
just check
just check-release
just release
```

`just --list` exposes exactly `check`, `check-release`, and `release`.
Deployment hosts do not need any of them; production operations use the raw
Compose commands in [Operations](operations.md).

## Ordinary flake gate

```sh
just check
```

This is exactly one `nix flake check`. Its declared checks own:

- repository formatting;
- ordinary full-package Go tests, `go vet`, and GolangCI-Lint;
- Ruff lint and formatting plus all Python contract tests;
- the upstream inventory, complete Vaultwarden schema, recovery projection,
  durable script set, and pinned toolchain wiring.

Ordinary Go execution compiles opt-in real tests, but those tests skip before
Docker, image, port, or network work unless their explicit test-only flag is
present. A green ordinary gate therefore does not claim any real container
scenario, full-tree race run, vulnerability scan, or cloud-provider behavior.

## Optional full release check

```sh
just check-release
```

This command is not a prerequisite for `just release` and writes no reusable
evidence. HEAD B must nevertheless be a full clean commit, and the whole
worktree including untracked files must remain unchanged while it runs. The
fixed gate order is:

1. `nix flake check`;
2. full-tree `go test -race ./... -count=1`;
3. `govulncheck ./...`;
4. both binaries, all local production/test images, and the pure Nix release
   bundle;
5. the targetless deployment lifecycle;
6. the real Moto/Restic integration;
7. the full backup/restore/drill lifecycle;
8. the accelerated worker degradation boundary;
9. the two-repository failure/catch-up lifecycle;
10. the initial external-data import lifecycle.

The check stops at the first failure and revalidates unchanged clean B. Success
prints the checked full commit; failure identifies the gate. It creates no
candidate, artifact, timestamp, cache, or other repository state. Operators who
need durable logs, signatures, or organizational approval records retain those
outside this project.

### Required-test proof

Every opt-in Go scenario runs through `scripts/verify_go_test.py` with a
`go test -json` stream. A required top-level test is accepted only when the
stream contains its `run`, then `pass`, never `skip` or `fail`, followed
by the exact package `pass`, and the subprocess exits zero. Malformed JSON,
a same-named test in another package, a subtest-only pass, or a missing package
result is a failure.

## Real scenario boundaries

### Targetless deployment

`TestTargetlessDeploymentLifecycle` uses the production images and the ordinary
backup E2E opt-in flag, with a 20-minute test timeout. It deliberately does not
start Moto. It proves all three services become healthy; one targetless warning
is logged; worker state stays idle and empty without a staging point; proxy
writes still succeed; manual backup returns `not_ready`; and a first user plus
an official-client-created item survive disabling registration and
force-recreating Vaultwarden while a later registration is rejected.

This test proves the local Compose targetless boundary. It does not create a
recovery point, contact a Restic repository, prove an external TLS edge, or
turn a remote-daemon deployment into release-gate evidence. The test process
and Docker bind-mount paths must share the same host namespace for the formal
gate.

### Moto and Restic

`TestMotoResticIntegration` uses the pinned Restic executable and locally
built pinned Moto image. It exercises S3 HTTP, dedicated format-2 repository
initialization, changed backups, content reuse, bounded listing, check/unlock,
latest and explicit restore, complete recovery verification, no-overwrite and
corrupt-latest refusal, fixed prune preview/apply, preservation of out-of-scope
objects, and a final repository check.

Moto proves the product/Restic/S3 protocol boundary only. It does not prove a
cloud vendor's IAM, endpoint, throttling, lifecycle, lock, durability, billing,
or regional behavior. Each real provider remains `unverified` until its
[provider evidence form](providers.md#provider-acceptance-evidence-form) is
completed with the production-shaped account, endpoint, prefix, roles, and
drill.

### Backup, restore, and drill

`TestBackupRestoreLifecycle` uses the exact project images, pinned Vaultwarden,
Restic, Moto, and Bitwarden CLI. A narrow registration fixture creates two
synthetic accounts because the CLI cannot register them; later vault operations
use the real CLI. The scenario proves:

- independent folders and items for both users, including a later generation;
- an ordinary file Send and an access-limited file Send whose exhausted state
  remains exhausted after recovery;
- exact attachment and Send bytes;
- changed content/archive identities, repository check, and latest restore;
- an isolated drill where both pre-recovery sessions fail and both fresh logins
  succeed;
- organization, membership, collection, organization-item, user-collection,
  and cipher-collection relationships in the recovery closure;
- cleanup of only scenario-owned containers, networks, volumes, credentials,
  client state, and temporary outputs.

This is not browser, email, push, production TLS-edge, or arbitrary upstream
compatibility evidence. A real organization invitation remains a manual drill.

### Worker timing and two repositories

`TestWorkerAcceleratedTargetDegradationIntegration` joins a real local
format-2 Restic repository and the HTTPS notification adapter to the
test-injected clock. Unit boundaries prove that `29:59.999` of continuous
failure is not degraded and `30:00.000` is degraded, with one degradation
notification, no duplicate on another failure, and one recovery notification.
The clock injection is not reachable through a production flag or environment
variable.

`TestMultiTargetFastCatchUp` uses two independently initialized real
Moto/Restic repositories and a controlled failing adapter. The healthy target
stores the point while the other reports partial/retrying; restoration catches
the failed target up to the same latest content without another source change.
Both repositories then pass check, exact restore, and an isolated client drill.
This short test proves container/network/adapter behavior without claiming a
real wall-clock wait or a cloud outage.

Resident synchronization has at most two targets in flight and publishes
results in configuration order. Focused race tests cover four-target dispatch,
failure isolation, cancellation, join, and deterministic result/notification
ordering.

### Initial import

`TestInitialImportLifecycle` creates a stopped synthetic external Vaultwarden
tree with committed WAL state, attachment, file Send, and old RSA identity. It
proves immutable-source preparation, Online Backup WAL absorption, exclusion
and rotation of RSA/sidecars, candidate-only startup, old-session rejection,
fresh official-client verification, strict confirmation before accept, the
single same-filesystem exchange, exact normal three-service startup, durable
history, finalization, and source preservation.

It does not authorize importing a live source, merging into nonempty data,
interpreting project-owned state or snapshots as import input, deleting the
original, or performing a real production cutover.

### Upgrade and rollback

The release gate does not select A and does not run a real A/B Compose
lifecycle. Focused `internal/upgrade` tests own decision ordering, frozen-tree
identity, opaque rollback, permission, tamper, conflict, cleanup, finalize, and
restart semantics. They do not prove a particular Vaultwarden or Restic pair.

The deployment operator owns exact A/B compatibility, real-client migration,
accept and rollback rehearsal, and cross-version Restic evidence through the
[upgrade runbook](upgrades.md#ab-acceptance-evidence). This evidence is not
consumed by the product and cannot be claimed from a successful B release.

## Deterministic release

From a committed Git revision without tracked changes:

```sh
release_path="$(just release)"
```

`just release` is only
`nix build --no-link --print-out-paths .#release`. The pure release build fails
before producing a bundle when the Git flake has no clean `self.rev`, prints one
immutable Nix store path on success, creates no `result` symlink or repository
output, and neither runs nor consumes `check-release` evidence. Git-flake source
filtering excludes ignored and untracked paths, so the derivation has no
implicit dependency on private or generated repository files.

The Nix bundle contains:

- exactly one immutable archive for each production image;
- production Compose and upgrade files plus an immutable-tag `.env.example`;
- public configuration and credential templates, including a complete
  targetless example;
- complete operator documentation, release metadata, upstream audit metadata,
  `LICENSE`, and `NOTICE.md`;
- no mutable image alias, Moto image, private deployment tree, source/tests,
  registry publication, SBOM, signature, attestation, or corresponding-source
  archive;
- a `SHA256SUMS` entry for every other regular file in the bundle.

`release.json` names the exact commit, immutable release, system, image archive
paths, and image references. It states build facts only; it does not claim that
optional checks, provider validation, transfer, deployment, or client
acceptance ran. Copy the store directory to a previously absent durable
destination before Nix garbage collection. Destination selection,
no-overwrite policy, access control, transport, signing, log retention, and
store-path retention are operator responsibilities.

External verification uses ordinary tools:

```sh
(cd "$release_path" && sha256sum --check SHA256SUMS)
docker image load --input "$release_path/images/backup.tar.gz"
docker image load --input "$release_path/images/proxy.tar.gz"
docker image load --input "$release_path/images/vaultwarden.tar.gz"
```

The operator validates immutable OCI identity and deploys with the bundle's
exact `VWLESS_IMAGE_TAG` as documented in [Operations](operations.md). Loading
does not require or create mutable aliases.

## Conditional operational evidence

Archive-only cold-host acceptance, each real cloud provider, and an actual
production cutover are separate boundaries. Run only in an isolated,
authorized environment using [Operations](operations.md) and
[Providers](providers.md). Record exact commit/release, commands, timestamps,
identities, client facts, rollback facts, and task-resource cleanup. If a
boundary was not actually run, record `unverified`; never promote Moto,
Compose rendering, a health check, a remote Docker-daemon rehearsal, or a
framework test into that result.

## Documentation and contract audit

Before final acceptance:

- validate tracked relative Markdown links and every documented file, service,
  profile, command, flag, and output against the tree;
- render all Compose profiles and the upgrade overlay with ignored synthetic
  private fixtures;
- prove `scripts/` contains the four durable Python tools and
  `tests/scripts/` their four matching tests plus `test_justfile.py`;
- prove the Just allowlist is exactly three recipes and their final argv matches;
- search for retired project-version markers, command names, checkout-relative
  private paths, host orchestration, hidden generated deployment trees, and
  stale timing claims;
- scan the diff and tracked content for secrets, private addresses, databases,
  logs, captures, runtime artifacts, and task paths;
- run focused checks before the complete clean-commit gates.

For every failure record the exact command, timing, result, narrow rerun, and
`introduced`, `pre-existing`, or `environment-caused` classification.
Compilation, skipped scenarios, framework existence, or likely cleanup are
never substitutes for observed required-test and package passes.
