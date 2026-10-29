# Product contracts

The release contains Vaultwarden, a request gateway (the `proxy` Compose
service), and a backup worker. Nix fixes tools, sources and images through the
locked inputs. The Go product binaries disable CGO. SQLite and native restic
own database copies, repositories and restoration; no project manifest is
needed to restore a snapshot.

## Source and effective content

Vaultwarden uses `/data/db.sqlite3`, `/data/rsa_key*`, `/data/attachments` and
`/data/sends`. Its active configuration file is the read-only
`/config/vaultwarden-runtime.json` containing `{}`. An imported `/data/config.json`
is preserved and backed up but does not configure this deployment. There is
no project data migration or compatibility layer; Vaultwarden performs its
own native upgrades on the working copy.

Every startup, gateway trigger, six-hour timer and ordinary manual backup uses
one effective-content projection. Every capture rereads source files. A forced
manual backup uses the same projection but creates a recovery point even if
unchanged. Metadata timestamps, inodes and SQLite physical reorganization do
not count as changes. The fingerprint includes logical database contents and
schema, relative file paths and types, bytes, link targets and stable problem
categories/counts. Unknown persistent tables, columns and factor types remain.

These exclusions affect only comparison; **each snapshot contains the entire
native database**, including excluded values:

| Content | Comparison projection |
| --- | --- |
| users | Exclude updated_at, last_verifying_at, login_verify_count, email_new, email_new_token, stamp_exception; include security_stamp |
| devices | Exclude only updated_at |
| twofactor type 0 | Exclude only last_used |
| twofactor type 1 | Remove only last_token, token_sent, attempts from JSON data |
| twofactor type 7 | Exclude only regenerated row uuid; retain credential counters and backup flags |
| temporary authentication | Exclude auth_requests, sso_auth, twofactor_incomplete, twofactor_duo_ctx, event, and factor types 1000–1004/2000 |
| sends | Exclude revision_date; project access_count as `CASE WHEN max_access_count IS NULL THEN NULL ELSE access_count END` |
| emergency_access | Exclude updated_at and last_notification_at; retain recovery_initiated_at and persistent authorization |

SQLite supplies typed, ordered logical hashing. Duplicate rows, NULL, blobs
and unknown structure participate. If semantic comparison cannot be evaluated,
the full copied database bytes and safe problem summary are compared instead.

Each capture uses the read-only SQLite online `.backup` operation, including
committed WAL content, then `PRAGMA integrity_check` on the standalone copy.
Attachment and Send reference checks run independently. File copying hashes
bytes and checks stability before/after reading and declared reference sizes.
Links are preserved without following them. Orphans, unknown files and empty
directories are preserved. Root icon_cache/ and tmp/ directories and replaced
SQLite sidecars are excluded.

| Source result | Meaning |
| --- | --- |
| complete | Database integrity and reference checks completed; required files were available and stable |
| incomplete | A database copy exists but integrity, references or source reads have issues; save it and available stable files |
| failed | No database copy, or a local staging write failed; publish no payload |

Capture is bounded to 30 minutes. After a reliable copy exists, a timeout may
produce an incomplete payload. Identical incomplete content and issues do not
create repeated snapshots; the source is still rechecked after five minutes.
A complete capture clears source faults. `complete` describes source checking,
not a globally atomic filesystem snapshot or verified remote storage.

## Triggers, independent targets and local state

Gateway hints are bounded route-template/method counters, at request start and
finish. Unknown routes, failures and aborted transfers are conservative hints;
raw unknown URLs, bodies and tokens never enter events. Explicit read-only and
temporary routes may be excluded in [excluded.go](../internal/routes/excluded.go).
Send consumption and persistent security writes trigger hints. Business requests
never wait for backup. Events arriving during capture belong to the next round.

| Trigger | Timing |
| --- | --- |
| Startup / manual backup | Immediate capture and comparison |
| Gateway events | 30 seconds quiet; maximum two minutes from first pending event |
| Fallback timer | Every six hours, independently of request checks |
| Failed/incomplete source | Five minutes; new events may trigger earlier |
| Failed target upload | 5, 10, 20, 40, 60 minutes, then hourly |

Targets are equal and independent. Each serializes its own repository operations;
other targets continue. New writes do not reset backoff. Retry obtains fresh
current source content; only the latest waiting version survives. Running
uploads use immutable directories. Native `backup --force` rereads each payload
while repository blocks still deduplicate. Only exit zero with a valid native
snapshot ID confirms an upload. The worker preserves the latest confirmed
snapshot and latest complete recovery point separately.

A successful upload is finalized with native restic tags `complete` or
`incomplete` and `vwless-fingerprint:<sha256>`. Before confirmation it carries
`vwless-pending`; a failed/interrupted upload may leave such a native snapshot.
These unconfirmed snapshots never establish coverage. Tag publication itself
must succeed before the worker confirms the resulting native snapshot ID.
Startup reads repository identity and snapshot tags to reconstruct comparison
baselines even after local cache deletion. Each deployment must use independent
repositories or prefixes.

`state/` holds atomic small records: deadlines, failures, backoff, report counts
and notification summaries. Target state is bound to name and native repository
identity. `cache/` holds rebuildable restic caches; `cache/work/` holds database
copies, staging, restic temporary files and verification caches on disk. The
single instance lock is acquired before cleaning abandoned work. At runtime
only the newest payload plus active uploads and the current capture are kept;
allow approximately N+2 native trees for N targets, plus restic workspace.
Cache removal never resets schedules or notification budgets. Stop the worker
before deleting its cache. Do not delete state to clear a fault.

## Verification

Each target independently opts into structure and full-read intervals. Omitted
types are unscheduled; timeout defaults to two hours. First enabled checks run
once a valid snapshot exists. Later deadlines persist across restarts. Missed
periods produce one catch-up check. Native `restic check --json` checks structure;
adding `--read-data` reads the whole repository. Each invocation uses a fresh
cache. A full read also satisfies a due structure check.

A failed regular automatic full read gets one retry after 24 hours. Failure of
that retry does not reschedule another retry or postpone the regular deadline.
A successful full read restarts its enabled periods from success. Structure
failures wait for their next regular period. Manual checks run once; successes
update enabled deadlines and failures do not start automatic retry rounds.
Only one full read runs globally. Active checks finish despite new writes, then
the target uploads its latest content. A due check follows one due upload so
continuous writes cannot starve verification. Other targets can upload while a
full read is active.

Verification also inspects whether claimed current and complete snapshots still
exist. Missing current coverage requests recapture/upload. Backups continue
through verification failures. Upload success clears only upload/coverage
faults; structure/full success clears structure faults; only full-read success
clears full-read faults. External restic checks do not update worker state.
The worker never runs repair, unlock, forget or prune automatically. Native
exit codes retain repository-missing, locked and incorrect-password categories;
generic diagnostics are not interpreted as proof of network failure/corruption.

## CLI and private HTTP

```text
serve [--config /config/backup.yaml]
backup [--force]
verify --target NAME [--read-data]
status
validate [--config /config/backup.yaml]
healthcheck
version
```

Configuration is read at startup. `validate` checks local YAML, credential files
and APNs keys without network calls. Root errors prevent startup; individual
invalid targets/channels are isolated. Explicit empty targets are unconfigured
and perform no capture or repository operations. Flags use Go's flag parser;
help is available via `help`, `-h` and `--help`. Stdin is unused.

Backup and verify wait for the accepted operation's actual result. Partial
failures and deferred uploads are reported per target and exit unsuccessfully,
without waiting for indefinite background retries. `--force` grants each target
one additional attempt past backoff without resetting failure counts or
interrupting active operations. Ctrl+C stops only CLI waiting. stdout contains
JSON results; stderr contains safe progress/errors. Exit codes: success 0,
failed/incomplete/deferred 1, input error 2, interrupted waiting 130.

The unauthenticated internal API belongs only on the trusted Compose network;
port 9090 is not published. Strict JSON rejects unknown fields and extra values.
Errors have `{"error":{"code":"stable_code","message":"Safe message"}}`.

| Endpoint | Request / response |
| --- | --- |
| POST /events | Bounded route summary; 204 |
| POST /backups | `{"force":false}`; 202 with operation ID and receipt |
| POST /verifications | `{"target":"NAME","read_data":false}`; 202 with receipt |
| GET /operations/{id} | queued/running/succeeded/failed, source and per-target outcomes |
| GET /status | Source, target coverage, verification, retries, faults, notification state and recent jobs |
| GET /healthz | 204 process responsiveness |

HTTP disconnects never cancel accepted jobs. The most recent 100 receipts are
bounded in memory; live jobs are retained and excess submissions receive 503.
A restart loses receipts. Missing receipts cause the CLI to report that it
cannot confirm completion, never success. API timestamps are UTC RFC3339;
unset timestamps are omitted. An old snapshot plus a recent unchanged source
check is current coverage, not backup lag.

## Notifications

Configured channels receive English Fault, Recovery, Reminder and Daily reports
with configured local time and UTC offset. Defaults are Asia/Shanghai, 21:00
daily and six-hour fault reminders. Omitted/empty channels disable delivery.
First faults, increased scope and recoveries generate reports; healthy captures,
unchanged checks, uploads and verifications only update status/daily counts.
Daily catch-up merges the missing period. Recovery explicitly retains other
unresolved faults and records incidents recovered before first delivery.

WeCom and DingTalk use Markdown; Feishu uses an interactive Markdown card.
Bark uses production APNs HTTP/2 with a PKCS#8 P-256 key, ES256 provider tokens
cached 50 minutes, default sound 1107, Markdown and a plain alert fallback;
no BarkServer is involved. Webhook acceptance requires HTTP success and an
explicit numeric success code zero. APNs acceptance requires HTTP/2 200.

Each channel persists merged pending/inflight summaries and its attempt budget.
Starts are at least ten seconds apart, with ten-second timeouts. A round allows
three attempts: first, one minute after its failure, five minutes after the
second failure. Retry-After is respected; APNs server errors wait at least 15
minutes. Exhaustion retains content until a new report opens a round. Events
merged during retry and restarts do not reset budgets. Permanent authentication
or configuration failures pause the channel until corrected and restarted.
Delivery failure appears in status and subsequent delivered reports, without
recursive notification faults. Channels never block captures or each other.

Whole entries are omitted with an omission count to fit limits: WeCom text
4096/body 16000 bytes; DingTalk text 4000/body 20000; Feishu body 20000;
APNs total 4096. Dynamic names are escaped. Reports exclude database values,
credentials, raw subprocess diagnostics and private source paths. Platform
acceptance is not device delivery. A crash after acceptance but before local
success persistence may produce duplicate delivery.

## Deployment and acceptance

All services use `restart: "on-failure:5"`; the host owns boot-time startup and
intervention after a service stops. Gateway drains requests for up to 120 seconds
and then flushes remaining hints for up to ten seconds; Compose grants 150
seconds. Dependency order stops gateway, worker, then Vaultwarden. Sudden death
is covered by startup and timed captures.

`nix flake check` covers Go integration/unit/race/vet, scripts, formatting and
route generation from the locked Vaultwarden source. `release` and
`test-runner-image` remain the product/acceptance outputs. The packaged Compose
suite uses synthetic accounts, native SQLite/restic, local object stores and
notification protocol peers. Long periods use controlled clocks; real product
I/O uses manual jobs without waiting for production backoff intervals. Real
cloud IAM and delivery to external groups/devices remain operator boundaries.
