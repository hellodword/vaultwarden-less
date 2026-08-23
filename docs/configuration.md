# Configuration

Both configuration files are strict JSON with `"schema_version": 1`.
Unknown fields, multiple/trailing top-level JSON values, unsupported URLs or backends,
unclean paths, invalid names, and out-of-contract durations stop startup. There
are no environment-variable overrides for these JSON contracts.

Configuration JSON must never contain passwords, tokens, private keys,
repository credentials, or authenticated URLs. Secret values are loaded from
regular, non-symlink files with permissions exactly `0600`. A secret file path
is configuration; its contents are not.

## proxy.json

The tracked [proxy.json](../config/proxy.json) is the complete schema.

| Field | Contract |
| --- | --- |
| `listen_address` | numeric IP plus port; Compose publishes it on host loopback |
| `upstream_url` | HTTP(S) origin with no credentials, query, or non-root path |
| `events_url` | HTTP(S) URL whose path is exactly `/v1/events` |
| `trusted_proxy_cidrs` | canonical CIDRs allowed to supply `X-Real-IP` and `X-Forwarded-Proto` |
| `max_request_bytes` | fixed at `629145600` (600 MiB) |
| `read_header_timeout` | fixed at `10s` |
| `read_timeout`, `write_timeout` | fixed at `30m` |
| `idle_timeout` | fixed at `2m` |
| `upstream_response_header_timeout` | fixed at `5m` |
| `event_timeout` | `1s` through `10s`; the example uses `2s` |
| `shutdown_timeout` | fixed at `2m` |

Narrow `trusted_proxy_cidrs` to the actual local front-proxy source addresses.
For untrusted peers, forwarding headers are ignored. For trusted peers, an
invalid or repeated `X-Real-IP`/`X-Forwarded-Proto` is rejected. All inbound
`Forwarded`, `X-Real-IP`, and `X-Forwarded-*` fields are deleted and rebuilt.

## backup.json

The tracked [backup.json](../config/backup.json) shows an append-only REST
backup identity and a separate maintenance identity.

The companion `.env.vaultwarden` explicitly pins `DATA_FOLDER=/data`, a
`sqlite:///data/db.sqlite3` database URL, and local attachment, Send, RSA,
template, icon-cache, and temporary paths. PostgreSQL/MySQL and Vaultwarden S3
storage must not override those values in a supported deployment.

### Service and schedule

`data_dir`, `state_dir`, `staging_dir`, and `cache_dir` must be clean absolute,
pairwise non-overlapping paths. The source `data_dir` and every worker-owned
directory must be a real `0700` directory. None of the writable worker
directories may overlap `data_dir`. `restic_binary` is also an absolute path.

The v1 schedule is visible but deliberately fixed:

| Field | v1 value |
| --- | --- |
| `debounce` | `30s` |
| `max_wait` | `2m` |
| `min_interval` | `5m` |
| `sample_interval` | `15m` |
| `missing_retry` | `30s` |
| `shutdown_timeout` | `2m` |

`target_timeout` may be `1m` through `2h`; `maintenance_timeout` may be `1m`
through `24h`. The internal event body limit must be 16–4096 bytes. Rate and
burst values are bounded, and at most 32 targets and 16 notifiers are allowed.

### Restic targets

Each target has a unique lowercase name, explicit backend, repository file,
backup credentials, and optional maintenance credentials. The repository file
itself is `0600`, because authenticated repository URLs and local topology can
be sensitive. A CA file, when configured, is also read from a `0600` file.

| `backend` | Repository-file prefix/form |
| --- | --- |
| `local` | clean absolute path |
| `rest` | `rest:https://...` or intentionally private `rest:http://...` |
| `s3` | `s3:https://...` or `s3:http://...` for MinIO/COS-compatible endpoints |
| `b2` | `b2:...` |
| `azure` | `azure:...` |
| `gcs` | `gs:...` |
| `swift` | `swift:...` |

For a local target, use a path below `/repositories` (for example,
`/repositories/primary`). Compose mounts the private host `./repositories`
directory into backup, maintenance, and restore containers. A local repository
is useful as an additional fast recovery copy but does not protect against loss
of that host or filesystem.

An optional `options` object is deliberately narrow. For `s3` targets it may
contain `s3.bucket-lookup` (`auto`, `dns`, or `path`), `s3.region`, and
`s3.list-objects-v1` (`true` or `false`). For example, COS and other providers
that require virtual-hosted bucket addressing can use:

```json
"options": {
  "s3.bucket-lookup": "dns",
  "s3.region": "ap-singapore"
}
```

Options for command-backed SFTP/rclone or arbitrary Restic flags are never
accepted.

`sftp:` and `rclone:` repositories are rejected because neither external tool
is included in the image.

`password_file` supplies Restic's repository password. `environment_files`
maps an approved environment name to a file whose contents become the value.
Approved names are:

- AWS: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`,
  `AWS_DEFAULT_REGION`;
- B2: `B2_ACCOUNT_ID`, `B2_ACCOUNT_KEY`;
- Azure: `AZURE_ACCOUNT_NAME`, `AZURE_ACCOUNT_KEY`, `AZURE_ACCOUNT_SAS`,
  `AZURE_ENDPOINT_SUFFIX`;
- GCS: `GOOGLE_PROJECT_ID`;
- REST: `RESTIC_REST_USERNAME`, `RESTIC_REST_PASSWORD`;
- Swift/OpenStack: `OS_AUTH_URL`, `OS_USERNAME`, `OS_PASSWORD`,
  `OS_REGION_NAME`, Keystone v1 `ST_AUTH`/`ST_USER`/`ST_KEY`, v2 tenant fields,
  v3 user/domain/project fields, trust/application-credential fields, token
  fields, and `SWIFT_DEFAULT_CONTAINER_POLICY` documented by Restic 0.19.1.

`environment_paths` currently accepts only
`GOOGLE_APPLICATION_CREDENTIALS`; Restic receives the mounted `0600` JSON file
path rather than its contents. The worker scrubs inherited Restic and backend
credential variables before constructing each target-specific process.

The long-running worker uses only `backup_credentials` and invokes only
`restic backup`. `repo` and `restore` require `maintenance_credentials`. Give
the backup identity only append/create permissions where the backend supports
that distinction; reserve delete, unlock, full read/check, and repository
administration for the maintenance identity.

### Notifiers

Notifiers operate in parallel. Their failures are persisted and retried but do
not change a backup result.

Generic webhook (HTTPS, no redirects; header values come from secret files):

```json
{
  "name": "automation",
  "type": "webhook",
  "timeout": "10s",
  "url": "https://hooks.example.com/vaultwarden-less",
  "headers_from_files": {
    "Authorization": "/run/secrets/webhook_authorization"
  }
}
```

Enterprise WeChat (the whole authenticated URL is secret):

```json
{
  "name": "wechat",
  "type": "wechat",
  "timeout": "10s",
  "url_file": "/run/secrets/wechat_url"
}
```

Bark through APNs HTTP/2:

```json
{
  "name": "bark",
  "type": "bark",
  "timeout": "10s",
  "endpoint": "https://api.push.apple.com",
  "device_token_file": "/run/secrets/bark_device_token",
  "auth_key_file": "/run/secrets/bark_apns_auth_key",
  "key_id": "KEY_ID",
  "team_id": "TEAM_ID",
  "topic": "me.fin.bark",
  "sound": "1107"
}
```

The endpoint is restricted to `https://api.push.apple.com` or
`https://api.sandbox.push.apple.com`; redirects and custom APNs origins are not
accepted.

No APNs private key is bundled or accepted in JSON. `auth_key_file` must hold a
PKCS#8 P-256 key. APNs HTTP status and `reason`, WeChat HTTP status and
`errcode == 0`, and generic webhook 2xx status are all required for delivery.

## Secret-free validation

Validate syntax alone:

```sh
docker compose --profile maintenance run --rm maintenance \
  config --config /config/backup.json
```

Also inspect every mounted secret's type, permissions, non-empty contents, and
backend consistency without printing it:

```sh
docker compose --profile maintenance run --rm maintenance \
  config --check-secrets --config /config/backup.json
```
