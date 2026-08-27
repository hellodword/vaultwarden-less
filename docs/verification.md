# Verification evidence and acceptance

The project has two deliberately different test modes. `test`, `race`, `vet`,
and `check` must be fast and require no daemon. External tests are compiled on
every ordinary `go test`; their first statement checks an explicit runtime
flag and skips before looking up tools, building images, contacting Docker, or
opening ports.

`integration` and `e2e` are different: they pass the flag and route
`go test -json` through `scripts/verify_go_test.py`. The verifier fails on a
nonzero command, malformed non-JSON output, any skipped test, any fail event,
or a missing exact top-level PASS. A successful shell command alone is not
accepted as evidence.

## Local gates

```sh
nix run .#just -- check
nix run .#just -- security
nix run .#just -- build
```

`check` covers generated-file equality, formatting, all Go/Python tests, race,
vet, GolangCI-Lint, Ruff, production Compose rendering for every profile, and
all flake checks. The SQLite suite includes an online backup of a database at
least 32 MiB while a WAL writer commits bounded multi-row marker transactions.
Acceptance requires a non-torn marker, integrity/FK success, exact schema and
migration, DELETE journal mode, and no WAL/SHM/journal sidecars.

## Real Moto and patched-Restic integration

```sh
nix run .#just -- integration
```

The required `TestMotoResticIntegration` PASS proves, using the Nix-built
patched Restic executable and locally built pinned Moto image:

1. exact Restic version, Docker availability, bucket creation, and repository
   init;
2. real backup, content-ID deduplication, a changed second snapshot, bounded
   listing, repository check, and unlock;
3. latest and explicit earlier restore plus full recovery-tree verification;
4. refusal to overwrite an output;
5. failure on a deliberately invalid latest snapshot without fallback, while
   explicit earlier selection still works;
6. four controlled historical in-scope snapshots and one differently hosted
   out-of-scope snapshot;
7. a nonempty exact dry-run removal set consisting only of controlled in-scope
   IDs, the identical apply set, per-ID deletion/retention checks, preservation
   of the out-of-scope ID, and a final real repository check.

Moto validates the actual Restic/S3 command boundary, not the behavior of a
production S3 vendor. A deployment must separately test its configured store.

## Real Vaultwarden lifecycle E2E

```sh
nix run .#just -- e2e
```

The required `TestVaultwardenBackupRestoreE2E` PASS uses the exact audited
Vaultwarden image, locally built proxy/worker/Moto images, patched Restic, and
the exact Nix Bitwarden CLI. There are no build tags or internal environment
variables. Test Compose uses project-scoped labeled volumes and one explicit
seed service; cleanup must leave no project containers, networks, or volumes.

Acceptance is exactly two recovery snapshots:

1. Register two accounts and create the initial organization through the two
   narrow protocol fixtures that the CLI does not expose. Then use the real
   CLI for login, sync, organization read, owner member listing, the second
   collection, an organization item, a folder and personal item, a second
   user's item, attachment, and ordinary file Send.
2. Hide that Send payload, start the worker, and observe zero snapshots for at
   least 35 seconds while the database references an unavailable file. Restore
   it and require snapshot 1.
3. Hide the same payload again. In that single inconsistent window, edit the
   item password, create a file Send with `maxAccessCount=1`, download it once
   through a fresh isolated recipient CLI, observe the server-side count, and
   prove a second fresh recipient cannot create any output. Fresh recipients
   are required because the CLI deliberately caches a two-minute access token
   for one recipient session. The repository must remain at one point. Restore
   the held payload and require snapshot 2, with a different content ID and no
   intermediate point.
4. Run real repository check, then prune dry-run and apply. Both must retain the
   exact two IDs and select none for removal. Restart the worker and require an
   unchanged check with the count still two.
5. Restore latest, stop production, and start a drill from the disposable copy.
   The pre-restore authenticated CLI session must fail. Fresh real-CLI sessions
   verify organization, both collections, organization item, folder, updated
   item, attachment bytes, ordinary Send bytes, exhausted access-limited Send,
   and the second user's item.
6. Restore snapshot 1 by its full ID, start a second drill, and verify the
   earlier item password through a fresh real CLI.

The CLI subprocess environment is exactly isolated `HOME`, `TMPDIR`,
`BITWARDENCLI_APPDATA_DIR`, `NODE_EXTRA_CA_CERTS`, and optional `BW_SESSION`.
Master passwords are supplied only with a private 0600 `--passwordfile`;
diagnostics redact passwords and sessions. Every isolated recipient also runs
`bw config server` explicitly because the Send URL does not configure the CLI
SDK endpoint used to request its access token.

## Explicit test ports

The real test commands accept optional fixed loopback ports. Nonzero values
must be distinct and within `1024..65535`; omit them for ephemeral ports.

Expected final evidence includes:

```text
go test verification passed: TestMotoResticIntegration
go test verification passed: TestVaultwardenBackupRestoreE2E
```

Do not expose verification ports publicly.

## Offline bundle and real-device acceptance

Bundle creation requires a clean commit so source, pins, image IDs, SBOMs, and
checksums describe one reviewable state:

```sh
nix run .#just -- bundle
nix run .#just -- bundle-load dist/vaultwarden-less-<version>-linux-amd64
nix run .#just -- bundle-verify dist/vaultwarden-less-<version>-linux-amd64
```

`bundle-load` verifies the archive checksum, runs `docker load`, and compares
all loaded IDs. This proves transfer, not credentials or storage. Before
accepting a deployment or relevant upstream upgrade, also initialize a fresh
deployment, validate its configuration, run a real backup, repository check,
latest restore, explicit earlier restore, and a manual drill on the actual target
and actual S3-compatible service.
