"""Contract tests for direct release gates and private candidate evidence."""

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

A_COMMIT = "a" * 40
B_COMMIT = "b" * 40


class FakeRun:
    def __init__(self, *, dirty: bool = False, fail_gate: str | None = None) -> None:
        self.dirty = dirty
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
                output = B_COMMIT + "\n"
            elif arguments[1:] == [
                "rev-parse",
                "--verify",
                A_COMMIT + "^{commit}",
            ]:
                output = A_COMMIT + "\n"
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
            expanded = [
                value.format(from_commit=A_COMMIT, to_commit=B_COMMIT)
                for value in gate.command
            ]
            if arguments == expanded:
                if self.fail_gate in gate.names:
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

    def test_runs_exact_direct_gate_and_writes_canonical_evidence(self) -> None:
        fake = FakeRun()
        instants = iter(
            (
                dt.datetime(2026, 8, 30, 10, 0, 0, tzinfo=dt.UTC),
                dt.datetime(2026, 8, 30, 10, 30, 0, tzinfo=dt.UTC),
            )
        )
        result = check_release.check_release(A_COMMIT, fake, lambda: next(instants))
        expected = {
            "from_commit": A_COMMIT,
            "to_commit": B_COMMIT,
            "started_at": "2026-08-30T10:00:00.000Z",
            "finished_at": "2026-08-30T10:30:00.000Z",
            "gates": list(check_release.EVIDENCE_GATES),
            "status": "passed",
        }
        self.assertEqual(result, expected)
        gates = [
            arguments for arguments, _capture in fake.calls if arguments[0] != "git"
        ]
        self.assertEqual(
            gates,
            [
                [
                    value.format(from_commit=A_COMMIT, to_commit=B_COMMIT)
                    for value in gate.command
                ]
                for gate in check_release.GATES
            ],
        )
        self.assertTrue(all(command[0] != "just" for command in gates))
        candidate = self.evidence_root / f"{A_COMMIT}..{B_COMMIT}" / "candidate.json"
        self.assertEqual(candidate.read_bytes(), check_release.canonical_json(expected))
        self.assertEqual(stat.S_IMODE(candidate.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(candidate.parent.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(self.evidence_root.stat().st_mode), 0o700)
        self.assertEqual(json.loads(candidate.read_bytes()), expected)

    def test_gate_failure_stops_without_evidence_or_success(self) -> None:
        fake = FakeRun(fail_gate="moto-restic")
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release(A_COMMIT, fake)
        self.assertFalse(self.evidence_root.exists())
        gate_names = [
            gate.names[0]
            for gate in check_release.GATES
            if any(
                arguments
                == [
                    value.format(from_commit=A_COMMIT, to_commit=B_COMMIT)
                    for value in gate.command
                ]
                for arguments, _capture in fake.calls
            )
        ]
        self.assertEqual(
            gate_names,
            [
                "nix-flake-check",
                "go-race",
                "go-coverage",
                "govulncheck",
                "products",
                "moto-restic",
            ],
        )

    def test_dirty_equal_and_noncanonical_commits_reject_before_gates(self) -> None:
        dirty = FakeRun(dirty=True)
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release(A_COMMIT, dirty)
        self.assertFalse(any(call[0][0] != "git" for call in dirty.calls))

        equal = FakeRun()
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release(B_COMMIT, equal)
        self.assertFalse(any(call[0][0] != "git" for call in equal.calls))

        untouched = FakeRun()
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release("A" * 40, untouched)
        self.assertEqual(untouched.calls, [])

    def test_gate_and_evidence_names_are_the_frozen_contract(self) -> None:
        self.assertEqual(
            check_release.EVIDENCE_GATES,
            (
                "nix-flake-check",
                "go-race",
                "go-coverage",
                "govulncheck",
                "products",
                "moto-restic",
                "backup-restore-drill",
                "multitarget-threshold",
                "dual-repository",
                "initial-import",
                "upgrade-accept",
                "upgrade-rollback",
            ),
        )


if __name__ == "__main__":
    unittest.main()
