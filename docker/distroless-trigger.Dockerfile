FROM golang:bookworm AS builder

WORKDIR /usr/src/app

# pre-copy/cache go.mod for pre-downloading dependencies and only redownloading them in subsequent builds if they change
COPY go.mod ./
RUN go mod download && go mod verify

COPY cmd ./cmd
RUN go build -x -v -trimpath -ldflags "-s -w" -buildvcs=false -o /usr/local/bin/trigger ./cmd/trigger

FROM golang:bookworm AS restic-builder

ARG RESTIC_VERSION="0.18.0"

WORKDIR /usr/src/app

ENV CGO_ENABLED=0
RUN git clone --depth=1 -b "v$RESTIC_VERSION" https://github.com/restic/restic restic && \
  cd restic && \
  go build -x -v -trimpath -ldflags "-s -w" -buildvcs=false -o /usr/local/bin/restic ./cmd/restic

FROM debian:12 AS base

# https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
ARG TIME_ZONE

RUN apt-get update && \
  DEBIAN_FRONTEND=noninteractive apt-get install -y git sqlite3 curl jq

RUN mkdir -p /opt/bin /opt/etc /opt/usr/bin && \
  cp /usr/share/zoneinfo/${TIME_ZONE:-UTC} /opt/etc/localtime && \
  cp -a --parents /etc/passwd /opt && \
  cp -a --parents /etc/group /opt && \
  cp -a --parents "$(which git)" /opt && \
  ldd "$(which git)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which sqlite3)" /opt && \
  ldd "$(which sqlite3)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which env)" /opt && \
  ldd "$(which env)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which date)" /opt && \
  ldd "$(which date)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which grep)" /opt && \
  ldd "$(which grep)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which bash)" /opt && \
  ldd "$(which bash)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which echo)" /opt && \
  ldd "$(which echo)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which curl)" /opt && \
  ldd "$(which curl)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which openssl)" /opt && \
  ldd "$(which openssl)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which tr)" /opt && \
  ldd "$(which tr)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which printf)" /opt && \
  ldd "$(which printf)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which cat)" /opt && \
  ldd "$(which cat)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which jq)" /opt && \
  ldd "$(which jq)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which touch)" /opt && \
  ldd "$(which touch)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which dirname)" /opt && \
  ldd "$(which dirname)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which pwd)" /opt && \
  ldd "$(which pwd)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which mkdir)" /opt && \
  ldd "$(which mkdir)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which mktemp)" /opt && \
  ldd "$(which mktemp)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  cp -a --parents "$(which rm)" /opt && \
  ldd "$(which rm)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
  true

COPY scripts /opt/scripts

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=base /opt /
COPY --from=restic-builder /usr/local/bin/restic /usr/local/bin/restic
COPY --from=builder /usr/local/bin/trigger /usr/local/bin/trigger
COPY config/trigger.json /config/trigger.json

ENV HOME=/home/nonroot

CMD ["/usr/local/bin/trigger", "-config", "/config/trigger.json"]
