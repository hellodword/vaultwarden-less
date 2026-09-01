# Upstream maintenance

Upstream changes are deliberate, one-component reviews. The goal is not to
follow the newest tag; it is to preserve the implemented behavior
with auditable evidence, exact inputs, a small diff, and a complete rollback
path.

An upstream candidate is outside the project's compatibility and support scope
until a reviewed project commit locks it, records it in the audited inventory,
builds the project-owned products, and passes the required acceptance. Running
the project with an independently substituted Vaultwarden, Restic, Bitwarden
CLI, Moto, Go, image, or wrapper version is diagnostic only and creates no
support promise.

[`UPSTREAMS.toml`](../UPSTREAMS.toml) is the human-reviewed inventory for
Vaultwarden, Bitwarden CLI, Restic, Moto, and Go. It records source repository,
tag, commit, and relevant audit paths; Vaultwarden also records the accepted
migration, complete schema, and recovery projection. Nix package sources,
hashes, and dependency graph are owned by `flake.nix` and the tool-generated
`flake.lock`, not duplicated in prose.

Never edit a dependency lockfile by hand. Change an authoritative input, then
use its declared dependency tool. Do not combine an upstream update with
unrelated cleanup, formatting churn, provider changes, or contract redesign.

## Baseline and candidate report

Start from a clean branch at a passing commit:

```sh
git status --short --branch
just check
nix develop --command go test -race ./... -count=1
nix develop --command govulncheck ./...
nix build --no-link .#backup .#proxy .#backup-image .#proxy-image .#vaultwarden-image .#moto-image
```

Create one ignored candidate report:

```sh
nix develop --command python3 scripts/upstream_diff.py <component> <candidate-tag>
```

Components are `vaultwarden`, `bitwarden-cli`, `restic`, `moto`, and `go`.
The standard-library script resolves current and candidate tags to commits from
the expected origin, emits the full name-status diff and audited-path subset,
and safely quotes hostile path or subject bytes. The report is an index, not an
approval. Read official release notes, security notices, provenance, license
changes, migrations, and the actual source diff.

Resolve all changing external facts from official upstream sources and current
local package output. Do not rely on remembered versions, image digests, CLI
flags, advisories, or maintenance status.

## Vaultwarden checklist

Vaultwarden is the primary durable compatibility boundary. Review at least:

- every SQLite migration since the accepted commit and its forward behavior;
- all database tables, columns, indexes, triggers, and migration records;
- models and paths for attachments and file-Send metadata, including unsafe or
  missing-path behavior;
- API routes that mutate ciphers, folders, Sends, organizations, collections,
  membership, groups, policies, access counts, login, and token state;
- every proxy event and ignored-route decision against source-line evidence;
- registration, authentication, KDF, organization bootstrap, token, and JSON
  behavior exercised by the narrow fixtures and official Bitwarden CLI;
- environment variables, defaults, healthcheck, non-root UID/GID, filesystem,
  SQLite, and signal behavior of the Nix package;
- web-vault and AGPL/provenance implications of the distributed image.

Build or obtain the candidate only through the declared Nix input. Start it
against a new ignored private directory and let it create a fresh SQLite
database. Capture the complete schema with the project script:

```sh
nix develop --command python3 scripts/capture_vaultwarden_schema.py \
  --database /absolute/path/to/fresh/db.sqlite3 \
  --output internal/upstream/vaultwarden-schema.json
```

The capture opens SQLite read-only with WAL awareness, checks integrity and the
exact recorded migration, and writes deterministic schema JSON. Review the
entire diff. A migration number alone is not compatibility evidence.

Update the Vaultwarden tag, commit, migration, schema, and recovery projection
as one reviewed unit. Then update every exact test expectation and image input
that is genuinely owned by that change. Do not add a parallel schema or fallback
unless a separately authorized durable contract requires it.

Acceptance requires the complete local gate, release reproducibility, real
backup/restore/drill lifecycle, two-target degradation/catch-up, and the A/B
upgrade suite with the prior actual Vaultwarden version as A and the candidate
actual version as B. If those versions do not differ, the result is only
framework evidence for that exact managed release pair; it is complete for
that claim and does not create a pending cross-version obligation. If they do
differ, a passing suite supports only that exact managed pair.

## Bitwarden CLI checklist

The official Bitwarden CLI is an E2E oracle, not a production dependency.
Review:

- release notes, authentication, server configuration, session, and password
  file behavior;
- JSON shapes and commands used for login, sync, item/folder, attachment,
  organization, collection, and Send operations;
- anonymous Send access-token issuance, expiry, caching, access limits, and
  isolated recipient application data;
- KDF, registration, user/organization key generation, EncString, AES-CBC-HMAC,
  RSA-OAEP, and Node crypto behavior used by the two narrow fixtures;
- the exact `bw --version` selected by the locked Nix package set.

If the current Nix input cannot provide the reviewed candidate, do not merely
change `UPSTREAMS.toml`. Either keep the current CLI or update the Nix input and
audit every package that moves with it. Registration and initial organization
creation remain bounded fixtures only because the CLI lacks those operations;
all later behavior must continue through the real CLI.

Run deterministic crypto vectors repeatedly, all fast/race checks, ordinary
and two-target E2E, and upgrade E2E. A green CLI version command does not prove
protocol compatibility.

## Restic checklist

Restic is the unmodified package from the one locked Nix package set. Review:

- official release/security notes and the exact source revision selected by
  Nix;
- `init`, `backup`, `snapshots`, `check`, `unlock`, `forget --prune`, and
  `restore` flags, exit codes, and JSON schemas;
- S3-compatible backend, credentials, region, bucket lookup, multipart,
  locking, caching, and timeout behavior;
- full snapshot IDs, host/path/tag filtering, subfolder restore, and object
  deletion semantics;
- process-group cancellation, pipe drain/wait, stderr limits/redaction, frame
  limits, UTF-8, and unknown-field behavior;
- license and transitive security changes relevant to distribution.

Do not introduce a local Restic patch or custom derivation merely to force a
version. Advance the Nix input only after reviewing other package movement, or
retain the current package. The toolchain check must match the recorded Restic
tag and readable source revision.

Run unit and race tests for the Restic adapter, the real Moto/Restic integration
including format-2 init/check/prune/restore failures, lean backup, fast
multi-target, initial-import, both upgrade E2Es, vulnerability checks, and
deterministic release creation. Compare actual A/B Restic versions: only a
distinct pair runs and may claim the format-2 cross-version round trip. An
equal pair records that matrix as not applicable. Never add an implicit
`restic migrate` call. Moto is not a substitute for a real configured storage
provider.

## Go checklist

Go affects product binaries, SQLite behavior, the race detector, and every Go
test. Review:

- release and security notes, including fixed standard-library vulnerabilities;
- compiler/linker, runtime, crypto, `net`, `os`, `database/sql`, and syscall
  changes;
- Linux/amd64 and CGO behavior for SQLite;
- `renameat2`, file identity, stable copy, process groups, cancellation, pipes,
  and race-detector changes;
- the Go requirement of every module and the exact `go version` from Nix;
- whether the same Nix update also changes Vaultwarden, Restic, Bitwarden CLI,
  Moto, SQLite, or analysis tools.

Use `go mod edit` inside the pinned development environment for `go` or
`toolchain` directives. Update `flake.lock` only through Nix's supported lock
command after choosing and auditing the exact Nix input revision. Update
`UPSTREAMS.toml` to the official Go tag and commit only after current official
source evidence is established. `GOTOOLCHAIN=local` and the actual toolchain
version must match the inventory.

Run `govulncheck ./...` before and after the change, all fast and race checks,
all products, real integration/lifecycles, upgrade E2E, and two clean release
builds. A toolchain bump is incomplete while any reachable known vulnerability
remains or any frozen upstream version moved without review.

## Moto checklist

Moto is test-only and never enters the production release archive. Review its
S3 server behavior used here: endpoint startup, authentication, bucket
creation, list/read/write/delete, multipart, object metadata, failures, and
cleanup. Confirm the actual Nix package version and readable source revision
against `UPSTREAMS.toml`.

Run the real integration, lean backup, fast multi-target, initial-import, and
both upgrade scenarios. Inspect that every required Go test genuinely ran and
passed and that all task-owned containers, networks, and volumes are gone. A
Moto update does not authorize a production provider claim.

## Shared Nix-input changes

All five audited components currently come from one locked Nix package set,
directly or as build/test tooling. A single Nix input change can therefore move
several components. Before applying it:

1. evaluate the candidate package versions and readable source revisions in an
   isolated clean copy;
2. compare every audited component, not only the one that motivated the update;
3. choose a revision that minimizes unrelated movement without missing a
   required security fix;
4. generate the lock update with Nix and inspect its complete diff;
5. update every moved inventory entry or reject the candidate;
6. validate in the isolated copy before applying the same declared commands to
   the main worktree.

Lockfile churn, download volume, or a successful evaluation is not proof that
the selected product set is unchanged.

## Required acceptance

Run focused tests first, then the complete fast release-candidate gate from a
distinct clean prior A commit to clean HEAD B:

```sh
just check-release <A-full-commit>
```

This runs the ordinary flake checks, full-tree race and coverage,
`govulncheck`, all products, targetless deployment, Moto/Restic integration,
full recovery, accelerated target-threshold evidence, two repositories,
initial import, and both A/B terminal cases. Required-test JSON must prove
run/PASS and package PASS with no
skip/fail for every real scenario. Exact fake-clock boundaries and the
accelerated real Restic/HTTPS-adapter test replace a wall-clock waiting claim;
the production clock has no test override. The gate intentionally excludes
cold-host and real-provider acceptance.

When Nix image composition, entrypoints, labels, or release logic changes,
generate the release from the checked exact A/B pair into one previously absent
ignored root:

```sh
just release <A-full-commit> <previously-absent-ignored-output-root>
```

The release command itself performs two independent builds and requires their
image archive bytes to match before publication. Require successful checksum
and standard `docker image load`, exactly six references for three image IDs,
matching OCI labels and entrypoints, no Moto image, the actual expected upstream
versions, exact candidate binding, canonical artifact evidence, and only
`images.tar.gz` plus `SHA256SUMS` in the output.
Repeat the archive-only cold deployment acceptance in
[Operations](operations.md) when Compose, image entrypoints, permissions, bind
paths, directory exchange, or deployment orchestration changes. Record a
non-triggered condition and its evidence as not applicable, never as passed.

For an actual Vaultwarden change, the upgrade suite must run exactly the full
accept case interrupted after decision and the deepest rollback case interrupted
after exchange. The focused unit fault matrix covers the other decision,
active-write, service-start, cleanup, finalize, conflict, permission, tamper,
and restart boundaries. A cross-version claim additionally requires the two
images to report genuinely different Vaultwarden versions; equal versions make
that conditional claim not applicable while still proving the exact-pair
orchestrator. The resulting claim applies only to that release pair, not to
other upstream versions or independently built images.

## Final review

Before committing the upstream unit:

- review `git status`, unstaged/staged diffs, generated-file ownership, and the
  exact lock diff;
- search tracked files for replaced tags, commits, migrations, package
  versions, API routes, image assumptions, and obsolete compatibility paths;
- confirm examples, contracts, architecture, operations, verification, and
  provider prose still agree;
- classify every failure as introduced, pre-existing, or environment-caused
  and rerun the narrowest failing command;
- remove ignored source checkouts, reports, releases, logs, databases, and
  runtime resources created by the review;
- stage only explicit task paths and keep unrelated user work untouched.

Record what changed, official evidence consulted, actual versions, commands and
results, reproducibility facts, real scenario coverage, known unverified
boundaries, cleanup, and the exact commit. Do not claim completion from a
version-string update or a test framework that did not execute its required
scenario.
