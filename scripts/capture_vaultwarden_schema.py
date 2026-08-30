"""Capture a deterministic complete SQLite schema from an audited Vaultwarden DB."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sqlite3
import stat
import sys
import tempfile
from pathlib import Path

import tomllib


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--database", required=True, type=Path)
    parser.add_argument("--upstreams", type=Path, default=Path("UPSTREAMS.toml"))
    parser.add_argument(
        "--output",
        type=Path,
        default=Path("internal/upstream/vaultwarden-schema.json"),
    )
    return parser.parse_args()


def schema_fingerprint(objects: list[dict[str, str]]) -> str:
    digest = hashlib.sha256()
    for item in objects:
        for name in ("type", "name", "table", "sql"):
            digest.update(item[name].encode("utf-8"))
            digest.update(b"\0")
        digest.update(b"\n")
    return "sha256:" + digest.hexdigest()


def require_regular(path: Path, description: str) -> None:
    information = path.lstat()
    if stat.S_ISLNK(information.st_mode) or not stat.S_ISREG(information.st_mode):
        raise ValueError(f"{description} must be a regular non-symlink file")


def require_output_parent(path: Path) -> Path:
    parent = path.parent if path.parent != Path("") else Path(".")
    information = parent.lstat()
    if stat.S_ISLNK(information.st_mode) or not stat.S_ISDIR(information.st_mode):
        raise ValueError("output parent must be a real directory")
    if path.exists() or path.is_symlink():
        require_regular(path, "existing output")
    return parent


def capture(database_path: Path, upstreams_path: Path) -> bytes:
    require_regular(database_path, "source database")
    require_regular(upstreams_path, "upstream inventory")
    upstreams = tomllib.loads(upstreams_path.read_text(encoding="utf-8"))
    vaultwarden = upstreams["vaultwarden"]
    expected_migration = vaultwarden["database_migration"]
    source_tag = vaultwarden["source_tag"]
    if not isinstance(expected_migration, str) or not isinstance(source_tag, str):
        raise TypeError("Vaultwarden audit metadata is invalid")

    uri = database_path.resolve(strict=True).as_uri() + "?mode=ro"
    database = sqlite3.connect(uri, uri=True)
    try:
        database.execute("PRAGMA query_only = ON")
        if database.execute("PRAGMA quick_check").fetchall() != [("ok",)]:
            raise ValueError("source database failed quick_check")
        migration = database.execute(
            "SELECT max(version) FROM __diesel_schema_migrations"
        ).fetchone()[0]
        if migration != expected_migration:
            raise ValueError("source database migration does not match UPSTREAMS.toml")
        rows = database.execute(
            """
            SELECT type, name, tbl_name, coalesce(sql, '')
            FROM sqlite_schema
            WHERE type IN ('index', 'table', 'trigger', 'view')
              AND name NOT LIKE 'sqlite_%'
            ORDER BY type, name
            """
        ).fetchall()
    finally:
        database.close()
    objects = [
        {"type": row[0], "name": row[1], "table": row[2], "sql": row[3].strip()}
        for row in rows
    ]
    if not objects:
        raise ValueError("source database has no application schema objects")
    contract = {
        "vaultwarden_version": source_tag,
        "database_migration": expected_migration,
        "fingerprint": schema_fingerprint(objects),
        "objects": objects,
    }
    return (json.dumps(contract, ensure_ascii=False, indent=2) + "\n").encode("utf-8")


def atomic_write(path: Path, data: bytes) -> None:
    parent = require_output_parent(path)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=parent
    )
    temporary = Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o644)
        with os.fdopen(descriptor, "wb", closefd=True) as output:
            descriptor = -1
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        temporary.unlink(missing_ok=True)


def main() -> int:
    args = parse_args()
    try:
        atomic_write(args.output, capture(args.database, args.upstreams))
        print(args.output)
    except (
        KeyError,
        OSError,
        sqlite3.Error,
        TypeError,
        ValueError,
        tomllib.TOMLDecodeError,
    ) as error:
        print(f"capture Vaultwarden schema failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
