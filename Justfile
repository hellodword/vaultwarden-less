set shell := ["bash", "-euo", "pipefail", "-c"]

PRODUCT_VERSION := env_var_or_default("PRODUCT_VERSION", "dev")

fmt:
    treefmt

fmt-check:
    treefmt --ci

unit:
    go test ./...

race:
    go test -race ./...

lint:
    golangci-lint run --build-tags systemtest ./...

vet:
    go vet -tags=systemtest ./...

[script]
build-check:
    work="$(mktemp -d)"
    trap 'rm -rf "${work}"' EXIT
    CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags="-buildid= -s -w -X github.com/hellodword/vaultwarden-less/internal/version.Product={{PRODUCT_VERSION}}" -o "${work}/backup-a" ./cmd/vaultwarden-less-backup
    CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags="-buildid= -s -w -X github.com/hellodword/vaultwarden-less/internal/version.Product={{PRODUCT_VERSION}}" -o "${work}/backup-b" ./cmd/vaultwarden-less-backup
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-buildid= -s -w -X github.com/hellodword/vaultwarden-less/internal/version.Product={{PRODUCT_VERSION}}" -o "${work}/proxy-a" ./cmd/vaultwarden-less-proxy
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-buildid= -s -w -X github.com/hellodword/vaultwarden-less/internal/version.Product={{PRODUCT_VERSION}}" -o "${work}/proxy-b" ./cmd/vaultwarden-less-proxy
    test -z "$(go tool buildid "${work}/backup-a")"
    test -z "$(go tool buildid "${work}/proxy-a")"
    test "$(sha256sum "${work}/backup-a" | cut -d' ' -f1)" = "$(sha256sum "${work}/backup-b" | cut -d' ' -f1)"
    test "$(sha256sum "${work}/proxy-a" | cut -d' ' -f1)" = "$(sha256sum "${work}/proxy-b" | cut -d' ' -f1)"
    test "$(${work}/backup-a --version | tr -d '\n')" = "vaultwarden-less-backup version {{PRODUCT_VERSION}}"
    test "$(${work}/proxy-a --version | tr -d '\n')" = "vaultwarden-less-proxy version {{PRODUCT_VERSION}}"

[script]
licenses:
    first="$(mktemp)"
    second="$(mktemp)"
    trap 'rm -f "${first}" "${second}"' EXIT
    go run ./tools/licensegen "${first}"
    go run ./tools/licensegen "${second}"
    cmp "${first}" "${second}"
    cmp "${first}" THIRD_PARTY_LICENSES.md

compose-check:
    VAULTWARDEN_ENV_FILE=/dev/null docker compose --env-file /dev/null config >/dev/null

# Run the real Restic 0.19.1 contract against an isolated Moto S3 container.
integration:
    go test -tags=systemtest ./internal/restore -run '^TestMotoResticRoundTrip$' -count=1

# Build the product image and run the real Vaultwarden/Bitwarden CLI recovery flow.
e2e:
    docker build --file docker/Dockerfile --build-arg PRODUCT_VERSION={{PRODUCT_VERSION}} --tag vaultwarden-less-e2e:dev .
    go test -tags=systemtest ./tests/e2e -run '^TestVaultwardenBackupRestoreE2E$' -count=1

ci-fast: fmt-check unit race vet lint build-check licenses compose-check

ci-full: ci-fast integration e2e
