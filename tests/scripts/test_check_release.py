"""Contract tests for the stateless optional release checks."""

from __future__ import annotations

import contextlib
import io
import os
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
    def test_cli_takes_no_arguments(self) -> None:
        self.assertEqual(vars(check_release.parser().parse_args([])), {})
        with self.assertRaises(SystemExit):
            check_release.parser().parse_args([B_COMMIT])

    def test_runs_exact_direct_gates_without_writing_state(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="check-release-test-")
        self.addCleanup(temporary.cleanup)
        repository = Path(temporary.name)
        fake = FakeRun()
        with mock.patch.object(check_release, "REPOSITORY", repository):
            self.assertEqual(check_release.check_release(fake), B_COMMIT)
        gates = [
            arguments for arguments, _capture in fake.calls if arguments[0] != "git"
        ]
        self.assertEqual(gates, [list(gate.command) for gate in check_release.GATES])
        self.assertTrue(all(command[0] != "just" for command in gates))
        self.assertEqual(list(repository.iterdir()), [])

    def test_gate_failure_names_gate_and_stops(self) -> None:
        fake = FakeRun(fail_gate="moto-restic")
        with self.assertRaisesRegex(
            check_release.ReleaseCheckError,
            "gate moto-restic exited with status 1",
        ):
            check_release.check_release(fake)
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

    def test_cli_reports_commit_and_actionable_failure(self) -> None:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(check_release.main([], FakeRun()), 0)
        self.assertEqual(stdout.getvalue(), f"release checks passed: {B_COMMIT}\n")
        self.assertEqual(stderr.getvalue(), "")

        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(check_release.main([], FakeRun(fail_gate="products")), 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertEqual(
            stderr.getvalue(),
            "check-release failed: gate products exited with status 1\n",
        )

    def test_gate_names_are_the_full_optional_contract(self) -> None:
        self.assertEqual(
            check_release.GATE_NAMES,
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
