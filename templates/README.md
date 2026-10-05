# Configuration templates

Start with [the deployment guide](../docs/deployment.md). Copy these files into
an operator-owned deployment directory, then edit the copies. Do not change
the packaged originals or store real credentials in the source checkout.

| File | Reader and purpose | Apply changes |
| --- | --- | --- |
| `compose.env` | Compose interpolation: host root, image tag, UID/GID and loopback port | `docker compose up -d` |
| `config/vaultwarden.env` | Compose service `env_file`, then Vaultwarden; minimal-feature defaults | Recreate Vaultwarden |
| `config/backup.yaml` | Backup worker: destinations and notifications | Restart backup |
| `credentials/backup/primary.env` | Worker using Docker's env-file parser; native restic backend credentials | Restart backup |
| `credentials/maintenance/primary.env` | Operator's `docker run --env-file`; separate maintenance principal | Next maintenance command |
| `credentials/restic/primary` | Native restic password file, shared by both credential roles | Next restic command; changing this file alone does not change a repository key |

`credentials/restic/primary` is deliberately empty. Fill the copied file with
the repository password before `restic init`, or the existing password when
adopting a repository. It is **raw password data**, not an env file: do not add
`PASSWORD=`, quotes or comments. Use one line for the password. Keep an
independent secure copy along with the repository location and recovery access.
Changing a repository password requires native restic key management.

Use private directory permissions (0700) and file permissions (0600), readable
by the selected non-root runtime UID/GID. Replace every example URL and key.
The two restic env files use literal values; Compose's env files instead support
interpolation and quoting. None of these files should be sourced by a shell.

Paths beginning `/data`, `/cache`, `/config` or `/run/credentials` refer to the
container mount contract. `DEPLOYMENT_ROOT` and bind sources are host paths.
Do not replace container paths with host paths inside `backup.yaml` or restic
env files. See [configuration reference](../docs/configuration.md) for precedence,
backend options and notifier fields.
