# Upgrade and compatibility policy

## Project versions

The project starts at `1.0.0` and follows SemVer:

- patch: compatible fixes, supported Go security-line updates, reachable
  dependency/SQLite fixes, and base-image remediation;
- minor: backward-compatible features and optional configuration additions;
- major: an intentionally breaking public configuration, CLI, protocol, or
  persisted-format contract.

The v1 recovery-point reader is long-lived. A future writer may add v2 but must
continue restoring v1. Stable status JSON remains at `schema_version: 1` until
a separately documented version is introduced.

The pre-1.0 `trigger.json`, SQL/Git backup scripts, and CLI are not v1 inputs.
They are not automatically deleted or converted. Old remote snapshots remain
outside v1 maintenance filters.

## Configuration and state migration

Configuration migration reads an old file and writes a new path. It never
overwrites the operator's only configuration in place. The private YAML and
credential files remain outside recovery points.

Worker and notification state are versioned and atomically replaced. A state
version that cannot be validated or migrated is preserved unchanged and stops
startup. Recovery then uses verified remote v1 points and an explicit operator
procedure; startup never silently resets state or initializes a repository.

## Vaultwarden upgrade gate

Before changing the Vaultwarden image tag:

1. Review the exact upstream release notes, database migrations, configuration
   changes, client compatibility, and license/security impact.
2. Require every configured target to hold the same newest `complete` content
   ID.
3. Run repository checks and a full isolated restore with application-level
   login, sync, and attachment validation.
4. Record the exact pre-upgrade snapshot IDs and retain the compatible old
   application image.

After upgrade, verify process health, login, client sync, attachment download,
the intended disabled features, a new complete backup on every target, and a
new isolated restore.

If rollback requires the old Vaultwarden version, stop service and restore the
pre-upgrade recovery point. Never ask an older binary to downgrade a database
already migrated by the newer release.

Vaultwarden `1.37.2` is fixed in v1 because its upstream release states that it
is required for clients `2026.8.0+`: [release
1.37.2](https://github.com/dani-garcia/vaultwarden/releases/tag/1.37.2).

## Restic upgrade gate

Restic is copied from `ghcr.io/restic/restic:0.19.1`; runtime verifies that
exact version. There is no self-update path.

Before a binary change, use the old binary to check all repositories and
complete a restore. Test the candidate against every repository, repeat the
restore and manifest/application validation, and confirm backend/credential
behavior. A repository-format migration is a separate destructive maintenance
decision requiring full check, drill, rollback plan, and an explicit upstream
command. It is never automatic.

## Go, SQLite, dependencies, and images

Production builds use Go `1.26.7`, `go-sqlite3 v1.14.50`, Cobra `v1.10.2`,
and goccy/go-yaml `v1.19.2`.
For every update:

- verify the official release/tag and module checksum;
- review transitive changes, licenses, build/install behavior, and telemetry;
- run focused tests, `go test ./...`, race tests, vet, and `govulncheck`;
- rebuild the supported `linux/amd64` images and attribute scanner findings;
- repeat the local Restic init→backup→check→restore→manifest/SQLite/hash loop.

Version-tagged images are not digest-pinned by project policy. Before rollout,
record the resolved registry digests in the change evidence so an unexpected
tag movement can be detected and the prior artifact can be recovered.

## Accepted boundaries

An upgrade does not operate on real `/data`, initialize/clean a real remote
repository, publish images, deploy services, or delete legacy histories by
itself. Those are distinct operator-authorized actions with their own recovery
paths.
