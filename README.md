# vaultwarden-less

`vaultwarden-less` 0.1.0 is an opinionated, Linux/amd64 backup and restore
tool for one supported Vaultwarden boundary: version 1.37.2 with SQLite
migration `20260505120000`. It places a transparent event proxy in front of
Vaultwarden, builds a verified recovery point from the live database and its
referenced attachment and file Send data, and sends that point to one or more
S3-backed Restic 0.19.1 repositories.

The product deliberately has no persistent scheduler database or notification
queue. Restic snapshots are the durable fact; process-local status is reset on
restart. A startup check and a check at most 12 hours after the preceding
successful one recover from lost in-memory signals.

## Supported deployment

- Nix supplies the pinned development, build, test, and operations environment.
- Just is the only outer command interface. Recipes invoke a tool or durable
  script inside exactly one `nix develop` environment.
- Dockerfile/Buildx builds the product image; the flake does not build or
  publish a product package or OCI image.
- Compose runs Vaultwarden, the loopback-only proxy, and the worker as the
  deployment directory's configured UID/GID. Nginx or another trusted edge
  must sanitize forwarding headers before sending root-path traffic to the
  proxy.

## Quick start

Install Nix with flakes enabled and provide a Docker daemon with Compose and
Buildx. Then use the project command app:

```sh
nix run .#just -- init
```

`init` creates only missing private directories and files. It never overwrites
existing content. Edit `.env.vaultwarden`, `config/backup.yaml`,
`credentials/backup/credentials.yaml`, and
`credentials/maintenance/credentials.yaml`; keep all of
them private. For a remote Docker daemon, set `DEPLOY_UID` and `DEPLOY_GID` in
`.env` to the owner of the bind-mounted remote deployment directory.

Validate, initialize the repository, and start the stack:

```sh
nix run .#just -- config-validate
nix run .#just -- repo-init primary
nix run .#just -- up
nix run .#just -- status
```

Only the proxy is published, at `127.0.0.1:8080` by default. The worker HTTP
API remains on the private Compose `events` network.

## Recovery boundary

A recovery point contains only:

- `data/db.sqlite3` copied with SQLite's online backup API;
- attachment files referenced by the copied database; and
- file Send payloads referenced by the copied database.

It excludes RSA keys, `config.json`, caches, templates, temporary files,
SQLite sidecars, Vaultwarden-created database backups, and orphan files. A
restored Vaultwarden generates a new RSA key, so old access, refresh, invite,
attachment, and download tokens can stop working. Users must sign in again.

The 0.1.0 configuration, manifest, runtime, and Restic tag contracts are new.
There is no migration from earlier state or snapshots, and new retention
commands do not delete snapshots carrying earlier product tags.

## Documentation

- [Configuration](docs/configuration.md)
- [Architecture](docs/architecture.md)
- [Operations](docs/operations.md)
- [Machine-readable contracts](docs/contracts.md)

`VERSION` is the single product version source. The pinned Go, Restic,
Vaultwarden, builder, and runtime image versions are part of the reviewed
release boundary.
