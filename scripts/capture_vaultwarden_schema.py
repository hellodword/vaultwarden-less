"""Capture a deterministic complete SQLite schema from an audited Vaultwarden DB."""

from __future__ import annotations

import argparse
import hashlib
import json
import sqlite3
import sys
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


def main() -> int:
    args = parse_args()
    try:
        upstreams = tomllib.loads(args.upstreams.read_text(encoding="utf-8"))
        expected_migration = upstreams["vaultwarden"]["database_migration"]
        uri = f"file:{args.database.resolve()}?mode=ro"
        database = sqlite3.connect(uri, uri=True)
        try:
            quick_check = database.execute("PRAGMA quick_check").fetchall()
            if quick_check != [("ok",)]:
                raise ValueError("source database failed quick_check")
            migration = database.execute(
                "SELECT max(version) FROM __diesel_schema_migrations"
            ).fetchone()[0]
            if migration != expected_migration:
                raise ValueError(
                    "source database migration does not match UPSTREAMS.toml"
                )
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
            "contract_version": 1,
            "vaultwarden_version": upstreams["vaultwarden"]["version"],
            "database_migration": expected_migration,
            "fingerprint": schema_fingerprint(objects),
            "objects": objects,
        }
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(
            json.dumps(contract, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
            newline="\n",
        )
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
