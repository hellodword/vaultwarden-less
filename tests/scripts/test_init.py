"""Regression tests for the private deployment initializer."""

from __future__ import annotations

import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPOSITORY = Path(__file__).resolve().parents[2]


class InitScriptTest(unittest.TestCase):
    def run_init(
        self, root: Path, *, success: bool = True
    ) -> subprocess.CompletedProcess[str]:
        completed = subprocess.run(
            [
                sys.executable,
                "scripts/init.py",
                "--root",
                os.fspath(root),
                "--version-file",
                "VERSION",
                "--upstreams",
                "UPSTREAMS.toml",
                "--vaultwarden-template",
                "deploy/vaultwarden.env.template",
            ],
            cwd=REPOSITORY,
            check=False,
            capture_output=True,
            text=True,
        )
        if success and completed.returncode != 0:
            self.fail(f"init failed: {completed.stderr}")
        if not success and completed.returncode == 0:
            self.fail("init unexpectedly succeeded")
        return completed

    def test_creates_v2_layout_once_with_private_modes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "deployment"
            root.mkdir()
            self.run_init(root)

            for relative in (
                "data",
                "staging",
                "cache",
                "restore",
                "config/backup",
                "config/vaultwarden",
                "credentials/backup",
                "credentials/maintenance",
                "credentials/restic",
                "credentials/notifiers",
            ):
                path = root / relative
                self.assertTrue(path.is_dir())
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700)

            private_files = (
                ".env",
                "config/backup/backup.yaml",
                "config/vaultwarden/vaultwarden.env",
                "credentials/backup/credentials.yaml",
                "credentials/maintenance/credentials.yaml",
                "credentials/restic/primary",
                "credentials/notifiers/credentials.yaml",
            )
            for relative in private_files:
                self.assertEqual(stat.S_IMODE((root / relative).stat().st_mode), 0o600)

            environment = (root / ".env").read_text(encoding="utf-8")
            version = (REPOSITORY / "VERSION").read_text(encoding="utf-8").strip()
            self.assertIn(f"PRODUCT_VERSION={version}\n", environment)
            self.assertIn(
                "VAULTWARDEN_IMAGE=vaultwarden-less-vaultwarden:", environment
            )
            self.assertIn("VAULTWARDEN_ENV_FILE=", environment)
            self.assertIn(
                "schema_version: 2",
                (root / "config/backup/backup.yaml").read_text(encoding="utf-8"),
            )

            password = (root / "credentials/restic/primary").read_text(encoding="utf-8")
            self.run_init(root)
            self.assertEqual(
                (root / "credentials/restic/primary").read_text(encoding="utf-8"),
                password,
            )

    def test_never_follows_or_replaces_an_existing_path(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "deployment"
            outside = Path(temporary) / "outside"
            root.mkdir()
            outside.mkdir()
            (root / "data").symlink_to(outside, target_is_directory=True)

            completed = self.run_init(root, success=False)
            self.assertIn("must be a real directory", completed.stderr)
            self.assertEqual(list(outside.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
