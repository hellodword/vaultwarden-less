ARG PRODUCT_VERSION=dev

FROM golang:1.26.7-trixie@sha256:277b40a9f20e4346f3b3386104f2a6c11caf2318a55c3a13ee89a920264bd717 AS source
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
COPY LICENSE THIRD_PARTY_LICENSES.md ./

FROM source AS backup-builder
ARG PRODUCT_VERSION=dev
ENV CGO_ENABLED=1
RUN go build -trimpath -buildvcs=false \
  -ldflags="-buildid= -s -w -X github.com/hellodword/vaultwarden-less/internal/version.Product=${PRODUCT_VERSION}" \
  -o /out/vaultwarden-less-backup ./cmd/vaultwarden-less-backup

FROM source AS proxy-builder
ARG PRODUCT_VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -buildvcs=false \
  -ldflags="-buildid= -s -w -X github.com/hellodword/vaultwarden-less/internal/version.Product=${PRODUCT_VERSION}" \
  -o /out/vaultwarden-less-proxy ./cmd/vaultwarden-less-proxy

FROM ghcr.io/restic/restic:0.19.1@sha256:0e991bdddc97843cab1dfea1a1d7019f87e7fffe8bae3a6a08b68b75bb10a888 AS restic

FROM gcr.io/distroless/base-debian13:nonroot@sha256:579896ee73cfae567c504ddb9ab118d44370cb3cd7cf222a02ce1a5c5f935c27
ARG PRODUCT_VERSION=dev
LABEL org.opencontainers.image.version=${PRODUCT_VERSION}
COPY --from=backup-builder /out/vaultwarden-less-backup /usr/local/bin/vaultwarden-less-backup
COPY --from=proxy-builder /out/vaultwarden-less-proxy /usr/local/bin/vaultwarden-less-proxy
COPY --from=restic /usr/bin/restic /usr/local/bin/restic
COPY LICENSE /licenses/vaultwarden-less/LICENSE
COPY THIRD_PARTY_LICENSES.md /licenses/vaultwarden-less/THIRD_PARTY_LICENSES.md
WORKDIR /
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vaultwarden-less-backup"]
CMD ["serve", "--config", "/config/backup.yaml"]
