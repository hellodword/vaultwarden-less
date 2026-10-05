# Product contracts

The product consists of an unmodified Vaultwarden selected by the locked Nix
package set, a Go HTTP proxy and a separate Go backup process. SQLite and
restic CLI tools own database copies and repository operations. Product Go
binaries disable CGO. The implementation does not gate operation on a version
string or source-tree state.

## Requests and checks

Every request except an explicit METHOD + path exclusion triggers at its
start and end, including unknown GET requests, errors and aborted transfers.
Parameters match one nonempty segment. GET exclusions do not imply HEAD.
Query strings are not route identity. Ambiguous paths are handled
conservatively. The exact maintained exclusions are in
[excluded.go](../internal/routes/excluded.go); the generated
[catalog](../internal/routes/catalog.txt) classifies known requests separately.

The proxy never waits for backup. It streams responses and WebSocket traffic
without an attachment size limit. It merges bounded route-template and
unknown-method counters, attempts delivery at most once per second, uses a
two-second request timeout and retries failures after one second. Unknown
methods outside the fixed method set become OTHER. It does not queue raw URLs.
Business port 8080 forwards to Vaultwarden; process health is on the separate
loopback listener 8081.

| Trigger or operation | Behavior |
| --- | --- |
| Process startup | Full comparison and fresh file reads |
| Request activity | 30 seconds quiet, at most two minutes from the first pending event |
| Ordinary timer | Every six hours, independently of request checks |
| Full timer | Every seven days, independently of ordinary checks |
| Manual check | Immediate; `--full` requests a full comparison |
| Source capture | Serial, at most 30 minutes |
| Incomplete or failed source | Retry after five minutes |
| Destination upload | At most one per target, timeout two hours |
| Failed destination | Retry after 5, 10, 20, 40, then 60 minutes |

New activity does not reset destination backoff. Requests arriving during a
capture remain pending. A required full comparison remains pending until it
has actually been compared reliably. No valid target means no capture.

## Database and file contents

The worker opens the live database read-only and runs SQLite `.backup`,
including committed WAL content. The project does not rewrite the live
database or sidecars. The resulting complete native database is always saved,
including content excluded from ordinary comparison.

SQLite performs metadata inspection, type-aware ordering, JSON processing and
`sha3_query` hashing. Schema, unknown tables and columns, unknown factor kinds,
NULL, BLOB, numeric types and duplicate rows participate. Ordinary checks omit
only the explicit business policy in
[policy.go](../internal/capture/policy.go). Known JSON paths are removed only
within their declared structures; new leaves, unknown containers and unknown
empty objects remain. Full checks ignore no business content. Pure physical
reorganization does not count as a logical change. If comparison cannot be
reliably evaluated, the worker warns and conservatively compares the complete
copied database.

The payload includes the whole native file tree: RSA material, configuration,
orphan and unknown files, symbolic links without following them, and empty
directories. Only root `icon_cache/`, root `tmp/`, and SQLite sidecars are
excluded; the database itself is replaced by the consistent copy. Request
checks may reuse unchanged file metadata hints. Startup, timed and full checks
reread all files. A file is copied and hashed in one pass with stability checks
before and after. Only completed immutable payload copies may be hardlinked.

Missing, unreadable or changing source files produce `incomplete`: the full
database and every available stable file remain. Database references are never
deleted and old files are never substituted. Unchanged available content and
completeness do not upload repeatedly. Repairing the files is a change.
Database copy or destination write failures fail the whole capture. A timeout
after the database copy may retain an incomplete payload.

## Repository state

Each upload uses native `restic backup . --json --host vaultwarden-less
--group-by host --ignore-ctime --no-scan` from its immutable payload, with the
`complete` or `incomplete` tag. Only exit zero with a native snapshot ID
advances that target's baseline. Healthy targets continue while another fails.
Only the newest waiting payload is retained per destination; an active upload
is immutable, and the newest version is compared again after it completes.

Startup queries only the newest native snapshot for the fixed host and uses
cached payloads only when their association matches that snapshot ID. A
missing cache leads to a new deduplicated native backup. The worker does not
download historical content to reconstruct comparison state. Ordinary
unchanged checks make no repository calls after initialization.

The local cache holds native payload directories and associations to native
snapshot IDs. It is disposable and is not a restore format. Approximately
`2N + 2` payload versions are retained for N destinations, plus transient
database workspace. Restic remains responsible for repository history and
deduplication. Operators should provision local space for those copies.

## CLI and HTTP

The backup executable accepts:

```text
serve [--config /config/backup.yaml]
check [--full]
status
validate [--config /config/backup.yaml]
healthcheck
version
```

`validate` reads local configuration, referenced environment files and APNs
keys only; it makes no repository or network calls. `check` reports acceptance,
not completion. `healthcheck` tests process responsiveness, not backup success.
`version` displays provenance. Successful structured results use stdout;
diagnostics and safe JSON errors use stderr. Exit codes are 0 for success,
1 for runtime failure and 2 for invalid input. Stdin is unused.
The proxy exposes `serve`, `healthcheck` and `version`.

The backup CLI uses Go's standard `flag` parser with explicit subcommands.
`help`, `help COMMAND`, `-h` and `--help` print text help to stdout and exit 0.
`--config PATH` and `--config=PATH` are accepted for `serve` and `validate`;
`--full`, `--full=true` and `--full=false` are accepted for `check`. There are
no positional operands, inherited environment options or shell completion
commands. Invalid commands/flags produce a safe JSON error without usage text.

The private backup listener at port 9090 provides:

| Endpoint | Result |
| --- | --- |
| `POST /events` | Bounded route summary, 204 |
| `POST /checks` with `{"full": false}` | Queued check, 202 and `{"accepted":true}` |
| `GET /status` | JSON source, schedule, destination and notifier state |
| `GET /healthz` | Process response, 204 |

These endpoints have no host publication or authentication negotiation.
They belong on the trusted deployment network.

## Notification delivery

Every check, including no change, and every target result enters each channel's
independent queue. Events merge, with at most one send per ten seconds.
Messages use a shared English Markdown report and keep request `started` and
`finished` counters separate. Source diagnostics and target states describe the
latest observation; check, report and request-phase counts accumulate. Dynamic
names are escaped. Whole detail entries are omitted to fit byte limits, with an
`Omitted details: N` marker and the essential summary retained.

WeCom uses native Markdown with a 4096-byte UTF-8 content limit; DingTalk uses
native Markdown with a conservative 4000-byte text budget. Feishu uses a schema
2.0 interactive card containing a Markdown element. Encoded request bodies are
limited to 16000 bytes for WeCom and 20000 for DingTalk and Feishu.
Webhooks require an explicit numeric success code of zero. Bark uses HTTP/2,
an ES256 JWT refreshed after 50 minutes and the native APNs alert plus Bark
extension fields. Its top-level `markdown` carries the report, with a plain-text
alert fallback; the complete encoded APNs payload, including both, is at most
4096 bytes. No Bark server participates in delivery.

Each attempt has a ten-second timeout. Transient failures retry one and five
minutes after the first failure, up to three attempts. Exhausted information
is retained until a new event; recovery includes the missed summary. Retry-After
is honored. APNs server failures wait at least 15 minutes. Permanent token,
topic, authorization and payload rejections stop that channel until restart.
One channel cannot block another or affect backup or client responses.

## Build and verification

The bundle contains three product images, Compose, templates, documentation
and license notices. A separate `test-runner-image` contains compiled
acceptance tests, official Bitwarden CLI, Docker CLI and native tools.
It accepts `--release-dir`, `--work-dir` and `--docker-socket` (default
`/var/run/docker.sock`); the Docker host and runner share those absolute paths.
Nix injects test image references and the Moto archive path into the entrypoint.
Product image references and restored service environment come from rendered
Compose. The suite has a 75-minute limit.

Source workflows follow Just → Nix → scripts. Nix owns tool selection, source
pins and build dependencies; scripts accept explicit artifacts/paths and never
invoke Nix. The deployment bundle remains a Docker/Compose product, with no
source-workflow tools required on its target host.

Acceptance covers client writes and reads, attachments and Sends, independent
targets, incomplete sources and repairs, cache loss, native restore, and
notification protocol failures and recovery. Fake-clock and race tests cover
long scheduling intervals and concurrent transitions. Local Moto and protocol
peers establish local behavior; real provider IAM, external robot groups and
Apple device delivery require separate operator verification.
