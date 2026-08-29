"""Fault-matrix tests for the resumable deployment host orchestrator."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import deployment

A_COMMIT = "a" * 40
B_COMMIT = "b" * 40
A_RELEASE = "RELEASE.2026-08-28T12-00-00Z-gaaaaaaaaaaaa"
B_RELEASE = "RELEASE.2026-08-28T13-00-00Z-gbbbbbbbbbbbb"
UPGRADE_ID = f"{A_COMMIT}..{B_COMMIT}"
PROJECT = "vwless-deployment-test"


def completed(
    arguments: list[str], stdout: str = "", returncode: int = 0
) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(arguments, returncode, stdout=stdout, stderr="")


class Fixture:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.current = root / "state" / "upgrade" / "current"
        for directory in (
            root / "state",
            root / "state" / "deployment",
            root / "state" / "upgrade",
            self.current,
            root / "state" / "upgrade" / "history",
            root / "state" / "upgrade" / "quarantine",
            root / "restore",
            root / "restore" / "drill",
        ):
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            directory.chmod(0o700)
        self.write_file(
            root / ".env",
            "DEPLOYMENT_SCHEMA_VERSION=1\n"
            f"RUNTIME_UID={os.geteuid()}\n"
            f"RUNTIME_GID={os.getegid()}\n"
            "LISTEN_PORT=8080\n"
            "DRILL_PORT=8081\n",
        )
        self.write_json(
            self.current / "prepare.json",
            {
                "schema_version": 1,
                "upgrade_id": UPGRADE_ID,
                "phase": "prepared",
                "from_commit": A_COMMIT,
                "from_release": A_RELEASE,
                "to_commit": B_COMMIT,
                "to_release": B_RELEASE,
                "prepared_at": "2026-08-28T12:30:00.000Z",
            },
        )
        self.write_file(
            self.current / "compose.env",
            (root / ".env").read_text(encoding="utf-8")
            + f"A_RELEASE={A_RELEASE}\nB_RELEASE={B_RELEASE}\n",
        )
        self.write_active("accept", initial=True)
        self.write_file(root / "restore" / "drill" / "manifest.json", "synthetic\n")

    @staticmethod
    def write_file(path: Path, value: str) -> None:
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.parent.chmod(0o700)
        path.write_text(value, encoding="utf-8")
        path.chmod(0o600)

    def write_json(self, path: Path, value: dict[str, object]) -> None:
        self.write_file(path, json.dumps(value, separators=(",", ":")) + "\n")

    def write_active(self, decision: str, *, initial: bool = False) -> None:
        if initial or decision == "rollback":
            commit, release = A_COMMIT, A_RELEASE
        else:
            commit, release = B_COMMIT, B_RELEASE
        self.write_json(
            self.root / "state" / "deployment" / "active.json",
            {
                "schema_version": 1,
                "project_commit": commit,
                "release": release,
                "activated_at": "2026-08-28T14:00:00.000Z",
            },
        )

    def decide(self, decision: str) -> None:
        path = self.current / "decision.json"
        if path.exists():
            return
        commit, release = (
            (B_COMMIT, B_RELEASE) if decision == "accept" else (A_COMMIT, A_RELEASE)
        )
        self.write_json(
            path,
            {
                "schema_version": 1,
                "upgrade_id": UPGRADE_ID,
                "decision": decision,
                "active_project_commit": commit,
                "active_release": release,
                "decided_at": "2026-08-28T14:00:00.000Z",
            },
        )
        self.write_active(decision)

    def archive(self, decision: str) -> None:
        destination = (
            self.root
            / "state"
            / "upgrade"
            / ("history" if decision == "accept" else "quarantine")
            / UPGRADE_ID
        )
        self.current.rename(destination)


class ImportFixture:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.current = root / "state" / "import" / "current"
        self.candidate = root / "import" / "candidate"
        for directory in (
            root / "state",
            root / "state" / "import",
            root / "state" / "import" / "history",
            self.current,
            root / "import",
            self.candidate,
        ):
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            directory.chmod(0o700)
        self.write_file(
            root / ".env",
            "DEPLOYMENT_SCHEMA_VERSION=1\n"
            f"RUNTIME_UID={os.geteuid()}\n"
            f"RUNTIME_GID={os.getegid()}\n"
            "LISTEN_PORT=8080\n"
            "DRILL_PORT=8081\n",
        )
        self.write_file(self.candidate / "synthetic", "verified candidate\n")
        self.write_json(
            self.current / "prepare.json",
            {
                "schema_version": 1,
                "import_id": "initial",
                "phase": "prepared",
                "project_commit": B_COMMIT,
                "release": B_RELEASE,
                "source_path": "import/source",
                "candidate_path": "import/candidate",
                "data_path": "data",
                "filesystem_device": 1,
                "source_tree_id": "sha256:" + "1" * 64,
                "candidate_tree_id": "sha256:" + "2" * 64,
                "source_rsa_key_id": "sha256:" + "3" * 64,
                "prepared_at": "2026-08-28T12:30:00.000Z",
            },
        )
        self.write_json(
            self.current / "verification.json",
            {
                "schema_version": 1,
                "import_id": "initial",
                "phase": "verified",
                "project_commit": B_COMMIT,
                "release": B_RELEASE,
                "source_tree_id": "sha256:" + "1" * 64,
                "candidate_tree_id": "sha256:" + "4" * 64,
                "database_migration": "20260828000000",
                "database_schema_fingerprint": "sha256:" + "5" * 64,
                "content_id": "sha256:" + "6" * 64,
                "archive_id": "sha256:" + "7" * 64,
                "client_login_confirmed": True,
                "verified_at": "2026-08-28T13:30:00.000Z",
            },
        )

    @staticmethod
    def write_file(path: Path, value: str) -> None:
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.parent.chmod(0o700)
        path.write_text(value, encoding="utf-8")
        path.chmod(0o600)

    def write_json(self, path: Path, value: dict[str, object]) -> None:
        self.write_file(path, json.dumps(value, separators=(",", ":")) + "\n")

    def write_decision(self) -> None:
        decision = self.current / "decision.json"
        if decision.exists():
            return
        self.write_json(
            decision,
            {
                "schema_version": 1,
                "import_id": "initial",
                "decision": "accept",
                "active_project_commit": B_COMMIT,
                "active_release": B_RELEASE,
                "decided_at": "2026-08-28T14:00:00.000Z",
            },
        )

    def complete_accept(self) -> None:
        self.write_decision()
        deployment_state = self.root / "state" / "deployment"
        deployment_state.mkdir(mode=0o700, exist_ok=True)
        deployment_state.chmod(0o700)
        self.write_json(
            deployment_state / "active.json",
            {
                "schema_version": 1,
                "project_commit": B_COMMIT,
                "release": B_RELEASE,
                "activated_at": "2026-08-28T14:00:00.000Z",
            },
        )
        shutil.rmtree(self.candidate)
        self.candidate.mkdir(mode=0o700)
        self.candidate.chmod(0o700)

    def archive(self) -> None:
        if self.candidate.exists():
            self.candidate.rmdir()
        self.current.rename(self.root / "state" / "import" / "history" / "initial")


class FakeDocker:
    def __init__(self, fixture: Fixture, decision: str) -> None:
        self.fixture = fixture
        self.decision = decision
        self.calls: list[list[str]] = []
        self.volume_present = True
        self.fail_suffix: tuple[str, ...] | None = None
        self.failed = False

    def __call__(
        self,
        arguments: list[str],
        *,
        cwd: Path,
        check: bool,
        text: bool,
        capture_output: bool,
    ) -> subprocess.CompletedProcess[str]:
        self.calls.append(arguments)
        if cwd != self.fixture.root or check or not text:
            raise AssertionError("orchestrator subprocess contract changed")
        if not capture_output:
            raise AssertionError("orchestrator subprocess output must stay bounded")
        suffix = tuple(arguments[-len(self.fail_suffix) :]) if self.fail_suffix else ()
        if self.fail_suffix and suffix == self.fail_suffix and not self.failed:
            self.failed = True
            return completed(arguments, returncode=1)

        if arguments[-3:] == ["upgrade-b", "upgrade", "accept"]:
            self.fixture.decide("accept")
        elif arguments[-3:] == ["rollback-a", "rollback", "apply"]:
            self.fixture.decide("rollback")
        elif arguments[-3:] in (
            ["upgrade-b", "upgrade", "cleanup-drill"],
            ["rollback-a", "upgrade", "cleanup-drill"],
        ):
            drill = self.fixture.root / "restore" / "drill"
            if drill.exists():
                shutil.rmtree(drill)
        elif arguments[-3:] in (
            ["upgrade-b", "upgrade", "finalize"],
            ["rollback-a", "upgrade", "finalize"],
        ):
            self.fixture.archive(self.decision)

        if arguments[:3] == ["docker", "volume", "ls"]:
            value = f"{PROJECT}_drill-data\n" if self.volume_present else ""
            return completed(arguments, value)
        if arguments[:3] == ["docker", "volume", "inspect"]:
            return completed(
                arguments,
                json.dumps(
                    {
                        "Name": f"{PROJECT}_drill-data",
                        "Labels": {
                            "com.docker.compose.project": PROJECT,
                            "com.docker.compose.volume": "drill-data",
                        },
                    }
                ),
            )
        if arguments[:3] == ["docker", "volume", "rm"]:
            self.volume_present = False
            return completed(arguments)
        if arguments[:3] == ["docker", "image", "inspect"]:
            reference = arguments[-1]
            product = next(
                product
                for product in deployment.PRODUCTS
                if f"-{product}:" in reference
            )
            digit = str(deployment.PRODUCTS.index(product) + 1)
            identity = (
                deployment.Identity(B_COMMIT, B_RELEASE)
                if self.decision == "accept"
                else deployment.Identity(A_COMMIT, A_RELEASE)
            )
            return completed(
                arguments,
                json.dumps(
                    {
                        "Id": "sha256:" + digit * 64,
                        "Config": {
                            "Labels": {
                                "org.opencontainers.image.revision": identity.commit,
                                "org.opencontainers.image.version": identity.release,
                            }
                        },
                    }
                ),
            )
        if arguments[:3] == ["docker", "container", "inspect"]:
            identifier = arguments[-1]
            product_index = int(identifier[0]) - 1
            product = deployment.PRODUCTS[product_index]
            digit = str(product_index + 1)
            tag = B_RELEASE if self.decision == "accept" else "local"
            return completed(
                arguments,
                json.dumps(
                    {
                        "Image": "sha256:" + digit * 64,
                        "Config": {"Image": f"vaultwarden-less-{product}:{tag}"},
                        "State": {"Running": True, "Health": {"Status": "healthy"}},
                    }
                ),
            )
        if "ps" in arguments and "--quiet" in arguments:
            service = arguments[-1]
            if service in deployment.SERVICES:
                digit = str(deployment.SERVICES.index(service) + 1)
                return completed(arguments, digit * 64 + "\n")
            return completed(arguments)
        return completed(arguments)


class FakeImportDocker:
    def __init__(self, fixture: ImportFixture) -> None:
        self.fixture = fixture
        self.calls: list[list[str]] = []
        self.fail_suffix: tuple[str, ...] | None = None
        self.failed = False

    def __call__(
        self,
        arguments: list[str],
        *,
        cwd: Path,
        check: bool,
        text: bool,
        capture_output: bool,
    ) -> subprocess.CompletedProcess[str]:
        self.calls.append(arguments)
        if cwd != self.fixture.root or check or not text or not capture_output:
            raise AssertionError("import orchestrator subprocess contract changed")
        suffix = tuple(arguments[-len(self.fail_suffix) :]) if self.fail_suffix else ()
        if self.fail_suffix and suffix == self.fail_suffix and not self.failed:
            self.failed = True
            return completed(arguments, returncode=1)

        if arguments[-3:] == ["deployment", "import", "accept"]:
            self.fixture.complete_accept()
        elif arguments[-3:] == ["deployment", "import", "finalize"]:
            self.fixture.archive()

        if arguments[:3] == ["docker", "image", "inspect"]:
            reference = arguments[-1]
            product = next(
                product
                for product in deployment.PRODUCTS
                if f"-{product}:" in reference
            )
            digit = str(deployment.PRODUCTS.index(product) + 1)
            return completed(
                arguments,
                json.dumps(
                    {
                        "Id": "sha256:" + digit * 64,
                        "Config": {
                            "Labels": {
                                "org.opencontainers.image.revision": B_COMMIT,
                                "org.opencontainers.image.version": B_RELEASE,
                            }
                        },
                    }
                ),
            )
        if arguments[:3] == ["docker", "container", "inspect"]:
            identifier = arguments[-1]
            product_index = int(identifier[0]) - 1
            product = deployment.PRODUCTS[product_index]
            digit = str(product_index + 1)
            return completed(
                arguments,
                json.dumps(
                    {
                        "Image": "sha256:" + digit * 64,
                        "Config": {"Image": f"vaultwarden-less-{product}:local"},
                        "State": {
                            "Running": True,
                            "Health": {"Status": "healthy"},
                        },
                    }
                ),
            )
        if "ps" in arguments and "--quiet" in arguments:
            service = arguments[-1]
            if service in deployment.SERVICES:
                digit = str(deployment.SERVICES.index(service) + 1)
                return completed(arguments, digit * 64 + "\n")
            return completed(arguments)
        return completed(arguments)


class DeploymentOrchestratorTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.fixture = Fixture(self.root)

    def orchestrator(self, fake: FakeDocker) -> deployment.Orchestrator:
        return deployment.Orchestrator(self.root, PROJECT, fake)

    def test_state_operations_use_networkless_admin_with_exact_argv(self) -> None:
        calls: list[list[str]] = []

        def run(
            arguments: list[str],
            *,
            cwd: Path,
            check: bool,
            text: bool,
            capture_output: bool,
        ) -> subprocess.CompletedProcess[str]:
            calls.append(arguments)
            self.assertEqual(cwd, self.root)
            self.assertFalse(check)
            self.assertTrue(text)
            self.assertTrue(capture_output)
            return completed(
                arguments,
                '{"schema_version":1,"status":"verified"}\n',
            )

        orchestrator = deployment.Orchestrator(self.root, PROJECT, run)
        result = orchestrator.state_operation(
            ["import", "verify", "--confirm-client-login"]
        )
        self.assertEqual(result, {"schema_version": 1, "status": "verified"})
        self.assertEqual(len(calls), 1)
        self.assertEqual(
            calls[0][-10:],
            [
                "--profile",
                "deployment",
                "run",
                "--rm",
                "--no-deps",
                "deployment-admin",
                "deployment",
                "import",
                "verify",
                "--confirm-client-login",
            ],
        )
        with self.assertRaises(deployment.UsageError):
            orchestrator.state_operation([])

    def test_accept_uses_exact_argv_and_is_already_complete_after_finalize(
        self,
    ) -> None:
        fake = FakeDocker(self.fixture, "accept")
        result = self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(result["status"], "completed")
        self.assertEqual(result["active_project_commit"], B_COMMIT)
        self.assertFalse(self.fixture.current.exists())
        self.assertTrue(
            (self.root / "state" / "upgrade" / "history" / UPGRADE_ID).is_dir()
        )
        self.assertIn(
            [
                "upgrade-b",
                "upgrade",
                "accept",
            ],
            [call[-3:] for call in fake.calls],
        )
        for call in fake.calls:
            self.assertIsInstance(call, list)
            self.assertNotIn("sed", call)
            self.assertNotIn("export", call)
            self.assertNotIn("prune", call)

        repeated = self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(repeated["status"], "already_complete")

    def test_accept_resumes_across_service_cleanup_and_finalize_failures(self) -> None:
        fake = FakeDocker(self.fixture, "accept")
        fake.fail_suffix = (
            "up",
            "--detach",
            "--wait",
            "--wait-timeout",
            "180",
            "vaultwarden",
            "backup",
            "proxy",
        )
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_upgrade("accept")
        decision_before = (self.fixture.current / "decision.json").read_bytes()
        self.assertTrue((self.root / "restore" / "drill").is_dir())

        fake.fail_suffix = ("--profile", "drill", "stop", "drill-vaultwarden")
        fake.failed = False
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(
            (self.fixture.current / "decision.json").read_bytes(), decision_before
        )
        self.assertTrue((self.root / "restore" / "drill").is_dir())

        fake.fail_suffix = ("upgrade-b", "upgrade", "finalize")
        fake.failed = False
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(
            (self.fixture.current / "decision.json").read_bytes(), decision_before
        )
        self.assertFalse((self.root / "restore" / "drill").exists())
        self.assertFalse(fake.volume_present)

        fake.fail_suffix = None
        result = self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(result["status"], "completed")

    def test_failure_before_decision_and_opposite_decision_have_no_later_effects(
        self,
    ) -> None:
        fake = FakeDocker(self.fixture, "accept")
        fake.fail_suffix = ("upgrade-b", "upgrade", "accept")
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_upgrade("accept")
        self.assertFalse((self.fixture.current / "decision.json").exists())
        self.assertTrue((self.root / "restore" / "drill").is_dir())

        self.fixture.decide("rollback")
        call_count = len(fake.calls)
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(len(fake.calls), call_count)

    def test_rollback_retags_only_three_images_and_resumes_completed_state(
        self,
    ) -> None:
        fake = FakeDocker(self.fixture, "rollback")
        result = self.orchestrator(fake).terminal_upgrade("rollback")
        self.assertEqual(result["status"], "completed")
        self.assertEqual(result["active_project_commit"], A_COMMIT)
        tags = [call for call in fake.calls if call[:3] == ["docker", "image", "tag"]]
        self.assertEqual(len(tags), 3)
        self.assertEqual(
            {call[-1] for call in tags},
            {f"vaultwarden-less-{product}:local" for product in deployment.PRODUCTS},
        )
        self.assertTrue(
            (self.root / "state" / "upgrade" / "quarantine" / UPGRADE_ID).is_dir()
        )
        repeated = self.orchestrator(fake).terminal_upgrade("rollback")
        self.assertEqual(repeated["status"], "already_complete")

    def test_tampered_state_is_rejected_before_docker(self) -> None:
        self.fixture.write_json(
            self.fixture.current / "decision.json",
            {
                "schema_version": 1,
                "upgrade_id": UPGRADE_ID,
                "decision": "accept",
                "active_project_commit": B_COMMIT,
                "active_release": B_RELEASE,
                "decided_at": "2026-08-28T14:00:00.000Z",
                "unexpected": True,
            },
        )
        fake = FakeDocker(self.fixture, "accept")
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(fake.calls, [])

        (self.fixture.current / "decision.json").unlink()
        self.fixture.write_file(
            self.root / ".env",
            "DEPLOYMENT_SCHEMA_VERSION=1\n"
            f"RUNTIME_UID=0{os.geteuid()}\n"
            f"RUNTIME_GID={os.getegid()}\n"
            "LISTEN_PORT=8080\n"
            "DRILL_PORT=8081\n",
        )
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_upgrade("accept")
        self.assertEqual(fake.calls, [])


class ImportOrchestratorTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.fixture = ImportFixture(self.root)

    def orchestrator(self, fake: FakeImportDocker) -> deployment.Orchestrator:
        return deployment.Orchestrator(self.root, PROJECT, fake)

    def test_accept_stops_candidate_starts_exact_runtime_and_is_repeatable(
        self,
    ) -> None:
        fake = FakeImportDocker(self.fixture)
        result = self.orchestrator(fake).terminal_import()
        self.assertEqual(result["status"], "completed")
        self.assertEqual(result["workflow"], "import")
        self.assertEqual(result["active_project_commit"], B_COMMIT)
        self.assertFalse(self.fixture.current.exists())
        self.assertTrue(
            (self.root / "state" / "import" / "history" / "initial").is_dir()
        )
        self.assertFalse(self.fixture.candidate.exists())
        suffixes = [call[-3:] for call in fake.calls]
        self.assertIn(["deployment", "import", "accept"], suffixes)
        self.assertIn(["deployment", "import", "finalize"], suffixes)
        self.assertTrue(
            any(
                call[-8:]
                == [
                    "up",
                    "--detach",
                    "--wait",
                    "--wait-timeout",
                    "180",
                    "vaultwarden",
                    "backup",
                    "proxy",
                ]
                for call in fake.calls
            )
        )
        for call in fake.calls:
            self.assertIsInstance(call, list)
            self.assertNotIn("sed", call)
            self.assertNotIn("export", call)
            self.assertNotIn("prune", call)
            self.assertNotIn("/var/run/docker.sock", call)

        repeated = self.orchestrator(fake).terminal_import()
        self.assertEqual(repeated["status"], "already_complete")

    def test_accept_replays_a_preexisting_decision_before_exchange(self) -> None:
        self.fixture.write_decision()
        self.assertFalse((self.root / "state" / "deployment").exists())
        self.assertTrue(any(self.fixture.candidate.iterdir()))
        fake = FakeImportDocker(self.fixture)

        result = self.orchestrator(fake).terminal_import()

        self.assertEqual(result["status"], "completed")
        accept_calls = [
            call
            for call in fake.calls
            if call[-3:] == ["deployment", "import", "accept"]
        ]
        self.assertEqual(len(accept_calls), 1)
        self.assertFalse(self.fixture.candidate.exists())

    def test_accept_resumes_after_decision_service_and_finalize_failures(self) -> None:
        fake = FakeImportDocker(self.fixture)
        fake.fail_suffix = ("deployment", "import", "accept")
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_import()
        self.assertFalse((self.fixture.current / "decision.json").exists())

        fake.fail_suffix = (
            "up",
            "--detach",
            "--wait",
            "--wait-timeout",
            "180",
            "vaultwarden",
            "backup",
            "proxy",
        )
        fake.failed = False
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_import()
        decision_before = (self.fixture.current / "decision.json").read_bytes()
        self.assertTrue(self.fixture.candidate.is_dir())
        self.assertEqual(list(self.fixture.candidate.iterdir()), [])

        fake.fail_suffix = ("deployment", "import", "finalize")
        fake.failed = False
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_import()
        self.assertEqual(
            (self.fixture.current / "decision.json").read_bytes(), decision_before
        )

        fake.fail_suffix = None
        result = self.orchestrator(fake).terminal_import()
        self.assertEqual(result["status"], "completed")
        accept_calls = [
            call
            for call in fake.calls
            if call[-3:] == ["deployment", "import", "accept"]
        ]
        self.assertEqual(len(accept_calls), 4)

    def test_tampered_import_state_is_rejected_before_docker(self) -> None:
        document = json.loads(
            (self.fixture.current / "verification.json").read_text(encoding="utf-8")
        )
        document["unexpected"] = True
        self.fixture.write_json(self.fixture.current / "verification.json", document)
        fake = FakeImportDocker(self.fixture)
        with self.assertRaises(deployment.DeploymentError):
            self.orchestrator(fake).terminal_import()
        self.assertEqual(fake.calls, [])


if __name__ == "__main__":
    unittest.main()
