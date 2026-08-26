"""Initialize and operate one explicit vaultwarden-less deployment."""

from __future__ import annotations

import argparse
import os
import re
import secrets
import stat
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

import yaml
from project_metadata import load_project_metadata

SOURCE_ROOT = Path(__file__).resolve().parents[1]
COMPOSE_TEMPLATE = SOURCE_ROOT / "deploy" / "compose.template.yml"
VAULTWARDEN_TEMPLATE = SOURCE_ROOT / "deploy" / "vaultwarden.env.template"
CONFIG_KEYS = {
    "schema_version",
    "runtime_uid",
    "runtime_gid",
    "listen_port",
    "drill_port",
}
MAX_ID = (1 << 32) - 2
DEPLOYMENT_NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
TOKEN_PATTERN = re.compile(r"@@[A-Z0-9_]+@@")
DOCKER_INPUTS = (
    "DOCKER_HOST",
    "DOCKER_TLS_VERIFY",
    "DOCKER_CERT_PATH",
    "DOCKER_API_VERSION",
)
PRODUCTION_PROJECT = "vaultwarden-less"
DRILL_PROJECT = "vaultwarden-less-drill"
ANONYMOUS_DOCKER_CONFIG = '{"auths":{"https://index.docker.io/v1/":{}}}\n'

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


class UniqueKeyLoader(yaml.SafeLoader):
    """Safe YAML loader that rejects duplicate mapping keys."""


def construct_unique_mapping(
    loader: UniqueKeyLoader, node: yaml.MappingNode, deep: bool = False
) -> dict[object, object]:
    loader.flatten_mapping(node)
    result: dict[object, object] = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in result:
            raise ValueError(f"duplicate YAML key: {key}")
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


UniqueKeyLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_unique_mapping
)


@dataclass(frozen=True)
class DeploymentConfig:
    schema_version: int
    runtime_uid: int
    runtime_gid: int
    listen_port: int
    drill_port: int


def parse_arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, default=SOURCE_ROOT)
    subcommands = parser.add_subparsers(dest="action", required=True)

    initialize = subcommands.add_parser("init")
    initialize.add_argument("--uid", required=True, type=int)
    initialize.add_argument("--gid", required=True, type=int)

    render = subcommands.add_parser("render")
    render.add_argument("--restore-name", default="prepared")

    compose = subcommands.add_parser("compose")
    compose.add_argument("arguments", nargs=argparse.REMAINDER)

    restore = subcommands.add_parser("restore")
    restore.add_argument("--target", default="primary")
    restore.add_argument("--output-name", default="prepared")
    restore.add_argument("--snapshot")

    drill = subcommands.add_parser("drill")
    drill.add_argument("drill_action", choices=("up", "status", "down"))
    drill.add_argument("--restore-name", default="prepared")

    subcommands.add_parser("check-compose")
    return parser.parse_args()


def absolute_root(path: Path) -> Path:
    root = path.absolute()
    if root == Path(root.anchor):
        raise ValueError("deployment root must not be the filesystem root")
    require_real_directory_chain(root)
    return root


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


def require_integer(value: object, label: str, minimum: int, maximum: int) -> int:
    if type(value) is not int or value < minimum or value > maximum:
        raise ValueError(f"{label} must be an integer from {minimum} to {maximum}")
    return value


def validate_config(document: object) -> DeploymentConfig:
    if not isinstance(document, dict):
        raise TypeError("deployment configuration must be one YAML mapping")
    keys = set(document)
    if keys != CONFIG_KEYS:
        missing = sorted(CONFIG_KEYS - keys)
        unknown = sorted(str(key) for key in keys - CONFIG_KEYS)
        detail = []
        if missing:
            detail.append("missing " + ", ".join(missing))
        if unknown:
            detail.append("unknown " + ", ".join(unknown))
        raise ValueError("deployment configuration fields: " + "; ".join(detail))
    schema_version = require_integer(document["schema_version"], "schema_version", 1, 1)
    runtime_uid = require_integer(document["runtime_uid"], "runtime_uid", 0, MAX_ID)
    runtime_gid = require_integer(document["runtime_gid"], "runtime_gid", 0, MAX_ID)
    listen_port = require_integer(document["listen_port"], "listen_port", 0, 65535)
    drill_port = require_integer(document["drill_port"], "drill_port", 0, 65535)
    if listen_port != 0 and listen_port == drill_port:
        raise ValueError("listen_port and drill_port must differ when nonzero")
    return DeploymentConfig(
        schema_version=schema_version,
        runtime_uid=runtime_uid,
        runtime_gid=runtime_gid,
        listen_port=listen_port,
        drill_port=drill_port,
    )


def load_deployment_config(root: Path) -> DeploymentConfig:
    path = root / "config" / "deployment.yaml"
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise ValueError("config/deployment.yaml must be a regular non-symlink file")
    if stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError("config/deployment.yaml must have mode 0600")
    document = yaml.load(path.read_text(encoding="utf-8"), Loader=UniqueKeyLoader)
    config = validate_config(document)
    require_real_directory(
        root / "config", 0o700, config.runtime_uid, config.runtime_gid
    )
    require_private_file(path, config.runtime_uid, config.runtime_gid)
    return config


def deployment_yaml(uid: int, gid: int) -> str:
    validate_config(
        {
            "schema_version": 1,
            "runtime_uid": uid,
            "runtime_gid": gid,
            "listen_port": 8080,
            "drill_port": 8081,
        }
    )
    return (
        "schema_version: 1\n"
        f"runtime_uid: {uid}\n"
        f"runtime_gid: {gid}\n"
        "listen_port: 8080\n"
        "drill_port: 8081\n"
    )


def initialize(root: Path, uid: int, gid: int) -> None:
    validate_config(
        {
            "schema_version": 1,
            "runtime_uid": uid,
            "runtime_gid": gid,
            "listen_port": 8080,
            "drill_port": 8081,
        }
    )
    if not VAULTWARDEN_TEMPLATE.is_file() or VAULTWARDEN_TEMPLATE.is_symlink():
        raise ValueError("Vaultwarden environment template must be a regular file")
    deployment_path = root / "config" / "deployment.yaml"
    if deployment_path.exists() or deployment_path.is_symlink():
        existing = load_deployment_config(root)
        if existing.runtime_uid != uid or existing.runtime_gid != gid:
            raise ValueError(
                "existing deployment UID/GID do not match the explicit init arguments"
            )
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
        ensure_directory(directory, uid, gid)

    create_private_file(deployment_path, deployment_yaml(uid, gid), uid, gid)
    existing = load_deployment_config(root)
    if existing.runtime_uid != uid or existing.runtime_gid != gid:
        raise AssertionError("created deployment identity changed unexpectedly")
    create_private_file(
        root / "config" / "vaultwarden" / "vaultwarden.env",
        VAULTWARDEN_TEMPLATE.read_text(encoding="utf-8"),
        uid,
        gid,
    )
    create_private_file(
        root / "config" / "backup" / "backup.yaml", BACKUP_CONFIG, uid, gid
    )
    for role in ("backup", "maintenance"):
        create_private_file(
            root / "credentials" / role / "credentials.yaml",
            ROLE_CREDENTIALS,
            uid,
            gid,
        )
    create_private_file(
        root / "credentials" / "restic" / "primary",
        secrets.token_urlsafe(48) + "\n",
        uid,
        gid,
    )
    create_private_file(
        root / "credentials" / "notifiers" / "credentials.yaml",
        NOTIFIER_CREDENTIALS,
        uid,
        gid,
    )


def require_deployment_name(value: str, label: str) -> str:
    if not DEPLOYMENT_NAME_PATTERN.fullmatch(value):
        raise ValueError(
            f"{label} must be 1-64 ASCII letters, digits, dot, underscore, or hyphen"
        )
    return value


def read_compose_template() -> str:
    info = COMPOSE_TEMPLATE.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise ValueError("Compose template must be a regular non-symlink file")
    return COMPOSE_TEMPLATE.read_text(encoding="utf-8")


def render_compose(root: Path, restore_name: str = "prepared") -> str:
    config = load_deployment_config(root)
    metadata = load_project_metadata(
        SOURCE_ROOT / "VERSION", SOURCE_ROOT / "UPSTREAMS.toml"
    )
    restore_name = require_deployment_name(restore_name, "restore name")
    replacements = {
        "@@USER_UID@@": str(config.runtime_uid),
        "@@USER_GID@@": str(config.runtime_gid),
        "@@IMAGE_BACKUP@@": metadata.backup_image,
        "@@IMAGE_PROXY@@": metadata.proxy_image,
        "@@IMAGE_VAULTWARDEN@@": metadata.vaultwarden_image,
        "@@PORT_PROXY@@": str(config.listen_port),
        "@@PORT_DRILL@@": str(config.drill_port),
        "@@RESTORE_NAME_DRILL@@": restore_name,
    }
    rendered = read_compose_template()
    for token, value in replacements.items():
        if token not in rendered:
            raise ValueError(f"Compose template is missing required token {token}")
        rendered = rendered.replace(token, value)
    unresolved = TOKEN_PATTERN.findall(rendered)
    if unresolved:
        raise ValueError(
            "Compose template has unresolved tokens: "
            + ", ".join(sorted(set(unresolved)))
        )
    if "${" in rendered:
        raise ValueError("Compose template must not use environment interpolation")
    return rendered


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


def run_compose(
    root: Path,
    arguments: list[str],
    *,
    project: str = PRODUCTION_PROJECT,
    restore_name: str = "prepared",
) -> None:
    rendered = render_compose(root, restore_name)
    with tempfile.TemporaryDirectory(prefix="vaultwarden-less-docker-config.") as name:
        docker_config = Path(name)
        config_file = docker_config / "config.json"
        config_file.write_text(ANONYMOUS_DOCKER_CONFIG, encoding="utf-8", newline="\n")
        config_file.chmod(0o600)
        command = [
            "docker",
            "--config",
            os.fspath(docker_config),
            "compose",
            "--env-file",
            "/dev/null",
            "--project-directory",
            os.fspath(root),
            "--project-name",
            project,
            "--file",
            "-",
            *arguments,
        ]
        subprocess.run(
            command,
            check=True,
            env=docker_environment(docker_config),
            input=rendered,
            text=True,
        )


def run_restore(
    root: Path, target: str, output_name: str, snapshot: str | None
) -> None:
    output_name = require_deployment_name(output_name, "restore output name")
    arguments = [
        "--profile",
        "restore",
        "run",
        "--rm",
        "--no-deps",
        "restore",
        "restore",
        "--target",
        target,
        "--output",
        "/restore/" + output_name,
        "--config",
        "/config/backup.yaml",
    ]
    if snapshot is not None:
        arguments.extend(("--snapshot", snapshot))
    run_compose(root, arguments)


def run_drill(root: Path, action: str, restore_name: str) -> None:
    if action == "up":
        restore_name = require_deployment_name(restore_name, "drill restore name")
        run_compose(
            root,
            ["--profile", "drill", "down", "--volumes", "--remove-orphans"],
            project=DRILL_PROJECT,
            restore_name=restore_name,
        )
        run_compose(
            root,
            ["--profile", "drill", "up", "--detach", "drill-vaultwarden"],
            project=DRILL_PROJECT,
            restore_name=restore_name,
        )
    elif action == "status":
        run_compose(
            root,
            ["--profile", "drill", "ps", "drill-vaultwarden"],
            project=DRILL_PROJECT,
        )
    else:
        run_compose(
            root,
            ["--profile", "drill", "down", "--volumes", "--remove-orphans"],
            project=DRILL_PROJECT,
        )


def check_compose() -> None:
    with tempfile.TemporaryDirectory(prefix="vaultwarden-less-compose-check.") as name:
        root = Path(name)
        uid, gid = os.geteuid(), os.getegid()
        initialize(root, uid, gid)
        run_compose(
            root,
            [
                "--profile",
                "maintenance",
                "--profile",
                "restore",
                "--profile",
                "drill",
                "config",
                "--quiet",
            ],
        )


def main() -> int:
    args = parse_arguments()
    try:
        if args.action == "check-compose":
            check_compose()
            return 0
        root = absolute_root(args.root)
        if args.action == "init":
            initialize(root, args.uid, args.gid)
        elif args.action == "render":
            sys.stdout.write(render_compose(root, args.restore_name))
        elif args.action == "compose":
            arguments = args.arguments
            if arguments and arguments[0] == "--":
                arguments = arguments[1:]
            if not arguments:
                raise ValueError(
                    "compose requires at least one Docker Compose argument"
                )
            run_compose(root, arguments)
        elif args.action == "restore":
            run_restore(root, args.target, args.output_name, args.snapshot)
        else:
            run_drill(root, args.drill_action, args.restore_name)
    except (
        KeyError,
        OSError,
        TypeError,
        ValueError,
        subprocess.CalledProcessError,
        yaml.YAMLError,
    ) as error:
        print(f"deploy {args.action} failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
