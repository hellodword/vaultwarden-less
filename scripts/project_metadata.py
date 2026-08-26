"""Read audited project metadata and derive local image identities."""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path

import tomllib

VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
IMAGE_DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}")
IMAGE_REPOSITORY_PATTERN = re.compile(
    r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+"
)


@dataclass(frozen=True)
class ProjectMetadata:
    product_version: str
    backup_image: str
    proxy_image: str
    vaultwarden_version: str
    vaultwarden_repository: str
    vaultwarden_digest: str
    vaultwarden_source: str
    vaultwarden_image: str
    moto_version: str
    moto_image: str
    upstreams: dict[str, object]


def require_version(value: object, label: str) -> str:
    if not isinstance(value, str) or not VERSION_PATTERN.fullmatch(value):
        raise ValueError(f"{label} must contain one MAJOR.MINOR.PATCH value")
    return value


def require_image_digest(value: object, label: str) -> str:
    if not isinstance(value, str) or not IMAGE_DIGEST_PATTERN.fullmatch(value):
        raise ValueError(f"{label} must contain one sha256 image digest")
    return value


def vaultwarden_local_image(version: str, digest: str) -> str:
    version = require_version(version, "Vaultwarden version")
    digest = require_image_digest(digest, "Vaultwarden image digest")
    return f"vaultwarden-less-vaultwarden:{version}-{digest[7:19]}"


def load_project_metadata(version_file: Path, upstreams_file: Path) -> ProjectMetadata:
    product_version = require_version(
        version_file.read_text(encoding="utf-8").strip(), "VERSION"
    )
    upstreams = tomllib.loads(upstreams_file.read_text(encoding="utf-8"))
    vaultwarden = upstreams["vaultwarden"]
    moto = upstreams["moto"]
    if not isinstance(vaultwarden, dict) or not isinstance(moto, dict):
        raise TypeError("UPSTREAMS.toml component metadata must be tables")

    vaultwarden_version = require_version(vaultwarden["version"], "vaultwarden.version")
    vaultwarden_digest = require_image_digest(
        vaultwarden["image_digest"], "vaultwarden.image_digest"
    )
    vaultwarden_repository = vaultwarden["image_repository"]
    if not isinstance(
        vaultwarden_repository, str
    ) or not IMAGE_REPOSITORY_PATTERN.fullmatch(vaultwarden_repository):
        raise ValueError("vaultwarden.image_repository is invalid")
    moto_version = require_version(moto["version"], "moto.version")

    return ProjectMetadata(
        product_version=product_version,
        backup_image=f"vaultwarden-less-backup:{product_version}",
        proxy_image=f"vaultwarden-less-proxy:{product_version}",
        vaultwarden_version=vaultwarden_version,
        vaultwarden_repository=vaultwarden_repository,
        vaultwarden_digest=vaultwarden_digest,
        vaultwarden_source=(
            f"{vaultwarden_repository}:{vaultwarden_version}@{vaultwarden_digest}"
        ),
        vaultwarden_image=vaultwarden_local_image(
            vaultwarden_version, vaultwarden_digest
        ),
        moto_version=moto_version,
        moto_image=f"vaultwarden-less-moto:{moto_version}",
        upstreams=upstreams,
    )
