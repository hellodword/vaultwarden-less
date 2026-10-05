# Maintenance

`flake.lock` owns the selected nixpkgs and formatting inputs.
[nix/upstreams.nix](../nix/upstreams.nix) owns the compatible Bitwarden CLI,
Moto and AWS SDK pins and their fetch hashes. Vaultwarden, restic and SQLite
come directly from the selected package set. `go.mod` and tool-generated
`go.sum` own the Go module graph. Use dependency tools to update their
lockfiles, then inspect the generated differences.

The official Bitwarden CLI is acceptance tooling. Its pin preserves the login
protocol supported by the selected Vaultwarden. Moto and its aligned AWS SDK
are test-only dependencies. Their upstream tests remain enabled.

## Upstream review

Run a candidate audit within the development shell:

```sh
nix develop --command python3 scripts/upstream_diff.py vaultwarden <candidate-git-ref>
```

The supported components are `vaultwarden`, `bitwarden-cli`, `restic`,
`moto` and `go`. The baseline comes from the source actually selected by the
flake. The candidate is fetched by Git ref. The command prints a source diff
for the component's relevant paths and does not change dependencies or
introduce an upgrade or runtime admission check.

For Vaultwarden, review routes and handlers, models and SQLite migrations,
`src/config.rs`, `.env.template` and `.env.example`. Revisit request
exclusions and business comparison policy using the relevant handler/model
semantics. An unknown route, table, column or JSON shape is conservatively
included at runtime.

The route catalog is the one generated classification asset:

```sh
nix develop --command python3 scripts/routes.py \
  /absolute/path/to/selected/vaultwarden/source \
  --output internal/routes/catalog.txt
```

Use the source path selected by the flake, review its diff, and check that a
second generation is identical. The exclusion list and content policy are
reviewed code, not generated database schema or recovery definitions.

For restic, review native backup/snapshot JSON, parent selection, S3 behavior,
restore and maintenance flags, output bounds and cancellation. For Go, review
networking, TLS/HTTP2, cryptography, filesystem behavior, subprocesses and race
tests. Consult official sources for changing external facts and security
notices.

## Validation and acceptance

```sh
nix develop --command just check
nix develop --command just release
nix develop --command just check-release
```

`check` is `nix flake check`: formatting, Go tests, vet, race tests and
Python script tests. Product builds disable CGO; race checking uses the
development toolchain. Native SQLite/restic integration tests run in ordinary
checks. Tests use a fake clock for long scheduling and notification retry
policies.

`release` builds the three images, Compose, templates and documentation.
`check-release` builds that bundle and `test-runner-image`, then runs the
packaged suite on the current local Docker socket. It requires a non-root
runtime user with Docker access. It reports an ignored work directory containing
results and synthetic diagnostic data. Keep useful failed-run artifacts for
reproduction.

To use a Docker-only isolated host, transfer the deployment bundle and the
test-runner image archive, load the runner, and run it with host networking:

```sh
set -euo pipefail
docker load --input "$RUNNER_ARCHIVE"
docker run --rm --network host --user "$(id -u):$(id -g)" \
  --group-add "$(stat -c %g /var/run/docker.sock)" \
  --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
  --mount "type=bind,source=$RELEASE_DIR,target=$RELEASE_DIR,readonly" \
  --mount "type=bind,source=$WORK_DIR,target=$WORK_DIR" \
  vaultwarden-less-test-runner:local \
  --release-dir "$RELEASE_DIR" --work-dir "$WORK_DIR"
```

Use absolute paths. The runner and host must see the work directory at the
same path; a bind canary verifies that boundary before product startup.
The runner includes compiled tests, official Bitwarden CLI, Docker CLI, native
restic/SQLite tools and its Moto archive. The host needs no Nix installation.
The suite timeout is 75 minutes.

The suite exercises client login, sync, editing, attachments, Sends, request
merging, two destinations, incomplete capture and repair, restart and cache
loss, and native restore from both repositories followed by client reads.
Four notification peers verify payloads, signing, explicit failures and retry
recovery. Docker DNS and a test CA route Apple's production hostname to the
HTTP/2 peer without a production endpoint setting.

Acceptance does not establish real provider durability/IAM or delivery to
external robot groups and Apple devices. Report those boundaries explicitly.
Before any authorized commit, review the complete status and diff, retain
needed diagnostics outside tracked inputs, and stage only explicit task paths.
