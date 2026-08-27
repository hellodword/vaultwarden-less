# Upstream maintenance

This project does not automate upstream upgrades. The goal is a repeatable
manual review: at any later date, a maintainer should be able to identify what
is pinned, inspect one candidate, understand every affected boundary, and
produce a small reviewable change.

## Inventory model

[`UPSTREAMS.toml`](../UPSTREAMS.toml) is the human-reviewed inventory for:

- Vaultwarden;
- the official Bitwarden CLI used by E2E;
- Restic;
- Moto; and
- Go.

Each section records an exact tag and commit plus the build digest/hash needed
by this repository. `audit_paths` names the upstream surfaces that matter most
here. `VERSION` is deliberately separate because it versions this product,
not an upstream.

`scripts/generate_upstreams.py` validates the complete inventory and schema
contract, then emits the constants consumed by Go to
`internal/upstream/generated.go`. Run:

```sh
nix run .#just -- generate
nix run .#just -- generate-check
```

Never edit the generated Go file. Never put the same upstream pin into Compose,
a release script, or prose as another authority.

The Nix flake lock owns the exact nixpkgs and treefmt inputs. Update it only
through Nix commands, never with a text editor. Go module sums and any upstream
module sums represented in the Restic patch are likewise produced by Go
tooling.

## Review one component at a time

Start from a clean branch and a passing baseline. Pick one explicit candidate
tag:

```sh
nix run .#just -- upstream-diff vaultwarden <candidate-tag>
```

Supported component names are `vaultwarden`, `bitwarden-cli`, `restic`,
`moto`, and `go`. The command:

1. clones or reuses an ignored checkout under `tmp/upstream-audit`;
2. resolves the currently recorded tag and verifies that it still reaches the
   recorded commit;
3. resolves the candidate tag without moving any project pin;
4. writes an ignored report containing audited-surface changes, full diff stat,
   commits, and all changed paths.

The report is a starting index, not approval. Read release notes, security
notices, migrations, and the actual source diff. Verify tag signatures or
release attestations when that upstream provides them. Record conclusions in
the project review or commit message, not in a permanent pile of generated
reports.

Do not combine unrelated upstream upgrades. If a shared nixpkgs update changes
multiple tools, identify every changed package and either review them together
as an explicit toolchain change or keep the prior flake input.

## Vaultwarden checklist

Vaultwarden is the primary compatibility boundary. Review at least:

- all SQLite migrations since the current commit;
- database models and every attachment/file-Send reference shape;
- routes that mutate ciphers, folders, Sends, organizations, collections,
  membership, groups, policies, and access counters;
- every ignored proxy route and its source-line evidence;
- environment variables, default behavior, healthcheck, UID and filesystem
  assumptions in the official image;
- registration, authentication, token, and API changes exercised by E2E; and
- license/provenance changes in the distributed image.

Obtain the official image by the candidate digest, not a floating tag. Start it
against a new empty directory and let it create a fresh database. Confirm its
latest migration and capture the complete schema:

```sh
nix develop --command python3 scripts/capture_vaultwarden_schema.py \
  --database /absolute/path/to/fresh/db.sqlite3 \
  --output internal/upstream/vaultwarden-schema.json
```

The capture rejects a database whose latest migration differs from the
inventory. Review the JSON diff object by object. A migration number alone is
not sufficient evidence because an image can drift at the same migration.

Then update the Vaultwarden inventory fields, regenerate constants, review the
proxy policy, and run unit, integration, E2E, restore, and drill checks. The
official image digest, source tag/commit, migration, full schema fingerprint,
manifest tuple, local tag, and E2E expectation must move as one change.

Two small crypto/protocol fixtures exist because the official CLI exposes
neither account registration nor initial organization creation. Their shapes
are reviewed against the recorded Bitwarden client revision and Vaultwarden
endpoints; all subsequent vault and organization operations use the official
CLI. The fixtures are not a general client implementation.

## Bitwarden CLI checklist

The official CLI is a test oracle, not a runtime dependency. Review:

- CLI release notes and authentication/session changes;
- anonymous Send access-token issuance, persistence, expiry, and reuse (the
  current CLI caches a recipient token, so access-count assertions must use a
  fresh appdata directory for each recipient and explicitly configure its
  server before token issuance);
- create/edit/read behavior used by `tests/e2e`;
- item, folder, attachment, Send, organization, and collection JSON shapes;
- KDF, registration, user/org key generation, EncString, RSA encapsulation,
  and Node crypto changes relevant to the narrow fixture; and
- the nixpkgs package version actually selected by the flake.

If nixpkgs does not provide the exact audited CLI version, do not merely retag
the inventory. Either advance the flake input after reviewing its other package
changes or keep the existing CLI. The Nix toolchain check compares
`bw --version` with the inventory.

## Restic checklist

Restic is built from the exact source commit in the inventory. Review:

- snapshot, backup, restore, forget/prune, check, init, and unlock CLI behavior;
- JSON output schemas and exit-code semantics;
- S3 backend and credential behavior;
- host/path/tag filtering and full snapshot IDs;
- restore subfolder behavior and verification;
- cancellation, locking, cache, and output-size assumptions; and
- licensing and security advisories.

`nix/patches/restic-go-1.26.patch` is an ordered, reproducible patch against
the pinned unmodified tree. Recreate it in an ignored temporary checkout with
the pinned Go toolchain and supported Go module commands. Do not hand-edit
`go.sum` lines. Keep the patch limited to the toolchain and necessary
dependency updates; when upstream incorporates them, shrink or remove it.

Update `source_hash` with Nix fetch tooling and `vendor_hash` through
`buildGoModule`'s normal mismatch/report cycle. Verify the upstream release
archive SHA separately when the release publishes one. Then run the real
Moto/Restic round trip, E2E, vulnerability checks, and a bundle build.

## Go checklist

Go affects both product binaries and the patched Restic build. Review:

- release and security notes;
- compiler/linker, runtime, crypto, net, os, database/sql, and syscall changes;
- Linux/amd64 and CGO behavior for SQLite;
- `renameat2`, process cancellation, and race-detector behavior; and
- whether Restic now supports the candidate without a local patch.

Update the inventory URL/hash, the product `go` and `toolchain` directives
using `go mod edit`, and regenerate the Restic patch with Go tooling. The Nix
toolchain check must report the exact inventory version and
`GOTOOLCHAIN=local`.

## Moto checklist

Moto is test-only and locally packaged by Nix. Review S3 endpoint, bucket,
listing, object, multipart, and authentication behavior used by integration and
E2E. Confirm the nixpkgs Moto package version equals the inventory before
changing the local image tag. A green mocked test does not replace review of
real S3-compatible storage behavior.

## Required convergence checks

After changing one upstream:

```sh
nix run .#just -- generate-check
nix run .#just -- check
nix run .#just -- security
nix run .#just -- build
nix run .#just -- integration
nix run .#just -- e2e
nix run .#just -- bundle
```

Also perform a manual restore and drill when Vaultwarden, Restic, Go, schema,
filesystem publication, or Compose behavior changed.

Before accepting the change, search the entire tracked tree for every replaced
version, tag, digest, migration, package name, and route. Any remaining match
must be generated evidence, a deliberate rejection test, or required audit
context; do not retain a runtime compatibility path without a current contract.
