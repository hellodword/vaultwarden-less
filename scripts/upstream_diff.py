"""Resolve one explicit upstream tag and write a binary-safe audit report."""

from __future__ import annotations

import argparse
import fnmatch
import json
import os
import re
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

import tomllib

COMPONENTS = ("vaultwarden", "bitwarden-cli", "restic", "moto", "go")
SECTION_NAMES = {"bitwarden-cli": "bitwarden_cli"}
TAG_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/+\-]{0,127}")
COMMIT_PATTERN = re.compile(r"[0-9a-f]{40}")
STATUS_PATTERN = re.compile(r"[ACDMRTUXB][0-9]{0,3}")
MARKDOWN_CONTROLS = "`*_<>[]()#!|&"


@dataclass(frozen=True)
class Change:
    status: str
    paths: tuple[bytes, ...]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("component", choices=COMPONENTS)
    parser.add_argument("candidate_tag")
    parser.add_argument("--upstreams", type=Path, default=Path("UPSTREAMS.toml"))
    return parser.parse_args()


def run(arguments: list[str], *, cwd: Path | None = None) -> bytes:
    return subprocess.run(
        arguments,
        cwd=cwd,
        check=True,
        capture_output=True,
    ).stdout


def git(repository: Path, arguments: list[str]) -> bytes:
    return run(["git", "-C", os.fspath(repository), *arguments])


def resolve_tag(repository: Path, tag: str, namespace: str) -> str:
    reference = f"refs/vaultwarden-less/{namespace}"
    git(
        repository,
        [
            "fetch",
            "--force",
            "--no-tags",
            "origin",
            f"refs/tags/{tag}:{reference}",
        ],
    )
    commit = (
        git(repository, ["rev-parse", f"{reference}^{{commit}}"])
        .decode("ascii")
        .strip()
    )
    if not COMMIT_PATTERN.fullmatch(commit):
        raise ValueError(f"upstream tag {tag!r} did not resolve to a commit")
    return commit


def parse_changes(data: bytes) -> list[Change]:
    fields = data.split(b"\0")
    if fields and fields[-1] == b"":
        fields.pop()
    changes: list[Change] = []
    index = 0
    while index < len(fields):
        try:
            status = fields[index].decode("ascii")
        except UnicodeDecodeError as error:
            raise ValueError("diff contains a non-ASCII status") from error
        index += 1
        if not STATUS_PATTERN.fullmatch(status):
            raise ValueError(f"diff contains invalid status {status!r}")
        path_count = 2 if status.startswith(("R", "C")) else 1
        if index + path_count > len(fields):
            raise ValueError("diff name-status output is truncated")
        paths = tuple(fields[index : index + path_count])
        if any(not path for path in paths):
            raise ValueError("diff contains an empty path")
        changes.append(Change(status=status, paths=paths))
        index += path_count
    return changes


def decode_path(value: bytes) -> str:
    return value.decode("utf-8", "surrogateescape")


def markdown_literal(value: str) -> str:
    encoded = json.dumps(value, ensure_ascii=True)
    for character in MARKDOWN_CONTROLS:
        encoded = encoded.replace(character, f"\\u{ord(character):04x}")
    return encoded


def audited(change: Change, patterns: list[str]) -> bool:
    return any(
        fnmatch.fnmatchcase(decode_path(path), pattern)
        for path in change.paths
        for pattern in patterns
    )


def change_line(change: Change) -> str:
    paths = " -> ".join(markdown_literal(decode_path(path)) for path in change.paths)
    return f"- {markdown_literal(change.status)} {paths}"


def parse_commits(data: bytes) -> list[tuple[str, str]]:
    commits: list[tuple[str, str]] = []
    for record in data.split(b"\0"):
        if not record:
            continue
        commit, separator, subject = record.partition(b"\t")
        if not separator:
            raise ValueError("git log output is malformed")
        commit_text = commit.decode("ascii")
        if not COMMIT_PATTERN.fullmatch(commit_text):
            raise ValueError("git log contains an invalid commit")
        commits.append((commit_text, subject.decode("utf-8", "surrogateescape")))
    return commits


def atomic_write(path: Path, text: str) -> None:
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as output:
            descriptor = -1
            output.write(text)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        temporary.unlink(missing_ok=True)


def main() -> int:
    args = parse_args()
    try:
        if not TAG_PATTERN.fullmatch(args.candidate_tag):
            raise ValueError("candidate tag has an invalid form")
        metadata = tomllib.loads(args.upstreams.read_text(encoding="utf-8"))
        section = metadata[SECTION_NAMES.get(args.component, args.component)]
        repository_url = section["repository"]
        base_tag = section["source_tag"]
        base_commit = section["source_commit"]
        audit_paths = section["audit_paths"]
        if (
            not isinstance(repository_url, str)
            or not repository_url.startswith("https://")
            or not isinstance(base_tag, str)
            or not COMMIT_PATTERN.fullmatch(base_commit)
            or not isinstance(audit_paths, list)
            or not all(isinstance(item, str) and item for item in audit_paths)
        ):
            raise ValueError("current upstream metadata is incomplete")

        root = Path("tmp/upstream-audit").resolve()
        root.mkdir(parents=True, exist_ok=True)
        component_root = root / args.component
        component_root.mkdir(exist_ok=True)
        repository = component_root / "repository"
        if repository.is_symlink():
            raise ValueError("upstream audit repository must not be a symlink")
        if not repository.exists():
            run(
                [
                    "git",
                    "clone",
                    "--filter=blob:none",
                    "--no-checkout",
                    repository_url,
                    os.fspath(repository),
                ]
            )
        actual_remote = (
            git(repository, ["remote", "get-url", "origin"]).decode("utf-8").strip()
        )
        if actual_remote != repository_url:
            raise ValueError("cached upstream repository has an unexpected origin")

        resolved_base = resolve_tag(repository, base_tag, "base")
        if resolved_base != base_commit:
            raise ValueError("current source tag no longer matches its audited commit")
        candidate_commit = resolve_tag(repository, args.candidate_tag, "candidate")
        git(repository, ["checkout", "--detach", "--force", candidate_commit])

        changes = parse_changes(
            git(
                repository,
                [
                    "diff",
                    "--name-status",
                    "-z",
                    "--find-renames",
                    "--find-copies",
                    base_commit,
                    candidate_commit,
                    "--",
                ],
            )
        )
        commits = parse_commits(
            git(
                repository,
                [
                    "log",
                    "-z",
                    "--format=%H%x09%s",
                    "--no-merges",
                    f"{base_commit}..{candidate_commit}",
                ],
            )
        )
        audit_changes = [change for change in changes if audited(change, audit_paths)]
        report = [
            f"# {args.component} upstream audit",
            "",
            f"- Current tag: {markdown_literal(base_tag)}",
            f"- Current commit: {markdown_literal(base_commit)}",
            f"- Candidate tag: {markdown_literal(args.candidate_tag)}",
            f"- Candidate commit: {markdown_literal(candidate_commit)}",
            "",
            "## Audited-surface changes",
            "",
            *(change_line(change) for change in audit_changes),
            *(["- (none)"] if not audit_changes else []),
            "",
            "## Non-merge commits",
            "",
            *(
                f"- {markdown_literal(commit)} {markdown_literal(subject)}"
                for commit, subject in commits
            ),
            *(["- (none)"] if not commits else []),
            "",
            "## All changed paths",
            "",
            *(change_line(change) for change in changes),
            *(["- (none)"] if not changes else []),
            "",
        ]
        safe_tag = re.sub(r"[^A-Za-z0-9._-]", "_", args.candidate_tag)
        destination = component_root / f"report-{safe_tag}.md"
        atomic_write(destination, "\n".join(report))
        print(destination)
    except (
        KeyError,
        OSError,
        subprocess.CalledProcessError,
        TypeError,
        ValueError,
        tomllib.TOMLDecodeError,
    ) as error:
        print(f"upstream diff failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
