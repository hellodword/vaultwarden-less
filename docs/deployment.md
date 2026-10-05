# Deployment

This guide prepares a new deployment from the release bundle. Read the
[configuration reference](configuration.md) while filling in the commented
[templates](../templates/README.md). After first startup, use
[operations](operations.md) for routine checks, maintenance, restore and upgrade.

## Prerequisites and bundle

The target host needs Linux/amd64, Docker Engine and the Docker Compose plugin.
It does not need Nix, Just, Go or repository scripts. Run the examples in Bash
as the selected non-root runtime user with Docker access. Provision an
operator-owned parent directory before creating the private deployment tree.

Provide external HTTPS for official Bitwarden clients. The bundle publishes
only the request proxy on host loopback. Public listening addresses, TLS,
access control and trusted forwarding headers belong to your external reverse
proxy. Keep the backup listener private. Size the data disk and cache for the
native database and files; [cache retention](contracts.md#repository-state)
can keep approximately `2N + 2` payload versions for `N` destinations.

Obtain the complete bundle from your build/distribution process. A maintainer
builds it with `just release`; the `result` output contains `images/`,
`compose.yml`, `templates/`, `docs/`, README and license notices. When copying
it to another host, copy file contents rather than a Nix store symlink.
Preserve the bundled image tag and notices. Keep each release in its own
directory for rollback.

Set these **host** paths to the unpacked release and a new deployment directory.
They are ordinary shell variables used by the examples; do not export them or
put them in `compose.env`:

```sh
set -euo pipefail
RELEASE_DIR=/absolute/path/to/release
DEPLOY_DIR=/srv/vaultwarden-less
test -f "$RELEASE_DIR/compose.yml"
for image in vaultwarden proxy backup; do
  docker load --input "$RELEASE_DIR/images/$image.tar.gz"
done
```

## Prepare configuration and credentials

The following creates a new tree and refuses an existing destination. It makes
the copied files private and writable by the operator. For an existing
deployment, retain its configuration and follow the upgrade guide.

```sh
set -euo pipefail
umask 077
test ! -e "$DEPLOY_DIR"
test ! -L "$DEPLOY_DIR"
mkdir -m 0700 -- "$DEPLOY_DIR"
cp -R -- "$RELEASE_DIR/templates/." "$DEPLOY_DIR/"
cp -- "$RELEASE_DIR/compose.yml" "$DEPLOY_DIR/compose.yml"
mkdir -m 0700 -- "$DEPLOY_DIR/data" "$DEPLOY_DIR/cache" "$DEPLOY_DIR/restore"
chmod -R u+rwX,go-rwx -- "$DEPLOY_DIR"
```

Edit the copies before starting containers:

1. `compose.yml`: set the shared `x-service-defaults.user` to the non-root
   numeric UID:GID that owns the deployment files (normally `id -u`/`id -g`).
   The default is `1000:1000`. The selected identity needs access to the private
   files and write access to `data/` and `cache/`. Adjust the proxy's host port
   in `127.0.0.1:8080:8080` if needed; keep container port 8080.
   `compose.env` contains only `VWLESS_IMAGE_TAG`; keep the release's value.
2. `config/vaultwarden.env`: review the minimal-feature profile; set `DOMAIN`
   to your external HTTPS origin if needed, and configure upstream SMTP settings
   only when using mail. These are Vaultwarden settings, not backup settings.
3. `config/backup.yaml`: keep the intended AWS S3, Cloudflare R2 and Backblaze B2
   target entries and remove the others. Review container credential paths and
   backend options. The template also includes Enterprise WeChat and Bark APNs:
   fill in the channels you keep and remove unused entries. `notifiers: []`
   disables notifications. Bark requires the authorized app's private key at
   `credentials/backup/bark-apns-auth-key.p8`; no key is shipped. Supply it or
   remove the Bark entry before running `validate`.
4. For each retained target, edit its backup env file, such as
   `credentials/backup/aws-s3-backup.env`. Replace the example repository,
   region and backup credentials. Adjust native restic variables for non-S3
   backends. Do not quote or interpolate their values. Remove unused targets'
   credential and password files.
5. Fill each retained target's empty `credentials/restic/<target>-repository-password.txt`
   with only its restic password, without a variable name, quotes or comments.
   For an existing repository, use its existing password. Keep an independent
   secure recovery copy; local cache cannot replace the repository password.

Use 0600 for configuration and credential files and 0700 for private directories.
Do not put real credentials into a source checkout or shell command history.
For additional repositories, copy the matching provider's files under unique
names and add a YAML entry with the new paths. Container paths such as
`/run/credentials/restic/aws-s3-repository-password.txt` stay as container paths;
the Compose file maps their host sources. Host paths such as `./data` and
`./config/vaultwarden.env` resolve relative to the deployment's `compose.yml`.

## Empty installation or existing data

For a new installation, leave `data/` empty. For adoption, use a **complete,
stopped SQLite Vaultwarden data directory**. Preserve the entire database and
any WAL/SHM sidecars, RSA keys, `config.json`, attachments, Sends, directories
and links. Stop every source writer before copying; never copy only a live
`db.sqlite3`. Keep the original directory or archive unchanged and copy into
the new destination with ownership readable/writable by the runtime identity.

Confirm that `data/db.sqlite3` is at the expected root, rather than nested
under an extra archive directory. Review existing `config.json`: its editable
settings override environment values, including security defaults. Do not
delete it blindly; retain a recovery copy and reconcile intentional settings.
Confirm version compatibility before startup because Vaultwarden can migrate
the database. For an upgrade or migration rehearsal, follow the preserved-copy
procedure in [operations](operations.md#upgrade-with-a-preserved-original).

## Validate and initialize repositories

Define a helper for the deployment's Compose file and render its configuration
without printing sensitive environment values. The absolute file paths allow
the helper to run from any working directory:

```sh
set -euo pipefail
compose() {
  docker compose --env-file "$DEPLOY_DIR/compose.env" \
    --file "$DEPLOY_DIR/compose.yml" "$@"
}
compose config --quiet
compose run --rm --no-deps backup validate
RESTIC_IMAGE=$(compose config --images backup)
```

`validate` checks local YAML, env files and APNs keys. It does not contact a
repository or prove that provider credentials, password or permissions work.
Prepare a maintenance env file and the `restic_repository` helper as described in
[native restic maintenance](operations.md#native-restic-maintenance). Maintenance
env files are prepared by the operator and are not included in the templates.
Match the target's backend options and select its maintenance file with
`RESTIC_ENV_FILE`. **Before starting backup**, run `restic_repository init`
once for a new empty repository. For an existing repository, use
`restic_repository snapshots` and `restic_repository check` instead.
Repeat for every target by changing `RESTIC_ENV_FILE`; never initialize over
an existing repository as a troubleshooting shortcut.

## Start and verify

```sh
compose up --detach
compose ps
compose exec --no-TTY backup /bin/vaultwarden-less-backup status
compose exec --no-TTY backup /bin/vaultwarden-less-backup check --full
```

A requested check is asynchronous. Inspect `status` again until the source
is complete, each intended target has a successful snapshot ID and time, and
no upload or retry remains pending. Confirm the snapshot remotely with native
restic. Health means process responsiveness, not successful data protection.
See [status interpretation](operations.md#checks-and-status) for failures.

Registration, invitations, Sends, emergency access and organization creation
are disabled by default. Existing accounts can log in through official clients;
Web Vault is disabled. For first enrollment, temporarily enable only the
necessary upstream setting through a controlled HTTPS endpoint, recreate
Vaultwarden with `compose up --detach vaultwarden`, enroll, then restore the
closed setting and recreate it again. Review `config.json` precedence first.
Disabling Sends prevents creation/modification but does not revoke existing
Sends or remove their data.

Verify representative client reads and writes through the external reverse
proxy and complete a restore rehearsal to a new directory. Provider IAM,
offsite durability and actual notification delivery to your devices/groups
remain operator checks.
