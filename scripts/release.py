"""Create, verify, and load deterministic clean-commit image releases."""

from __future__ import annotations

import argparse
import copy
import ctypes
import datetime as dt
import errno
import gzip
import hashlib
import io
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

FULL_COMMIT = re.compile(r"[0-9a-f]{40}")
RELEASE = re.compile(
    r"RELEASE[.](?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z)-g(?P<prefix>[0-9a-f]{12})"
)
IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}")
PRODUCTS = ("backup", "proxy", "vaultwarden")
IMAGE_ATTRIBUTES = {product: f"{product}-image" for product in PRODUCTS}


@dataclass(frozen=True)
class Identity:
    commit: str
    timestamp: int
    release: str
    created: str


def parser() -> argparse.ArgumentParser:
    command = argparse.ArgumentParser()
    subcommands = command.add_subparsers(dest="command", required=True)
    create = subcommands.add_parser("create")
    create.add_argument("--output-root", required=True, type=Path)
    load = subcommands.add_parser("load")
    load.add_argument("release_directory", type=Path)
    return command


def run(
    arguments: list[str], *, check: bool = True
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        arguments,
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
    created = instant.strftime("%Y-%m-%dT%H:%M:%SZ")
    release_time = instant.strftime("%Y-%m-%dT%H-%M-%SZ")
    release = f"RELEASE.{release_time}-g{commit[:12]}"
    return Identity(
        commit=commit, timestamp=timestamp, release=release, created=created
    )


def references(release: str) -> list[str]:
    return [
        reference
        for product in PRODUCTS
        for reference in (
            f"vaultwarden-less-{product}:local",
            f"vaultwarden-less-{product}:{release}",
        )
    ]


def build_image(attribute: str) -> Path:
    completed = run(
        [
            "nix",
            "build",
            "--no-link",
            "--print-out-paths",
            f".#{attribute}",
        ]
    )
    paths = [Path(line) for line in completed.stdout.splitlines() if line]
    if len(paths) != 1 or not paths[0].is_file():
        raise ValueError(f"Nix returned an invalid output for {attribute}")
    return paths[0]


def inspect(reference: str, *, required: bool = True) -> dict[str, object] | None:
    completed = run(["docker", "image", "inspect", reference], check=False)
    if completed.returncode != 0:
        if required:
            raise ValueError(f"required image reference {reference} is unavailable")
        return None
    try:
        documents = json.loads(completed.stdout)
    except json.JSONDecodeError as error:
        raise ValueError("Docker image inspection returned invalid JSON") from error
    if (
        not isinstance(documents, list)
        or len(documents) != 1
        or not isinstance(documents[0], dict)
    ):
        raise ValueError("Docker image inspection returned an invalid document")
    return documents[0]


def image_id(document: dict[str, object]) -> str:
    value = document.get("Id")
    if not isinstance(value, str) or not IMAGE_ID.fullmatch(value):
        raise ValueError("Docker image has an invalid ID")
    return value


def image_labels(document: dict[str, object]) -> dict[str, str]:
    config = document.get("Config")
    labels = config.get("Labels") if isinstance(config, dict) else None
    if not isinstance(labels, dict) or not all(
        isinstance(key, str) and isinstance(value, str) for key, value in labels.items()
    ):
        raise ValueError("Docker image has invalid labels")
    return labels


def validate_images(expected_references: list[str], identity: Identity | None) -> None:
    by_product: dict[str, set[str]] = {product: set() for product in PRODUCTS}
    revisions: set[str] = set()
    releases: set[str] = set()
    created_values: set[str] = set()
    for reference in expected_references:
        document = inspect(reference)
        if document is None:
            raise AssertionError("required inspect unexpectedly returned None")
        product = next(
            product
            for product in PRODUCTS
            if reference.startswith(f"vaultwarden-less-{product}:")
        )
        by_product[product].add(image_id(document))
        labels = image_labels(document)
        revisions.add(labels.get("org.opencontainers.image.revision", ""))
        releases.add(labels.get("org.opencontainers.image.version", ""))
        created_values.add(labels.get("org.opencontainers.image.created", ""))
    if any(len(ids) != 1 for ids in by_product.values()):
        raise ValueError("local and immutable image references do not share an ID")
    if len({next(iter(ids)) for ids in by_product.values()}) != len(PRODUCTS):
        raise ValueError("production images do not have three distinct IDs")
    if identity is not None:
        if (
            revisions != {identity.commit}
            or releases != {identity.release}
            or created_values != {identity.created}
        ):
            raise ValueError("production image labels do not match the clean commit")
    else:
        if len(revisions) != 1 or len(releases) != 1 or len(created_values) != 1:
            raise ValueError("loaded production image labels disagree")
        revision = next(iter(revisions))
        release = next(iter(releases))
        created = next(iter(created_values))
        match = RELEASE.fullmatch(release)
        if (
            not FULL_COMMIT.fullmatch(revision)
            or match is None
            or not revision.startswith(match.group("prefix"))
        ):
            raise ValueError("loaded production release identity is invalid")
        release_date = match.group("date")
        expected_created = (
            release_date[:10] + "T" + release_date[11:19].replace("-", ":") + "Z"
        )
        if created != expected_created:
            raise ValueError("loaded production created label is invalid")


def archive_references(path: Path) -> list[str]:
    with tarfile.open(path, "r:*") as archive:
        try:
            member = archive.getmember("manifest.json")
        except KeyError as error:
            raise ValueError("Docker image archive has no manifest.json") from error
        source = archive.extractfile(member)
        if source is None:
            raise ValueError("Docker image archive manifest is not a file")
        try:
            manifest = json.load(source)
        except json.JSONDecodeError as error:
            raise ValueError("Docker image archive manifest is invalid") from error
    if not isinstance(manifest, list):
        raise TypeError("Docker image archive manifest is not an array")
    result: list[str] = []
    for entry in manifest:
        tags = entry.get("RepoTags") if isinstance(entry, dict) else None
        if not isinstance(tags, list) or not all(isinstance(tag, str) for tag in tags):
            raise ValueError("Docker image archive contains invalid references")
        result.extend(tags)
    if len(result) != len(set(result)):
        raise ValueError("Docker image archive contains duplicate references")
    return result


def safe_member(member: tarfile.TarInfo) -> None:
    path = PurePosixPath(member.name)
    if path.is_absolute() or ".." in path.parts or member.name in ("", "."):
        raise ValueError("Docker image archive contains an unsafe path")
    if not member.isfile() and not member.isdir():
        raise ValueError("Docker image archive contains a special entry")


def canonical_json(value: object) -> bytes:
    return json.dumps(
        value, ensure_ascii=True, separators=(",", ":"), sort_keys=True
    ).encode("ascii")


def canonical_manifest(data: bytes, expected_references: list[str]) -> bytes:
    try:
        document = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError("Docker image archive manifest is invalid") from error
    if not isinstance(document, list):
        raise TypeError("Docker image archive manifest is not an array")
    positions = {
        reference: index for index, reference in enumerate(expected_references)
    }
    normalized: list[dict[str, object]] = []
    observed: list[str] = []
    for entry in document:
        tags = entry.get("RepoTags") if isinstance(entry, dict) else None
        if (
            not isinstance(tags, list)
            or not tags
            or not all(isinstance(tag, str) and tag in positions for tag in tags)
        ):
            raise ValueError("Docker image archive contains invalid references")
        ordered_tags = sorted(tags, key=positions.__getitem__)
        observed.extend(ordered_tags)
        normalized_entry = dict(entry)
        normalized_entry["RepoTags"] = ordered_tags
        normalized.append(normalized_entry)
    if len(observed) != len(set(observed)) or set(observed) != set(expected_references):
        raise ValueError("Docker image archive references are not exact")
    normalized.sort(key=lambda entry: positions[entry["RepoTags"][0]])
    return canonical_json(normalized)


def canonical_index(data: bytes, expected_references: list[str]) -> bytes:
    try:
        document = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError("Docker OCI image index is invalid") from error
    manifests = document.get("manifests") if isinstance(document, dict) else None
    if not isinstance(manifests, list):
        raise TypeError("Docker OCI image index has no manifests array")
    positions = {
        reference: index for index, reference in enumerate(expected_references)
    }
    normalized: list[tuple[int, dict[str, object]]] = []
    observed: list[str] = []
    for descriptor in manifests:
        annotations = (
            descriptor.get("annotations") if isinstance(descriptor, dict) else None
        )
        image_name = (
            annotations.get("io.containerd.image.name")
            if isinstance(annotations, dict)
            else None
        )
        reference = image_name.rsplit("/", 1)[-1] if isinstance(image_name, str) else ""
        if reference not in positions:
            raise ValueError("Docker OCI image index contains an unexpected reference")
        observed.append(reference)
        normalized.append((positions[reference], descriptor))
    if len(observed) != len(set(observed)) or set(observed) != set(expected_references):
        raise ValueError("Docker OCI image index references are not exact")
    normalized.sort(key=lambda item: item[0])
    result = dict(document)
    result["manifests"] = [descriptor for _, descriptor in normalized]
    return canonical_json(result)


def canonicalize_archive(
    source_path: Path,
    destination_path: Path,
    expected_references: list[str],
) -> None:
    with (
        tarfile.open(source_path, "r:*") as source,
        destination_path.open("xb") as compressed,
    ):
        members = sorted(source.getmembers(), key=lambda item: item.name.encode())
        names = [member.name for member in members]
        if len(names) != len(set(names)) or "manifest.json" not in names:
            raise ValueError("Docker image archive member names are not exact")
        for member in members:
            safe_member(member)
        with (
            gzip.GzipFile(
                filename="", mode="wb", fileobj=compressed, compresslevel=9, mtime=0
            ) as gzip_output,
            tarfile.open(
                fileobj=gzip_output, mode="w|", format=tarfile.PAX_FORMAT
            ) as destination,
        ):
            for member in members:
                normalized = copy.copy(member)
                normalized.uid = 0
                normalized.gid = 0
                normalized.uname = ""
                normalized.gname = ""
                normalized.mtime = 0
                normalized.pax_headers = {}
                payload = source.extractfile(member) if member.isfile() else None
                if member.isfile() and payload is None:
                    raise ValueError("Docker image archive member is unreadable")
                if payload is not None and member.name in {
                    "manifest.json",
                    "index.json",
                    "oci-layout",
                }:
                    data = payload.read()
                    if member.name == "manifest.json":
                        data = canonical_manifest(data, expected_references)
                    elif member.name == "index.json":
                        data = canonical_index(data, expected_references)
                    else:
                        try:
                            data = canonical_json(json.loads(data))
                        except (UnicodeDecodeError, json.JSONDecodeError) as error:
                            raise ValueError("Docker OCI layout is invalid") from error
                    normalized.size = len(data)
                    payload = io.BytesIO(data)
                destination.addfile(normalized, payload)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_checksum(directory: Path) -> None:
    archive = directory / "images.tar.gz"
    checksum = directory / "SHA256SUMS"
    checksum.write_text(
        f"{sha256_file(archive)}  images.tar.gz\n", encoding="ascii", newline="\n"
    )
    checksum.chmod(0o644)


def verify_directory(directory: Path) -> tuple[Path, list[str]]:
    information = directory.lstat()
    if stat.S_ISLNK(information.st_mode) or not stat.S_ISDIR(information.st_mode):
        raise ValueError("release path must be a real directory")
    if {entry.name for entry in directory.iterdir()} != {"images.tar.gz", "SHA256SUMS"}:
        raise ValueError("release directory must contain exactly two files")
    archive = directory / "images.tar.gz"
    checksum = directory / "SHA256SUMS"
    for path in (archive, checksum):
        information = path.lstat()
        if stat.S_ISLNK(information.st_mode) or not stat.S_ISREG(information.st_mode):
            raise ValueError("release contains a non-regular file")
    expected_line = f"{sha256_file(archive)}  images.tar.gz\n"
    if checksum.read_text(encoding="ascii") != expected_line:
        raise ValueError("release checksum is invalid")
    return archive, archive_references(archive)


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


def create_release(output_root: Path) -> None:
    identity = git_identity()
    if output_root.exists() or output_root.is_symlink():
        raise FileExistsError(output_root)
    parent = output_root.parent if output_root.parent != Path("") else Path(".")
    parent_information = parent.lstat()
    if stat.S_ISLNK(parent_information.st_mode) or not stat.S_ISDIR(
        parent_information.st_mode
    ):
        raise ValueError("release output parent must be a real directory")
    temporary: Path | None = Path(
        tempfile.mkdtemp(prefix=f".{output_root.name}.", dir=parent)
    )
    try:
        for product in PRODUCTS:
            archive = build_image(IMAGE_ATTRIBUTES[product])
            run(["docker", "image", "load", "--input", os.fspath(archive)])
        for product in PRODUCTS:
            local = f"vaultwarden-less-{product}:local"
            immutable = f"vaultwarden-less-{product}:{identity.release}"
            local_document = inspect(local)
            if local_document is None:
                raise AssertionError("local image unexpectedly missing")
            existing = inspect(immutable, required=False)
            if existing is not None and image_id(existing) != image_id(local_document):
                raise ValueError(
                    "immutable image reference already names different content"
                )
            run(["docker", "image", "tag", local, immutable])
        expected = references(identity.release)
        validate_images(expected, identity)
        raw_archive = temporary / "images.tar"
        run(
            [
                "docker",
                "image",
                "save",
                "--output",
                os.fspath(raw_archive),
                *expected,
            ]
        )
        archived = archive_references(raw_archive)
        if len(archived) != len(expected) or set(archived) != set(expected):
            raise ValueError("Docker image archive references are not exact")
        canonicalize_archive(raw_archive, temporary / "images.tar.gz", expected)
        if archive_references(temporary / "images.tar.gz") != expected:
            raise ValueError("canonical Docker image references are not exact")
        raw_archive.unlink()
        write_checksum(temporary)
        if git_identity() != identity:
            raise ValueError("Git identity changed during release creation")
        rename_no_replace(temporary, output_root)
        temporary = None
    finally:
        if temporary is not None:
            shutil.rmtree(temporary, ignore_errors=True)


def load_release(directory: Path) -> None:
    archive, archived_references = verify_directory(directory)
    immutable = [
        reference
        for reference in archived_references
        if not reference.endswith(":local")
    ]
    if len(immutable) != len(PRODUCTS):
        raise ValueError("release does not contain three immutable references")
    releases = {reference.rsplit(":", 1)[1] for reference in immutable}
    if len(releases) != 1:
        raise ValueError("release contains inconsistent immutable tags")
    release = next(iter(releases))
    expected = references(release)
    if archived_references != expected:
        raise ValueError("release image references are not exact")
    run(["docker", "image", "load", "--input", os.fspath(archive)])
    validate_images(expected, None)


def main() -> int:
    arguments = parser().parse_args()
    try:
        if arguments.command == "create":
            create_release(arguments.output_root)
            print(arguments.output_root)
        else:
            load_release(arguments.release_directory)
            print(arguments.release_directory)
    except (
        OSError,
        subprocess.SubprocessError,
        tarfile.TarError,
        TypeError,
        ValueError,
    ) as error:
        print(
            f"release {arguments.command} failed: {type(error).__name__}",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
