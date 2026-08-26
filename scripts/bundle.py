"""Create, load, and verify the offline deployment image bundle."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import tomllib
from project_metadata import load_project_metadata

VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
IMAGE_ID_PATTERN = re.compile(r"sha256:[0-9a-f]{64}")
LOCAL_TAG_PATTERN = re.compile(
    r"vaultwarden-less-(backup|proxy|vaultwarden):[a-z0-9][a-z0-9._-]{0,127}"
)
DOCKER_INPUTS = (
    "DOCKER_HOST",
    "DOCKER_TLS_VERIFY",
    "DOCKER_CERT_PATH",
    "DOCKER_API_VERSION",
)
ANONYMOUS_DOCKER_CONFIG = '{"auths":{"https://index.docker.io/v1/":{}}}\n'


def parser() -> argparse.ArgumentParser:
    command = argparse.ArgumentParser()
    subcommands = command.add_subparsers(dest="command", required=True)

    create = subcommands.add_parser("create")
    create.add_argument("--output-root", type=Path, default=Path("dist"))
    create.add_argument("--version-file", type=Path, default=Path("VERSION"))
    create.add_argument("--upstreams", type=Path, default=Path("UPSTREAMS.toml"))

    for name in ("load", "verify"):
        operation = subcommands.add_parser(name)
        operation.add_argument("bundle", type=Path)
    return command


def run(
    arguments: list[str],
    *,
    capture: bool = False,
    environment: dict[str, str] | None = None,
) -> str:
    completed = subprocess.run(
        arguments,
        check=True,
        env=environment,
        stdout=subprocess.PIPE if capture else None,
        text=True,
    )
    return completed.stdout if capture else ""


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def docker_environment(config_directory: Path) -> dict[str, str]:
    path = os.environ.get("PATH")
    if not path:
        raise ValueError("PATH is required to locate Nix-provided Docker tools")
    environment = {
        "PATH": path,
        "TMPDIR": os.environ.get("TMPDIR", "/tmp"),
        "DOCKER_CONFIG": os.fspath(config_directory),
    }
    for name in DOCKER_INPUTS:
        value = os.environ.get(name)
        if value is not None:
            environment[name] = value
    return environment


def docker_arguments(environment: dict[str, str], *arguments: str) -> list[str]:
    return ["docker", "--config", environment["DOCKER_CONFIG"], *arguments]


def clean_commit() -> str:
    status = run(
        ["git", "status", "--porcelain=v1", "--untracked-files=all"],
        capture=True,
    )
    if status:
        raise ValueError("bundle creation requires a clean Git worktree")
    commit = run(["git", "rev-parse", "HEAD"], capture=True).strip()
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("Git HEAD is not a full commit ID")
    return commit


def inspect_image(tag: str, environment: dict[str, str]) -> dict[str, object]:
    documents = json.loads(
        run(
            docker_arguments(environment, "image", "inspect", tag),
            capture=True,
            environment=environment,
        )
    )
    if len(documents) != 1:
        raise ValueError(f"Docker returned an invalid inspection for {tag}")
    document = documents[0]
    image_id = document.get("Id")
    if not isinstance(image_id, str) or not IMAGE_ID_PATTERN.fullmatch(image_id):
        raise ValueError(f"Docker returned an invalid image ID for {tag}")
    if document.get("Os") != "linux" or document.get("Architecture") != "amd64":
        raise ValueError(f"image {tag} is not linux/amd64")
    return document


def load_nix_image(attribute: str, environment: dict[str, str]) -> None:
    paths = run(
        ["nix", "build", "--no-link", "--print-out-paths", f".#{attribute}"],
        capture=True,
    ).splitlines()
    if len(paths) != 1:
        raise ValueError(f"Nix returned an invalid output for {attribute}")
    run(
        docker_arguments(environment, "load", "--input", paths[0]),
        environment=environment,
    )


def write_sboms(
    directory: Path,
    images: list[dict[str, str]],
    environment: dict[str, str],
) -> None:
    sbom_directory = directory / "sbom"
    sbom_directory.mkdir()
    licenses: dict[str, list[str]] = {}
    for image in images:
        role = image["role"]
        destination = sbom_directory / f"{role}.cdx.json"
        run(
            [
                "syft",
                "scan",
                "--quiet",
                f"docker:{image['tag']}",
                "--output",
                f"cyclonedx-json={destination}",
            ],
            environment=environment,
        )
        document = json.loads(destination.read_text(encoding="utf-8"))
        identifiers: set[str] = set()
        for component in document.get("components", []):
            for license_entry in component.get("licenses", []):
                license_value = license_entry.get("license", {})
                identifier = license_value.get("id") or license_value.get("name")
                if isinstance(identifier, str) and identifier:
                    identifiers.add(identifier)
        licenses[role] = sorted(identifiers)
    (directory / "licenses.json").write_text(
        json.dumps(licenses, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
        newline="\n",
    )


def create_bundle(args: argparse.Namespace, environment: dict[str, str]) -> None:
    if platform.machine() != "x86_64":
        raise ValueError("bundle creation currently supports only x86_64 builders")
    project = load_project_metadata(args.version_file, args.upstreams)
    version = project.product_version
    commit = clean_commit()
    image_tags = [
        {"role": "backup", "tag": project.backup_image},
        {"role": "proxy", "tag": project.proxy_image},
        {
            "role": "vaultwarden",
            "tag": project.vaultwarden_image,
            "source_digest": project.vaultwarden_digest,
        },
    ]

    load_nix_image("backup-image", environment)
    load_nix_image("proxy-image", environment)
    run(
        docker_arguments(environment, "pull", project.vaultwarden_source),
        environment=environment,
    )
    pulled = inspect_image(project.vaultwarden_source, environment)
    repo_digests = pulled.get("RepoDigests", [])
    if not isinstance(repo_digests, list) or not any(
        isinstance(value, str) and value.endswith("@" + project.vaultwarden_digest)
        for value in repo_digests
    ):
        raise ValueError("pulled Vaultwarden image does not expose the pinned digest")
    run(
        docker_arguments(
            environment,
            "tag",
            project.vaultwarden_source,
            project.vaultwarden_image,
        ),
        environment=environment,
    )

    for image in image_tags:
        inspected = inspect_image(image["tag"], environment)
        image["id"] = str(inspected["Id"])

    destination = args.output_root / f"vaultwarden-less-{version}-linux-amd64"
    if destination.exists():
        raise ValueError(f"bundle destination already exists: {destination}")
    args.output_root.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(
        prefix=f".{destination.name}.", dir=args.output_root
    ) as temporary_name:
        temporary = Path(temporary_name)
        images_tar = temporary / "images.tar"
        run(
            docker_arguments(
                environment,
                "save",
                "--output",
                os.fspath(images_tar),
                *(image["tag"] for image in image_tags),
            ),
            environment=environment,
        )
        checksum = sha256_file(images_tar)
        (temporary / "images.tar.sha256").write_text(
            f"{checksum}  images.tar\n", encoding="ascii", newline="\n"
        )
        metadata = {
            "bundle_schema_version": 1,
            "product_version": version,
            "git_commit": commit,
            "flake_lock_sha256": "sha256:" + sha256_file(Path("flake.lock")),
            "platform": "linux/amd64",
            "upstreams": project.upstreams,
            "images": image_tags,
        }
        (temporary / "bundle.json").write_text(
            json.dumps(metadata, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
            newline="\n",
        )
        run(
            [
                "git",
                "archive",
                "--format=tar",
                f"--output={temporary / 'source.tar'}",
                "HEAD",
            ]
        )
        run(
            [
                "git",
                "archive",
                "--format=tar",
                f"--output={temporary / 'licenses.tar'}",
                "HEAD",
                "LICENSE",
                "NOTICE.md",
                "licenses",
            ]
        )
        write_sboms(temporary, image_tags, environment)
        os.rename(temporary, destination)
    print(destination)


def read_bundle(path: Path) -> dict[str, object]:
    if not path.is_dir() or path.is_symlink():
        raise ValueError("bundle path must be a real directory")
    metadata = json.loads((path / "bundle.json").read_text(encoding="utf-8"))
    if (
        metadata.get("bundle_schema_version") != 1
        or metadata.get("platform") != "linux/amd64"
        or not isinstance(metadata.get("upstreams"), dict)
        or not isinstance(metadata.get("images"), list)
    ):
        raise ValueError("bundle metadata is invalid or unsupported")
    version = metadata.get("product_version")
    commit = metadata.get("git_commit")
    lock_hash = metadata.get("flake_lock_sha256")
    if (
        not isinstance(version, str)
        or not VERSION_PATTERN.fullmatch(version)
        or not isinstance(commit, str)
        or not re.fullmatch(r"[0-9a-f]{40}", commit)
        or not isinstance(lock_hash, str)
        or not IMAGE_ID_PATTERN.fullmatch(lock_hash)
    ):
        raise ValueError("bundle provenance is invalid")
    images = metadata["images"]
    if len(images) != 3:
        raise ValueError("bundle image set is incomplete")
    roles: set[str] = set()
    for image in images:
        if not isinstance(image, dict):
            raise TypeError("bundle image entry is invalid")
        role = image.get("role")
        tag = image.get("tag")
        image_id = image.get("id")
        tag_match = LOCAL_TAG_PATTERN.fullmatch(tag) if isinstance(tag, str) else None
        if (
            not isinstance(role, str)
            or role not in {"backup", "proxy", "vaultwarden"}
            or role in roles
            or tag_match is None
            or tag_match.group(1) != role
            or not isinstance(image_id, str)
            or not IMAGE_ID_PATTERN.fullmatch(image_id)
        ):
            raise ValueError("bundle image identity is invalid")
        roles.add(role)
        source_digest = image.get("source_digest")
        if role == "vaultwarden":
            if not isinstance(source_digest, str) or not IMAGE_ID_PATTERN.fullmatch(
                source_digest
            ):
                raise ValueError("Vaultwarden source digest is invalid")
        elif source_digest is not None:
            raise ValueError("unexpected source digest on local product image")
    return metadata


def verify_checksum(path: Path) -> None:
    sidecar = (path / "images.tar.sha256").read_text(encoding="ascii")
    expected = sidecar.removesuffix("\n").split("  ")
    if len(expected) != 2 or expected[1] != "images.tar":
        raise ValueError("image checksum sidecar is invalid")
    actual = sha256_file(path / "images.tar")
    if not re.fullmatch(r"[0-9a-f]{64}", expected[0]) or actual != expected[0]:
        raise ValueError("images.tar checksum mismatch")


def verify_loaded(path: Path, environment: dict[str, str]) -> None:
    metadata = read_bundle(path)
    verify_checksum(path)
    for image in metadata["images"]:
        tag = image.get("tag")
        expected_id = image.get("id")
        inspected = inspect_image(tag, environment)
        if inspected["Id"] != expected_id:
            raise ValueError(f"loaded image ID does not match bundle: {tag}")


def load_bundle(path: Path, environment: dict[str, str]) -> None:
    read_bundle(path)
    verify_checksum(path)
    run(
        docker_arguments(
            environment, "load", "--input", os.fspath(path / "images.tar")
        ),
        environment=environment,
    )
    verify_loaded(path, environment)


def main() -> int:
    args = parser().parse_args()
    try:
        with tempfile.TemporaryDirectory(
            prefix="vaultwarden-less-docker-config."
        ) as docker_config_name:
            docker_config = Path(docker_config_name)
            config = docker_config / "config.json"
            config.write_text(ANONYMOUS_DOCKER_CONFIG, encoding="utf-8", newline="\n")
            config.chmod(0o600)
            environment = docker_environment(docker_config)
            if args.command == "create":
                create_bundle(args, environment)
            elif args.command == "load":
                load_bundle(args.bundle, environment)
            else:
                verify_loaded(args.bundle, environment)
    except (
        json.JSONDecodeError,
        KeyError,
        OSError,
        subprocess.CalledProcessError,
        TypeError,
        ValueError,
        tomllib.TOMLDecodeError,
    ) as error:
        print(f"bundle {args.command} failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
