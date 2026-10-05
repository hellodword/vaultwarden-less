"""Review a candidate Git ref against the source actually selected by the flake.

Use just upstream-diff. This reports changes; it is never an upgrade or runtime
admission gate. It does not modify the flake or any source pin.
"""

import argparse
import difflib
import io
import subprocess
import tarfile
import tempfile
from pathlib import Path

COMPONENTS = {
    "vaultwarden": (
        "https://github.com/dani-garcia/vaultwarden",
        [
            "src/api/**",
            "src/db/**",
            "migrations/sqlite/**",
            "src/main.rs",
            "src/config.rs",
            "src/auth.rs",
            ".env.template",
            ".env.example",
        ],
    ),
    "restic": (
        "https://github.com/restic/restic",
        ["cmd/**", "internal/**", "go.mod", "go.sum", "CHANGELOG.md"],
    ),
    "bitwarden-cli": (
        "https://github.com/bitwarden/clients",
        ["apps/cli/**", "libs/**"],
    ),
    "moto": (
        "https://github.com/getmoto/moto",
        ["moto/**", "pyproject.toml", "CHANGELOG.md"],
    ),
    "go": ("https://go.googlesource.com/go", ["src/**", "go.mod", "VERSION"]),
}


def files(root: Path, patterns: list[str]) -> set[Path]:
    result = set()
    for pattern in patterns:
        # pathlib's trailing ** matches directories, so include their files.
        glob = pattern + "/*" if pattern.endswith("/**") else pattern
        result.update(
            path.relative_to(root) for path in root.glob(glob) if path.is_file()
        )
    return result


def audit_changes(baseline: Path, candidate: Path, component: str) -> str:
    paths = files(baseline, COMPONENTS[component][1]) | files(
        candidate, COMPONENTS[component][1]
    )
    changes = []
    for path in sorted(paths):
        before = (baseline / path).read_bytes() if (baseline / path).is_file() else b""
        after = (candidate / path).read_bytes() if (candidate / path).is_file() else b""
        if before == after:
            continue
        changes.extend(
            difflib.unified_diff(
                before.decode("utf-8", errors="replace").splitlines(keepends=True),
                after.decode("utf-8", errors="replace").splitlines(keepends=True),
                fromfile=f"baseline/{path}",
                tofile=f"candidate/{path}",
            )
        )
    return "".join(changes)


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("component", choices=COMPONENTS)
    parser.add_argument("ref", help="candidate Git commit, branch, or tag")
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    args = parser.parse_args(argv)
    if not args.ref or args.ref.startswith("-"):
        parser.error("candidate ref must not start with a dash")
    baseline = args.baseline.resolve()
    if not baseline.exists():
        parser.error("baseline must be a source directory or archive")
    scratch = args.work_dir.resolve()
    scratch.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=scratch) as directory:
        workspace = Path(directory)
        if baseline.is_file():
            unpacked = workspace / "baseline"
            unpacked.mkdir()
            with tarfile.open(baseline) as archive:
                archive.extractall(unpacked, filter="data")
            children = list(unpacked.iterdir())
            baseline = (
                children[0] if len(children) == 1 and children[0].is_dir() else unpacked
            )
        repository = workspace / "git"
        subprocess.run(["git", "init", "--quiet", str(repository)], check=True)
        subprocess.run(
            [
                "git",
                "-C",
                str(repository),
                "fetch",
                "--depth=1",
                COMPONENTS[args.component][0],
                args.ref,
            ],
            check=True,
        )
        archive = subprocess.run(
            ["git", "-C", str(repository), "archive", "FETCH_HEAD"],
            check=True,
            stdout=subprocess.PIPE,
        )
        candidate = workspace / "candidate"
        candidate.mkdir()
        with tarfile.open(fileobj=io.BytesIO(archive.stdout)) as stream:
            stream.extractall(candidate, filter="data")
        print(
            audit_changes(baseline, candidate, args.component)
            or "No changes in the audited paths.",
            end="\n",
        )


if __name__ == "__main__":
    main()
