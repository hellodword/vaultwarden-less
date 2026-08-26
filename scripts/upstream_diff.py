"""Resolve one explicit upstream tag and write a read-only audit diff report."""

from __future__ import annotations

import argparse
import fnmatch
import re
import subprocess
import sys
from pathlib import Path

import tomllib

UPSTREAMS = {
    "vaultwarden": (
        "vaultwarden",
        "https://github.com/dani-garcia/vaultwarden.git",
    ),
    "bitwarden-cli": (
        "bitwarden_cli",
        "https://github.com/bitwarden/clients.git",
    ),
    "restic": ("restic", "https://github.com/restic/restic.git"),
    "moto": ("moto", "https://github.com/getmoto/moto.git"),
    "go": ("go", "https://go.googlesource.com/go"),
}
TAG_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/+\-]{0,127}")
COMMIT_PATTERN = re.compile(r"[0-9a-f]{40}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("component", choices=sorted(UPSTREAMS))
    parser.add_argument("candidate_tag")
    parser.add_argument("--upstreams", type=Path, default=Path("UPSTREAMS.toml"))
    return parser.parse_args()


def run(arguments: list[str], *, capture: bool = False) -> str:
    completed = subprocess.run(
        arguments,
        check=True,
        stdout=subprocess.PIPE if capture else None,
        text=True,
    )
    return completed.stdout if capture else ""


def git(repository: Path, arguments: list[str], *, capture: bool = False) -> str:
    return run(["git", "-C", str(repository), *arguments], capture=capture)


def resolve_tag(repository: Path, tag: str, namespace: str) -> str:
    reference = f"refs/vaultwarden-less/{namespace}"
    run(
        [
            "git",
            "-C",
            str(repository),
            "fetch",
            "--force",
            "--no-tags",
            "origin",
            f"refs/tags/{tag}:{reference}",
        ]
    )
    commit = git(repository, ["rev-parse", f"{reference}^{{commit}}"], capture=True)
    commit = commit.strip()
    if not COMMIT_PATTERN.fullmatch(commit):
        raise ValueError(f"upstream tag {tag!r} did not resolve to a commit")
    return commit


def main() -> int:
    args = parse_args()
    try:
        if not TAG_PATTERN.fullmatch(args.candidate_tag):
            raise ValueError("candidate tag has an invalid form")
        metadata = tomllib.loads(args.upstreams.read_text(encoding="utf-8"))
        section_name, remote = UPSTREAMS[args.component]
        section = metadata[section_name]
        base_tag = section["source_tag"]
        base_commit = section["source_commit"]
        audit_paths = section["audit_paths"]
        if (
            not isinstance(base_tag, str)
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
                    remote,
                    str(repository),
                ]
            )
        actual_remote = git(
            repository, ["remote", "get-url", "origin"], capture=True
        ).strip()
        if actual_remote != remote:
            raise ValueError("cached upstream repository has an unexpected origin")

        resolved_base = resolve_tag(repository, base_tag, "base")
        if resolved_base != base_commit:
            raise ValueError("current source tag no longer matches its audited commit")
        candidate_commit = resolve_tag(repository, args.candidate_tag, "candidate")
        git(repository, ["checkout", "--detach", "--force", candidate_commit])

        changed_output = git(
            repository,
            ["diff", "--name-status", base_commit, candidate_commit],
            capture=True,
        )
        changed_lines = [line for line in changed_output.splitlines() if line]
        audit_lines = []
        for line in changed_lines:
            path = line.split("\t")[-1]
            if any(fnmatch.fnmatchcase(path, pattern) for pattern in audit_paths):
                audit_lines.append(line)
        stat = git(
            repository,
            ["diff", "--stat", base_commit, candidate_commit],
            capture=True,
        ).rstrip()
        commits = git(
            repository,
            [
                "log",
                "--format=%H %s",
                "--no-merges",
                f"{base_commit}..{candidate_commit}",
            ],
            capture=True,
        ).rstrip()
        report = [
            f"# {args.component} upstream audit",
            "",
            f"- Current tag: `{base_tag}`",
            f"- Current commit: `{base_commit}`",
            f"- Candidate tag: `{args.candidate_tag}`",
            f"- Candidate commit: `{candidate_commit}`",
            "",
            "## Audited-surface changes",
            "",
            "```text",
            *(audit_lines or ["(none)"]),
            "```",
            "",
            "## Full diff stat",
            "",
            "```text",
            stat or "(none)",
            "```",
            "",
            "## Non-merge commits",
            "",
            "```text",
            commits or "(none)",
            "```",
            "",
            "## All changed paths",
            "",
            "```text",
            *(changed_lines or ["(none)"]),
            "```",
            "",
        ]
        safe_tag = re.sub(r"[^A-Za-z0-9._-]", "_", args.candidate_tag)
        destination = component_root / f"report-{safe_tag}.md"
        destination.write_text("\n".join(report), encoding="utf-8", newline="\n")
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
