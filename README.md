# vaultwarden-less

`vaultwarden-less` is a Linux/amd64 runtime and recovery toolkit for one pinned
Vaultwarden release. It runs Vaultwarden behind a loopback-only, write-aware
proxy and builds verified SQLite recovery points for zero or more Restic
targets. Recovery points include the database plus referenced attachments and
file Sends; they deliberately exclude Vaultwarden RSA keys.

The project owns the Vaultwarden, backup, and proxy containers; consistent
recovery-point construction; multi-target synchronization; status and manual
backup requests; repository maintenance; and verified restore outputs. TLS,
public access control, provider administration, existing-data adoption,
release transitions, production-data replacement, publication, and
redistribution compliance remain operator responsibilities.

The exact supported behavior is frozen in [Product contracts](docs/contracts.md).
This is an early-project breaking redesign: older CLI, configuration, Compose,
and state-document formats have no compatibility layer. Recovery manifests,
identity tags, and Restic repository format remain stable.

## Build and verification

Use the pinned Nix environment; do not install project tooling globally.

```sh
just check
just check-release
just release
```

- `just check` runs the ordinary flake checks: formatting, all ordinary Go
  tests, `go vet`, worker/proxy race tests, maintenance-script tests, and
  upstream pin assertions.
- `just check-release` runs the two release-level Docker E2E scenarios. The Go
  test process and Docker daemon must share bind-mount paths; otherwise the
  mandatory canary fails rather than skipping.
- `just release` is exactly `nix build .#release`. It accepts only a clean Git
  flake and creates the standard `result` link for a bundle identified as
  `git-<full-commit>`.

## Documentation

- [Configuration](docs/configuration.md) — deployment tree, permissions,
  target and notifier YAML, AWS/B2 inputs, and targetless configuration.
- [Operations](docs/operations.md) — release transfer, startup, official-client
  enrollment, data adoption, repository work, restore, exercises, production
  recovery, and release transitions.
- [Product contracts](docs/contracts.md) — the sole normative public contract.
- [Maintenance](docs/maintenance.md) — upstream pins, schema capture, diff
  review, generated contracts, and maintainer checks.
- [NOTICE](NOTICE.md) — licensing and the included Vaultwarden image's AGPL
  corresponding-source obligation.

The reviewed component inventory is [UPSTREAMS.toml](UPSTREAMS.toml). The
deployment host needs the release bundle, Docker Engine, the Docker Compose
plugin, checksum tools, and ordinary file-copy tools; it does not need this
source tree, Nix, Go, Python, or Just.
