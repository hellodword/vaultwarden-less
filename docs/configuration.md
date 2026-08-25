# Configuration

`backup.yaml` is private YAML with schema version 1:

```yaml
schema_version: 1
target_timeout: 30m
maintenance_timeout: 12h
targets: []
notifiers: []
```

Targets and notifiers are sorted by name. The validator reports the first
semantic error in a fixed order. YAML syntax errors are exposed as the stable
message `decode backup config: invalid YAML`; syntax parser details are not a
configuration contract.

## S3 target

The repository form is exactly an S3 locator without embedded credentials:

```yaml
targets:
  - name: primary
    repository: s3:https://s3.example.test/bucket/vaultwarden
    restic_options:
      s3.bucket-lookup: path
      s3.region: us-east-1
```

The endpoint may use `http://`; configuration output and startup logs then
include `plaintext_s3_endpoint`. Query strings, fragments, user information,
empty bucket components, control characters, and path traversal components are
rejected. Only `s3.bucket-lookup` (`auto`, `dns`, or `path`) and `s3.region`
are accepted options.

All supported object-storage providers use this same S3 contract. Examples of
the endpoint and option conventions are available from the providers:

- AWS S3: `s3:https://s3.us-east-1.amazonaws.com/bucket/prefix`
- Backblaze B2 S3: `s3:https://s3.us-west-004.backblazeb2.com/bucket/prefix`
- Tencent COS: `s3:https://cos.ap-singapore.myqcloud.com/bucket/prefix`
- Alibaba OSS: `s3:https://oss-cn-hangzhou.aliyuncs.com/bucket/prefix`
- Scaleway: `s3:https://s3.fr-par.scw.cloud/bucket/prefix`
- Cloudflare R2: `s3:https://account-id.r2.cloudflarestorage.com/bucket/prefix`

The credentials file may contain extra target names and keys. The worker reads
only `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` for each configured target.
Repository checks verify the mounted material; they do not assert a provider
role name. Use `config --check-repository-credentials` to opt into that check.

The one Restic password file is shared by every target. Treat it as having the
blast radius of the complete configured repository set.

## Notifications

Notification material is kept in private `backup.yaml`:

```yaml
notifiers:
  - name: operations
    type: wechat
    timeout: 10s
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=REPLACE_ME
```

The other supported type is `bark`, which uses its APNs device token, key,
team, topic, and optional sound fields. Use
`config --check-notifier-material` independently of repository checks.

## Zero-target mode

An empty target list is valid. The worker does not open Restic, read repository
credentials or the password file, create a local recovery point, or touch the
staging directory. Events, status, notification queue handling, and liveness
continue to work.

## Compose settings

The product image uses fixed UID/GID `65532:65532`. Vaultwarden settings come
only from `.env.vaultwarden`; do not put its application configuration in a
second Compose environment file. Put Nginx or Caddy in front of the loopback
proxy binding.
