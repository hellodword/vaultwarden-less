#!/usr/bin/env python3
"""Validate and publish the VERSION-matched linux/amd64 product image."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile


VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repository", required=True)
    parser.add_argument("--version-file", required=True, type=Path)
    parser.add_argument("--tag")
    return parser.parse_args()


def run(arguments: list[str], *, capture: bool = False) -> str:
    completed = subprocess.run(
        arguments,
        check=True,
        capture_output=capture,
        text=True,
    )
    return completed.stdout if capture else ""


def current_tag(explicit: str | None) -> str:
    if explicit:
        return explicit
    if os.environ.get("GITHUB_REF_TYPE") == "tag":
        value = os.environ.get("GITHUB_REF_NAME", "")
        if value:
            return value
    return run(["git", "describe", "--tags", "--exact-match"], capture=True).strip()


def verify_image_files(image: str) -> None:
    container = run(["docker", "create", image], capture=True).strip()
    if not re.fullmatch(r"[0-9a-f]{64}", container):
        raise RuntimeError("docker create returned an invalid container ID")
    try:
        with tempfile.TemporaryDirectory(prefix="vaultwarden-less-image-") as raw:
            archive_path = Path(raw) / "rootfs.tar"
            run(["docker", "export", "--output", os.fspath(archive_path), container])
            with tarfile.open(archive_path, mode="r:") as archive:
                names = {member.name.removeprefix("./").rstrip("/") for member in archive.getmembers()}
        required = {
            "usr/local/bin/vaultwarden-less-backup",
            "usr/local/bin/vaultwarden-less-proxy",
            "usr/local/bin/restic",
            "licenses/vaultwarden-less/LICENSE",
            "licenses/vaultwarden-less/NOTICE.md",
            "licenses/vaultwarden-less/third-party/Apache-2.0.txt",
            "licenses/vaultwarden-less/third-party/go-stdlib-BSD-3-Clause.txt",
            "licenses/vaultwarden-less/third-party/goccy-go-yaml-MIT.txt",
            "licenses/vaultwarden-less/third-party/mattn-go-sqlite3-MIT.txt",
            "licenses/vaultwarden-less/third-party/restic-BSD-2-Clause.txt",
            "licenses/vaultwarden-less/third-party/spf13-pflag-BSD-3-Clause.txt",
            "licenses/vaultwarden-less/third-party/sqlite-public-domain.txt",
        }
        missing = sorted(required - names)
        if missing:
            raise RuntimeError(f"image is missing required runtime files: {missing}")
        forbidden = {
            "VERSION",
            "go.mod",
            "go.sum",
            "config/backup.yaml",
            "run/credentials",
            "src",
            "cmd",
            "internal",
            "tests",
        }
        if any(name in forbidden or any(name.startswith(prefix + "/") for prefix in forbidden) for name in names):
            raise RuntimeError("image contains a forbidden source, fixture, config, or credential path")
    finally:
        subprocess.run(
            ["docker", "rm", container],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )


def main() -> int:
    args = parse_args()
    try:
        if not re.fullmatch(
            r"ghcr\.io/[a-z0-9]+(?:[._-][a-z0-9]+)*/[a-z0-9]+(?:[._-][a-z0-9]+)*",
            args.repository,
        ):
            raise ValueError("repository must be a lowercase GHCR image path")
        version = args.version_file.read_text(encoding="utf-8").strip()
        if not VERSION_PATTERN.fullmatch(version):
            raise ValueError("VERSION must contain one MAJOR.MINOR.PATCH value")
        tag = current_tag(args.tag)
        if tag != f"v{version}":
            raise ValueError(f"release tag must be v{version}")

        with tempfile.TemporaryDirectory(prefix="vaultwarden-less-release-") as raw:
            output = Path(raw) / "bin"
            run(
                [
                    sys.executable,
                    "scripts/build.py",
                    "--output",
                    os.fspath(output),
                    "--version-file",
                    os.fspath(args.version_file),
                ]
            )

        local_image = f"vaultwarden-less-release:{version}"
        common = [
            "--platform",
            "linux/amd64",
            "--provenance=false",
            "--sbom=false",
            "--file",
            "docker/Dockerfile",
            "--build-arg",
            f"PRODUCT_VERSION={version}",
        ]
        run(["docker", "buildx", "build", "--load", *common, "--tag", local_image, "."])

        inspected = json.loads(
            run(["docker", "image", "inspect", local_image], capture=True)
        )
        labels = inspected[0].get("Config", {}).get("Labels", {})
        if labels.get("org.opencontainers.image.version") != version:
            raise RuntimeError("image OCI version label does not match VERSION")
        if inspected[0].get("Architecture") != "amd64" or inspected[0].get("Os") != "linux":
            raise RuntimeError("image platform is not linux/amd64")
        if inspected[0].get("Config", {}).get("User") != "65532:65532":
            raise RuntimeError("image runtime user is not the fixed non-root UID/GID")
        checks = (
            (
                "/usr/local/bin/vaultwarden-less-backup",
                f"vaultwarden-less-backup {version}\n",
            ),
            (
                "/usr/local/bin/vaultwarden-less-proxy",
                f"vaultwarden-less-proxy {version}\n",
            ),
            ("/usr/local/bin/restic", "restic 0.19.1"),
        )
        for executable, expected in checks:
            output = run(
                [
                    "docker",
                    "run",
                    "--rm",
                    "--entrypoint",
                    executable,
                    local_image,
                    "version" if executable.endswith("restic") else "--version",
                ],
                capture=True,
            )
            if output != expected and not output.startswith(expected):
                raise RuntimeError(f"{executable} reported an unexpected version")
        verify_image_files(local_image)

        remote_image = f"{args.repository}:{version}"
        run(
            [
                "docker",
                "buildx",
                "build",
                "--push",
                *common,
                "--tag",
                remote_image,
                ".",
            ]
        )
    except (
        IndexError,
        json.JSONDecodeError,
        OSError,
        subprocess.CalledProcessError,
        ValueError,
        RuntimeError,
    ) as error:
        print(f"release failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
