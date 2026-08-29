"""Contract tests for the clean A/B release-check orchestrator."""

from __future__ import annotations

import os
import subprocess
import sys
import unittest
from pathlib import Path

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
            else:
                raise AssertionError(f"unexpected Git command {arguments}")
            return subprocess.CompletedProcess(arguments, 0, output, "")
        if arguments[:1] != ["just"] or capture_output:
            raise AssertionError(f"unexpected gate command {arguments}")
        if self.fail_gate is not None and arguments[1] == self.fail_gate:
            raise subprocess.CalledProcessError(1, arguments)
        return subprocess.CompletedProcess(arguments, 0, "", "")


class ReleaseCheckTest(unittest.TestCase):
    def test_runs_exact_fast_gate_and_rechecks_clean_identity(self) -> None:
        fake = FakeRun()
        result = check_release.check_release(A_COMMIT, fake)
        self.assertEqual(
            result,
            {
                "schema_version": 1,
                "status": "passed",
                "from_commit": A_COMMIT,
                "to_commit": B_COMMIT,
            },
        )
        gates = [
            arguments for arguments, _capture in fake.calls if arguments[0] == "just"
        ]
        self.assertEqual(
            gates,
            [list(command) for command in check_release.GATES]
            + [["just", "_e2e-upgrade", A_COMMIT, B_COMMIT]],
        )
        self.assertNotIn(["just", "soak"], gates)
        self.assertTrue(
            all(capture for arguments, capture in fake.calls if arguments[0] == "git")
        )

    def test_dirty_or_equal_commit_rejects_before_any_gate(self) -> None:
        dirty = FakeRun(dirty=True)
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release(A_COMMIT, dirty)
        self.assertFalse(any(call[0][0] == "just" for call in dirty.calls))

        equal = FakeRun()
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release(B_COMMIT, equal)
        self.assertFalse(any(call[0][0] == "just" for call in equal.calls))

    def test_gate_failure_stops_without_claiming_success(self) -> None:
        fake = FakeRun(fail_gate="_integration")
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release(A_COMMIT, fake)
        gates = [
            arguments[1] for arguments, _capture in fake.calls if arguments[0] == "just"
        ]
        self.assertEqual(
            gates,
            ["check", "_race-release", "_security", "_build", "_integration"],
        )

    def test_noncanonical_commit_is_rejected_without_subprocesses(self) -> None:
        fake = FakeRun()
        with self.assertRaises(check_release.ReleaseCheckError):
            check_release.check_release("A" * 40, fake)
        self.assertEqual(fake.calls, [])


if __name__ == "__main__":
    unittest.main()
