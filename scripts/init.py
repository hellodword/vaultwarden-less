"""Create missing private deployment paths without replacing existing data."""

from __future__ import annotations

import argparse
import os
import re
import secrets
import stat
import sys
from pathlib import Path

import tomllib

BACKUP_CONFIG = """schema_version: 2

targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/vaultwarden
    restic_options:
      s3.bucket-lookup: path
      s3.region: ap-singapore

notifiers: []
"""

ROLE_CREDENTIALS = """schema_version: 2
targets:
  primary:
    AWS_ACCESS_KEY_ID: replace-me
    AWS_SECRET_ACCESS_KEY: replace-me
"""

NOTIFIER_CREDENTIALS = """schema_version: 2
notifiers: {}
"""

VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
IMAGE_DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--version-file", required=True, type=Path)
    parser.add_argument("--upstreams", required=True, type=Path)
    parser.add_argument("--vaultwarden-template", required=True, type=Path)
    parser.add_argument("--uid", type=int, default=os.getuid())
    parser.add_argument("--gid", type=int, default=os.getgid())
    return parser.parse_args()


def require_real_directory_chain(path: Path) -> None:
    current = Path(path.anchor)
    for component in path.parts[1:]:
        current /= component
        info = current.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise ValueError(
                f"deployment path contains a symlink or non-directory: {current}"
            )


def require_real_directory(path: Path, mode: int, uid: int, gid: int) -> None:
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise ValueError(f"{path} must be a real directory")
    if stat.S_IMODE(info.st_mode) != mode:
        raise ValueError(f"{path} must have mode {mode:04o}")
    if info.st_uid != uid or info.st_gid != gid:
        raise ValueError(f"{path} must be owned by {uid}:{gid}")


def ensure_directory(path: Path, uid: int, gid: int) -> None:
    created = False
    try:
        path.mkdir(mode=0o700)
        created = True
    except FileExistsError:
        pass
    if created:
        os.chown(path, uid, gid, follow_symlinks=False)
    require_real_directory(path, 0o700, uid, gid)


def require_private_file(path: Path, uid: int, gid: int) -> None:
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise ValueError(f"{path} must be a non-symlink regular file")
    if stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError(f"{path} must have mode 0600")
    if info.st_uid != uid or info.st_gid != gid:
        raise ValueError(f"{path} must be owned by {uid}:{gid}")


def create_private_file(path: Path, content: str, uid: int, gid: int) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags, 0o600)
    except FileExistsError:
        require_private_file(path, uid, gid)
        return
    try:
        os.fchmod(descriptor, 0o600)
        os.fchown(descriptor, uid, gid)
        data = content.encode("utf-8")
        offset = 0
        while offset < len(data):
            offset += os.write(descriptor, data[offset:])
        os.fsync(descriptor)
    except OSError:
        os.close(descriptor)
        path.unlink(missing_ok=True)
        raise
    else:
        os.close(descriptor)
    require_private_file(path, uid, gid)


def read_upstreams(path: Path) -> tuple[str, str]:
    data = tomllib.loads(path.read_text(encoding="utf-8"))
    version = data["vaultwarden"]["version"]
    digest = data["vaultwarden"]["image_digest"]
    if not VERSION_PATTERN.fullmatch(version) or not IMAGE_DIGEST_PATTERN.fullmatch(
        digest
    ):
        raise ValueError("UPSTREAMS.toml has invalid Vaultwarden image metadata")
    return version, digest


def main() -> int:
    args = parse_args()
    try:
        root = args.root.absolute()
        if root == Path(root.anchor):
            raise ValueError("deployment root must not be the filesystem root")
        require_real_directory_chain(root)
        version = args.version_file.read_text(encoding="utf-8").strip()
        if not VERSION_PATTERN.fullmatch(version):
            raise ValueError("VERSION must contain one MAJOR.MINOR.PATCH value")
        if "\n" in os.fspath(root) or "\r" in os.fspath(root):
            raise ValueError("deployment root contains a line break")
        vaultwarden_version, vaultwarden_digest = read_upstreams(args.upstreams)
        template = args.vaultwarden_template
        if not template.is_file() or template.is_symlink():
            raise ValueError("Vaultwarden environment template must be a regular file")

        directories = (
            root / "data",
            root / "staging",
            root / "cache",
            root / "restore",
            root / "config",
            root / "config" / "backup",
            root / "config" / "vaultwarden",
            root / "credentials",
            root / "credentials" / "backup",
            root / "credentials" / "maintenance",
            root / "credentials" / "restic",
            root / "credentials" / "notifiers",
        )
        for directory in directories:
            ensure_directory(directory, args.uid, args.gid)

        vaultwarden_local_image = (
            f"vaultwarden-less-vaultwarden:{vaultwarden_version}-"
            f"{vaultwarden_digest.removeprefix('sha256:')[:12]}"
        )
        create_private_file(
            root / ".env",
            (
                f"DEPLOY_UID={args.uid}\n"
                f"DEPLOY_GID={args.gid}\n"
                f"DEPLOY_ROOT={root}\n"
                f"PRODUCT_VERSION={version}\n"
                f"BACKUP_IMAGE=vaultwarden-less-backup:{version}\n"
                f"PROXY_IMAGE=vaultwarden-less-proxy:{version}\n"
                f"VAULTWARDEN_IMAGE={vaultwarden_local_image}\n"
                f"VAULTWARDEN_ENV_FILE={root / 'config' / 'vaultwarden' / 'vaultwarden.env'}\n"
                "LISTEN_PORT=8080\n"
                "DRILL_PORT=8081\n"
                "DRILL_DOMAIN=http://127.0.0.1:8081\n"
                f"DRILL_RECOVERY_PATH={root / 'restore' / 'prepared'}\n"
            ),
            args.uid,
            args.gid,
        )
        create_private_file(
            root / "config" / "vaultwarden" / "vaultwarden.env",
            template.read_text(encoding="utf-8"),
            args.uid,
            args.gid,
        )
        create_private_file(
            root / "config" / "backup" / "backup.yaml",
            BACKUP_CONFIG,
            args.uid,
            args.gid,
        )
        for role in ("backup", "maintenance"):
            create_private_file(
                root / "credentials" / role / "credentials.yaml",
                ROLE_CREDENTIALS,
                args.uid,
                args.gid,
            )
        create_private_file(
            root / "credentials" / "restic" / "primary",
            secrets.token_urlsafe(48) + "\n",
            args.uid,
            args.gid,
        )
        create_private_file(
            root / "credentials" / "notifiers" / "credentials.yaml",
            NOTIFIER_CREDENTIALS,
            args.uid,
            args.gid,
        )
    except (
        KeyError,
        OSError,
        TypeError,
        ValueError,
        tomllib.TOMLDecodeError,
    ) as error:
        print(f"init failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
