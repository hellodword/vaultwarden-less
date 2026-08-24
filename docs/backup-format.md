# v1 recovery-point format

Every recovery point is a directory with this shape before Restic reads it:

```text
current/
├── manifest.json
└── data/
    ├── db.sqlite3
    ├── attachments/<cipher_uuid>/<attachment_id>
    ├── sends/<send_uuid>/<file_id>
    ├── rsa_key*
    ├── config.json
    └── templates/**
```

Directories are `0700`; files are `0600`. Symlinks, devices, FIFOs, sockets,
path traversal, unsafe database identifiers, and files that change while being
copied are not accepted as ordinary snapshot content.

## Database

`data/db.sqlite3` is a newly created binary SQLite database produced through
the [SQLite Online Backup API](https://www.sqlite.org/backup.html). It is not a
copy of the live main file and is never a SQL text dump. The worker converts
the independent copy to DELETE journal mode, excludes its temporary sidecars,
and requires:

- `PRAGMA integrity_check` to return exactly `ok`;
- `PRAGMA foreign_key_check` to return no rows;
- Vaultwarden's core tables and attachment/Send reference columns;
- latest Diesel migration `20260505120000`, the schema shipped by Vaultwarden
  `1.37.2`.

The fixed snapshot database—not the changing live database—is then queried for
attachment and file Send references. Vaultwarden 1.37.2 currently generates
20-character lowercase hexadecimal attachment IDs and 64-character lowercase
hexadecimal Send file IDs. The recovery reader treats these durable values as
opaque 1–255 byte ASCII alphanumeric/hyphen path components so historical
local data is not rejected merely because an older generator used a different
safe length. Parent IDs remain canonical lowercase UUIDs. Recorded and copied
sizes must agree.

## Included and excluded assets

The v1 allowlist follows Vaultwarden's [backup
guidance](https://github.com/dani-garcia/vaultwarden/wiki/Backing-up-your-vault):

- required binary database;
- every locally stored attachment referenced by that database;
- every locally stored file Send referenced by that database;
- existing root files whose names begin `rsa_key`;
- existing `/data/config.json` admin overrides;
- every regular file below `/data/templates`.

Everything else is excluded, including live `db.sqlite3-wal` and
`db.sqlite3-shm`, icon cache, temporary uploads, built-in/old backups, unknown
directories, MinIO content, `.env`, Compose/project configuration, TLS files,
proxy/worker configuration, worker state/cache, Restic credentials, and notification
credentials.

The included `/data/config.json` is a Vaultwarden data asset. It is not a
replacement for the independently managed deployment configuration and secret
inventory.

## manifest.json

The manifest is deterministic JSON and contains no runtime timestamp, hostname,
absolute machine path, repository address, user data excerpt, or secret:

```json
{
  "schema_version": 1,
  "vaultwarden_version": "1.37.2",
  "database_migration": "20260505120000",
  "status": "complete",
  "files": [
    {
      "path": "data/db.sqlite3",
      "size": 262144,
      "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    }
  ],
  "missing": [],
  "content_id": "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
}
```

`files` is strictly sorted by path and covers every regular recovery file
except `manifest.json` itself. `missing` is sorted by path/reason. Supported
reasons are `not_found`, `size_mismatch`, and `changed_during_copy`.

The content ID is SHA-256 of canonical compact JSON containing the format
version, Vaultwarden version, database migration, integrity status, sorted file
records, and sorted missing records; the `content_id` field itself is excluded
from that calculation. Restore recalculates it and every file hash.

## Complete and incomplete sets

An unavailable referenced or concurrently changing file aborts the first two
attempts. The worker waits 30 seconds and re-creates the database and entire
set each time. The third attempt may create `status: incomplete`, lists every
missing item, still uploads all recoverable data, and creates a priority,
persistent logical alert after the first target succeeds.

`complete` and `incomplete` are separate Restic tags and separate retention
groups. An incomplete history cannot evict the newest complete history.
Restore selects the newest complete snapshot unless an exact incomplete
snapshot is named together with `--allow-incomplete`.

## Restic identity

Worker backups use stable metadata:

- host: `vaultwarden-less-v1`;
- tags: `vaultwarden-less-v1`, `complete` or `incomplete`, and
  `content-id=<64 lowercase hex>`;
- paths: `/staging/current/data` and `/staging/current/manifest.json`.

Maintenance filters all three dimensions before retention is evaluated. It
does not select old SQL/Git snapshots, snapshots for another application, or
unrelated hosts/paths/tags.

The v1 restore reader is a long-lived compatibility commitment. Future
releases may write a newer format only with an explicit schema version and
must continue reading v1 recovery points. Configuration migrations read an old
path and write a new path; they do not rewrite an operator's only copy in
place.
