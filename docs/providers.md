# Storage providers

Vaultwarden-less is provider-neutral. It passes one declared repository,
ordered Restic S3 options, and an isolated environment to the pinned Restic
binary. The examples below use Restic's
[documented S3-compatible repository syntax](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html).
Confirm the current provider documentation, account, region, bucket, prefix,
and key scope before initializing a repository.

## Common contract

For each target, put the repository and options in `config/backup.yaml`:

```yaml
targets:
  - name: primary
    repository: s3:https://<endpoint>/<bucket>/<dedicated-prefix>
    options:
      - name: s3.region
        value: <signing-region>
      - name: s3.bucket-lookup
        value: dns
```

Put provider credentials in both role files, using different provider keys:

```yaml
targets:
  primary:
    environment:
      AWS_ACCESS_KEY_ID: replace-me
      AWS_SECRET_ACCESS_KEY: replace-me
```

Temporary S3 credentials additionally require `AWS_SESSION_TOKEN`. The
schema accepts it, but the token must remain valid for the longest product
operation: backup targets can run for two hours and maintenance commands for
24 hours. Prefer provider-managed workload credentials only when they can be
materialized as this complete, fixed role environment; the product does not
query instance metadata or inherit ambient credentials.

### Minimum effective access

Restic needs to list the selected bucket/prefix, read and write objects, delete
objects during unlock/prune, and complete or abort multipart uploads. In AWS
S3 action terms, a practical lower bound is bucket-scoped `s3:ListBucket`,
`s3:GetBucketLocation`, and `s3:ListBucketMultipartUploads`, plus
prefix-scoped `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`,
`s3:AbortMultipartUpload`, and `s3:ListMultipartUploadParts`. AWS publishes a
[bucket read/write policy example](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_examples_s3_rw-bucket.html).

Translate those operations to the provider's IAM model. Do not grant bucket
creation, account administration, key administration, public ACL, lifecycle
administration, retention bypass, or access to unrelated buckets merely to
make a backup succeed. A prefix restriction is useful only if the provider
enforces it for list, multipart, read, write, and delete consistently; verify
with `repo init`, a real backup, `repo check`, restore, dry-run prune, and a
drill.

Create independent keys for the resident backup and one-shot maintenance
roles. Rotate one role at a time: create a replacement key, update its private
YAML atomically, validate, exercise that role, deactivate the old key, observe
for missed use, and only then delete it. Never delete the only working key
before a repository check and restore have succeeded with its replacement.

### Lifecycle and object-lock warning

Restic owns repository retention. A provider lifecycle rule that expires the
current object, transitions it to an unreadable archive class, or rewrites
arbitrary objects can silently destroy repository integrity. Do not apply
age-based current-object deletion or archive transition to an active Restic
prefix. A provider-specific rule that removes only hidden/noncurrent versions
may be appropriate when Restic and the provider explicitly document it, but it
still requires a real backup, prune, check, and restore test.

Object lock or bucket retention can protect against deletion, but it can also
make Restic unlock, forget, and prune fail until retention expires. Some
providers make compliance settings irreversible or allow only extension. Test
the exact provider, API compatibility, retention period, and full maintenance
cycle in a separate repository before enabling a lock. Keep an independently
recoverable target rather than treating provider retention as a substitute for
tested Restic snapshots.

## AWS S3

```yaml
repository: s3:s3.<region>.amazonaws.com/<bucket>/<prefix>
options:
  - name: s3.region
    value: <region>
  - name: s3.bucket-lookup
    value: dns
```

Use an IAM role with temporary credentials when the deployment has a safe
credential delivery mechanism; otherwise use a dedicated IAM user, never root
account keys. Scope its identity or bucket policy to the one bucket and prefix
using the common operations above. AWS recommends temporary credentials where
possible and documents a no-interruption sequence for
[updating access keys](https://docs.aws.amazon.com/IAM/latest/UserGuide/id-credentials-access-keys-update.html).

Review [S3 Object Lock](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html)
before enabling it: it requires versioning, and compliance mode can prevent
deletion even by the account root user. AWS documents how
[lifecycle interacts with locked versions](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock-managing.html).
Do not attach an expiration rule to the Restic prefix.

## Cloudflare R2

```yaml
repository: s3:https://<account-id>.r2.cloudflarestorage.com/<bucket>/<prefix>
options:
  - name: s3.region
    value: auto
  - name: s3.bucket-lookup
    value: path
```

Create an R2 API token with **Object Read & Write** and scope it to the one
bucket. The [R2 S3 guide](https://developers.cloudflare.com/r2/get-started/s3/)
maps its access-key ID and secret to the standard AWS variables. Cloudflare
also documents [bucket-scoped API tokens](https://developers.cloudflare.com/r2/api/tokens/)
and [temporary S3 credentials](https://developers.cloudflare.com/r2/api/s3/temporary-credentials/).
Create a second token before revoking the old one and test each product role
independently.

R2 lifecycle rules can expire objects; do not configure them on the active
repository prefix. R2 has provider-specific
[bucket lock rules](https://developers.cloudflare.com/r2/buckets/bucket-locks/),
while its [S3 compatibility table](https://developers.cloudflare.com/r2/api/s3/api/)
does not imply every AWS Object Lock operation is supported. A lock overrides
lifecycle deletion and can block Restic maintenance, so validate the exact R2
behavior before use.

## Tencent Cloud COS

```yaml
repository: s3:https://cos.<region>.myqcloud.com/<bucket-name-appid>/<prefix>
options:
  - name: s3.region
    value: <region>
  - name: s3.bucket-lookup
    value: dns
```

COS maps `SecretId` to `AWS_ACCESS_KEY_ID` and `SecretKey` to
`AWS_SECRET_ACCESS_KEY`. Its official
[S3-compatible client guide](https://intl.cloud.tencent.com/document/product/436/34688?lang=en)
requires the full `<BucketName-APPID>` name and current virtual-hosted access;
keep `s3.bucket-lookup=dns` and use HTTPS.

Create dedicated CAM identities and a custom policy scoped to the selected
bucket/prefix. Review COS
[authorization policy evaluation](https://cloud.tencent.com/document/product/436/41516)
and audit/rotate long-lived keys; Tencent documents the risk of
[inactive and unrotated access keys](https://cloud.tencent.com/document/product/598/120122).

COS [object lock](https://cloud.tencent.com/document/product/436/55293)
has provider prerequisites and retention limitations. Its
[lifecycle configuration](https://cloud.tencent.com/document/product/436/17031)
can transition or delete objects asynchronously. Neither should be enabled on
the repository prefix until real init, backup, check, prune, restore, and drill
operations pass under the intended policy.

## Alibaba Cloud OSS

```yaml
repository: s3:https://oss-<region>.aliyuncs.com/<bucket>/<prefix>
options:
  - name: s3.region
    value: <region>
  - name: s3.bucket-lookup
    value: dns
```

OSS supports only virtual-hosted request style for its S3-compatible API, as
documented in the
[OSS/S3 compatibility guide](https://help.aliyun.com/en/oss/developer-reference/compatibility-with-amazon-s3).
Use a dedicated RAM user or short-lived STS credentials, not the Alibaba Cloud
account AccessKey. Give it only the equivalent list, read, write, delete, and
multipart actions for the selected bucket/prefix. Alibaba documents
[RAM least-privilege identities](https://help.aliyun.com/zh/ram/user-guide/overview-of-ram-users)
and recommends [regular AccessKey rotation](https://help.aliyun.com/zh/ram/regularly-rotate-accesskey-pairs-of-ram-users).

An OSS [WORM retention policy](https://help.aliyun.com/zh/oss/developer-reference/worm)
can become non-shortenable after it is locked. OSS
[lifecycle rules](https://help.aliyun.com/zh/oss/user-guide/overview-54/)
can delete existing as well as future objects and API updates can replace the
whole rule set. Keep the active Restic prefix out of those rules unless the
full immutable-repository maintenance behavior has been proven.

## Scaleway Object Storage

```yaml
repository: s3:https://s3.<region>.scw.cloud/<bucket>/<prefix>
options:
  - name: s3.region
    value: <region>
  - name: s3.bucket-lookup
    value: dns
```

Use a current region such as `fr-par`, `nl-ams`, `pl-waw`, or `it-mil`; confirm
the list and endpoint in Scaleway's
[Object Storage concepts](https://www.scaleway.com/en/docs/object-storage/concepts/).
Create a non-human IAM application and
[a dedicated API key](https://www.scaleway.com/en/docs/iam/how-to/create-api-keys/)
with the correct preferred Object Storage project. Apply only the documented
[Object Storage permission sets](https://www.scaleway.com/en/docs/iam/reference-content/permission-sets/)
needed for bucket listing and object read/write/delete. Scaleway's IAM
[rotation guidance](https://www.scaleway.com/en/docs/iam/faq/)
is to generate a new API key and delete the old one after migration.

Scaleway supports both
[lifecycle rules](https://www.scaleway.com/en/docs/object-storage/api-cli/lifecycle-rules-api/)
and [Object Lock](https://www.scaleway.com/en/docs/object-storage/how-to/use-object-lock/).
Expiration can delete data and compliance retention can block deletion; keep
both away from an active repository until the intended Restic workflow passes.

## Backblaze B2

```yaml
repository: s3:https://s3.<region>.backblazeb2.com/<bucket>/<prefix>
options:
  - name: s3.region
    value: <region>
  - name: s3.bucket-lookup
    value: path
```

Backblaze exposes both a native B2 backend and an S3-compatible API. Restic
documents that the native backend can be used but
[recommends the S3-compatible API](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html)
because of native B2 error-handling behavior. Use the S3 endpoint shown for the
bucket in the Backblaze console.

Create a standard application key, not the master key. Map the application key
ID to `AWS_ACCESS_KEY_ID` and the application key to
`AWS_SECRET_ACCESS_KEY`, following Backblaze's
[S3-compatible app-key guide](https://www.backblaze.com/docs/cloud-storage-s3-compatible-app-keys).
Scope it to one bucket and, when compatible with listing, the repository
prefix; grant read and write so Restic can list, upload, download, and delete.
Backblaze documents bucket/prefix scope, expiration, and key capabilities in
[Application Keys](https://www.backblaze.com/docs/en/cloud-storage-application-keys).
Create and test a replacement key before deleting the old one.

Backblaze [Object Lock](https://www.backblaze.com/docs/cloud-storage-object-lock)
cannot be disabled after it is enabled on a bucket and causes deletion or
lifecycle attempts on locked files to fail. Its
[lifecycle rules](https://www.backblaze.com/docs/cloud-storage-configure-and-manage-lifecycle-rules)
hide or delete file versions. Restic specifically recommends B2's **Keep only
the last version** rule for S3-backend objects that Restic has hidden; that is
different from expiring current objects by age. Verify the exact rule with a
disposable repository and do not let either lifecycle or lock settings
invalidate Restic's own retention and pruning model.

## Provider acceptance

Configuration validation proves syntax, modes, ownership, and role isolation;
it does not contact the provider. Before production use, run this sequence for
each target with both roles where applicable:

1. initialize a known-empty dedicated prefix;
2. create at least two real recovery snapshots with changed Vaultwarden data;
3. list bounded snapshots and run `repo check`;
4. preview the fixed prune set without applying it;
5. restore latest and one explicit earlier full snapshot ID;
6. start an isolated drill and verify data with fresh sessions;
7. rotate each role key and repeat validation plus a real repository action;
8. document provider-side lock/lifecycle settings and repeat after any change.

Provider dashboards and successful uploads are not enough. The acceptance fact
is a verified Restic round trip and drill using the exact production endpoint,
credentials, password, prefix, and policy.
