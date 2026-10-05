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

```sh
nix develop --command just check
nix develop --command just release
nix develop --command just check-release
```

`check` runs ordinary Nix checks. `release` builds the deployment bundle.
`check-release` runs the packaged acceptance suite on the current Docker host,
using official Bitwarden CLI operations, two Moto repositories and four
notification protocol peers. Its retained results are reported on stdout.
The Docker host and runner must share the same absolute work path.

- [Configuration](docs/configuration.md): deployment paths, environment files,
  destinations and notification channels.
- [Operations](docs/operations.md): startup, checks, native repository
  maintenance, restore and upgrade.
- [Product contracts](docs/contracts.md): trigger, comparison, scheduling and
  failure behavior.
- [Maintenance](docs/maintenance.md): Nix inputs, upstream review and tests.
- [NOTICE](NOTICE.md): distribution and corresponding-source obligations.
