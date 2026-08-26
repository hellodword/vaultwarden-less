set shell := ["bash", "-euo", "pipefail", "-c"]
set positional-arguments := true

version := trim(read("VERSION"))
system := "x86_64-linux"
compose := "docker compose --env-file .env --file deploy/compose.yml"
deploy := "nix develop .#deploy --command"

# Regenerate tracked upstream constants from audited inputs.
generate:
    nix develop --command python3 scripts/generate_upstreams.py
    nix develop --command gofmt -w internal/upstream/generated.go

# Prove tracked generated files are current.
generate-check:
    nix build --no-link .#checks.{{system}}.generated

# Format repository-owned source files.
fmt:
    nix fmt

# Check formatting without changing files.
fmt-check:
    nix build --no-link .#checks.{{system}}.format

# Run the Go test suite.
test:
    nix develop --command go test ./...
    nix develop --command python3 -m unittest discover --start-directory tests/scripts --pattern 'test_*.py'

# Run concurrency-sensitive tests with the race detector.
race:
    nix develop --command go test -race ./...

# Run Go static analysis.
vet:
    nix develop --command go vet ./...

# Run the configured Go and Python linters.
lint:
    nix develop --command golangci-lint run
    nix develop --command ruff check scripts tests/scripts
    nix develop --command ruff format --check scripts tests/scripts

# Render and validate shipped Compose topology.
compose-check:
    {{deploy}} env DEPLOY_UID=65532 DEPLOY_GID=65532 DEPLOY_ROOT=/tmp/vaultwarden-less-compose-check PRODUCT_VERSION={{version}} BACKUP_IMAGE=vaultwarden-less-backup:{{version}} PROXY_IMAGE=vaultwarden-less-proxy:{{version}} VAULTWARDEN_IMAGE=vaultwarden-less-vaultwarden:contract VAULTWARDEN_ENV_FILE=/dev/null DRILL_RECOVERY_PATH=/tmp/vaultwarden-less-compose-check/restore/prepared MOTO_IMAGE=vaultwarden-less-moto:contract E2E_PROJECT=vaultwarden-less-compose-check E2E_PUBLISH_ADDRESS=127.0.0.1 docker compose --env-file /dev/null --file deploy/compose.yml --file tests/e2e/compose.yml --profile maintenance --profile restore --profile drill config --quiet

# Run the normal local quality gate.
check: generate-check fmt-check test race vet lint compose-check
    nix flake check

# Build binaries, patched Restic, and every local image with Nix.
build:
    nix build --no-link .#backup .#proxy .#restic-patched .#backup-image .#proxy-image .#moto-image

# Run dependency vulnerability checks.
security:
    nix develop --command govulncheck ./...

# Run the real Moto/Restic integration boundary.
integration moto_host_port="0":
    nix develop --command go test -tags=systemtest ./internal/restore -run '^TestMotoResticRoundTrip$' -count=1 -timeout=15m -args -moto-host-port "$1"

# Run the shipped Vaultwarden backup/restore scenario.
e2e proxy_host_port="0" drill_host_port="0":
    nix develop --command env VWLESS_E2E_PROXY_HOST_PORT="$1" VWLESS_E2E_DRILL_HOST_PORT="$2" go test -tags=systemtest ./tests/e2e -run '^TestVaultwardenBackupRestoreE2E$' -count=1 -timeout=45m

# Run every local gate, including Docker integration boundaries.
check-full: check security build integration e2e

# Create a clean-commit, linux/amd64 offline deployment bundle.
bundle:
    nix develop --command python3 scripts/bundle.py create --output-root dist

# Verify checksums, load all bundle images, and compare image IDs.
bundle-load bundle:
    {{deploy}} python3 scripts/bundle.py load "$1"

# Compare currently loaded local images with bundle metadata.
bundle-verify bundle:
    {{deploy}} python3 scripts/bundle.py verify "$1"

# Create missing private deployment files and directories.
init:
    {{deploy}} python3 scripts/init.py --root . --version-file VERSION --upstreams UPSTREAMS.toml --vaultwarden-template deploy/vaultwarden.env.template

# Start the production topology using only preloaded local images.
up:
    {{deploy}} {{compose}} up --detach

# Stop production services without deleting recovery data.
down:
    {{deploy}} {{compose}} down

# Show production service status.
ps:
    {{deploy}} {{compose}} ps

# Follow production service logs.
logs:
    {{deploy}} {{compose}} logs --follow

# Request a worker backup check.
request-backup:
    {{deploy}} {{compose}} exec --no-TTY backup vaultwarden-less-backup request-backup

# Read the worker's in-memory status.
status:
    {{deploy}} {{compose}} exec --no-TTY backup vaultwarden-less-backup status

# Validate private product configuration and all credential boundaries.
config-validate:
    {{deploy}} {{compose}} run --rm --no-deps backup config validate --config /config/backup.yaml

# Initialize one configured Restic repository.
repo-init target="primary":
    {{deploy}} {{compose}} --profile maintenance run --rm --no-deps maintenance repo init --target "$1" --config /config/backup.yaml

# List a bounded number of recovery snapshots.
repo-snapshots target="primary" limit="100":
    {{deploy}} {{compose}} --profile maintenance run --rm --no-deps maintenance repo snapshots --target "$1" --limit "$2" --config /config/backup.yaml

# Check one Restic repository.
repo-check target="primary" subset="":
    {{deploy}} {{compose}} --profile maintenance run --rm --no-deps maintenance repo check --target "$1" --read-data-subset "$2" --config /config/backup.yaml

# Unlock one Restic repository.
repo-unlock target="primary":
    {{deploy}} {{compose}} --profile maintenance run --rm --no-deps maintenance repo unlock --target "$1" --config /config/backup.yaml

# Preview fixed retention changes.
repo-prune target="primary":
    {{deploy}} {{compose}} --profile maintenance run --rm --no-deps maintenance repo prune --target "$1" --config /config/backup.yaml

# Apply fixed retention changes.
repo-prune-apply target="primary":
    {{deploy}} {{compose}} --profile maintenance run --rm --no-deps maintenance repo prune --apply --target "$1" --config /config/backup.yaml

# Restore the latest recovery point into a new output directory.
restore target="primary" output="/restore/prepared":
    {{deploy}} {{compose}} --profile restore run --rm --no-deps restore restore --target "$1" --output "$2" --config /config/backup.yaml

# Restore one exact snapshot into a new output directory.
restore-snapshot snapshot target="primary" output="/restore/prepared":
    {{deploy}} {{compose}} --profile restore run --rm --no-deps restore restore --snapshot "$1" --target "$2" --output "$3" --config /config/backup.yaml

# Reset the disposable drill volume and start the isolated drill.
drill-up:
    {{deploy}} python3 scripts/drill.py up

# Show the isolated drill status.
drill-status:
    {{deploy}} python3 scripts/drill.py status

# Stop the drill and delete only its disposable copied-data volume.
drill-down:
    {{deploy}} python3 scripts/drill.py down

# Produce an ignored report for one explicit upstream candidate tag.
upstream-diff component candidate:
    nix develop --command python3 scripts/upstream_diff.py "$1" "$2"
