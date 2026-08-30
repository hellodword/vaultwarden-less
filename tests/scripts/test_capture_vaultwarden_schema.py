"""Contract tests for deterministic Vaultwarden SQLite schema capture."""

from __future__ import annotations

import json
import os
import sqlite3
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import capture_vaultwarden_schema as capture_schema


class CaptureVaultwardenSchemaTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.migration = "20260505120000"
        self.upstreams = self.root / "UPSTREAMS.toml"
        self.upstreams.write_text(
            "\n".join(
                (
                    "[vaultwarden]",
                    'source_tag = "1.37.2"',
                    f'database_migration = "{self.migration}"',
                    "",
                )
            ),
            encoding="utf-8",
        )

    def create_database(
        self, path: Path, *, migration: str | None = None, wal: bool = False
    ) -> sqlite3.Connection:
        database = sqlite3.connect(path)
        if wal:
            self.assertEqual(
                database.execute("PRAGMA journal_mode = WAL").fetchone()[0], "wal"
            )
            database.execute("PRAGMA wal_autocheckpoint = 0")
        database.execute(
            "CREATE TABLE __diesel_schema_migrations "
            "(version TEXT PRIMARY KEY NOT NULL, run_on TIMESTAMP NOT NULL)"
        )
        database.execute(
            "INSERT INTO __diesel_schema_migrations VALUES (?, '2026-08-27')",
            (migration or self.migration,),
        )
        database.execute(
            "CREATE TABLE users (id TEXT PRIMARY KEY NOT NULL, name TEXT NOT NULL)"
        )
        database.execute("CREATE INDEX users_name ON users(name)")
        database.commit()
        return database

    def test_special_path_wal_and_deterministic_bytes(self) -> None:
        database_path = self.root / "database ? [雪].sqlite3"
        database = self.create_database(database_path, wal=True)
        self.addCleanup(database.close)
        database.execute("CREATE TABLE wal_only (id INTEGER PRIMARY KEY, value BLOB)")
        database.commit()

        first = capture_schema.capture(database_path, self.upstreams)
        second = capture_schema.capture(database_path, self.upstreams)

        self.assertEqual(first, second)
        document = json.loads(first)
        self.assertEqual(
            list(document),
            [
                "vaultwarden_version",
                "database_migration",
                "fingerprint",
                "objects",
            ],
        )
        self.assertEqual(document["database_migration"], self.migration)
        self.assertEqual(document["vaultwarden_version"], "1.37.2")
        self.assertIn("wal_only", {item["name"] for item in document["objects"]})
        self.assertTrue(document["fingerprint"].startswith("sha256:"))
        self.assertTrue(first.endswith(b"\n"))

    def test_corruption_and_migration_mismatch_are_rejected(self) -> None:
        corrupt = self.root / "corrupt.sqlite3"
        corrupt.write_bytes(b"not a SQLite database")
        with self.assertRaises(sqlite3.Error):
            capture_schema.capture(corrupt, self.upstreams)

        mismatch = self.root / "mismatch.sqlite3"
        self.create_database(mismatch, migration="19990101000000").close()
        with self.assertRaisesRegex(ValueError, "migration"):
            capture_schema.capture(mismatch, self.upstreams)

    def test_source_inventory_and_output_symlinks_are_rejected(self) -> None:
        database_path = self.root / "database.sqlite3"
        self.create_database(database_path).close()
        database_link = self.root / "database-link.sqlite3"
        database_link.symlink_to(database_path)
        with self.assertRaisesRegex(ValueError, "non-symlink"):
            capture_schema.capture(database_link, self.upstreams)

        upstreams_link = self.root / "UPSTREAMS-link.toml"
        upstreams_link.symlink_to(self.upstreams)
        with self.assertRaisesRegex(ValueError, "non-symlink"):
            capture_schema.capture(database_path, upstreams_link)

        output = self.root / "schema.json"
        output_target = self.root / "target.json"
        output_target.write_text("preserve", encoding="utf-8")
        output.symlink_to(output_target)
        with self.assertRaisesRegex(ValueError, "non-symlink"):
            capture_schema.atomic_write(output, b"replacement")
        self.assertEqual(output_target.read_text(encoding="utf-8"), "preserve")

    def test_failed_capture_or_replace_leaves_no_partial_output(self) -> None:
        database_path = self.root / "database.sqlite3"
        self.create_database(database_path, migration="19990101000000").close()
        output = self.root / "schema.json"
        output.write_bytes(b"preserve")

        with self.assertRaises(ValueError):
            capture_schema.atomic_write(
                output,
                capture_schema.capture(database_path, self.upstreams),
            )
        self.assertEqual(output.read_bytes(), b"preserve")

        with (
            mock.patch.object(
                capture_schema.os, "replace", side_effect=OSError("forced failure")
            ),
            self.assertRaises(OSError),
        ):
            capture_schema.atomic_write(output, b"replacement")
        self.assertEqual(output.read_bytes(), b"preserve")
        self.assertEqual(
            list(self.root.glob(f".{output.name}.*.tmp")),
            [],
        )


if __name__ == "__main__":
    unittest.main()
