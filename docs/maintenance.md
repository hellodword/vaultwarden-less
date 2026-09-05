# Maintenance

Upstream changes are one-component reviews against exact source evidence, not
automatic updates to the newest tag. A candidate is unsupported until a clean
project commit records it, all generated contracts are reviewed, the pinned
Nix graph supplies it, and the relevant checks pass.

## Authoritative inputs

[UPSTREAMS.toml](../UPSTREAMS.toml) is the reviewed inventory for Vaultwarden,
the official Bitwarden CLI, Restic, Moto, and Go. It records repository, tag,
commit, and audited paths; Vaultwarden also records the accepted SQLite
migration and generated schema/projection paths.

`flake.nix` and the tool-generated `flake.lock` own the Nix dependency graph.
`go.mod` and the tool-generated `go.sum` own the Go module graph. Never edit a
dependency lockfile by hand. Change an authoritative input and use the declared
dependency tool, then review the complete generated diff. A shared Nix input
can move several inventoried components, so inspect every one before accepting
the change.

Resolve changing facts from current official release notes, security notices,
source repositories, registries, and local package output. Do not rely on
remembered versions, flags, advisories, or maintenance status.

## Upstream diff

Start from a clean passing commit and generate the ignored report for exactly
one candidate:

```sh
git status --short --branch
just check
nix develop --command python3 scripts/upstream_diff.py \
  vaultwarden <candidate-tag>
```

The allowed component names are `vaultwarden`, `bitwarden-cli`, `restic`,
`moto`, and `go`. The script verifies the expected origin, resolves current and
candidate tags to commits, and records the complete name-status change plus the
configured audit-path subset under an ignored temporary path. The report is an
index for human source review, not approval.

Review official release/security notes, provenance, license changes, the full
source diff, and every path selected by `audit_paths`. Do not mix an upstream
change with unrelated cleanup, configuration redesign, or formatting churn.

## Vaultwarden schema and projection

Vaultwarden is the durable-data and API boundary. Review every intervening
SQLite migration; tables, columns, indexes, triggers, and migration records;
attachment and file-Send models and paths; API write routes and ignored proxy
routes; registration/authentication behavior; environment defaults; signals;
filesystem ownership; Web Vault exclusion; and distribution obligations.

Build the candidate only through the declared Nix input. Start it against a
new ignored private directory and let it create a fresh SQLite database. With
that process stopped, capture the deterministic schema:

```sh
nix develop --command python3 scripts/capture_vaultwarden_schema.py \
  --database /absolute/path/to/fresh/db.sqlite3 \
  --output internal/upstream/vaultwarden-schema.json
```

The script opens SQLite read-only with WAL awareness, checks integrity and the
recorded migration, and writes deterministic JSON. Review every generated
change. Update the tag, commit, migration, complete schema, and
`internal/upstream/recovery-projection.json` as one coherent unit. The
projection is reviewed policy, not generator output: each included/excluded
table and column needs source evidence, and attachment/Send closure must remain
complete.

The two JSON contracts are tracked generated artifacts because builds and
tests consume them. Temporary databases, source checkouts, reports, logs, and
captures remain ignored and must be removed after review. Never inspect an
unknown local database; only the newly created test database belongs in this
workflow.

## Component review boundaries

For the official Bitwarden CLI, review the server-selection, login, unlock,
sync, item/folder, attachment, and Send commands used by the two E2E scenarios,
plus the PBKDF2/HKDF, RSA-OAEP, and AES-CBC-HMAC behavior required by the narrow
registration helper. The CLI is test tooling, not a production dependency.

For Restic, review the unmodified `init`, `backup`, `snapshots`, `check`,
`unlock`, `forget --prune`, and `restore` arguments and JSON; format-2
repositories; S3 behavior; identity tags; full snapshot IDs; subprocess
cancellation; output bounds; and license/security changes. The project never
runs repository-format migration implicitly.

For Moto, review only the S3 server behavior used by tests: endpoint startup,
bucket creation, list/read/write/delete, multipart behavior, error responses,
and cleanup. Moto is excluded from release output and proves no real-provider
IAM or durability claim.

For Go, review compiler/runtime, crypto, networking, `database/sql`, filesystem,
process, syscall, CGO SQLite, race-detector, and module-requirement changes.
The pinned Nix Go version, `go.mod`, and `UPSTREAMS.toml` must agree.

## Validation

Run the narrowest affected tests first. The ordinary project gate is:

```sh
just check
```

It runs formatting (gofmt, nixfmt, and Ruff), `go test ./... -count=1`,
`go vet ./...`, worker/proxy race tests, both maintenance-script suites, and
Nix assertions tying packages to the reviewed inventory.

From one unchanged clean commit on an `x86_64-linux` KVM host, run the formal
release acceptance test:

```sh
just check-release
```

This is exactly `nix build --no-link -L .#release-test`. It builds the release
archives, the test-only Moto image, the Go E2E executable, and a disposable
NixOS guest. Inside that guest, a non-root user and the guest's local Docker
daemon run exactly
`TestTargetlessDeploymentAndFullDataAdoption` and
`TestTwoTargetBackupRestore` against the three archives from that same release
bundle and the official Bitwarden CLI. Their mandatory bind canary must pass,
and the scenarios dynamically create all configuration, credentials, TLS CA,
Moto endpoints, client application data, and runtime resources before
verifying cleanup.

The VM test is intentionally absent from `just check`, the default package, and
the release bundle. A machine without KVM must report the missing feature; do
not replace the gate with TCG emulation, a remote Docker daemon, an SSH-driven
distribution exercise, or an independently built set of images.

Build the strict release from the same clean commit:

```sh
just release
(cd result && sha256sum --check SHA256SUMS)
```

Require the exact allowlisted file set, standard loading of all three image
archives, one shared `git-<full-commit>` tag, matching OCI version/revision
labels, and absence of source, tests, Moto, private data, schemas, scripts, and
extra documentation. Release building does not run or consume other evidence.

## Final review

Before committing one upstream unit:

- inspect status and both staged and unstaged diffs;
- confirm every external version and revision with authoritative evidence;
- review generated-artifact ownership and the complete tool-generated lock
  diff, if any;
- search code, tests, templates, and documentation for the replaced identity
  and obsolete assumptions;
- classify failures as introduced, pre-existing, or environment-caused and
  rerun the narrowest failing command;
- remove ignored reports, databases, logs, releases, client state, and runtime
  resources; and
- stage only explicit task paths.

Record the exact project commit, upstream before/after identities, evidence
consulted, generated files, commands and results, unverified external
boundaries, and cleanup. Do not claim compatibility from a version string, a
compiled-but-skipped scenario, Moto, or a test framework that did not execute
the required path.
