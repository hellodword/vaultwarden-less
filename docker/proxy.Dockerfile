FROM golang:1.26.7-trixie AS builder

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/vaultwarden-less-proxy ./cmd/vaultwarden-less-proxy

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /out/vaultwarden-less-proxy /usr/local/bin/vaultwarden-less-proxy
COPY LICENSE THIRD_PARTY_LICENSES.md /licenses/

WORKDIR /
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vaultwarden-less-proxy"]
