# Verification evidence and acceptance

The project separates fast/static evidence, built products, real container
boundaries, upgrade A/B evidence, deterministic release evidence, and an
archive-only cold deployment. A later layer does not retroactively turn an
earlier skipped or static check into a real pass.

All project commands run in the pinned Nix environment:

```sh
nix develop --command just --list
```

## Fast gate

```sh
nix develop --command just check
```

`check` runs:

- repository formatting verification;
- every ordinary Go test and all seven Python/Just contract-test files;
- `go vet`, GolangCI-Lint, Ruff lint, and Ruff formatting checks;
- all declared flake checks, including package/toolchain, Compose contract,
  recovery projection, script set, and formatting boundaries.

It deliberately does not run full-tree race, `govulncheck`, product image
builds outside declared flake checks, or any real Docker E2E. Those belong to
the release-candidate layer.

External Go tests compile during ordinary `go test`, but their first action
checks an explicit runtime flag and skips before Docker lookup, image work,
ports, or network access. A green `check` therefore proves the skip-safe test
packages compile; it does not prove a real container scenario ran.

The unit suites include strict configuration/YAML/private-path validation,
fixed environment isolation, Restic JSON/framing/redaction/cancellation,
SQLite online backup during concurrent WAL writes, complete schema and
projection validation, canonical content and archive identities, stable-copy
races, exact restore verification, proxy event policy, worker retry/degraded
state, import source/WAL/exchange/resumption, upgrade state/tamper/idempotency,
deterministic crypto vectors, and pure
port boundaries.

## Release-candidate race, security, and products

```sh
nix develop --command go test -race ./... -count=1
nix develop --command govulncheck ./...
nix build --no-link .#backup .#proxy .#backup-image .#proxy-image .#vaultwarden-image .#moto-image
```

The public `check-release <A-full-commit>` gate runs these after `check`.
Full-tree race is release evidence, not a daily cost. `govulncheck ./...` is a
reachability scan using the locked Go toolchain; it does not claim a complete
container, operating-system, JavaScript, provider, or supply-chain
vulnerability inventory.

The product command builds both public binaries plus backup, proxy,
Vaultwarden, and test-only Moto images. The Vaultwarden gate remains
image-internal rather than a public flake package. A build proves Nix can
realize the declared products from locked inputs; it does not start them or
prove their deployment configuration.

## Required-test verifier

Every real Go scenario is run with `go test -json` through
`scripts/verify_go_test.py`. The verifier requires the exact
`<package>:<top-level-test>` identity and accepts it only after:

1. the required test emits `run`;
2. it emits `pass`, never `skip` or `fail`;
3. the exact package later emits `pass`;
4. the subprocess exits zero;
5. every input line is valid Go test JSON in the required order.

A shell exit code, similarly named test in another package, subtest-only pass,
malformed output, or missing final package pass is rejected. Successful output
includes one explicit `go test verification passed: ...` line.
The verifier checks the event stream, not whether child argv contains the
literal token `-json`: project recipes use `go test -json`, while the
repository-external runner may supply the equivalent locked
`go tool test2json` stream.

## Moto/Restic integration

```sh
nix develop --command python3 scripts/verify_go_test.py \
  --require github.com/hellodword/vaultwarden-less/internal/restore:TestMotoResticIntegration \
  -- go test -json ./internal/restore -run '^TestMotoResticIntegration$' \
  -count=1 -timeout=20m -args -run-moto-integration
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/internal/restore:TestMotoResticIntegration
```

This uses the Nix-built Restic executable and locally built pinned Moto image.
It proves real S3 HTTP, bucket/repository initialization with exact Restic
format 2, changed backups, content deduplication, bounded listing, repository
check/unlock, latest and explicit restore, complete recovery-tree verification,
no-overwrite behavior, corrupt-latest refusal without fallback, controlled
fixed-retention preview/apply sets, out-of-scope preservation, and final
repository check. No product path invokes `restic migrate`.

Moto proves the product/Restic/S3 protocol boundary. It does not prove AWS,
R2, COS, OSS, Scaleway, or B2 production behavior, IAM, throttling, object lock,
lifecycle, durability, billing, or regional outage behavior.

## Ordinary backup, restore, and drill lifecycle

```sh
nix develop --command python3 scripts/verify_go_test.py \
  --require github.com/hellodword/vaultwarden-less/tests/e2e:TestBackupRestoreLifecycle \
  -- go test -json ./tests/e2e -run '^TestBackupRestoreLifecycle$' \
  -count=1 -timeout=30m -args -run-backup-e2e
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/tests/e2e:TestBackupRestoreLifecycle
```

The scenario uses the exact audited Vaultwarden image, project proxy/worker,
Moto, pinned Restic, and exact Nix Bitwarden CLI. A narrow protocol fixture
performs only account registration, which the CLI does not expose; all later
vault operations use the real CLI.

It proves:

1. one synthetic user, one login, one attachment no larger than 4 KiB, and one
   file Send no larger than 32 KiB;
2. a first verified recovery point, then one login update and a second logical
   generation with a different content identity;
3. a real repository check and latest restore whose snapshot/content/archive
   identities match the newest point;
4. an isolated drill in which the pre-recovery session fails, a fresh official
   CLI login succeeds, the updated login is present, and attachment/Send bytes
   match exactly;
5. task-scoped containers, networks, volumes, credentials, appdata, and
   temporary outputs are removed.

This proves the exercised API/CLI paths for the exact project-managed and
audited component set. It is not a full Vaultwarden feature test, browser UI
test, email/push test, production TLS edge test, or evidence for a substituted
Vaultwarden or Bitwarden CLI version.

## Initial SQLite import lifecycle

The release-candidate gate runs the focused scenario equivalently to:

```sh
nix develop --command python3 scripts/verify_go_test.py \
  --require github.com/hellodword/vaultwarden-less/tests/e2e:TestInitialImportLifecycle \
  -- go test -json ./tests/e2e -run '^TestInitialImportLifecycle$' -args -run-import-e2e
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/tests/e2e:TestInitialImportLifecycle
```

The scenario uses the exact clean production images, current pinned
Vaultwarden, Moto/Restic, the official pinned `bw` CLI, and a test-owned HTTPS
edge. It creates one external stopped source with a real login, a small
attachment, a small file Send, the old RSA identity, and a committed persistent
WAL marker. It then proves:

1. prepare leaves the external/project source tree byte-identical, absorbs the
   WAL transaction through Online Backup, and leaves no candidate sidecar or
   old RSA file;
2. the import gate starts only the candidate on loopback, the old official-CLI
   session fails after RSA rotation, and a new login through HTTPS reads the
   login, attachment, and Send exactly;
3. missing client confirmation and accept-before-verification fail with the
   exact structured codes;
4. stopped-candidate verification records real client confirmation, the
   audited migration/schema and snapshot closure, private modes, no sidecars,
   and a different RSA digest;
5. terminal accept atomically promotes the verified tree, starts the exact
   normal three-service runtime, finalizes history, and an exact rerun reports
   `already_complete`;
6. source remains unchanged through finalize, the newly authenticated session
   survives promotion, the old session remains invalid, and another fresh
   normal HTTPS login reads every test object byte-for-byte;
7. only the scenario's Compose resources, credentials, client stores, and
   temporary trees are removed.

This is evidence for the fixed initial-import contract and current pinned
Vaultwarden migration boundary. It does not authorize importing a live source,
merging into active data, substituting another upstream version, or deleting
the external original.

## Two-repository fast catch-up and soak

The release-candidate fast case runs equivalently to:

```sh
nix develop --command python3 scripts/verify_go_test.py \
  --require github.com/hellodword/vaultwarden-less/tests/e2e:TestMultiTargetFastCatchUp \
  -- go test -json ./tests/e2e -run '^TestMultiTargetFastCatchUp$' \
  -count=1 -timeout=25m -args -run-multitarget-fast-e2e
```

It uses two independently initialized real Moto/Restic repositories and a
test-only deterministic 403 endpoint. Primary creates the point while
secondary reports partial/retrying; removal of the exact adapter and restoration
of the real secondary catches up the same content ID without another source
mutation. Both repositories must pass check and exact restore, and both restore
trees must pass an isolated official-client drill. There is no degradation
wait in this fast case.

The separate conditional wall-clock gate is:

```sh
nix develop --command just soak
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/tests/e2e:TestMultiTargetSoak
```

The soak holds the secondary failure for at least 30 real minutes, requires the
retrying-to-degraded transition, exactly one degraded notification, a healthy
primary with no duplicate logical snapshot through another full retry
interval, then restores and catches up secondary. It has no product clock hook
and is never part of `check` or `check-release`.

Neither case proves cross-provider consistency or a cloud vendor's outage
behavior. Repeat the controlled procedure on production-like targets before
acceptance.

## Upgrade and rollback lifecycle

```sh
nix develop --command just check-release <A-full-commit>
```

The clean current HEAD is B. The final release-candidate phase requires both:

```text
github.com/hellodword/vaultwarden-less/tests/e2e:TestUpgradeAcceptLifecycle
github.com/hellodword/vaultwarden-less/tests/e2e:TestUpgradeRollbackLifecycle
```

Both commits must be clean schema-1 products with deterministic immutable
releases. The accept case runs A-owned prepare/freeze, B migration/evidence and
healthy drill, writes the accept decision, simulates an interruption before
host service orchestration, then resumes to exact healthy B services, history,
retained immutable A images, client data, and an `already_complete` replay.
The rollback case reaches verified B, corrupts the final drill/accept boundary,
writes rollback and exchanges the frozen tree, simulates an interruption before
active A publication, then resumes to exact A RSA/data/service identity,
quarantined B, fresh client verification, and an `already_complete` replay.
The focused Go/Python fault matrix owns the remaining decision, active write,
service start, cleanup, finalize, conflict, tamper, permission, and restart
boundaries without repeating costly full lifecycles.

The tests inspect actual Vaultwarden and Restic versions inside both immutable
image sets. Equal Vaultwarden versions record the cross-version claim as not
applicable while preserving complete exact-pair orchestration evidence.
Different Vaultwarden versions permit an exact-pair claim only after both
cases pass. Equal Restic versions similarly make its matrix not applicable;
different versions additionally require A format-2 init/write, B
list/check/restore/write, and A list/check/restore of B's point. No result
extends to a component version or image outside the project-managed pair.

## Release-candidate gate

```sh
nix develop --command just check-release <A-full-commit>
```

The argument must be one full lowercase A commit. HEAD is B; A and B must be
different complete commits, and the worktree including untracked files must be
clean before and after. The gate is exactly: daily `check`, full-tree race,
`govulncheck`, all production images/binaries plus Moto, Moto/Restic
integration, lean backup, fast multi-target, initial import, and finally both
A/B upgrade cases. It stops on the first failure and emits one schema-1 success
document only after revalidating the unchanged pair.

The gate intentionally excludes the 30-minute soak, archive-only cold-host
acceptance, and same-commit double release. Run those separately when their
documented conditions trigger; never describe a non-triggered condition as a
pass.

Optional nonzero real-test ports are loopback-only, canonical decimal
`1024..65535`, and must differ inside one scenario. Omit them to use ephemeral
ports. A test must never publish a verification endpoint publicly.

## Deterministic release

Release creation requires an exact clean commit and a previously absent output
directory:

```sh
nix develop --command just release <ignored-output-root>
```

Acceptance requires:

- exactly `images.tar.gz` and `SHA256SUMS`;
- checksum verification and direct standard `docker image load`;
- exactly six production references, no Moto reference, and three distinct
  image IDs with local/immutable pairs;
- OCI revision, version, and created labels derived from the clean commit;
- expected wrapper entrypoints and actual audited upstream versions;
- byte-identical outputs from two builds of the same commit and inputs;
- atomic refusal to overwrite an existing output and no half-release on
  failure.

The release has no bundle manifest, source archive, SBOM, license archive,
registry reference, or generated deployment tree. `NOTICE.md` describes the
project's actual provenance boundary without claiming those absent artifacts.

## Archive-only cold acceptance

Run [the cold deployment procedure](operations.md#cold-deployment-acceptance)
on an ordinary isolated Linux/amd64 host. Deployment operations there use only
Git, Docker Engine, Compose, the tracked clone, and the two-file archive—no Nix,
Go, Python, Restic installation, source-derived helper, or hidden generated
deployment tree.

Acceptance includes exact clone/release identity, private-copy modes, standard
image load, both role checks, production startup, real client data, durable
backup across a full service restart, latest and earlier restore, data and
session-invalidation checks in a drill, two-target degradation/catch-up, and
zero acceptance-owned resources afterward.

An external normal Bitwarden client may act as the data verifier; it is not a
dependency installed on the deployment host. The operator deployment steps
must remain executable from the documentation without reading Go/Nix source.

## Documentation and contract audit

Before final acceptance:

- validate every tracked relative Markdown link and every documented file,
  service, profile, command, flag, and output against the current tree;
- render all Compose profiles and the upgrade overlay with ignored private
  fixtures;
- prove `scripts/` contains exactly the six documented durable Python scripts,
  `tests/scripts/` contains their six same-named contract tests plus the Just
  contract test, and each has a live command/test reference;
- prove `just --list` exposes exactly the documented 18 names and three groups,
  every other recipe is private, and every public/private dry-run argv matches
  its contract;
- search for obsolete schema, bundle, generated deployment, patch, command,
  test, and compatibility terminology;
- scan tracked content and the full diff for credentials, private addresses,
  runtime artifacts, databases, captures, logs, and task paths;
- run formatting, focused docs/static checks, and the final full gates from the
  exact clean commit being accepted.

## Evidence discipline

For every command record its exact invocation, start/end time, exit status,
required-test and package result, cleanup state, and failure classification.
On failure, rerun the narrowest command and classify it as introduced,
pre-existing, or environment-caused before using a safe alternate path.

Do not mark a gate complete because the framework exists, a package compiled,
a test skipped, static Compose rendered, a subprocess exited without required
events, or cleanup merely appeared likely. Known boundaries—especially real
provider behavior and distinct-version A/B—remain explicit until actually run.
