"""Run the complete fast release-candidate gate for one clean A/B pair."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from collections.abc import Callable, Sequence
from pathlib import Path

SCHEMA_VERSION = 1
FULL_COMMIT = re.compile(r"^[0-9a-f]{40}$")
REPOSITORY = Path(__file__).resolve().parents[1]
GATES: tuple[tuple[str, ...], ...] = (
    ("just", "check"),
    ("just", "_race-release"),
    ("just", "_security"),
    ("just", "_build"),
    ("just", "_integration"),
    ("just", "_e2e-backup"),
    ("just", "_e2e-multitarget-fast"),
    ("just", "_e2e-import"),
)

RunCommand = Callable[..., subprocess.CompletedProcess[str]]


class ReleaseCheckError(Exception):
    """A bounded release-check precondition or gate failure."""


def _run(
    run_command: RunCommand,
    arguments: Sequence[str],
    *,
    capture: bool,
) -> subprocess.CompletedProcess[str]:
    try:
        return run_command(
            list(arguments),
            cwd=REPOSITORY,
            check=True,
            text=True,
            capture_output=capture,
        )
    except (OSError, subprocess.CalledProcessError) as error:
        raise ReleaseCheckError("release check command failed") from error


def _git_output(run_command: RunCommand, *arguments: str) -> str:
    result = _run(run_command, ("git", *arguments), capture=True)
    return result.stdout.strip()


def clean_pair(from_commit: str, run_command: RunCommand) -> tuple[str, str]:
    if FULL_COMMIT.fullmatch(from_commit) is None:
        raise ReleaseCheckError("A commit must be full lowercase hexadecimal")
    if _git_output(run_command, "status", "--porcelain=v1", "--untracked-files=all"):
        raise ReleaseCheckError("release check requires a clean Git worktree")
    to_commit = _git_output(run_command, "rev-parse", "--verify", "HEAD")
    if FULL_COMMIT.fullmatch(to_commit) is None or from_commit == to_commit:
        raise ReleaseCheckError("release commits are invalid or equal")
    resolved_from = _git_output(
        run_command, "rev-parse", "--verify", from_commit + "^{commit}"
    )
    if resolved_from != from_commit:
        raise ReleaseCheckError("A identity does not resolve to the exact commit")
    return from_commit, to_commit


def check_release(
    from_commit: str,
    run_command: RunCommand = subprocess.run,
) -> dict[str, object]:
    source, target = clean_pair(from_commit, run_command)
    for command in GATES:
        _run(run_command, command, capture=False)
    _run(
        run_command,
        ("just", "_e2e-upgrade", source, target),
        capture=False,
    )
    final_source, final_target = clean_pair(from_commit, run_command)
    if final_source != source or final_target != target:
        raise ReleaseCheckError("release identity changed while checking")
    return {
        "schema_version": SCHEMA_VERSION,
        "status": "passed",
        "from_commit": source,
        "to_commit": target,
    }


def parser() -> argparse.ArgumentParser:
    command = argparse.ArgumentParser(description=__doc__)
    command.add_argument("from_commit")
    return command


def main(
    arguments: Sequence[str] | None = None,
    run_command: RunCommand = subprocess.run,
) -> int:
    parsed = parser().parse_args(arguments)
    try:
        result = check_release(parsed.from_commit, run_command)
    except ReleaseCheckError:
        json.dump(
            {
                "schema_version": SCHEMA_VERSION,
                "error": {
                    "code": "release_check_failed",
                    "message": "Release candidate validation failed",
                },
            },
            sys.stderr,
            separators=(",", ":"),
        )
        sys.stderr.write("\n")
        return 1
    json.dump(result, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
