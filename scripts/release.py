"""Export the exact Nix-built release for a checked clean commit."""

from __future__ import annotations

import argparse
import ctypes
import datetime as dt
import errno
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

import check_release

FULL_COMMIT = re.compile(r"[0-9a-f]{40}")
EVIDENCE_TIME = re.compile(
    r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}[.][0-9]{3}Z"
)
ARCHIVES = ("backup.tar.gz", "proxy.tar.gz", "vaultwarden.tar.gz")
RELEASE_FILES = ("SHA256SUMS", *ARCHIVES)
REPOSITORY = Path(__file__).resolve().parents[1]


@dataclass(frozen=True)
class Identity:
    commit: str
    timestamp: int
    release: str
    created: str


def parser() -> argparse.ArgumentParser:
    command = argparse.ArgumentParser(description=__doc__)
    command.add_argument("output_directory", type=Path)
    return command


def run(
    arguments: list[str], *, check: bool = True
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        arguments,
        cwd=REPOSITORY,
        check=check,
        text=True,
        capture_output=True,
    )


def git_identity() -> Identity:
    status = run(["git", "status", "--porcelain=v1", "--untracked-files=all"])
    if status.stdout:
        raise ValueError("release creation requires a clean Git worktree")
    commit = run(["git", "rev-parse", "--verify", "HEAD"]).stdout.strip()
    if not FULL_COMMIT.fullmatch(commit):
        raise ValueError("Git HEAD is not a full commit")
    timestamp_text = run(["git", "show", "-s", "--format=%ct", "HEAD"]).stdout.strip()
    if not timestamp_text.isdecimal():
        raise ValueError("Git commit timestamp is invalid")
    timestamp = int(timestamp_text)
    instant = dt.datetime.fromtimestamp(timestamp, dt.UTC).replace(microsecond=0)
    return Identity(
        commit=commit,
        timestamp=timestamp,
        release=f"RELEASE.{instant:%Y-%m-%dT%H-%M-%SZ}-g{commit[:12]}",
        created=f"{instant:%Y-%m-%dT%H:%M:%SZ}",
    )


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def strict_json(data: bytes) -> object:
    def unique(pairs: list[tuple[str, object]]) -> dict[str, object]:
        result: dict[str, object] = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("release evidence contains a duplicate key")
            result[key] = value
        return result

    try:
        return json.loads(data, object_pairs_hook=unique)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError("release evidence is invalid JSON") from error


def evidence_time(value: object) -> dt.datetime:
    if not isinstance(value, str) or EVIDENCE_TIME.fullmatch(value) is None:
        raise ValueError("release evidence timestamp is invalid")
    try:
        return dt.datetime.strptime(value, "%Y-%m-%dT%H:%M:%S.%fZ").replace(
            tzinfo=dt.UTC
        )
    except ValueError as error:
        raise ValueError("release evidence timestamp is invalid") from error


def validate_candidate(commit: str) -> tuple[Path, str]:
    evidence_root = check_release.EVIDENCE_ROOT
    release_directory = evidence_root / commit
    candidate = release_directory / "candidate.json"
    try:
        relative = candidate.relative_to(REPOSITORY)
    except ValueError as error:
        raise ValueError("release evidence escaped the repository") from error
    check_release.validate_directory(evidence_root.parent, private=False)
    check_release.validate_directory(evidence_root, private=True)
    check_release.validate_directory(release_directory, private=True)
    try:
        candidate_information = candidate.lstat()
    except FileNotFoundError as error:
        raise ValueError("candidate evidence is missing") from error
    check_release.validate_existing_private_file(candidate)
    if candidate_information.st_size > 16 * 1024:
        raise ValueError("candidate evidence is too large")
    run(["git", "check-ignore", "--quiet", "--", os.fspath(relative)])
    data = candidate.read_bytes()
    document = strict_json(data)
    expected_keys = {
        "commit",
        "started_at",
        "finished_at",
        "gates",
        "status",
    }
    if not isinstance(document, dict) or set(document) != expected_keys:
        raise ValueError("candidate evidence fields are invalid")
    if data != check_release.canonical_json(document):
        raise ValueError("candidate evidence is not canonical")
    if (
        document["commit"] != commit
        or document["gates"] != list(check_release.EVIDENCE_GATES)
        or document["status"] != "passed"
    ):
        raise ValueError("candidate evidence does not match the release")
    started = evidence_time(document["started_at"])
    finished = evidence_time(document["finished_at"])
    if finished < started:
        raise ValueError("candidate evidence timestamps are reversed")
    return candidate, "sha256:" + hashlib.sha256(data).hexdigest()


def validate_output(output_directory: Path) -> Path:
    if output_directory.name in {"", ".", ".."}:
        raise ValueError("release output directory name is invalid")
    absolute = (
        output_directory
        if output_directory.is_absolute()
        else Path.cwd() / output_directory
    ).absolute()
    if absolute.exists() or absolute.is_symlink():
        raise FileExistsError(absolute)
    parent = absolute.parent
    information = parent.lstat()
    if stat.S_ISLNK(information.st_mode) or not stat.S_ISDIR(information.st_mode):
        raise ValueError("release output parent must be a real directory")
    if parent.resolve(strict=True) != parent:
        raise ValueError("release output parent has a symlink or traversal component")
    try:
        relative = absolute.relative_to(REPOSITORY)
    except ValueError:
        pass
    else:
        run(["git", "check-ignore", "--quiet", "--", os.fspath(relative)])
    return absolute


def validate_bundle(directory: Path) -> dict[str, str]:
    information = directory.lstat()
    if stat.S_ISLNK(information.st_mode) or not stat.S_ISDIR(information.st_mode):
        raise ValueError("release bundle must be a real directory")
    if {entry.name for entry in directory.iterdir()} != set(RELEASE_FILES):
        raise ValueError("release bundle files are not exact")
    for name in RELEASE_FILES:
        information = (directory / name).lstat()
        if stat.S_ISLNK(information.st_mode) or not stat.S_ISREG(information.st_mode):
            raise ValueError("release bundle contains a non-regular file")
    hashes = {name: sha256_file(directory / name) for name in RELEASE_FILES}
    expected = "".join(f"{hashes[name]}  {name}\n" for name in ARCHIVES)
    if (directory / "SHA256SUMS").read_text(encoding="ascii") != expected:
        raise ValueError("release bundle checksum is invalid")
    return hashes


def build_bundle() -> Path:
    completed = run(
        [
            "nix",
            "build",
            "--no-link",
            "--print-out-paths",
            ".#release",
        ]
    )
    paths = [Path(line) for line in completed.stdout.splitlines() if line]
    if len(paths) != 1:
        raise ValueError("Nix returned an invalid release output")
    validate_bundle(paths[0])
    return paths[0]


def copy_bundle(source: Path, destination: Path) -> dict[str, str]:
    if any(destination.iterdir()):
        raise ValueError("release temporary directory is not empty")
    for name in RELEASE_FILES:
        target = destination / name
        with (source / name).open("rb") as input_file, target.open("xb") as output:
            shutil.copyfileobj(input_file, output, length=1024 * 1024)
            output.flush()
            os.fsync(output.fileno())
        target.chmod(0o644)
    hashes = validate_bundle(destination)
    sync_directory(destination)
    return hashes


def rename_no_replace(source: Path, destination: Path) -> None:
    libc = ctypes.CDLL(None, use_errno=True)
    renameat2 = getattr(libc, "renameat2", None)
    if renameat2 is None:
        raise OSError(errno.ENOSYS, "renameat2 is unavailable")
    renameat2.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    renameat2.restype = ctypes.c_int
    if renameat2(-100, os.fsencode(source), -100, os.fsencode(destination), 1) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), destination)


def write_artifact_evidence(
    candidate: Path,
    candidate_hash: str,
    identity: Identity,
    file_hashes: dict[str, str],
    now: dt.datetime,
) -> None:
    result = {
        "candidate_sha256": candidate_hash,
        "commit": identity.commit,
        "release": identity.release,
        "files": {name: "sha256:" + file_hashes[name] for name in RELEASE_FILES},
        "verified_at": check_release.format_time(now),
        "status": "verified",
    }
    check_release.atomic_write(
        candidate.with_name("artifact.json"), check_release.canonical_json(result)
    )


def sync_directory(directory: Path) -> None:
    descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def create_release(
    output_directory: Path,
    now: dt.datetime | None = None,
) -> Path:
    identity = git_identity()
    output = validate_output(output_directory)
    candidate, candidate_hash = validate_candidate(identity.commit)
    bundle = build_bundle()
    temporary = Path(
        tempfile.mkdtemp(prefix=f".{output.name}.build.", dir=output.parent)
    )
    try:
        file_hashes = copy_bundle(bundle, temporary)
        if git_identity() != identity:
            raise ValueError("Git identity changed during release creation")
        final_candidate, final_candidate_hash = validate_candidate(identity.commit)
        if final_candidate != candidate or final_candidate_hash != candidate_hash:
            raise ValueError("candidate evidence changed during release creation")
        write_artifact_evidence(
            candidate,
            candidate_hash,
            identity,
            file_hashes,
            now if now is not None else dt.datetime.now(dt.UTC),
        )
        if git_identity() != identity:
            raise ValueError("Git identity changed before release publication")
        rename_no_replace(temporary, output)
        sync_directory(output.parent)
        return output
    finally:
        shutil.rmtree(temporary, ignore_errors=True)


def main(arguments: list[str] | None = None) -> int:
    parsed = parser().parse_args(arguments)
    try:
        output = create_release(parsed.output_directory)
        print(output)
    except (
        check_release.ReleaseCheckError,
        OSError,
        subprocess.SubprocessError,
        ValueError,
    ) as error:
        print(
            f"release failed: {type(error).__name__}",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
