set shell := ["bash", "-euo", "pipefail", "-c"]
set positional-arguments := true

version := trim(read("VERSION"))

# Format repository-owned source files.
fmt:
    nix develop --command treefmt

# Check formatting without changing files.
fmt-check:
    nix develop --command treefmt --ci

# Run the Go test suite.
test:
    nix develop --command go test ./...

# Run the Go test suite with the race detector.
race:
    nix develop --command go test -race ./...

# Run go vet across production and system-test code.
vet:
    nix develop --command go vet -tags=systemtest ./...

# Run the configured Go linter.
lint:
    nix develop --command golangci-lint run --build-tags systemtest ./...

# Build both reproducible product binaries into dist/.
build:
    nix develop --command python3 scripts/build.py --output dist --version-file VERSION

# Build the local amd64 product image.
image:
    nix develop --command docker buildx build --load --provenance=false --sbom=false --platform linux/amd64 --file docker/Dockerfile --build-arg PRODUCT_VERSION={{version}} --tag vaultwarden-less:dev .

# Render and validate the shipped Compose topology.
compose-check:
    nix develop --command env DEPLOY_UID=65532 DEPLOY_GID=65532 DEPLOY_ROOT=/tmp/vaultwarden-less-compose-check PRODUCT_VERSION={{version}} PRODUCT_IMAGE=vaultwarden-less:dev VAULTWARDEN_ENV_FILE=/dev/null DRILL_RECOVERY_PATH=/tmp/vaultwarden-less-compose-check/restore/drill docker compose --env-file /dev/null --file compose.yml --file compose.drill.yml --file compose.e2e.yml --profile maintenance --profile restore --profile drill config >/dev/null

# Run the real Moto/Restic integration boundary.
integration:
    nix develop --command go test -tags=systemtest ./internal/restore -run '^TestMotoResticRoundTrip$' -count=1

# Run the shipped Compose backup and restore scenario.
e2e:
    nix develop --command go test -tags=systemtest ./tests/e2e -run '^TestVaultwardenBackupRestoreE2E$' -count=1 -timeout=45m

# Run the fast project quality gate.
ci-fast: fmt-check test race vet lint build compose-check

# Run every project quality gate, including external boundaries.
ci-full: ci-fast integration e2e

# Create missing private deployment files and directories.
init:
    nix develop --command python3 scripts/init.py --root . --version-file VERSION

# Start the production topology.
up:
    nix develop --command docker compose --env-file .env up --detach

# Stop the production topology without deleting recovery data.
down:
    nix develop --command docker compose --env-file .env down

# Show production service status.
ps:
    nix develop --command docker compose --env-file .env ps

# Follow production service logs.
logs:
    nix develop --command docker compose --env-file .env logs --follow

# Request a worker backup check.
request-backup:
    nix develop --command docker compose --env-file .env exec --no-TTY backup vaultwarden-less-backup request-backup

# Read the worker's in-memory status.
status:
    nix develop --command docker compose --env-file .env exec --no-TTY backup vaultwarden-less-backup status

# Validate the private product configuration.
config-validate:
    nix develop --command docker compose --env-file .env run --rm --no-deps backup config validate --config /config/backup.yaml

# Initialize one configured Restic repository.
repo-init target="primary":
    nix develop --command docker compose --env-file .env --profile maintenance run --rm --no-deps maintenance repo init --target "$1" --config /config/backup.yaml

# List recovery snapshots for one target.
repo-snapshots target="primary":
    nix develop --command docker compose --env-file .env --profile maintenance run --rm --no-deps maintenance repo snapshots --target "$1" --config /config/backup.yaml

# Check one Restic repository.
repo-check target="primary" subset="":
    nix develop --command docker compose --env-file .env --profile maintenance run --rm --no-deps maintenance repo check --target "$1" --read-data-subset "$2" --config /config/backup.yaml

# Unlock one Restic repository.
repo-unlock target="primary":
    nix develop --command docker compose --env-file .env --profile maintenance run --rm --no-deps maintenance repo unlock --target "$1" --config /config/backup.yaml

# Preview retention changes for one target.
repo-prune target="primary":
    nix develop --command docker compose --env-file .env --profile maintenance run --rm --no-deps maintenance repo prune --target "$1" --config /config/backup.yaml

# Apply retention changes for one target.
repo-prune-apply target="primary":
    nix develop --command docker compose --env-file .env --profile maintenance run --rm --no-deps maintenance repo prune --apply --target "$1" --config /config/backup.yaml

# Restore the latest recovery point into a new output directory.
restore target="primary" output="/restore/prepared":
    nix develop --command docker compose --env-file .env --profile restore run --rm --no-deps restore restore --target "$1" --output "$2" --config /config/backup.yaml

# Restore one exact 64-character snapshot ID into a new output directory.
restore-snapshot snapshot target="primary" output="/restore/prepared":
    nix develop --command docker compose --env-file .env --profile restore run --rm --no-deps restore restore --snapshot "$1" --target "$2" --output "$3" --config /config/backup.yaml

# Start the isolated restored Vaultwarden drill instance.
drill-up:
    nix develop --command docker compose --env-file .env --file compose.yml --file compose.drill.yml --profile drill up --detach drill-vaultwarden

# Show drill instance status.
drill-status:
    nix develop --command docker compose --env-file .env --file compose.yml --file compose.drill.yml --profile drill ps drill-vaultwarden

# Stop the drill instance without deleting restored output.
drill-down:
    nix develop --command docker compose --env-file .env --file compose.yml --file compose.drill.yml --profile drill stop drill-vaultwarden

# Validate and publish the VERSION-matched amd64 GHCR image.
release repository="ghcr.io/hellodword/vaultwarden-less":
    nix develop --command python3 scripts/release.py --repository "$1" --version-file VERSION
