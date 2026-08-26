#!/usr/bin/env python3
"""Create missing private deployment paths without replacing existing data."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import secrets
import stat
import sys


BACKUP_CONFIG = """schema_version: 1

targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/vaultwarden
    restic_options:
      s3.bucket-lookup: path
      s3.region: ap-singapore

notifiers: []
"""

CREDENTIALS = """schema_version: 1
targets:
  primary:
    AWS_ACCESS_KEY_ID: replace-me
    AWS_SECRET_ACCESS_KEY: replace-me
"""

VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--version-file", required=True, type=Path)
    parser.add_argument("--uid", type=int, default=os.getuid())
    parser.add_argument("--gid", type=int, default=os.getgid())
    return parser.parse_args()


def require_real_directory_chain(path: Path) -> None:
    current = Path(path.anchor)
    for component in path.parts[1:]:
        current /= component
        info = current.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise ValueError(f"deployment path contains a symlink or non-directory: {current}")


def require_real_directory(path: Path, mode: int, uid: int, gid: int) -> None:
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise ValueError(f"{path} must be a real directory")
    if stat.S_IMODE(info.st_mode) != mode:
        raise ValueError(f"{path} must have mode {mode:04o}")
    if info.st_uid != uid or info.st_gid != gid:
        raise ValueError(f"{path} must be owned by {uid}:{gid}")


def ensure_directory(path: Path, uid: int, gid: int) -> None:
    try:
        path.mkdir(mode=0o700)
    except FileExistsError:
        pass
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

        directories = (
            root / "data",
            root / "staging",
            root / "cache",
            root / "restore",
            root / "config",
            root / "credentials",
            root / "credentials" / "backup",
            root / "credentials" / "maintenance",
            root / "credentials" / "common",
        )
        for directory in directories:
            ensure_directory(directory, args.uid, args.gid)

        vaultwarden_template = root / ".env.vaultwarden.template"
        if not vaultwarden_template.is_file() or vaultwarden_template.is_symlink():
            raise ValueError(".env.vaultwarden.template must be a regular file")

        create_private_file(
            root / ".env",
            (
                f"DEPLOY_UID={args.uid}\n"
                f"DEPLOY_GID={args.gid}\n"
                f"DEPLOY_ROOT={root}\n"
                f"PRODUCT_VERSION={version}\n"
                f"PRODUCT_IMAGE=ghcr.io/hellodword/vaultwarden-less:{version}\n"
                f"VAULTWARDEN_ENV_FILE={root / '.env.vaultwarden'}\n"
                "LISTEN_PORT=8080\n"
                "DRILL_PORT=8081\n"
                "DRILL_DOMAIN=http://127.0.0.1:8081\n"
                f"DRILL_RECOVERY_PATH={root / 'restore' / 'drill'}\n"
            ),
            args.uid,
            args.gid,
        )
        create_private_file(
            root / ".env.vaultwarden",
            vaultwarden_template.read_text(encoding="utf-8"),
            args.uid,
            args.gid,
        )
        create_private_file(
            root / "config" / "backup.yaml",
            BACKUP_CONFIG,
            args.uid,
            args.gid,
        )
        create_private_file(
            root / "credentials" / "backup" / "credentials.yaml",
            CREDENTIALS,
            args.uid,
            args.gid,
        )
        create_private_file(
            root / "credentials" / "maintenance" / "credentials.yaml",
            CREDENTIALS,
            args.uid,
            args.gid,
        )
        create_private_file(
            root / "credentials" / "common" / "restic-password",
            secrets.token_urlsafe(48) + "\n",
            args.uid,
            args.gid,
        )
    except (OSError, ValueError) as error:
        print(f"init failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
