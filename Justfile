set shell := ["bash", "-euo", "pipefail", "-c"]
set positional-arguments

system := "x86_64-linux"
compose := "docker compose --env-file .env --file compose.yml"
drill_compose := "docker compose --project-name vaultwarden-less-drill --env-file .env --file compose.yml --profile drill"
upgrade_compose := "docker compose --env-file state/upgrade/current/compose.env --file compose.yml --file compose.upgrade.yml"

# Run the ordinary format, test, vet, lint, and flake gate.
[group('development')]
check: _fmt-check _test _vet _lint
    nix flake check

# Run the complete fast release-candidate gate for clean A and HEAD B commits.
[group('development')]
check-release from_commit:
    nix develop --command python3 scripts/check_release.py "$1"

# Create the exact two-file image release from a clean commit.
[group('development')]
release output_root:
    nix run .#release -- --output-root "$1"

# Start the production topology only after both configuration roles pass.
[group('operations')]
up: _config-validate
    {{ compose }} up --detach vaultwarden backup proxy

# Read the resident worker's strict JSON status.
[group('operations')]
status:
    {{ compose }} exec --no-TTY backup /bin/vaultwarden-less-backup status

# Ask the resident worker to run one debounced recovery check.
[group('operations')]
backup:
    {{ compose }} exec --no-TTY backup /bin/vaultwarden-less-backup request-backup

# Run an explicit networked maintenance command with structured argv.
[group('operations')]
maintenance *args: _config-validate
    {{ compose }} --profile maintenance run --rm --no-deps maintenance "$@"

# Restore latest or one full snapshot ID to a previously absent name.
[group('operations')]
restore target output snapshot="latest": _config-validate
    {{ compose }} --profile restore run --rm --no-deps restore restore --target "$1" --snapshot "$3" --output "$2"

# Start an isolated drill from restore/drill in its own Compose project.
[group('operations')]
drill-up: _config-validate
    {{ drill_compose }} up --detach drill-vaultwarden

# Remove only the isolated drill project and its disposable volume.
[group('operations')]
drill-down:
    {{ drill_compose }} down --volumes

# Create immutable A-owned upgrade identity before loading B.
[group('upgrade')]
upgrade-prepare to_commit to_release: _config-validate
    {{ compose }} --profile maintenance run --rm --no-deps maintenance upgrade prepare --to-commit "$1" --to-release "$2"

# Stop writes in fixed order and let immutable A freeze repository and rollback evidence.
[group('upgrade')]
upgrade-freeze: _config-validate
    {{ compose }} stop proxy
    {{ compose }} stop backup vaultwarden
    {{ upgrade_compose }} --profile upgrade run --rm --no-deps upgrade-a upgrade freeze

# Validate B configuration, start only B Vaultwarden, wait, and record migration evidence.
[group('upgrade')]
upgrade-start:
    {{ upgrade_compose }} --profile maintenance run --rm --no-deps config-check-backup
    {{ upgrade_compose }} --profile maintenance run --rm --no-deps config-check-maintenance
    {{ upgrade_compose }} --profile upgrade up --detach --no-deps --wait --wait-timeout 180 upgrade-vaultwarden-b
    {{ upgrade_compose }} --profile upgrade run --rm --no-deps upgrade-b upgrade migrate-check

# Stop active B, create and restore B evidence, then start its isolated drill.
[group('upgrade')]
upgrade-verify:
    {{ upgrade_compose }} --profile upgrade stop proxy backup vaultwarden upgrade-vaultwarden-b
    {{ upgrade_compose }} --profile upgrade run --rm --no-deps upgrade-b upgrade verify
    {{ upgrade_compose }} --profile drill up --detach --wait --wait-timeout 180 drill-vaultwarden

[private]
_fmt:
    nix fmt

[private]
_fmt-check:
    nix build --no-link .#checks.{{ system }}.format

[private]
_test:
    nix develop --command go test ./...
    nix develop --command python3 -m unittest discover --start-directory tests/scripts --pattern 'test_*.py'

[private]
_vet:
    nix develop --command go vet ./...

[private]
_lint:
    nix develop --command golangci-lint run
    nix develop --command ruff check scripts tests/scripts
    nix develop --command ruff format --check scripts tests/scripts

[private]
_race-release:
    nix develop --command go test -race ./... -count=1

[private]
_security:
    nix develop --command govulncheck ./...

[private]
_build:
    nix build --no-link .#backup .#proxy .#backup-image .#proxy-image .#vaultwarden-image .#moto-image

[private]
_integration:
    nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/internal/restore:TestMotoResticIntegration -- go test -json ./internal/restore -run '^TestMotoResticIntegration$' -count=1 -timeout=20m -args -run-moto-integration

[private]
_integration-worker-timing:
    nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/internal/worker:TestWorkerAcceleratedTargetDegradationIntegration -- go test -json ./internal/worker -run '^TestWorkerAcceleratedTargetDegradationIntegration$' -count=1 -timeout=5m -args -run-accelerated-worker-integration

[private]
_e2e-backup:
    nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/tests/e2e:TestBackupRestoreLifecycle -- go test -json ./tests/e2e -run '^TestBackupRestoreLifecycle$' -count=1 -timeout=30m -args -run-backup-e2e

[private]
_e2e-multitarget-fast:
    nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/tests/e2e:TestMultiTargetFastCatchUp -- go test -json ./tests/e2e -run '^TestMultiTargetFastCatchUp$' -count=1 -timeout=25m -args -run-multitarget-fast-e2e

[private]
_e2e-import:
    nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/tests/e2e:TestInitialImportLifecycle -- go test -json ./tests/e2e -run '^TestInitialImportLifecycle$' -count=1 -timeout=30m -args -run-import-e2e

[private]
_e2e-upgrade from_commit to_commit:
    nix develop --command python3 scripts/verify_go_test.py --require github.com/hellodword/vaultwarden-less/tests/e2e:TestUpgradeAcceptLifecycle --require github.com/hellodword/vaultwarden-less/tests/e2e:TestUpgradeRollbackLifecycle -- go test -json ./tests/e2e -run '^(TestUpgradeAcceptLifecycle|TestUpgradeRollbackLifecycle)$' -count=1 -timeout=95m -args -run-upgrade-accept-e2e -run-upgrade-rollback-e2e -upgrade-from-commit "$1" -upgrade-to-commit "$2"

[private]
_config-validate:
    {{ compose }} --profile maintenance run --rm --no-deps config-check-backup
    {{ compose }} --profile maintenance run --rm --no-deps config-check-maintenance

[private]
_image-load release_directory:
    nix run .#image-load -- "$1"

[private]
_schema-capture database output="internal/upstream/vaultwarden-schema.json":
    nix develop --command python3 scripts/capture_vaultwarden_schema.py --database "$1" --output "$2"

[private]
_upstream-diff component candidate:
    nix develop --command python3 scripts/upstream_diff.py "$1" "$2"
