"""Contract tests for the B-only release gate and candidate evidence."""

from __future__ import annotations

import datetime as dt
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import check_release

B_COMMIT = "b" * 40


class FakeRun:
    def __init__(
        self,
        *,
        dirty: bool = False,
        commit: str = B_COMMIT,
        fail_gate: str | None = None,
    ) -> None:
        self.dirty = dirty
        self.commit = commit
        self.fail_gate = fail_gate
        self.calls: list[tuple[list[str], bool]] = []

    def __call__(
        self,
        arguments: list[str],
        *,
        cwd: Path,
        check: bool,
        text: bool,
        capture_output: bool,
    ) -> subprocess.CompletedProcess[str]:
        if cwd != check_release.REPOSITORY or not check or not text:
            raise AssertionError("release check subprocess contract changed")
        self.calls.append((arguments, capture_output))
        if arguments[:1] == ["git"]:
            if arguments[1:3] == ["status", "--porcelain=v1"]:
                output = " M tracked\n" if self.dirty else ""
            elif arguments[1:] == ["rev-parse", "--verify", "HEAD"]:
                output = self.commit + "\n"
            elif arguments[1:3] == ["check-ignore", "--quiet"]:
                if capture_output:
                    raise AssertionError(
                        "ignore validation unexpectedly captured output"
                    )
                output = ""
            else:
                raise AssertionError(f"unexpected Git command {arguments}")
            return subprocess.CompletedProcess(arguments, 0, output, "")
        if capture_output or arguments[:1] == ["just"]:
            raise AssertionError(f"unexpected gate command {arguments}")
        for gate in check_release.GATES:
            if arguments == list(gate.command):
                if self.fail_gate == gate.name:
                    raise subprocess.CalledProcessError(1, arguments)
                return subprocess.CompletedProcess(arguments, 0, "", "")
        raise AssertionError(f"unknown gate command {arguments}")


class ReleaseCheckTest(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="check-release-test-")
        self.addCleanup(temporary.cleanup)
        self.repository = Path(temporary.name) / "repository"
        (self.repository / "tmp").mkdir(parents=True)
        self.evidence_root = self.repository / "tmp" / "release-evidence"
        self.repository_patch = mock.patch.object(
            check_release, "REPOSITORY", self.repository
        )
        self.repository_patch.start()
        self.addCleanup(self.repository_patch.stop)
        self.evidence_patch = mock.patch.object(
            check_release, "EVIDENCE_ROOT", self.evidence_root
        )
        self.evidence_patch.start()
        self.addCleanup(self.evidence_patch.stop)

    def test_cli_takes_no_release_pair_arguments(self) -> None:
        self.assertEqual(vars(check_release.parser().parse_args([])), {})
        with self.assertRaises(SystemExit):
            check_release.parser().parse_args([B_COMMIT])

    def test_runs_exact_direct_gates_and_writes_canonical_evidence(self) -> None:
        fake = FakeRun()
        instants = iter(
            (
                dt.datetime(2026, 8, 30, 10, 0, 0, tzinfo=dt.UTC),
                dt.datetime(2026, 8, 30, 10, 30, 0, tzinfo=dt.UTC),
            )
        )
        result = check_release.check_release(fake, lambda: next(instants))
        expected = {
            "commit": B_COMMIT,
            "started_at": "2026-08-30T10:00:00.000Z",
            "finished_at": "2026-08-30T10:30:00.000Z",
            "gates": list(check_release.EVIDENCE_GATES),
            "status": "passed",
        }
        self.assertEqual(result, expected)
        gates = [
            arguments for arguments, _capture in fake.calls if arguments[0] != "git"
        ]
        self.assertEqual(gates, [list(gate.command) for gate in check_release.GATES])
        self.assertTrue(all(command[0] != "just" for command in gates))
        candidate = self.evidence_root / B_COMMIT / "candidate.json"
        self.assertEqual(candidate.read_bytes(), check_release.canonical_json(expected))
        self.assertEqual(stat.S_IMODE(candidate.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(candidate.parent.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(self.evidence_root.stat().st_mode), 0o700)
        self.assertEqual(json.loads(candidate.read_bytes()), expected)

    def test_gate_failure_stops_without_evidence(self) -> None:
        fake = FakeRun(fail_gate="moto-restic")
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release(fake)
        self.assertFalse(self.evidence_root.exists())
        gate_names = [
            gate.name
            for gate in check_release.GATES
            if any(arguments == list(gate.command) for arguments, _ in fake.calls)
        ]
        self.assertEqual(
            gate_names,
            [
                "nix-flake-check",
                "go-race",
                "govulncheck",
                "products",
                "targetless-deployment",
                "moto-restic",
            ],
        )

    def test_dirty_and_invalid_head_reject_before_gates(self) -> None:
        for fake in (FakeRun(dirty=True), FakeRun(commit="not-a-commit")):
            with self.subTest(fake=fake):
                with self.assertRaises(check_release.ReleaseCheckError):
                    check_release.check_release(fake)
                self.assertFalse(any(call[0][0] != "git" for call in fake.calls))

    def test_gate_and_evidence_names_are_the_frozen_contract(self) -> None:
        self.assertEqual(
            check_release.EVIDENCE_GATES,
            (
                "nix-flake-check",
                "go-race",
                "govulncheck",
                "products",
                "targetless-deployment",
                "moto-restic",
                "backup-restore-drill",
                "multitarget-threshold",
                "dual-repository",
                "initial-import",
            ),
        )


if __name__ == "__main__":
    unittest.main()
