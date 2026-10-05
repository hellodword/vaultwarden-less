"""Review a candidate Git ref against the source actually selected by the flake.

Run inside nix develop. This reports changes; it is never an upgrade or runtime
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
        "vaultwarden",
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
        "restic",
        "https://github.com/restic/restic",
        ["cmd/**", "internal/**", "go.mod", "go.sum", "CHANGELOG.md"],
    ),
    "bitwarden-cli": (
        "bitwarden-cli",
        "https://github.com/bitwarden/clients",
        ["apps/cli/**", "libs/**"],
    ),
    "moto": (
        "python3Packages.moto",
        "https://github.com/getmoto/moto",
        ["moto/**", "pyproject.toml", "CHANGELOG.md"],
    ),
    "go": ("go", "https://go.googlesource.com/go", ["src/**", "go.mod", "VERSION"]),
}


def selected_source(component: str, root: Path) -> Path:
    attr = COMPONENTS[component][0]
    expression = (
        "let flake = builtins.getFlake (toString ./.); "
        'pkgs = import ./nix/upstreams.nix { pkgs = import flake.inputs.nixpkgs { system = "x86_64-linux"; }; }; '
        f"in pkgs.{attr}.src"
    )
    result = subprocess.run(
        [
            "nix",
            "build",
            "--no-link",
            "--print-out-paths",
            "--impure",
            "--expr",
            expression,
        ],
        cwd=root,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    )
    return Path(result.stdout.strip())


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
    paths = files(baseline, COMPONENTS[component][2]) | files(
        candidate, COMPONENTS[component][2]
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


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("component", choices=COMPONENTS)
    parser.add_argument("ref", help="candidate Git commit, branch, or tag")
    args = parser.parse_args()
    if not args.ref or args.ref.startswith("-"):
        parser.error("candidate ref must not start with a dash")
    root = Path(__file__).resolve().parent.parent
    scratch = root / "tmp/agent/upstream-audit"
    scratch.mkdir(parents=True, exist_ok=True)
    baseline = selected_source(args.component, root)
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
                COMPONENTS[args.component][1],
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
