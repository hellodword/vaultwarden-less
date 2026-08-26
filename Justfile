set shell := ["bash", "-euo", "pipefail", "-c"]
set positional-arguments := true

system := "x86_64-linux"
deploy := "nix develop .#deploy --command python3 scripts/deploy.py --root ."

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

# Run the fast Go and Python test suites.
test:
    nix develop --command go test ./...
    nix develop --command python3 -m unittest discover --start-directory tests/scripts --pattern 'test_*.py'

# Run concurrency-sensitive fast tests with the race detector.
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

# Render and validate every shipped Compose profile without external state.
compose-check:
    nix develop .#deploy --command python3 scripts/deploy.py check-compose

# Run the normal local quality gate.
check: generate-check fmt-check test race vet lint compose-check
    nix flake check

# Build binaries, patched Restic, and every locally produced image with Nix.
build:
    nix build --no-link .#backup .#proxy .#restic-patched .#backup-image .#proxy-image .#moto-image

# Run dependency vulnerability checks.
security:
    nix develop --command govulncheck ./...

# Run the real Moto/Restic integration boundary and prove it did not skip.
integration moto_host_port="0":
    nix develop --command python3 scripts/verify_go_test.py --require TestMotoResticIntegration -- go test -json ./internal/restore -run '^TestMotoResticIntegration$' -count=1 -timeout=20m -args -run-moto-integration -moto-host-port "$1"

# Run the real Vaultwarden/Bitwarden CLI/Restic lifecycle and prove it did not skip.
e2e proxy_host_port="0" drill_host_port="0":
    nix develop --command python3 scripts/verify_go_test.py --require TestVaultwardenBackupRestoreE2E -- go test -json ./tests/e2e -run '^TestVaultwardenBackupRestoreE2E$' -count=1 -timeout=45m -args -run-vaultwarden-e2e -proxy-host-port "$1" -drill-host-port "$2"

# Run every local gate, including Docker integration boundaries.
check-full: check security build integration e2e

# Create a clean-commit linux/amd64 offline deployment bundle.
bundle:
    nix develop --command python3 scripts/bundle.py create --output-root dist

# Verify checksums, load all bundle images, and compare image IDs.
bundle-load bundle:
    nix develop .#deploy --command python3 scripts/bundle.py load "$1"

# Compare currently loaded local images with bundle metadata.
bundle-verify bundle:
    nix develop .#deploy --command python3 scripts/bundle.py verify "$1"

# Create missing private deployment files for one explicit runtime identity.
init uid gid:
    {{deploy}} init --uid "$1" --gid "$2"

# Start the production topology using only preloaded local images.
up:
    {{deploy}} compose -- up --detach

# Stop production services without deleting recovery data.
down:
    {{deploy}} compose -- down

# Show production service status.
ps:
    {{deploy}} compose -- ps

# Follow production service logs.
logs:
    {{deploy}} compose -- logs --follow

# Request a worker backup check.
request-backup:
    {{deploy}} compose -- exec --no-TTY backup vaultwarden-less-backup request-backup

# Read the worker's in-memory status.
status:
    {{deploy}} compose -- exec --no-TTY backup vaultwarden-less-backup status

# Validate private product configuration and all credential boundaries.
config-validate:
    {{deploy}} compose -- run --rm --no-deps backup config validate --config /config/backup.yaml

# Initialize one configured Restic repository.
repo-init target="primary":
    {{deploy}} compose -- --profile maintenance run --rm --no-deps maintenance repo init --target "$1" --config /config/backup.yaml

# List a bounded number of recovery snapshots.
repo-snapshots target="primary" limit="100":
    {{deploy}} compose -- --profile maintenance run --rm --no-deps maintenance repo snapshots --target "$1" --limit "$2" --config /config/backup.yaml

# Check one Restic repository.
repo-check target="primary" subset="":
    {{deploy}} compose -- --profile maintenance run --rm --no-deps maintenance repo check --target "$1" --read-data-subset "$2" --config /config/backup.yaml

# Unlock one Restic repository.
repo-unlock target="primary":
    {{deploy}} compose -- --profile maintenance run --rm --no-deps maintenance repo unlock --target "$1" --config /config/backup.yaml

# Preview fixed retention changes.
repo-prune target="primary":
    {{deploy}} compose -- --profile maintenance run --rm --no-deps maintenance repo prune --target "$1" --config /config/backup.yaml

# Apply fixed retention changes.
repo-prune-apply target="primary":
    {{deploy}} compose -- --profile maintenance run --rm --no-deps maintenance repo prune --apply --target "$1" --config /config/backup.yaml

# Restore the latest recovery point into one new, validated output name.
restore target="primary" output_name="prepared":
    {{deploy}} restore --target "$1" --output-name "$2"

# Restore one exact snapshot into one new, validated output name.
restore-snapshot snapshot target="primary" output_name="prepared":
    {{deploy}} restore --snapshot "$1" --target "$2" --output-name "$3"

# Reset the disposable drill volume and start an isolated restored Vaultwarden.
drill-up restore_name="prepared":
    {{deploy}} drill up --restore-name "$1"

# Show the isolated drill status.
drill-status:
    {{deploy}} drill status

# Stop the drill and delete only its disposable copied-data volume.
drill-down:
    {{deploy}} drill down

# Produce an ignored report for one explicit upstream candidate tag.
upstream-diff component candidate:
    nix develop --command python3 scripts/upstream_diff.py "$1" "$2"
