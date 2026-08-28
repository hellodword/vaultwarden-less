set shell := ["bash", "-euo", "pipefail", "-c"]
set positional-arguments := true

system := "x86_64-linux"
compose := "docker compose --env-file .env --file compose.yml"
drill_compose := "docker compose --project-name vaultwarden-less-drill --env-file .env --file compose.yml --profile drill"
upgrade_compose := "docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml"
upgrade_overlay := "docker compose --env-file .env --file compose.yml --file compose.upgrade.yml"

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

# Run the normal local quality gate.
check: fmt-check test race vet lint
    nix flake check

# Build both binaries and every Nix-produced image.
build:
    nix build --no-link .#backup .#proxy .#backup-image .#proxy-image .#vaultwarden-image .#moto-image

# Run project dependency vulnerability checks.
security:
    nix develop --command govulncheck ./...

# Run the real Moto/Restic integration boundary and prove it did not skip.
integration moto_host_port="0":
    nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/internal/restore:TestMotoResticIntegration -- go test -json ./internal/restore -run '^TestMotoResticIntegration$' -count=1 -timeout=20m -args -run-moto-integration -moto-host-port "$1"

# Run the real one-target backup, restore, and drill lifecycle.
e2e-backup proxy_host_port="0" drill_host_port="0" preloaded_images="false":
    extra=(); if [[ "$3" == "true" ]]; then extra=(-preloaded-images); elif [[ "$3" != "false" ]]; then echo "preloaded_images must be true or false" >&2; exit 2; fi; nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/tests/e2e:TestBackupRestoreLifecycle -- go test -json ./tests/e2e -run '^TestBackupRestoreLifecycle$' -count=1 -timeout=55m -args -run-backup-e2e -proxy-host-port "$1" -drill-host-port "$2" "${extra[@]}"

# Run the real two-repository degradation and catch-up lifecycle.
e2e-multitarget proxy_host_port="0" drill_host_port="0" preloaded_images="false":
    extra=(); if [[ "$3" == "true" ]]; then extra=(-preloaded-images); elif [[ "$3" != "false" ]]; then echo "preloaded_images must be true or false" >&2; exit 2; fi; nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/tests/e2e:TestMultiTargetDegradationAndCatchUp -- go test -json ./tests/e2e -run '^TestMultiTargetDegradationAndCatchUp$' -count=1 -timeout=75m -args -run-multitarget-e2e -proxy-host-port "$1" -drill-host-port "$2" "${extra[@]}"

# Run the isolated A/B framework, forced failures, explicit rollback, and acceptance lifecycle.
e2e-upgrade from_commit to_commit proxy_host_port="0" drill_host_port="0" preloaded_images="false":
    extra=(); if [[ "$5" == "true" ]]; then extra=(-preloaded-images); elif [[ "$5" != "false" ]]; then echo "preloaded_images must be true or false" >&2; exit 2; fi; nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/tests/e2e:TestUpgradeRollbackLifecycle -- go test -json ./tests/e2e -run '^TestUpgradeRollbackLifecycle$' -count=1 -timeout=110m -args -run-upgrade-e2e -upgrade-from-commit "$1" -upgrade-to-commit "$2" -proxy-host-port "$3" -drill-host-port "$4" "${extra[@]}"

# Run every fast, security, image, integration, and non-upgrade real gate.
check-full: check security build integration e2e-backup e2e-multitarget

# Create the exact two-file image release from a clean commit.
release output_root:
    nix run .#release -- --output-root "$1"

# Verify and load one exact two-file image release.
image-load release_directory:
    nix run .#image-load -- "$1"

# Validate both isolated credential roles and every private deployment input.
config-validate:
    {{compose}} --profile maintenance run --rm --no-deps config-check-backup
    {{compose}} --profile maintenance run --rm --no-deps config-check-maintenance

# Start the production topology only after both configuration roles pass.
up: config-validate
    {{compose}} up --detach vaultwarden backup proxy

# Stop and remove only the normal production Compose project.
down:
    {{compose}} down

# Show the normal production service state.
ps:
    {{compose}} ps

# Show bounded recent production logs.
logs:
    {{compose}} logs --tail 200 vaultwarden backup proxy

# Ask the resident worker to run one debounced recovery check.
request-backup:
    {{compose}} exec --no-TTY backup /bin/vaultwarden-less-backup request-backup

# Read the resident worker's strict JSON status.
status:
    {{compose}} exec --no-TTY backup /bin/vaultwarden-less-backup status

# Initialize one configured repository using the isolated maintenance role.
repo-init target: config-validate
    {{compose}} --profile maintenance run --rm --no-deps maintenance repo init --target "$1"

# List bounded recovery snapshots for one target.
repo-snapshots target limit="100": config-validate
    {{compose}} --profile maintenance run --rm --no-deps maintenance repo snapshots --target "$1" --limit "$2"

# Check repository metadata without reading pack data.
repo-check target: config-validate
    {{compose}} --profile maintenance run --rm --no-deps maintenance repo check --target "$1"

# Remove a stale repository lock using the maintenance role.
repo-unlock target: config-validate
    {{compose}} --profile maintenance run --rm --no-deps maintenance repo unlock --target "$1"

# Preview the fixed retention policy; this never mutates the repository.
repo-prune target: config-validate
    {{compose}} --profile maintenance run --rm --no-deps maintenance repo prune --target "$1"

# Explicitly apply the fixed retention policy.
repo-prune-apply target: config-validate
    {{compose}} --profile maintenance run --rm --no-deps maintenance repo prune --target "$1" --apply

# Restore latest or one full snapshot ID to a previously absent name.
restore target output snapshot="latest": config-validate
    {{compose}} --profile restore run --rm --no-deps restore restore --target "$1" --snapshot "$3" --output "$2"

# Start an isolated drill from restore/drill in its own Compose project.
drill-up: config-validate
    {{drill_compose}} up --detach drill-vaultwarden

# Show the isolated drill state.
drill-status:
    {{drill_compose}} ps

# Remove only the isolated drill project and its disposable volume.
drill-down:
    {{drill_compose}} down --volumes

# Create immutable A-owned upgrade identity before loading B.
upgrade-prepare to_commit to_release: config-validate
    {{compose}} --profile maintenance run --rm --no-deps maintenance upgrade prepare --to-commit "$1" --to-release "$2"

# Stop writes in the fixed order and let immutable A freeze repository and rollback evidence.
upgrade-freeze: config-validate
    {{compose}} stop proxy
    {{compose}} stop backup vaultwarden
    {{upgrade_compose}} --profile upgrade run --rm --no-deps upgrade-a upgrade freeze

# Start only immutable B Vaultwarden so no proxy or resident backup can serve before acceptance.
upgrade-start-b:
    {{upgrade_compose}} --profile maintenance run --rm --no-deps config-check-backup
    {{upgrade_compose}} --profile maintenance run --rm --no-deps config-check-maintenance
    {{upgrade_compose}} --profile upgrade up --detach --no-deps --wait --wait-timeout 180 upgrade-vaultwarden-b

# Record B's exact audited migration and schema after Vaultwarden migration.
upgrade-migrate-check:
    {{upgrade_compose}} --profile upgrade run --rm --no-deps upgrade-b upgrade migrate-check

# Stop active B, create and really restore B evidence, then start the isolated B drill.
upgrade-verify:
    {{upgrade_compose}} --profile upgrade stop proxy backup vaultwarden upgrade-vaultwarden-b
    {{upgrade_compose}} --profile upgrade run --rm --no-deps upgrade-b upgrade verify
    {{upgrade_compose}} --profile drill up --detach --wait --wait-timeout 180 drill-vaultwarden

# Accept only while upgrade-b can reach the healthy drill, then expose B and remove disposable drill runtime state.
upgrade-accept:
    nix develop --command python3 scripts/deployment.py upgrade accept

# Stop every B service, resume A's exchange, retag exact A images, and start ordinary A services.
upgrade-rollback:
    nix develop --command python3 scripts/deployment.py upgrade rollback

# Deterministically capture an audited Vaultwarden SQLite schema candidate.
schema-capture database output="internal/upstream/vaultwarden-schema.json":
    nix develop --command python3 scripts/capture_vaultwarden_schema.py --database "$1" --output "$2"

# Produce an ignored report for one explicit upstream candidate tag.
upstream-diff component candidate:
    nix develop --command python3 scripts/upstream_diff.py "$1" "$2"
