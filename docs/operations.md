# Operations

All supported operator actions start with `nix run .#just --`. Nix provides
the pinned tools; Just selects one operation; the leaf recipe enters the dev
shell once. Do not invoke the shipped scripts or Compose topology as a second
public interface.

## Development and release gates

```text
fmt             format repository-owned files
fmt-check       verify formatting
test            run Go tests
race            run Go tests with the race detector
vet             run go vet
lint            run golangci-lint
build           build and reproducibility-check both binaries
image           build the local Linux/amd64 image
compose-check   render and validate Compose
integration     run the real Moto/Restic round trip
e2e             run the shipped Compose backup/restore scenario
ci-fast         aggregate local quality gates
ci-full         add integration and E2E
```

`nix flake check` verifies only flake, formatter, and toolchain wiring. It is
not the product quality gate. `VERSION` is the sole product version source;
release tags must exactly equal `v$(cat VERSION)`. Releases publish one
Linux/amd64 GHCR image after full validation. The project intentionally does
not add multi-architecture output, SBOMs, signatures, vulnerability scanning,
or automatic dependency updates.

## Routine deployment

```sh
nix run .#just -- init
nix run .#just -- config-validate
nix run .#just -- repo-init primary
nix run .#just -- up
nix run .#just -- ps
nix run .#just -- logs
```

`init` is safe to repeat: it verifies existing private files and creates only
missing ones. Back up `.env.vaultwarden`, `backup.yaml`, role credentials, and
the Restic password outside the Vaultwarden recovery repository under an
appropriate private-access policy. `config-validate` checks the resident
backup role; each repository or restore command independently validates the
maintenance role before contacting Restic.

The proxy listens only on loopback. Terminate TLS and strip any external path
prefix in Nginx/CDN, replace untrusted forwarding headers there, and forward
root-path HTTP and WebSocket traffic to the proxy. `down` stops Compose without
deleting source or restored data.

## Backup health

```sh
nix run .#just -- request-backup
nix run .#just -- status
```

`request-backup` acknowledges an in-memory demand; it does not promise that a
snapshot has already been created. `/healthz` and CLI healthchecks mean only
that their HTTP process is serving. They do not test SQLite, Restic, S3, or
notifications. Status is lost on restart; `repo-snapshots` is the persistent
repository view.

The scheduler may defer a normal event for quieting and the five-minute
minimum check-start interval. If proxy and worker both fail before delivery,
the startup check and the no-demand 12-hour fallback are the accepted maximum
recovery mechanisms. Slow or failed early targets delay later ordered targets;
a recovered target uploads only the newest point.

## Repository maintenance

Commands take the target name as an optional Just argument, defaulting to
`primary`:

```sh
nix run .#just -- repo-snapshots primary
nix run .#just -- repo-check primary
nix run .#just -- repo-check primary 1/12
nix run .#just -- repo-unlock primary
nix run .#just -- repo-prune primary
nix run .#just -- repo-prune-apply primary
```

The first prune command is a Restic dry-run; only `repo-prune-apply` deletes
according to the fixed retention policy. Both are restricted to the new host,
path, and product tag. Earlier product snapshots are never automatically
deleted and require a separate, explicit operator decision.

## Restore and drill

Restore publishes only a fully verified absent output under `/restore`:

```sh
nix run .#just -- restore primary /restore/drill
nix run .#just -- restore-snapshot FULL_64_HEX_ID primary /restore/old
nix run .#just -- drill-up
nix run .#just -- drill-status
nix run .#just -- drill-down
```

The default selects the latest new-product snapshot. It never falls back when
that snapshot is corrupt or invalid; select an older full snapshot ID
explicitly. The output must not exist, and its real parent directory must be
mode 0700. Restic restores into a private sibling, verification runs once, and
an atomic rename publishes the result. Failure cleans the sibling and leaves
the requested output absent. There is no overwrite, incomplete mode, output
reuse, or post-rename power-loss durability guarantee.

The drill uses the same pinned Vaultwarden image on isolated networks and a
separate loopback port. `drill-down` stops it but deliberately retains restored
data. Restored RSA material is absent: Vaultwarden creates a new key and old
access, refresh, invite, attachment, and download tokens may all be invalid.
Verify login plus item, folder, attachment, file Send, organization, and
collection access with fresh sessions.

Run a full manual drill at initial deployment, at least quarterly, after every
Vaultwarden upgrade, and after storage migration. Vaultwarden upgrades must
review and change together: the Compose image tag/digest, supported
version/migration tuple, proxy route audit, tests, and documentation.

## Failure and recovery limits

- The live `/data` mount is read-only to the worker; backups never pause,
  modify, or delete production Vaultwarden data.
- Staging is rebuilt after crashes and is not persistent control state.
- Target histories may differ because failures catch up only to the newest
  recovery point.
- Notifications are best effort and can be lost at process shutdown; snapshots
  and structured logs are authoritative.
- Restore refuses an existing destination. Removing or replacing real restore
  data is a separate destructive operation requiring an exact operator choice.
