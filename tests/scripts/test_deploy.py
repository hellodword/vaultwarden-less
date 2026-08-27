"""Contract tests for deployment configuration, rendering, and orchestration."""

from __future__ import annotations

import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import yaml

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import deploy


class DeploymentScriptTest(unittest.TestCase):
    def deployment(self, parent: Path) -> Path:
        root = parent / "deployment"
        root.mkdir()
        deploy.initialize(root, os.geteuid(), os.getegid())
        return root

    def write_deployment_config(self, root: Path, content: str) -> None:
        path = root / "config" / "deployment.yaml"
        path.write_text(content, encoding="utf-8", newline="\n")
        path.chmod(0o600)

    def test_init_creates_private_layout_once(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.deployment(Path(temporary))
            expected_directories = (
                "data",
                "staging",
                "cache",
                "restore",
                "config",
                "config/backup",
                "config/vaultwarden",
                "credentials",
                "credentials/backup",
                "credentials/maintenance",
                "credentials/restic",
                "credentials/notifiers",
            )
            for relative in expected_directories:
                path = root / relative
                self.assertTrue(path.is_dir())
                self.assertFalse(path.is_symlink())
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700)
                self.assertEqual(path.stat().st_uid, os.geteuid())
                self.assertEqual(path.stat().st_gid, os.getegid())

            private_files = (
                "config/deployment.yaml",
                "config/backup/backup.yaml",
                "config/vaultwarden/vaultwarden.env",
                "credentials/backup/credentials.yaml",
                "credentials/maintenance/credentials.yaml",
                "credentials/restic/primary",
                "credentials/notifiers/credentials.yaml",
            )
            for relative in private_files:
                path = root / relative
                self.assertTrue(path.is_file())
                self.assertFalse(path.is_symlink())
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
                self.assertEqual(path.stat().st_uid, os.geteuid())
                self.assertEqual(path.stat().st_gid, os.getegid())

            self.assertFalse((root / ".env").exists())
            self.assertEqual(
                (root / "config" / "deployment.yaml").read_text(encoding="utf-8"),
                deploy.deployment_yaml(os.geteuid(), os.getegid()),
            )
            password = (root / "credentials" / "restic" / "primary").read_bytes()
            deploy.initialize(root, os.geteuid(), os.getegid())
            self.assertEqual(
                (root / "credentials" / "restic" / "primary").read_bytes(),
                password,
            )

    def test_init_rejects_identity_change_and_symlinks(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            root = self.deployment(temporary_path)
            with self.assertRaisesRegex(ValueError, "do not match"):
                deploy.initialize(root, os.geteuid() + 1, os.getegid())

            second_root = temporary_path / "second"
            outside = temporary_path / "outside"
            second_root.mkdir()
            outside.mkdir()
            (second_root / "data").symlink_to(outside, target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "must be a real directory"):
                deploy.initialize(second_root, os.geteuid(), os.getegid())
            self.assertEqual(list(outside.iterdir()), [])

    def test_deployment_yaml_is_strict(self) -> None:
        valid = deploy.deployment_yaml(os.geteuid(), os.getegid())
        invalid_documents = {
            "duplicate YAML key": valid + "listen_port: 9000\n",
            "missing drill_port": valid.replace("drill_port: 8081\n", ""),
            "unknown extra": valid + "extra: 1\n",
            "one YAML mapping": "- schema_version\n- 1\n",
            "expected a single document": valid + "---\n" + valid,
            "schema_version must be an integer": valid.replace(
                "schema_version: 1", "schema_version: true"
            ),
            "runtime_uid must be an integer": valid.replace(
                f"runtime_uid: {os.geteuid()}", "runtime_uid: 4294967295"
            ),
            "listen_port must be an integer": valid.replace(
                "listen_port: 8080", "listen_port: 65536"
            ),
            "must differ": valid.replace("drill_port: 8081", "drill_port: 8080"),
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = self.deployment(Path(temporary))
            for expected, document in invalid_documents.items():
                with self.subTest(expected=expected):
                    self.write_deployment_config(root, document)
                    with self.assertRaisesRegex(Exception, expected):
                        deploy.load_deployment_config(root)

    def test_deployment_numeric_boundaries_are_explicit(self) -> None:
        valid = {
            "schema_version": 1,
            "runtime_uid": 0,
            "runtime_gid": deploy.MAX_ID,
            "listen_port": 0,
            "drill_port": 0,
        }
        self.assertEqual(deploy.validate_config(valid).runtime_uid, 0)
        self.assertEqual(deploy.validate_config(valid).runtime_gid, deploy.MAX_ID)
        invalid = {
            "negative UID": ("runtime_uid", -1),
            "large GID": ("runtime_gid", deploy.MAX_ID + 1),
            "boolean GID": ("runtime_gid", True),
            "negative listen port": ("listen_port", -1),
            "large drill port": ("drill_port", 65536),
        }
        for label, (field, value) in invalid.items():
            document = dict(valid)
            document[field] = value
            with self.subTest(label=label), self.assertRaises(ValueError):
                deploy.validate_config(document)

    def test_config_rejects_wrong_mode_and_non_regular_file(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.deployment(Path(temporary))
            path = root / "config" / "deployment.yaml"
            path.chmod(0o644)
            with self.assertRaisesRegex(ValueError, "mode 0600"):
                deploy.load_deployment_config(root)
            path.unlink()
            path.mkdir()
            with self.assertRaisesRegex(ValueError, "regular non-symlink"):
                deploy.load_deployment_config(root)

    def test_render_resolves_only_the_declared_tokens(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.deployment(Path(temporary))
            rendered = deploy.render_compose(root, "audit-2026.08")
            self.assertNotIn("@@", rendered)
            self.assertNotIn("${", rendered)
            self.assertIn(f'user: "{os.geteuid()}:{os.getegid()}"', rendered)
            self.assertIn("vaultwarden-less-backup:", rendered)
            self.assertIn("vaultwarden-less-proxy:", rendered)
            self.assertIn("vaultwarden-less-vaultwarden:", rendered)
            self.assertIn('source_path="/source/$$1"', rendered)
            self.assertIn('- "audit-2026.08"', rendered)
            document = yaml.safe_load(rendered)
            drill_copy = document["services"]["drill-copy"]
            self.assertEqual(drill_copy["user"], "0:0")
            self.assertEqual(drill_copy["network_mode"], "none")
            self.assertEqual(
                set(drill_copy["cap_add"]), {"CHOWN", "DAC_OVERRIDE", "FOWNER"}
            )
            for service in (
                "vaultwarden",
                "proxy",
                "backup",
                "maintenance",
                "restore",
                "drill-vaultwarden",
            ):
                self.assertEqual(
                    document["services"][service]["user"],
                    f"{os.geteuid()}:{os.getegid()}",
                )

    def test_render_rejects_missing_unknown_and_environment_tokens(self) -> None:
        template = deploy.read_compose_template()
        variants = {
            "missing required token": template.replace("@@PORT_PROXY@@", "8080"),
            "unresolved tokens": template + "\n# @@UNKNOWN@@\n",
            "environment interpolation": template + "\n# ${FORBIDDEN}\n",
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = self.deployment(Path(temporary))
            for expected, content in variants.items():
                with (
                    self.subTest(expected=expected),
                    mock.patch.object(
                        deploy, "read_compose_template", return_value=content
                    ),
                    self.assertRaisesRegex(ValueError, expected),
                ):
                    deploy.render_compose(root)

    def test_restore_and_drill_names_are_bounded(self) -> None:
        for valid in ("a", "prepared", "audit-2026.08_x", "a" * 64):
            self.assertEqual(deploy.require_deployment_name(valid, "name"), valid)
        for invalid in ("", ".hidden", "../escape", "with/slash", "a" * 65, "空"):
            with (
                self.subTest(invalid=invalid),
                self.assertRaisesRegex(ValueError, "1-64 ASCII"),
            ):
                deploy.require_deployment_name(invalid, "name")

    def test_docker_environment_inherits_the_caller(self) -> None:
        supplied = {
            "PATH": "/nix/tools",
            "TMPDIR": "/safe-tmp",
            "CALLER_SETTING": "preserved",
        }
        with mock.patch.dict(os.environ, supplied, clear=True):
            environment = deploy.docker_environment(Path("/isolated-docker-config"))
        self.assertEqual(
            environment,
            {
                "PATH": "/nix/tools",
                "TMPDIR": "/safe-tmp",
                "DOCKER_CONFIG": "/isolated-docker-config",
                "CALLER_SETTING": "preserved",
            },
        )

    def test_compose_uses_stdin_and_explicit_project_boundary(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.deployment(Path(temporary))
            with mock.patch.object(subprocess, "run") as run:
                deploy.run_compose(root, ["config", "--quiet"])
            run.assert_called_once()
            arguments = run.call_args.args[0]
            keywords = run.call_args.kwargs
            self.assertEqual(arguments[0], "docker")
            self.assertIn("--config", arguments)
            self.assertIn("--env-file", arguments)
            self.assertEqual(arguments[arguments.index("--env-file") + 1], "/dev/null")
            self.assertEqual(
                arguments[arguments.index("--project-directory") + 1], os.fspath(root)
            )
            self.assertEqual(
                arguments[arguments.index("--project-name") + 1],
                deploy.PRODUCTION_PROJECT,
            )
            self.assertEqual(arguments[arguments.index("--file") + 1], "-")
            self.assertEqual(keywords["env"]["HOME"], os.environ["HOME"])
            self.assertIn("DOCKER_CONFIG", keywords["env"])
            self.assertNotIn("${", keywords["input"])
            self.assertTrue(keywords["check"])
            self.assertTrue(keywords["text"])

    def test_restore_maps_a_name_to_one_container_directory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with mock.patch.object(deploy, "run_compose") as compose:
                deploy.run_restore(root, "secondary", "audit-1", "snapshot-id")
            arguments = compose.call_args.args[1]
            self.assertIn("/restore/audit-1", arguments)
            self.assertIn("snapshot-id", arguments)
            with self.assertRaisesRegex(ValueError, "1-64 ASCII"):
                deploy.run_restore(root, "primary", "../escape", None)

    def test_drill_has_a_separate_project_and_disposable_volume(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with mock.patch.object(deploy, "run_compose") as compose:
                deploy.run_drill(root, "up", "prepared")
            self.assertEqual(compose.call_count, 2)
            down = compose.call_args_list[0]
            up = compose.call_args_list[1]
            self.assertEqual(down.kwargs["project"], deploy.DRILL_PROJECT)
            self.assertIn("--volumes", down.args[1])
            self.assertEqual(up.kwargs["project"], deploy.DRILL_PROJECT)
            self.assertIn("drill-vaultwarden", up.args[1])


if __name__ == "__main__":
    unittest.main()
