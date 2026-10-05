"""Execute the documented filesystem guards against disposable trees."""

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def example(name: str) -> str:
    document = (ROOT / "docs/operations.md").read_text()
    return document.split(f"<!-- {name} -->\n```sh\n", 1)[1].split("\n```", 1)[0]


class OperatorExamples(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name).resolve()
        self.deployment = self.root / "deployment"
        (self.deployment / "data").mkdir(parents=True)
        (self.deployment / "data/keep").write_text("original")
        (self.deployment / "data/link").symlink_to("keep")
        binary = self.root / "bin"
        binary.mkdir()
        docker = binary / "docker"
        docker.write_text(
            f"#!{shutil.which('bash')}\n"
            'printf "%s\\n" "$*" >> "$CALLS"\nprintf "ok\\n"\n'
        )
        docker.chmod(0o700)
        self.calls = self.root / "calls"
        self.environment = os.environ | {
            "PATH": str(binary) + os.pathsep + os.environ["PATH"],
            "DEPLOYMENT_ROOT": str(self.deployment),
            "RESTIC_IMAGE": "synthetic-restic",
            "RESTIC_ENV_FILE": str(
                self.deployment / "credentials/maintenance/aws-s3-maintenance.env"
            ),
            "SNAPSHOT_ID": "a" * 64,
            "CALLS": str(self.calls),
        }

    def run_example(self, name: str, **values: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            ["bash", "-c", example(name)],
            env=self.environment | values,
            capture_output=True,
            text=True,
            check=False,
        )

    def test_restore_accepts_new_path_then_refuses_to_reuse_it(self) -> None:
        for provider in ("aws-s3", "cloudflare-r2", "backblaze-b2"):
            with self.subTest(provider=provider):
                target = self.root / f"restore-{provider}"
                env_file = str(
                    self.deployment
                    / f"credentials/maintenance/{provider}-maintenance.env"
                )
                result = self.run_example(
                    "native-restore-example",
                    RESTORE_DIR=str(target),
                    RESTIC_ENV_FILE=env_file,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = self.calls.read_text()
                self.assertIn(f"--env-file {env_file}", calls.splitlines()[-2])
                self.assertIn(
                    "restore " + "a" * 64 + " --verify --target /restore", calls
                )
                self.assertIn("PRAGMA integrity_check;", calls)
                (target / "evidence").write_text("retained")
                result = self.run_example(
                    "native-restore-example",
                    RESTORE_DIR=str(target),
                    RESTIC_ENV_FILE=env_file,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls.read_text(), calls)
                self.assertEqual((target / "evidence").read_text(), "retained")

    def test_restore_refuses_existing_files_and_dangling_links(self) -> None:
        for kind in ("file", "directory", "link"):
            with self.subTest(kind=kind):
                target = self.root / kind
                if kind == "file":
                    target.write_text("keep")
                elif kind == "directory":
                    target.mkdir()
                else:
                    target.symlink_to(self.root / "missing")
                result = self.run_example(
                    "native-restore-example", RESTORE_DIR=str(target)
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.calls.exists())
                self.assertTrue(target.exists() or target.is_symlink())

    def test_upgrade_preserves_source_and_refuses_existing_or_nested_target(
        self,
    ) -> None:
        target = self.root / "upgrade"
        result = self.run_example("upgrade-copy-example", UPGRADE_DATA=str(target))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((target / "keep").read_text(), "original")
        self.assertTrue((target / "link").is_symlink())
        (target / "keep").write_text("migrated copy")
        for path in (target, self.deployment / "data/child"):
            result = self.run_example("upgrade-copy-example", UPGRADE_DATA=str(path))
            self.assertNotEqual(result.returncode, 0)
        self.assertEqual((target / "keep").read_text(), "migrated copy")
        self.assertEqual((self.deployment / "data/keep").read_text(), "original")
        self.assertFalse((self.deployment / "data/child").exists())


if __name__ == "__main__":
    unittest.main()
