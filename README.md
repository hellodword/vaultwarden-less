# vaultwarden-less

`vaultwarden-less` is an opinionated Linux/amd64 backup and restore system for
one explicitly audited Vaultwarden release. It places a transparent event proxy
in front of Vaultwarden, creates a verified recovery point from SQLite plus the
attachment and file-Send payloads referenced by that database, and stores that
point in one or more S3-backed Restic repositories.

The project is intentionally early. It does not carry compatibility code for
older configuration, manifests, databases, tags, or deployment layouts. A
change to a durable contract must be made explicit, versioned, tested, and
documented.

## Sources of truth

The repository is designed to be understandable before it is upgradeable:

- [`VERSION`](VERSION) is the product version.
- [`UPSTREAMS.toml`](UPSTREAMS.toml) records every audited upstream tag,
  commit, digest, source hash, and audit surface.
- [`internal/upstream/vaultwarden-schema.json`](internal/upstream/vaultwarden-schema.json)
  is the complete accepted SQLite schema.
- [`Justfile`](Justfile) is the operator and developer command index.
- [`deploy/compose.template.yml`](deploy/compose.template.yml) is the only
  shipped deployment topology; `scripts/deploy.py` renders its small declared
  token set from strict deployment YAML and audited metadata.
- [`docs/upstream-maintenance.md`](docs/upstream-maintenance.md) is the
  one-upstream-at-a-time review procedure.

Hosted workflows, registry publication, and container build recipes outside
Nix are not part of the project. Nix builds the product binaries, patched
Restic, and all project-owned images locally. The official Vaultwarden image is
pulled only by its audited digest and retagged with a local-only name.

## Recovery boundary

A recovery point contains exactly:

- an SQLite online-backup copy at `data/db.sqlite3`;
- attachment files referenced by that copy; and
- file-Send payloads referenced by that copy.

It excludes RSA keys, `config.json`, caches, templates, temporary files,
SQLite sidecars, Vaultwarden-created backups, and orphaned payloads. After a
restore, Vaultwarden creates a new RSA key, so existing access, refresh, invite,
attachment, and download tokens may stop working. Users should establish fresh
sessions.

The scheduler and notifier queues are in memory. Restic snapshots are the
durable fact. Startup and periodic checks recover from lost process-local
signals; they do not reconstruct notification history.

## Commands

Use the pinned Just app as the only outer interface:

```sh
nix run .#just -- --list
nix run .#just -- check
nix run .#just -- check-full
```

Recipes enter either the full development shell or the lightweight deployment
shell themselves. Do not install project tools globally.

For deployment, build the complete image bundle on a fast Linux/amd64 machine,
copy it to the target, and load it there:

```sh
nix run .#just -- bundle
nix run .#just -- bundle-load dist/vaultwarden-less-<version>-linux-amd64
nix run .#just -- init <runtime-uid> <runtime-gid>
```

The bundle is a checked `docker save` archive. The deployment device performs
`docker load`; it does not compile Vaultwarden-less or Restic. See
[Operations](docs/operations.md#offline-image-transfer) before initial
deployment.

After editing the generated private configuration:

```sh
nix run .#just -- config-validate
nix run .#just -- repo-init primary
nix run .#just -- up
nix run .#just -- status
```

Only the proxy is published, on loopback by default. A trusted TLS edge must
remove untrusted forwarding headers and send root-path HTTP/WebSocket traffic
to that proxy. The worker API remains on a private Compose network.

## Documentation

- [Architecture](docs/architecture.md)
- [Configuration](docs/configuration.md)
- [Contracts](docs/contracts.md)
- [Operations and recovery](docs/operations.md)
- [Verification evidence and acceptance](docs/verification.md)
- [Upstream review and upgrade strategy](docs/upstream-maintenance.md)
- [Licensing and provenance](NOTICE.md)
