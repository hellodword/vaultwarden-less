#!/usr/bin/env python3
"""Build and verify the two reproducible vaultwarden-less binaries."""

from __future__ import annotations

import argparse
import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile


MODULE = "github.com/hellodword/vaultwarden-less/internal/version.Product"
VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--version-file", required=True, type=Path)
    return parser.parse_args()


def read_version(path: Path) -> str:
    value = path.read_text(encoding="utf-8").strip()
    if not VERSION_PATTERN.fullmatch(value):
        raise ValueError("VERSION must contain one MAJOR.MINOR.PATCH value")
    return value


def digest(path: Path) -> str:
    result = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def build(output: Path, package: str, version: str, cgo_enabled: bool) -> None:
    environment = os.environ.copy()
    environment["CGO_ENABLED"] = "1" if cgo_enabled else "0"
    subprocess.run(
        [
            "go",
            "build",
            "-trimpath",
            "-buildvcs=false",
            f"-ldflags=-buildid= -s -w -X {MODULE}={version}",
            "-o",
            os.fspath(output),
            package,
        ],
        check=True,
        env=environment,
    )


def verify_binary(path: Path, command_name: str, version: str) -> None:
    build_id = subprocess.run(
        ["go", "tool", "buildid", os.fspath(path)],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    if build_id:
        raise RuntimeError(f"{command_name} contains a Go build ID")
    output = subprocess.run(
        [os.fspath(path), "--version"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    expected = f"{command_name} {version}\n"
    if output != expected:
        raise RuntimeError(f"{command_name} reported an unexpected version")


def main() -> int:
    args = parse_args()
    try:
        version = read_version(args.version_file)
        args.output.mkdir(parents=True, exist_ok=True)
        products = (
            (
                "vaultwarden-less-backup",
                "./cmd/vaultwarden-less-backup",
                True,
            ),
            (
                "vaultwarden-less-proxy",
                "./cmd/vaultwarden-less-proxy",
                False,
            ),
        )
        with tempfile.TemporaryDirectory(prefix="vaultwarden-less-build-") as raw:
            temporary = Path(raw)
            for command_name, package, cgo_enabled in products:
                first = temporary / f"{command_name}-first"
                second = temporary / f"{command_name}-second"
                build(first, package, version, cgo_enabled)
                build(second, package, version, cgo_enabled)
                verify_binary(first, command_name, version)
                verify_binary(second, command_name, version)
                if digest(first) != digest(second):
                    raise RuntimeError(f"{command_name} is not reproducible")
                destination = args.output / command_name
                staged = args.output / f".{command_name}.new"
                shutil.copyfile(first, staged)
                staged.chmod(0o755)
                os.replace(staged, destination)
    except (OSError, subprocess.CalledProcessError, ValueError, RuntimeError) as error:
        print(f"build failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
