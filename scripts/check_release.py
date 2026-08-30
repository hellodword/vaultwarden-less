"""Run the fixed release-candidate gates and write exact A/B evidence."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import stat
import subprocess
import sys
import tempfile
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from pathlib import Path

FULL_COMMIT = re.compile(r"^[0-9a-f]{40}$")
REPOSITORY = Path(__file__).resolve().parents[1]
EVIDENCE_ROOT = REPOSITORY / "tmp" / "release-evidence"


@dataclass(frozen=True)
class Gate:
    names: tuple[str, ...]
    command: tuple[str, ...]


def verified_go_test(
    package: str,
    pattern: str,
    required: Sequence[str],
    *arguments: str,
    timeout: str,
) -> tuple[str, ...]:
    command = [
        sys.executable,
        "scripts/verify_go_test.py",
    ]
    for test in required:
        command.extend(("--require", f"{package}:{test}"))
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
    Gate(("nix-flake-check",), ("nix", "flake", "check")),
    Gate(("go-race",), ("go", "test", "-race", "./...", "-count=1")),
    Gate(("go-coverage",), ("go", "test", "-cover", "./...")),
    Gate(("govulncheck",), ("govulncheck", "./...")),
    Gate(
        ("products",),
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
        ),
    ),
    Gate(
        ("moto-restic",),
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/internal/restore",
            "^TestMotoResticIntegration$",
            ("TestMotoResticIntegration",),
            "-run-moto-integration",
            timeout="20m",
        ),
    ),
    Gate(
        ("backup-restore-drill",),
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^TestBackupRestoreLifecycle$",
            ("TestBackupRestoreLifecycle",),
            "-run-backup-e2e",
            timeout="30m",
        ),
    ),
    Gate(
        ("multitarget-threshold",),
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/internal/worker",
            "^TestWorkerAcceleratedTargetDegradationIntegration$",
            ("TestWorkerAcceleratedTargetDegradationIntegration",),
            "-run-accelerated-worker-integration",
            timeout="5m",
        ),
    ),
    Gate(
        ("dual-repository",),
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^TestMultiTargetFastCatchUp$",
            ("TestMultiTargetFastCatchUp",),
            "-run-multitarget-fast-e2e",
            timeout="25m",
        ),
    ),
    Gate(
        ("initial-import",),
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^TestInitialImportLifecycle$",
            ("TestInitialImportLifecycle",),
            "-run-import-e2e",
            timeout="30m",
        ),
    ),
    Gate(
        ("upgrade-accept", "upgrade-rollback"),
        verified_go_test(
            "github.com/hellodword/vaultwarden-less/tests/e2e",
            "^(TestUpgradeAcceptLifecycle|TestUpgradeRollbackLifecycle)$",
            ("TestUpgradeAcceptLifecycle", "TestUpgradeRollbackLifecycle"),
            "-run-upgrade-accept-e2e",
            "-run-upgrade-rollback-e2e",
            "-upgrade-from-commit",
            "{from_commit}",
            "-upgrade-to-commit",
            "{to_commit}",
            timeout="95m",
        ),
    ),
)
EVIDENCE_GATES = tuple(name for gate in GATES for name in gate.names)

RunCommand = Callable[..., subprocess.CompletedProcess[str]]
Now = Callable[[], dt.datetime]


class ReleaseCheckError(Exception):
    """A bounded release-check precondition, gate, or evidence failure."""


def run_checked(
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


def git_output(run_command: RunCommand, *arguments: str) -> str:
    result = run_checked(run_command, ("git", *arguments), capture=True)
    return result.stdout.strip()


def clean_pair(from_commit: str, run_command: RunCommand) -> tuple[str, str]:
    if FULL_COMMIT.fullmatch(from_commit) is None:
        raise ReleaseCheckError("A commit must be full lowercase hexadecimal")
    if git_output(run_command, "status", "--porcelain=v1", "--untracked-files=all"):
        raise ReleaseCheckError("release check requires a clean Git worktree")
    to_commit = git_output(run_command, "rev-parse", "--verify", "HEAD")
    if FULL_COMMIT.fullmatch(to_commit) is None or from_commit == to_commit:
        raise ReleaseCheckError("release commits are invalid or equal")
    resolved_from = git_output(
        run_command, "rev-parse", "--verify", from_commit + "^{commit}"
    )
    if resolved_from != from_commit:
        raise ReleaseCheckError("A identity does not resolve to the exact commit")
    return from_commit, to_commit


def format_time(value: dt.datetime) -> str:
    if value.tzinfo is None:
        raise ReleaseCheckError("release evidence clock is not timezone-aware")
    return (
        value.astimezone(dt.UTC)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z")
    )


def canonical_json(value: object) -> bytes:
    return (
        json.dumps(value, ensure_ascii=True, separators=(",", ":"), sort_keys=True)
        + "\n"
    ).encode("ascii")


def validate_directory(path: Path, *, private: bool) -> None:
    information = path.lstat()
    if stat.S_ISLNK(information.st_mode) or not stat.S_ISDIR(information.st_mode):
        raise ReleaseCheckError("release evidence directory is unsafe")
    if information.st_uid != os.geteuid() or information.st_gid != os.getegid():
        raise ReleaseCheckError("release evidence directory ownership is unsafe")
    if private and stat.S_IMODE(information.st_mode) != 0o700:
        raise ReleaseCheckError("release evidence directory mode is unsafe")


def ensure_directory(path: Path, *, private: bool) -> None:
    try:
        path.mkdir(mode=0o700)
    except FileExistsError:
        pass
    validate_directory(path, private=private)


def validate_existing_private_file(path: Path) -> None:
    try:
        information = path.lstat()
    except FileNotFoundError:
        return
    if (
        stat.S_ISLNK(information.st_mode)
        or not stat.S_ISREG(information.st_mode)
        or stat.S_IMODE(information.st_mode) != 0o600
        or information.st_uid != os.geteuid()
        or information.st_gid != os.getegid()
        or information.st_nlink != 1
    ):
        raise ReleaseCheckError("release evidence file is unsafe")


def atomic_write(path: Path, data: bytes) -> None:
    validate_existing_private_file(path)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", dir=path.parent
    )
    temporary = Path(temporary_name)
    open_descriptor = True
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb", closefd=True) as output:
            open_descriptor = False
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if open_descriptor:
            os.close(descriptor)
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def candidate_path(
    from_commit: str,
    to_commit: str,
    run_command: RunCommand,
) -> Path:
    temporary_root = EVIDENCE_ROOT.parent
    ensure_directory(temporary_root, private=False)
    ensure_directory(EVIDENCE_ROOT, private=True)
    pair = EVIDENCE_ROOT / f"{from_commit}..{to_commit}"
    ensure_directory(pair, private=True)
    path = pair / "candidate.json"
    try:
        relative = path.relative_to(REPOSITORY)
    except ValueError as error:
        raise ReleaseCheckError("release evidence escaped the repository") from error
    run_checked(
        run_command,
        ("git", "check-ignore", "--quiet", "--", os.fspath(relative)),
        capture=False,
    )
    return path


def check_release(
    from_commit: str,
    run_command: RunCommand = subprocess.run,
    now: Now = lambda: dt.datetime.now(dt.UTC),
) -> dict[str, object]:
    source, target = clean_pair(from_commit, run_command)
    started_at = format_time(now())
    for gate in GATES:
        command = tuple(
            value.format(from_commit=source, to_commit=target) for value in gate.command
        )
        run_checked(run_command, command, capture=False)
    final_source, final_target = clean_pair(from_commit, run_command)
    if final_source != source or final_target != target:
        raise ReleaseCheckError("release identity changed while checking")
    result: dict[str, object] = {
        "from_commit": source,
        "to_commit": target,
        "started_at": started_at,
        "finished_at": format_time(now()),
        "gates": list(EVIDENCE_GATES),
        "status": "passed",
    }
    try:
        atomic_write(
            candidate_path(source, target, run_command), canonical_json(result)
        )
    except OSError as error:
        raise ReleaseCheckError("release evidence write failed") from error
    return result


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
        sys.stderr.buffer.write(
            canonical_json(
                {
                    "error": {
                        "code": "release_check_failed",
                        "message": "Release candidate validation failed",
                    }
                }
            )
        )
        return 1
    sys.stdout.buffer.write(canonical_json(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
