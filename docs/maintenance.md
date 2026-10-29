# Maintenance

This page is for source maintainers. Target-machine setup is covered by
[deployment](deployment.md); operators use native Docker/Compose and restic.

## Source workflow

Run Just from the checkout root. The recipes call Nix, which selects pinned
tools, source inputs and build artifacts before invoking a script. Scripts
never build or query Nix outputs themselves. If Just is not already available,
enter `nix develop` once to get the repository tools, then use the same recipes.

| Just command | Nix boundary | Work performed |
| --- | --- | --- |
| `just check` | `nix flake check` | Formatting, unit/integration tests, vet, race and script tests |
| `just release` | `nix build .#release` | Three product images and deployment files in `result` |
| `just check-release [args]` | `nix run .#check-release` | Build release/runner inputs, then run `scripts/check_release.py` |
| `just routes [args]` | `nix run .#routes` | Generate the catalog from the selected Vaultwarden source |
| `just upstream-diff COMPONENT REF [args]` | `nix run .#upstream-diff-COMPONENT` | Compare that component's selected source against a Git ref |

Optional arguments are forwarded as individual arguments, including paths with
spaces. For example, select another local socket and shared work directory with:

```sh
just check-release --work-dir /absolute/shared/work \
  --docker-socket /run/user/1000/docker.sock
```

The default work directories are ignored paths under
`tmp/agent/`, relative to the invoking checkout. Docker bind paths containing
commas are rejected because Docker uses commas as mount-field separators.

Nix wrappers live in [nix/apps.nix](../nix/apps.nix). Each upstream audit app
references only its own baseline, so auditing Go does not fetch Vaultwarden or
Moto sources. Container paths such as `/data`, `/cache`, `/config` and
`/run/credentials` are product mount contracts; host source, work and socket
paths are explicit inputs. The acceptance harness reads product image references
and restored service environment from rendered Compose. Test image references
are injected by Nix, where those images are built.

## Dependencies

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

Run a candidate audit from the checkout:

```sh
just upstream-diff vaultwarden <candidate-git-ref>
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
just routes
# Optional review copy, without changing the tracked catalog:
just routes --output tmp/agent/routes-candidate.txt
```

Nix supplies the source path selected by the flake. Review the generated diff
and check that a second generation is identical. The exclusion list and content
policy are
reviewed code, not generated database schema or recovery definitions.

For restic, review native backup/snapshot/tag JSON, forced content reads, S3 behavior,
restore and maintenance flags, output bounds and cancellation. For Go, review
networking, TLS/HTTP2, cryptography, filesystem behavior, subprocesses and race
tests. Consult official sources for changing external facts and security
notices.

## Validation and acceptance

```sh
just check
just release
just check-release
```

`check` is `nix flake check`: formatting, Go tests, vet, race tests and
Python script tests. Ordinary checks have no Docker tool or daemon dependency.
Product builds disable CGO; race checking uses the
development toolchain. Native SQLite/restic integration tests run in ordinary
checks. Tests use a fake clock for long scheduling and notification retry
policies.

`release` builds the three images, Compose, templates and documentation.
`check-release` builds that bundle and `test-runner-image`, then runs the
packaged suite on the current local Docker socket. It requires a non-root
runtime user with Docker access. The wrapper supplies artifact paths and the
runner image reference to the script; there is no nested Nix invocation. A
private Docker config and explicit Unix socket isolate the run from ambient
Docker contexts, TLS settings and Compose configuration. It reports a work directory containing
results and synthetic diagnostic data. Keep useful failed-run artifacts for
reproduction.

To use a Docker-only isolated host, transfer the deployment bundle and the
test-runner image archive, load the runner, and run it with host networking:

```sh
set -euo pipefail
: "${RUNNER_ARCHIVE:?}" "${RUNNER_IMAGE:?}" "${RELEASE_DIR:?}" "${WORK_DIR:?}"
DOCKER_SOCKET=${DOCKER_SOCKET:-/var/run/docker.sock}
mkdir -m 0700 -- "$WORK_DIR/docker-config"
printf '%s\n' '{"auths":{}}' > "$WORK_DIR/docker-config/config.json"
unset DOCKER_HOST DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_AUTH_CONFIG
export DOCKER_CONFIG="$WORK_DIR/docker-config"
docker --host "unix://$DOCKER_SOCKET" load --input "$RUNNER_ARCHIVE"
docker --host "unix://$DOCKER_SOCKET" run --rm --pull never --network host --user "$(id -u):$(id -g)" \
  --group-add "$(stat -c %g "$DOCKER_SOCKET")" \
  --mount "type=bind,source=$DOCKER_SOCKET,target=$DOCKER_SOCKET" \
  --mount "type=bind,source=$RELEASE_DIR,target=$RELEASE_DIR,readonly" \
  --mount "type=bind,source=$WORK_DIR,target=$WORK_DIR" \
  "$RUNNER_IMAGE" \
  --release-dir "$RELEASE_DIR" --work-dir "$WORK_DIR" --docker-socket "$DOCKER_SOCKET"
```

Set `RUNNER_IMAGE` to the reference reported by `docker load` for the transferred
runner archive. Create the private `WORK_DIR` beforehand. Use absolute paths.
The runner and host must see the work directory and socket at the
same path; a bind canary verifies that boundary before product startup.
The runner includes compiled tests, official Bitwarden CLI, Docker CLI, native
restic/SQLite tools and its Moto archive. The host needs no Nix installation.
The suite timeout is 75 minutes.

The suite first verifies the shipped security environment and rejection of
registration and Send creation. It uses explicit Compose overrides only for
test enrollment and subsequent Send coverage; it leaves the template unchanged.
Restored services use the original profile, including disabled Send creation,
while verifying existing Sends remain readable.

The suite exercises client login, sync, editing, attachments, Sends, request
merging, two destinations, incomplete capture and repair, restart and cache
loss, and native restore from both repositories followed by client reads.
Four notification peers verify payloads, signing, explicit failures and retry
recovery. Docker DNS and a test CA route Apple's production hostname to the
HTTP/2 peer without a production endpoint setting.

The real-image suite also captures a 192 MiB incompressible attachment, removes
native snapshots, deletes and corrupts synthetic object-store packs, verifies
fault persistence across successful uploads, and imports a native archive with
WAL/SHM, keys, attachments, Sends and inactive config.json. It executes the
documented import commands and restores both repositories with native restic.
Persistent deadlines, the six-hour fallback, 24-hour single verification retry,
30-day full-read interval, daily catch-up and reminder policies use a controlled
clock in the ordinary checks; acceptance uses manual jobs for actual I/O.

Acceptance does not establish real provider durability/IAM or delivery to
external robot groups and Apple devices. Report those boundaries explicitly.
Before any authorized commit, review the complete status and diff, retain
needed diagnostics outside tracked inputs, and stage only explicit task paths.
