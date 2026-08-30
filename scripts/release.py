"""Build and verify a deterministic two-file release for an exact A/B pair."""

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

import check_release
import tomllib

FULL_COMMIT = re.compile(r"[0-9a-f]{40}")
RELEASE = re.compile(
    r"RELEASE[.](?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z)-g(?P<prefix>[0-9a-f]{12})"
)
IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}")
EVIDENCE_TIME = re.compile(
    r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}[.][0-9]{3}Z"
)
PRODUCTS = ("backup", "proxy", "vaultwarden")
IMAGE_ATTRIBUTES = {product: f"{product}-image" for product in PRODUCTS}
ENTRYPOINTS = {
    "backup": ["/bin/vaultwarden-less-backup"],
    "proxy": ["/bin/vaultwarden-less-proxy"],
    "vaultwarden": ["/bin/vaultwarden-less-vaultwarden"],
}
REPOSITORY = Path(__file__).resolve().parents[1]
UPSTREAMS = REPOSITORY / "UPSTREAMS.toml"


@dataclass(frozen=True)
class Identity:
    commit: str
    timestamp: int
    release: str
    created: str


def parser() -> argparse.ArgumentParser:
    command = argparse.ArgumentParser(description=__doc__)
    command.add_argument("from_commit")
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
    created = instant.strftime("%Y-%m-%dT%H:%M:%SZ")
    release_time = instant.strftime("%Y-%m-%dT%H-%M-%SZ")
    release = f"RELEASE.{release_time}-g{commit[:12]}"
    return Identity(
        commit=commit, timestamp=timestamp, release=release, created=created
    )


def release_pair(from_commit: str) -> tuple[str, Identity]:
    if FULL_COMMIT.fullmatch(from_commit) is None:
        raise ValueError("A commit must be full lowercase hexadecimal")
    identity = git_identity()
    if from_commit == identity.commit:
        raise ValueError("release commits must be distinct")
    resolved = run(
        ["git", "rev-parse", "--verify", from_commit + "^{commit}"]
    ).stdout.strip()
    if resolved != from_commit:
        raise ValueError("A identity does not resolve to the exact commit")
    return from_commit, identity


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


def image_entrypoint(document: dict[str, object]) -> list[str]:
    config = document.get("Config")
    entrypoint = config.get("Entrypoint") if isinstance(config, dict) else None
    if not isinstance(entrypoint, list) or not all(
        isinstance(value, str) for value in entrypoint
    ):
        raise ValueError("Docker image has an invalid entrypoint")
    return entrypoint


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
        if image_entrypoint(document) != ENTRYPOINTS[product]:
            raise ValueError("production image entrypoint is invalid")
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


def upstream_versions() -> tuple[str, str]:
    with UPSTREAMS.open("rb") as source:
        document = tomllib.load(source)
    try:
        restic = document["restic"]["source_tag"]
        vaultwarden = document["vaultwarden"]["source_tag"]
    except (KeyError, TypeError) as error:
        raise ValueError("upstream version audit record is invalid") from error
    if (
        not isinstance(restic, str)
        or not restic.startswith("v")
        or not isinstance(vaultwarden, str)
        or not vaultwarden
    ):
        raise ValueError("upstream version audit record is invalid")
    return restic.removeprefix("v"), vaultwarden


def image_version(reference: str, entrypoint: str, *arguments: str) -> str:
    result = run(
        [
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            "--read-only",
            "--user",
            "65534:65534",
            "--cap-drop",
            "ALL",
            "--entrypoint",
            entrypoint,
            reference,
            *arguments,
        ]
    )
    return result.stdout.strip()


def validate_upstream_versions(identity: Identity) -> None:
    expected_restic, expected_vaultwarden = upstream_versions()
    restic_output = image_version(
        f"vaultwarden-less-backup:{identity.release}",
        "/bin/restic",
        "version",
        "--json",
    )
    try:
        restic_document = json.loads(restic_output)
    except json.JSONDecodeError as error:
        raise ValueError("Restic image version output is invalid") from error
    if (
        not isinstance(restic_document, dict)
        or restic_document.get("version") != expected_restic
    ):
        raise ValueError("Restic image version does not match UPSTREAMS.toml")
    vaultwarden_output = image_version(
        f"vaultwarden-less-vaultwarden:{identity.release}",
        "/bin/vaultwarden",
        "--version",
    )
    vaultwarden_lines = vaultwarden_output.splitlines()
    if (
        not vaultwarden_lines
        or vaultwarden_lines[0] != f"Vaultwarden {expected_vaultwarden}"
    ):
        raise ValueError("Vaultwarden image version does not match UPSTREAMS.toml")


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


def validate_candidate(from_commit: str, to_commit: str) -> tuple[Path, str]:
    evidence_root = check_release.EVIDENCE_ROOT
    pair = evidence_root / f"{from_commit}..{to_commit}"
    candidate = pair / "candidate.json"
    try:
        relative = candidate.relative_to(REPOSITORY)
    except ValueError as error:
        raise ValueError("release evidence escaped the repository") from error
    check_release.validate_directory(evidence_root.parent, private=False)
    check_release.validate_directory(evidence_root, private=True)
    check_release.validate_directory(pair, private=True)
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
        "from_commit",
        "to_commit",
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
        document["from_commit"] != from_commit
        or document["to_commit"] != to_commit
        or document["gates"] != list(check_release.EVIDENCE_GATES)
        or document["status"] != "passed"
    ):
        raise ValueError("candidate evidence does not match the release pair")
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
    )
    absolute = absolute.absolute()
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


def build_release(directory: Path, identity: Identity) -> None:
    if any(directory.iterdir()):
        raise ValueError("release build directory is not empty")
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
            raise ValueError("immutable image reference names different content")
        run(["docker", "image", "tag", local, immutable])
    expected = references(identity.release)
    validate_images(expected, identity)
    raw_archive = directory / "images.tar"
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
    canonicalize_archive(raw_archive, directory / "images.tar.gz", expected)
    if archive_references(directory / "images.tar.gz") != expected:
        raise ValueError("canonical Docker image references are not exact")
    raw_archive.unlink()
    write_checksum(directory)
    verified_archive, verified_references = verify_directory(directory)
    if (
        verified_archive != directory / "images.tar.gz"
        or verified_references != expected
    ):
        raise ValueError("release build verification failed")


def verify_repeatable(first: Path, second: Path) -> None:
    verify_directory(first)
    verify_directory(second)
    for name in ("images.tar.gz", "SHA256SUMS"):
        if (first / name).read_bytes() != (second / name).read_bytes():
            raise ValueError("independent release builds are not byte-identical")


def load_and_validate(directory: Path, identity: Identity) -> None:
    archive, archived_references = verify_directory(directory)
    expected = references(identity.release)
    if archived_references != expected or any(
        reference.startswith("vaultwarden-less-moto:")
        for reference in archived_references
    ):
        raise ValueError("release image references are not exact")
    run(["docker", "image", "load", "--input", os.fspath(archive)])
    validate_images(expected, identity)
    validate_upstream_versions(identity)


def write_artifact_evidence(
    candidate: Path,
    candidate_hash: str,
    from_commit: str,
    identity: Identity,
    release_directory: Path,
    now: dt.datetime,
) -> None:
    result = {
        "candidate_sha256": candidate_hash,
        "from_commit": from_commit,
        "to_commit": identity.commit,
        "release": identity.release,
        "files": {
            name: "sha256:" + sha256_file(release_directory / name)
            for name in ("SHA256SUMS", "images.tar.gz")
        },
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
    from_commit: str,
    output_directory: Path,
    now: dt.datetime | None = None,
) -> Path:
    source, identity = release_pair(from_commit)
    output = validate_output(output_directory)
    candidate, candidate_hash = validate_candidate(source, identity.commit)
    temporary: list[Path] = []
    try:
        for build_number in (1, 2):
            temporary.append(
                Path(
                    tempfile.mkdtemp(
                        prefix=f".{output.name}.build-{build_number}.",
                        dir=output.parent,
                    )
                )
            )
        build_release(temporary[0], identity)
        build_release(temporary[1], identity)
        verify_repeatable(temporary[0], temporary[1])
        load_and_validate(temporary[0], identity)
        final_source, final_identity = release_pair(from_commit)
        if final_source != source or final_identity != identity:
            raise ValueError("Git identity changed during release creation")
        final_candidate, final_candidate_hash = validate_candidate(
            source, identity.commit
        )
        if final_candidate != candidate or final_candidate_hash != candidate_hash:
            raise ValueError("candidate evidence changed during release creation")
        write_artifact_evidence(
            candidate,
            candidate_hash,
            source,
            identity,
            temporary[0],
            now if now is not None else dt.datetime.now(dt.UTC),
        )
        final_source, final_identity = release_pair(from_commit)
        if final_source != source or final_identity != identity:
            raise ValueError("Git identity changed before release publication")
        rename_no_replace(temporary[0], output)
        temporary.pop(0)
        sync_directory(output.parent)
        return output
    finally:
        for path in temporary:
            shutil.rmtree(path, ignore_errors=True)


def main(arguments: list[str] | None = None) -> int:
    parsed = parser().parse_args(arguments)
    try:
        output = create_release(parsed.from_commit, parsed.output_directory)
        print(output)
    except (
        check_release.ReleaseCheckError,
        OSError,
        subprocess.SubprocessError,
        tarfile.TarError,
        TypeError,
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
