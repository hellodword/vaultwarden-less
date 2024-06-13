FROM golang:bookworm as builder

WORKDIR /usr/src/app

# pre-copy/cache go.mod for pre-downloading dependencies and only redownloading them in subsequent builds if they change
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd ./cmd
RUN go build -x -v -trimpath -ldflags "-s -w" -buildvcs=false -o /usr/local/bin/syslog-parser ./cmd/syslog-parser

FROM debian:12 as base

# https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
ARG TIME_ZONE

RUN apt-get update && \
  DEBIAN_FRONTEND=noninteractive apt-get install -y git sqlite3 unzip curl jq restic

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
  cp -a --parents "$(which restic)" /opt && \
  ldd "$(which restic)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
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
  true

COPY scripts /opt/scripts

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=base /opt /
COPY --from=builder /usr/local/bin/syslog-parser /usr/local/bin/syslog-parser
COPY syslog-parser.json /

CMD ["/usr/local/bin/syslog-parser", "-config", "/syslog-parser.json"]
