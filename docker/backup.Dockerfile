FROM golang:1.26.7-trixie AS builder

WORKDIR /src
ENV CGO_ENABLED=1 GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/vaultwarden-less-backup ./cmd/vaultwarden-less-backup

FROM ghcr.io/restic/restic:0.19.1 AS restic

FROM gcr.io/distroless/base-nossl-debian13:nonroot

COPY --from=builder /out/vaultwarden-less-backup /usr/local/bin/vaultwarden-less-backup
COPY --from=restic /usr/bin/restic /usr/local/bin/restic
COPY LICENSE THIRD_PARTY_LICENSES.md /licenses/

WORKDIR /
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vaultwarden-less-backup"]
CMD ["serve", "--config", "/config/backup.json"]
