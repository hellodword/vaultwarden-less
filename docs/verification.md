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
- every ordinary Go test and all five Python script contract-test files;
- all Go packages under the race detector;
- `go vet`, GolangCI-Lint, Ruff lint, and Ruff formatting checks;
- all declared flake checks, including package/toolchain, Compose contract,
  recovery projection, script set, and formatting boundaries.

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

## Security and products

```sh
nix develop --command just security
nix develop --command just build
```

`security` is the declared `govulncheck ./...` reachability scan using the
locked Go toolchain. It does not claim a complete container, operating-system,
JavaScript, provider, or supply-chain vulnerability inventory.

`build` builds both binaries plus backup, proxy, Vaultwarden, and test-only
Moto images. It proves Nix can realize the declared products from locked
inputs. It does not start those images or prove their deployment configuration.

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

## Moto/Restic integration

```sh
nix develop --command just integration
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/internal/restore:TestMotoResticIntegration
```

This uses the Nix-built Restic executable and locally built pinned Moto image.
It proves real S3 HTTP, bucket/repository initialization, changed backups,
content deduplication, bounded listing, repository check/unlock, latest and
explicit restore, complete recovery-tree verification, no-overwrite behavior,
corrupt-latest refusal without fallback, controlled fixed-retention preview and
apply sets, out-of-scope preservation, and final repository check.

Moto proves the product/Restic/S3 protocol boundary. It does not prove AWS,
R2, COS, OSS, Scaleway, or B2 production behavior, IAM, throttling, object lock,
lifecycle, durability, billing, or regional outage behavior.

## Ordinary backup, restore, and drill lifecycle

```sh
nix develop --command just e2e-backup
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/tests/e2e:TestBackupRestoreLifecycle
```

The scenario uses the exact audited Vaultwarden image, project proxy/worker,
Moto, pinned Restic, and exact Nix Bitwarden CLI. Two narrow protocol fixtures
perform only account registration and initial organization creation, which the
CLI does not expose; all subsequent vault operations use the real CLI.

It proves:

1. two users, organization/collections, organization and personal items,
   folder, attachment, ordinary Send, and one access-limited Send;
2. a missing referenced payload prevents snapshot publication while preserving
   the prior point;
3. the same inconsistent window contains several source changes but creates no
   intermediate point;
4. exactly two recovery points, changed logical identity, real repository
   check, fixed prune preview/apply, and worker restart continuity;
5. latest restore and isolated drill, pre-recovery session failure, fresh
   sessions, exact item/organization/attachment/Send verification, and
   exhausted access-limit behavior;
6. explicit earlier full-ID restore and earlier data through a second drill;
7. task-scoped containers, networks, volumes, credentials, appdata, and temp
   outputs are removed.

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

## Two-repository degradation and catch-up

```sh
nix develop --command just e2e-multitarget
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/tests/e2e:TestMultiTargetDegradationAndCatchUp
```

The scenario uses two independently initialized real Moto/Restic repositories.
It proves initial equality, controlled failure of one target, success retained
on the other, the real 30-minute transition to partial/degraded, recovery and
catch-up without another source mutation, equal newest content identity, and
repository check/restore/drill against both targets. Snapshot count or time
alone is never used as equality evidence.

It does not prove cross-provider consistency or a cloud vendor's outage
behavior. Repeat the controlled procedure on production-like targets before
acceptance.

## Upgrade and rollback lifecycle

```sh
nix develop --command just e2e-upgrade <A-full-commit> <B-full-commit>
```

Required identity:

```text
github.com/hellodword/vaultwarden-less/tests/e2e:TestUpgradeRollbackLifecycle
```

Both commits must be clean schema-1 products with deterministic immutable
releases. The suite proves:

- A-owned prepare/freeze and same-filesystem rollback evidence;
- B-only migration/schema audit, isolated repository evidence, restore/drill,
  and explicit accept;
- fail-closed B proxy/worker before accept;
- forced migration-permission, invalid-target, and corrupt-drill failures,
  each followed by explicit immutable-A rollback;
- an isolated evidence/accept success path;
- tamper, wrong-owner, terminal conflict, restart, and cleanup boundaries.

Different clean commits and immutable tags prove the framework for that exact
managed release pair. If both images report the same actual Vaultwarden
version, the framework result is complete and creates no pending cross-version
support obligation. Exact-pair cross-version compatibility may additionally be
claimed only when two project-managed releases report different actual
Vaultwarden versions and the entire suite ran with that pair. No result extends
to a Vaultwarden, Restic, Bitwarden CLI, or other component version that the
project does not manage.

## Aggregated non-upgrade gate

```sh
nix develop --command just check-full
```

`check-full` is exactly `check`, `security`, `build`, `integration`,
`e2e-backup`, and `e2e-multitarget`. It intentionally does not guess A/B
commits and therefore does not include `e2e-upgrade`. A final acceptance that
changes upgrade behavior or an audited upstream must run both.

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
- prove `scripts/` and `tests/scripts/` contain exactly the five documented
  same-named files and each has a live command/test reference;
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
