# Configuration templates

Start with [the deployment guide](../docs/deployment.md). Copy these files into
an operator-owned deployment directory, then edit the copies. Do not change
the packaged originals or store real credentials in the source checkout.

| File | Reader and purpose | Apply changes |
| --- | --- | --- |
| `compose.env` | Compose interpolation: host root, image tag, UID/GID and loopback port | `docker compose up -d` |
| `config/vaultwarden.env` | Compose service `env_file`, then Vaultwarden; minimal-feature defaults | Recreate Vaultwarden |
| `config/backup.yaml` | Backup worker: destinations and notifications | Restart backup |
| `credentials/backup/<target>-backup.env` | Worker using Docker's env-file parser; native restic backend credentials | Restart backup |
| `credentials/restic/<target>-repository-password.txt` | Native restic password file, shared by both roles for that repository | Next restic command; changing this file alone does not change a repository key |

The supplied `backup.yaml` lists three independent destinations. Keep the
ones you intend to use and remove the others, along with their unused credential
files. These names identify destinations in status and notifications; the
repository URL selects the actual restic backend. All three examples use S3 or
an S3-compatible API.

| Target | File in `credentials/backup/` | File in `credentials/restic/` |
| --- | --- | --- |
| AWS S3 (`aws-s3`) | `aws-s3-backup.env` | `aws-s3-repository-password.txt` |
| Cloudflare R2 (`cloudflare-r2`) | `cloudflare-r2-backup.env` | `cloudflare-r2-repository-password.txt` |
| Backblaze B2 (`backblaze-b2`) | `backblaze-b2-backup.env` | `backblaze-b2-repository-password.txt` |

For each retained target, fill in its backup env file with the repository URL,
region, password path and backup credentials. Prepare separate maintenance
credentials when needed, following [native restic maintenance](../docs/operations.md#native-restic-maintenance).
Maintenance env files are documented there and are not shipped as templates.
Each target has its own empty password file. Fill it with the repository password
before `restic init`, or the existing password when adopting a repository. It is
**raw password data**: do not add `PASSWORD=`, quotes or comments. Use one line
for the password. Keep an independent secure copy along with the repository location and recovery
access. Changing a repository password requires native restic key management.

Two notifier examples are included in `config/backup.yaml`: `wecom-operations`
for an Enterprise WeChat group robot, and `bark-apns-operations` for direct Apple
APNs delivery to Bark. Replace the webhook and APNs placeholders for the channels
you keep. For Bark, supply the authorized app's P-256 PKCS#8 private key as
`credentials/backup/bark-apns-auth-key.p8`; no key file is shipped. The device
token, key and topic must be authorized for the same app. An unrelated Apple
developer key cannot push to the official Bark app. Local `validate` fails
until that key is supplied or the Bark entry is removed. Remove other unused
notifier entries too; `notifiers: []` disables notifications.

Use private directory permissions (0700) and file permissions (0600), readable
by the selected non-root runtime UID/GID. Replace every example URL and key.
Restic env files use literal values; Compose's env files instead support
interpolation and quoting. None of these files should be sourced by a shell.

Paths beginning `/data`, `/cache`, `/config` or `/run/credentials` refer to the
container mount contract. `DEPLOYMENT_ROOT` and bind sources are host paths.
Do not replace container paths with host paths inside `backup.yaml` or restic
env files. See [configuration reference](../docs/configuration.md) for precedence,
backend options and notifier fields.
