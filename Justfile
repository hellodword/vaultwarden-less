set shell := ["bash", "-euo", "pipefail", "-c"]
set positional-arguments := true

system := "x86_64-linux"

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

# Create the exact two-file image release from a clean commit.
release output_root:
    nix run .#release -- --output-root "$1"

# Verify and load one exact two-file image release.
image-load release_directory:
    nix run .#image-load -- "$1"

# Deterministically capture an audited Vaultwarden SQLite schema candidate.
schema-capture database output="internal/upstream/vaultwarden-schema.json":
    nix develop --command python3 scripts/capture_vaultwarden_schema.py --database "$1" --output "$2"

# Produce an ignored report for one explicit upstream candidate tag.
upstream-diff component candidate:
    nix develop --command python3 scripts/upstream_diff.py "$1" "$2"
