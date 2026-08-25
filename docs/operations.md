# Operations

## Health and status

Use the proxy and backup health endpoints for process liveness. `/healthz`
does not test SQLite, Restic, S3, or notifications. Use `status` for source
health, target retry times, dirty generations, and protection:

```json
{
  "schema_version": 1,
  "dirty": false,
  "backup_in_progress": false,
  "source": {
    "state": "healthy",
    "last_attempt_at": "...",
    "last_success_at": "...",
    "next_retry_at": "",
    "consecutive_failures": 0,
    "error_code": ""
  },
  "last_sample_at": "...",
  "last_snapshot_at": "...",
  "pending_recovery_point": null,
  "protection": {
    "state": "protected",
    "latest_complete_content_id": "sha256:...",
    "configured_targets": 2,
    "targets_with_latest_complete": ["primary"]
  },
  "targets": []
}
```

`protected` means the latest complete content ID is confirmed in at least one
configured target. An incomplete point never removes an earlier complete
protection point. All timestamps are UTC with millisecond precision and arrays
are non-null and stable-sorted.

## Notifications

The durable notification queue emits `recovery_point_available`,
`source_degraded`, `source_recovered`, `target_degraded`, and
`target_recovered`. A transition is emitted once when a source or target first
becomes degraded and once when it recovers; changes in the error code do not
repeat the degraded transition.

While a notifier is available, successes are FIFO. During an outage, state
transitions retain their relative order and pending successes collapse into the
newest success with a saturating `merged_successes` count. Backup continues if
notification persistence is unavailable.

## Retention and maintenance

`repo prune` always validates every product candidate before invoking Restic.
Any candidate anomaly blocks all deletion with
`snapshot_anomaly_blocks_prune`. Complete and incomplete points are retained
separately:

- complete: all points within 24 hours, plus 48 hourly, 14 daily, 8 weekly,
  12 monthly, and 3 yearly points;
- incomplete: all points within 24 hours, plus 7 daily points.

Dry-run is the default. Apply mode parses both dry-runs before executing either
forget operation and exposes only stable keep/remove IDs and counts.

## Restore

Restore requires an absolute clean output path outside `/data`, with an existing
private 0700 parent chain. An absent or empty output is restored into a private
temporary sibling, verified by Restic and the project manifest checks, synced,
and atomically renamed into place. A parent-fsync uncertainty returns
`restore_published_unconfirmed` while retaining the published output.

A non-empty output is never overwritten. It is strictly verified and must match
a valid candidate's status and content ID. Repeating the same command confirms
durability without calling Restic restore again; an explicit snapshot ID must
also match.

Start a recovered database with the Vaultwarden version recorded in its
manifest. Then use Vaultwarden's normal forward-upgrade path. The restore
command prepares data; it does not fetch images or migrate the database.

## Recovery handling

Keep the shared password, role credentials, `backup.yaml`, and the output of
`status` under the deployment's private access policy. After a process or host
restart, the worker reconciles `current` and `.previous`, completes safe
cleanup of recognized staging entries, and retries unresolved target intents.
