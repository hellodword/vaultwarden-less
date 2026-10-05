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

Set these **host** paths to the unpacked release and a new deployment directory:

```sh
set -euo pipefail
export RELEASE_DIR=/absolute/path/to/release
export DEPLOYMENT_ROOT=/srv/vaultwarden-less
test -f "$RELEASE_DIR/compose.yml"
for image in vaultwarden proxy backup; do
  docker load --input "$RELEASE_DIR/images/$image.tar.gz"
done
```

## Prepare configuration and credentials

The following creates a new tree and refuses an existing destination. For an
existing deployment, retain its configuration and follow the upgrade guide.

```sh
set -euo pipefail
umask 077
test ! -e "$DEPLOYMENT_ROOT"
test ! -L "$DEPLOYMENT_ROOT"
mkdir -m 0700 -- "$DEPLOYMENT_ROOT"
cp -R -- "$RELEASE_DIR/templates/." "$DEPLOYMENT_ROOT/"
mkdir -m 0700 -- "$DEPLOYMENT_ROOT/data" "$DEPLOYMENT_ROOT/cache" "$DEPLOYMENT_ROOT/restore"
```

Edit the copies before starting containers:

1. `compose.env`: set the same absolute `DEPLOYMENT_ROOT`, non-root numeric
   `RUNTIME_UID`/`RUNTIME_GID` (normally `id -u`/`id -g`) and loopback port.
   Keep the release's `VWLESS_IMAGE_TAG`. The selected identity needs access to
   the private files and write access to `data/` and `cache/`.
2. `config/vaultwarden.env`: review the minimal-feature profile; set `DOMAIN`
   to your external HTTPS origin if needed, and configure upstream SMTP settings
   only when using mail. These are Vaultwarden settings, not backup settings.
3. `config/backup.yaml`: select target names, container credential paths and
   backend options. `notifiers: []` disables notifications; add complete entries
   from the configuration reference when required.
4. `credentials/backup/primary.env` and `credentials/maintenance/primary.env`:
   replace the example repository and credentials. Use the same repository and
   password, with separate principals for backup and maintenance. Adjust native
   restic variables for non-S3 backends. Do not quote or interpolate their values.
5. `credentials/restic/primary`: fill this empty file with only the restic
   password, without a variable name, quotes or comments. For an existing
   repository, use its existing password. Keep an independent secure recovery
   copy; local cache cannot replace the repository password.

Use 0600 for configuration and credential files and 0700 for private directories.
Do not put real credentials into a source checkout or shell command history.
Repeat the destination files and YAML entry for additional repositories.
Container paths such as `/run/credentials/restic/primary` stay as container
paths; the Compose file maps their host sources.

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

Define a helper for the supplied Compose file and render its configuration
without printing sensitive environment values:

```sh
set -euo pipefail
compose() {
  docker compose --env-file "$DEPLOYMENT_ROOT/compose.env" \
    --file "$RELEASE_DIR/compose.yml" "$@"
}
compose config --quiet
compose run --rm --no-deps backup validate
RESTIC_IMAGE=$(compose config --images backup)
```

`validate` checks local YAML, env files and APNs keys. It does not contact a
repository or prove that provider credentials, password or permissions work.
Set up the `restic_primary` helper from
[native restic maintenance](operations.md#native-restic-maintenance), matching
the target's backend options. **Before starting backup**, run
`restic_primary init` once for a new empty repository. For an existing
repository, use `restic_primary snapshots` and `restic_primary check` instead.
Repeat for every target; never initialize over an existing repository as a
troubleshooting shortcut.

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
