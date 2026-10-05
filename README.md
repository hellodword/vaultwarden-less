# vaultwarden-less

Run the upstream SQLite Vaultwarden behind a transparent request proxy, with
an independent Go worker that saves the complete native data tree to one or
more restic repositories. Backups include RSA keys, attachments, Sends,
configuration, orphan files, empty directories and symbolic links.

The worker uses SQLite's online backup command against a read-only source
mount. Ordinary checks ignore explicitly identified authentication noise when
deciding whether to upload; the saved database always contains every table
and row. Full checks compare all database content. Missing source files produce
an explicitly incomplete backup containing the full database and available
files. Each destination has its own successful baseline and retry schedule.

Restic owns snapshot IDs, storage, deduplication, retention and restore. A
native `restic restore ID --verify --target NEW_DIRECTORY` produces
`db.sqlite3` and the native file tree at its root.

The deployment bundle contains three Linux/amd64 images, Compose, configuration
templates and operator documentation. It needs Docker Engine and Compose on
the deployment host. TLS and public access control belong to the operator;
the shipped proxy port binds to loopback.

For deployment, read these in order:

- [Deployment](docs/deployment.md): prerequisites, bundle, configuration,
  existing data, repository initialization and first startup.
- [Configuration](docs/configuration.md): file readers, security defaults,
  precedence, destinations and notification channels. Inline comments in
  [templates](templates/README.md) explain what to fill in.
- [Operations](docs/operations.md): status, checks, native repository
  maintenance, restore and upgrade after deployment.
- [Product contracts](docs/contracts.md): trigger, comparison, scheduling and
  failure behavior.

For source development, start with [Maintenance](docs/maintenance.md). Just
selects Nix outputs; Nix supplies tools and inputs; scripts perform the work:

```sh
just check
just release
just check-release
```

`check` runs ordinary checks without a Docker daemon. `release` builds the
deployment bundle. `check-release` runs the packaged Docker acceptance suite
and reports retained results. Target deployment hosts need Docker and Compose;
they do not need Just, Nix or the source scripts.

- [NOTICE](NOTICE.md): distribution and corresponding-source obligations.
