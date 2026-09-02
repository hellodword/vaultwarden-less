"""Run the optional release checks for one clean commit."""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from pathlib import Path

FULL_COMMIT = re.compile(r"^[0-9a-f]{40}$")
REPOSITORY = Path(__file__).resolve().parents[1]


@dataclass(frozen=True)
class Gate:
    name: str
    command: tuple[str, ...]


def verified_go_test(
    package: str,
    pattern: str,
    required: str,
    *arguments: str,
    timeout: str,
) -> tuple[str, ...]:
    command = [
        sys.executable,
        "scripts/verify_go_test.py",
    ]
    command.extend(("--require", f"{package}:{required}"))
    command.extend(
        (
            "--",
            "go",
            "test",
            "-json",
            "." + package.removeprefix("github.com/hellodword/vaultwarden-less"),
            "-run",
            pattern,
            "-count=1",
            f"-timeout={timeout}",
            "-args",
            *arguments,
        )
    )
    return tuple(command)


GATES: tuple[Gate, ...] = (
    Gate("nix-flake-check", ("nix", "flake", "check")),
    Gate("go-race", ("go", "test", "-race", "./...", "-count=1")),
    Gate("govulncheck", ("govulncheck", "./...")),
    Gate(
        "products",
        (
            "nix",
            "build",
            "--no-link",
            ".#backup",
            ".#proxy",
            ".#backup-image",
            ".#proxy-image",
            ".#vaultwarden-image",
            ".#moto-image",
            ".#release",
        ),
    ),
    Gate(
        "targetless-deployment",
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^TestTargetlessDeploymentLifecycle$",
            "TestTargetlessDeploymentLifecycle",
            "-run-backup-e2e",
            timeout="20m",
        ),
    ),
    Gate(
        "moto-restic",
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/internal/restore",
            "^TestMotoResticIntegration$",
            "TestMotoResticIntegration",
            "-run-moto-integration",
            timeout="20m",
        ),
    ),
    Gate(
        "backup-restore-drill",
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^TestBackupRestoreLifecycle$",
            "TestBackupRestoreLifecycle",
            "-run-backup-e2e",
            timeout="30m",
        ),
    ),
    Gate(
        "multitarget-threshold",
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/internal/worker",
            "^TestWorkerAcceleratedTargetDegradationIntegration$",
            "TestWorkerAcceleratedTargetDegradationIntegration",
            "-run-accelerated-worker-integration",
            timeout="5m",
        ),
    ),
    Gate(
        "dual-repository",
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^TestMultiTargetFastCatchUp$",
            "TestMultiTargetFastCatchUp",
            "-run-multitarget-fast-e2e",
            timeout="25m",
        ),
    ),
    Gate(
        "initial-import",
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^TestInitialImportLifecycle$",
            "TestInitialImportLifecycle",
            "-run-import-e2e",
            timeout="30m",
        ),
    ),
)
GATE_NAMES = tuple(gate.name for gate in GATES)

RunCommand = Callable[..., subprocess.CompletedProcess[str]]


class ReleaseCheckError(Exception):
    """A bounded release-check precondition or gate failure."""


def run_checked(
    run_command: RunCommand,
    arguments: Sequence[str],
    *,
    capture: bool,
    context: str,
) -> subprocess.CompletedProcess[str]:
    try:
        return run_command(
            list(arguments),
            cwd=REPOSITORY,
            check=True,
            text=True,
            capture_output=capture,
        )
    except OSError as error:
        raise ReleaseCheckError(f"{context} could not start") from error
    except subprocess.CalledProcessError as error:
        raise ReleaseCheckError(
            f"{context} exited with status {error.returncode}"
        ) from error


def git_output(run_command: RunCommand, *arguments: str) -> str:
    result = run_checked(
        run_command,
        ("git", *arguments),
        capture=True,
        context="Git identity check",
    )
    return result.stdout.strip()


def clean_commit(run_command: RunCommand) -> str:
    if git_output(run_command, "status", "--porcelain=v1", "--untracked-files=all"):
        raise ReleaseCheckError("release check requires a clean Git worktree")
    commit = git_output(run_command, "rev-parse", "--verify", "HEAD")
    if FULL_COMMIT.fullmatch(commit) is None:
        raise ReleaseCheckError("release commit is invalid")
    return commit


def check_release(
    run_command: RunCommand = subprocess.run,
) -> str:
    commit = clean_commit(run_command)
    for gate in GATES:
        run_checked(
            run_command,
            gate.command,
            capture=False,
            context=f"gate {gate.name}",
        )
    if clean_commit(run_command) != commit:
        raise ReleaseCheckError("release identity changed while checking")
    return commit


def parser() -> argparse.ArgumentParser:
    return argparse.ArgumentParser(description=__doc__)


def main(
    arguments: Sequence[str] | None = None,
    run_command: RunCommand = subprocess.run,
) -> int:
    parser().parse_args(arguments)
    try:
        commit = check_release(run_command)
    except ReleaseCheckError as error:
        print(f"check-release failed: {error}", file=sys.stderr)
        return 1
    print(f"release checks passed: {commit}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
