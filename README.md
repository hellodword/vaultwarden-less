# vaultwarden-less

`vaultwarden-less` is a small Linux/amd64 service that watches a Vaultwarden
SQLite data directory, creates verified recovery points, and uploads them to
Restic 0.19.1 through the S3 API. The supported Vaultwarden image is 1.37.2;
the supported data source is `/data/db.sqlite3` plus its current attachment and
RSA key files.

The product image contains the backup worker, the event proxy, and Restic.
Compose runs every service as UID/GID `65532:65532`. Put Nginx or Caddy in
front of the proxy; the proxy publishes only to loopback by default.

## Quick start

1. Create the private bind-mounted directories `data`, `state`, `staging`,
   `cache`, `restore`, `config`, and the credential directories.
2. Copy `.env.vaultwarden.template` to `.env.vaultwarden` and set the
   Vaultwarden application values.
3. Copy `config/backup.template.yaml` to `config/backup.yaml`. Configure one
   or more `s3:https://...` repositories and the AWS-style credentials in
   `credentials/backup/credentials.yaml`. Keep the shared Restic password in
   `credentials/common/restic-password`.
4. Validate the files and start the stack:

   ```sh
   docker compose --env-file /dev/null config
   docker compose up -d vaultwarden proxy backup
   ```

   The backup password is shared by all configured targets, so compromise of
   that material reaches every target repository. Notification material stays
   in the private `backup.yaml`; maintenance and restore containers can read
   it when they are invoked.

The worker can run with zero targets. In that mode local event handling,
status, notifications, and liveness remain available without loading Restic
or repository credentials.

## Documentation

- [Configuration](docs/configuration.md)
- [Architecture](docs/architecture.md)
- [Operations](docs/operations.md)
- [Stable contracts](docs/contracts.md)

The project is intentionally single-platform and uses Go 1.26.7, Restic
0.19.1, and Vaultwarden 1.37.2 as one fixed version boundary.
