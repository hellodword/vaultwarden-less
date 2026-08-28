"""Resume terminal deployment workflows through structured Docker CLI argv."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import stat
import subprocess
import sys
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

SCHEMA_VERSION = 1
MAX_STATE_BYTES = 1 << 20
PRODUCTS = ("vaultwarden", "backup", "proxy")
SERVICES = ("vaultwarden", "backup", "proxy")
TIMESTAMP_PATTERN = re.compile(
    r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$"
)
COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")
RELEASE_PATTERN = re.compile(
    r"^RELEASE\.[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z-g([0-9a-f]{12})$"
)
PROJECT_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_-]{0,62}$")
CONTAINER_PATTERN = re.compile(r"^[0-9a-f]{12,64}$")
IMAGE_ID_PATTERN = re.compile(r"^sha256:[0-9a-f]{64}$")
DECIMAL_PATTERN = re.compile(r"^[1-9][0-9]*$")


class DeploymentError(Exception):
    """A bounded operator-facing orchestration failure."""


class UsageError(Exception):
    """A stable command-line usage failure."""


class Parser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        del message
        raise UsageError("invalid deployment arguments")


@dataclass(frozen=True)
class Identity:
    commit: str
    release: str


@dataclass(frozen=True)
class UpgradeContext:
    directory: Path
    environment: Path
    upgrade_id: str
    source: Identity
    target: Identity
    decision: str | None


RunCommand = Callable[..., subprocess.CompletedProcess[str]]


def _duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise DeploymentError("deployment state contains duplicate JSON keys")
        result[key] = value
    return result


def _reject_constant(value: str) -> None:
    del value
    raise DeploymentError("deployment state contains an invalid JSON number")


def _same_file(left: os.stat_result, right: os.stat_result) -> bool:
    return (
        left.st_dev,
        left.st_ino,
        left.st_mode,
        left.st_nlink,
        left.st_uid,
        left.st_gid,
        left.st_size,
        left.st_mtime_ns,
        left.st_ctime_ns,
    ) == (
        right.st_dev,
        right.st_ino,
        right.st_mode,
        right.st_nlink,
        right.st_uid,
        right.st_gid,
        right.st_size,
        right.st_mtime_ns,
        right.st_ctime_ns,
    )


def _private_directory(path: Path) -> None:
    try:
        metadata = path.lstat()
    except OSError as error:
        raise DeploymentError(
            "deployment state directory is missing or unsafe"
        ) from error
    if (
        not stat.S_ISDIR(metadata.st_mode)
        or stat.S_ISLNK(metadata.st_mode)
        or stat.S_IMODE(metadata.st_mode) != 0o700
        or metadata.st_uid != os.geteuid()
        or metadata.st_gid != os.getegid()
    ):
        raise DeploymentError("deployment state directory is missing or unsafe")


def _private_bytes(path: Path) -> bytes:
    try:
        before = path.lstat()
    except OSError as error:
        raise DeploymentError("deployment state file is missing or unsafe") from error
    if (
        not stat.S_ISREG(before.st_mode)
        or stat.S_ISLNK(before.st_mode)
        or stat.S_IMODE(before.st_mode) != 0o600
        or before.st_nlink != 1
        or before.st_uid != os.geteuid()
        or before.st_gid != os.getegid()
        or before.st_size < 1
        or before.st_size > MAX_STATE_BYTES
    ):
        raise DeploymentError("deployment state file is missing or unsafe")
    flags = os.O_RDONLY | os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "rb", closefd=True) as source:
            opened = os.fstat(source.fileno())
            if not _same_file(before, opened):
                raise DeploymentError("deployment state changed while opening")
            data = source.read(MAX_STATE_BYTES + 1)
    except DeploymentError:
        raise
    except OSError as error:
        raise DeploymentError("deployment state file is missing or unsafe") from error
    try:
        after = path.lstat()
    except OSError as error:
        raise DeploymentError("deployment state changed while reading") from error
    if (
        len(data) != opened.st_size
        or len(data) > MAX_STATE_BYTES
        or not _same_file(opened, after)
    ):
        raise DeploymentError("deployment state changed while reading")
    return data


def _private_json(path: Path) -> dict[str, Any]:
    data = _private_bytes(path)
    try:
        text = data.decode("utf-8")
        document = json.loads(
            text,
            object_pairs_hook=_duplicate_keys,
            parse_constant=_reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise DeploymentError("deployment state JSON is invalid") from error
    if not isinstance(document, dict):
        raise DeploymentError("deployment state JSON is invalid")
    canonical = json.dumps(document, ensure_ascii=False, separators=(",", ":")) + "\n"
    if text != canonical:
        raise DeploymentError("deployment state JSON is not canonical")
    return document


def _exact_fields(document: dict[str, Any], fields: Sequence[str]) -> None:
    if list(document) != list(fields) or set(document) != set(fields):
        raise DeploymentError("deployment state JSON fields are invalid")


def _identity(commit: Any, release: Any) -> Identity:
    if not isinstance(commit, str) or COMMIT_PATTERN.fullmatch(commit) is None:
        raise DeploymentError("deployment identity is invalid")
    if not isinstance(release, str):
        raise DeploymentError("deployment identity is invalid")
    matched = RELEASE_PATTERN.fullmatch(release)
    if matched is None or matched.group(1) != commit[:12]:
        raise DeploymentError("deployment identity is invalid")
    return Identity(commit, release)


def _timestamp(value: Any) -> None:
    if not isinstance(value, str) or TIMESTAMP_PATTERN.fullmatch(value) is None:
        raise DeploymentError("deployment state timestamp is invalid")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise DeploymentError("deployment state timestamp is invalid") from error
    if parsed.strftime("%Y-%m-%dT%H:%M:%S.%f")[:23] + "Z" != value:
        raise DeploymentError("deployment state timestamp is invalid")


def _read_active(root: Path) -> Identity:
    directory = root / "state" / "deployment"
    _private_directory(root / "state")
    _private_directory(directory)
    document = _private_json(directory / "active.json")
    _exact_fields(
        document,
        ("schema_version", "project_commit", "release", "activated_at"),
    )
    if (
        type(document["schema_version"]) is not int
        or document["schema_version"] != SCHEMA_VERSION
    ):
        raise DeploymentError("active deployment schema is invalid")
    _timestamp(document["activated_at"])
    return _identity(document["project_commit"], document["release"])


def _read_environment(path: Path, expected_fields: Sequence[str]) -> dict[str, str]:
    try:
        text = _private_bytes(path).decode("utf-8")
    except UnicodeDecodeError as error:
        raise DeploymentError("deployment environment is invalid") from error
    if not text.endswith("\n") or "\r" in text:
        raise DeploymentError("deployment environment is invalid")
    result: dict[str, str] = {}
    for line in text[:-1].split("\n"):
        if "=" not in line:
            raise DeploymentError("deployment environment is invalid")
        name, value = line.split("=", 1)
        if (
            name in result
            or not value
            or any(character.isspace() for character in value)
        ):
            raise DeploymentError("deployment environment is invalid")
        result[name] = value
    if list(result) != list(expected_fields):
        raise DeploymentError("deployment environment fields are invalid")
    return result


def _read_upgrade_context(root: Path, directory: Path) -> UpgradeContext:
    _private_directory(root / "state")
    _private_directory(root / "state" / "upgrade")
    _private_directory(directory)
    prepare = _private_json(directory / "prepare.json")
    _exact_fields(
        prepare,
        (
            "schema_version",
            "upgrade_id",
            "phase",
            "from_commit",
            "from_release",
            "to_commit",
            "to_release",
            "prepared_at",
        ),
    )
    if (
        type(prepare["schema_version"]) is not int
        or prepare["schema_version"] != SCHEMA_VERSION
        or prepare["phase"] != "prepared"
    ):
        raise DeploymentError("upgrade preparation is invalid")
    source = _identity(prepare["from_commit"], prepare["from_release"])
    target = _identity(prepare["to_commit"], prepare["to_release"])
    upgrade_id = f"{source.commit}..{target.commit}"
    if prepare["upgrade_id"] != upgrade_id or source == target:
        raise DeploymentError("upgrade preparation identity is invalid")
    _timestamp(prepare["prepared_at"])

    base = _read_environment(
        root / ".env",
        (
            "DEPLOYMENT_SCHEMA_VERSION",
            "RUNTIME_UID",
            "RUNTIME_GID",
            "LISTEN_PORT",
            "DRILL_PORT",
        ),
    )
    if (
        base["DEPLOYMENT_SCHEMA_VERSION"] != "1"
        or DECIMAL_PATTERN.fullmatch(base["RUNTIME_UID"]) is None
        or DECIMAL_PATTERN.fullmatch(base["RUNTIME_GID"]) is None
        or DECIMAL_PATTERN.fullmatch(base["LISTEN_PORT"]) is None
        or DECIMAL_PATTERN.fullmatch(base["DRILL_PORT"]) is None
    ):
        raise DeploymentError("deployment environment values are invalid")
    runtime_uid = int(base["RUNTIME_UID"])
    runtime_gid = int(base["RUNTIME_GID"])
    listen_port = int(base["LISTEN_PORT"])
    drill_port = int(base["DRILL_PORT"])
    if (
        runtime_uid < 1
        or runtime_uid > 4_294_967_294
        or runtime_gid < 1
        or runtime_gid > 4_294_967_294
        or listen_port < 1024
        or listen_port > 65535
        or drill_port < 1024
        or drill_port > 65535
        or listen_port == drill_port
    ):
        raise DeploymentError("deployment environment values are invalid")
    environment_path = directory / "compose.env"
    environment = _read_environment(
        environment_path,
        (*base.keys(), "A_RELEASE", "B_RELEASE"),
    )
    if (
        any(environment[name] != value for name, value in base.items())
        or environment["A_RELEASE"] != source.release
        or environment["B_RELEASE"] != target.release
    ):
        raise DeploymentError("upgrade Compose environment is inconsistent")

    decision_path = directory / "decision.json"
    decision: str | None = None
    try:
        decision_path.lstat()
    except FileNotFoundError:
        pass
    except OSError as error:
        raise DeploymentError("upgrade decision path is unsafe") from error
    else:
        document = _private_json(decision_path)
        _exact_fields(
            document,
            (
                "schema_version",
                "upgrade_id",
                "decision",
                "active_project_commit",
                "active_release",
                "decided_at",
            ),
        )
        if (
            type(document["schema_version"]) is not int
            or document["schema_version"] != SCHEMA_VERSION
            or document["upgrade_id"] != upgrade_id
        ):
            raise DeploymentError("upgrade decision is invalid")
        decision = document["decision"]
        if not isinstance(decision, str) or decision not in {"accept", "rollback"}:
            raise DeploymentError("upgrade decision is invalid")
        active = _identity(
            document["active_project_commit"], document["active_release"]
        )
        expected = target if decision == "accept" else source
        if active != expected:
            raise DeploymentError("upgrade decision identity is invalid")
        _timestamp(document["decided_at"])
    return UpgradeContext(
        directory, environment_path, upgrade_id, source, target, decision
    )


class Orchestrator:
    def __init__(
        self,
        root: Path,
        project_name: str,
        run_command: RunCommand = subprocess.run,
    ) -> None:
        try:
            self.root = root.resolve(strict=True)
        except OSError as error:
            raise DeploymentError("deployment root is unavailable") from error
        if not self.root.is_dir() or PROJECT_PATTERN.fullmatch(project_name) is None:
            raise DeploymentError("deployment root or Compose project is invalid")
        self.project_name = project_name
        self.run_command = run_command

    def terminal_upgrade(self, requested: str) -> dict[str, Any]:
        current = self.root / "state" / "upgrade" / "current"
        try:
            metadata = current.lstat()
        except FileNotFoundError:
            context = self._completed_context(requested)
            self._verify_terminal_runtime(context, requested)
            self._assert_drill_absent(context)
            return self._result("already_complete", context, requested)
        except OSError as error:
            raise DeploymentError("upgrade current state is unsafe") from error
        if not stat.S_ISDIR(metadata.st_mode) or stat.S_ISLNK(metadata.st_mode):
            raise DeploymentError("upgrade current state is unsafe")
        context = _read_upgrade_context(self.root, current)
        if context.decision is not None and context.decision != requested:
            raise DeploymentError(
                "the opposite terminal upgrade decision already exists"
            )
        if requested == "accept":
            self._accept(context)
        elif requested == "rollback":
            self._rollback(context)
        else:
            raise DeploymentError("unsupported terminal upgrade decision")
        archived = self._completed_context(requested)
        return self._result("completed", archived, requested)

    def _accept(self, context: UpgradeContext) -> None:
        self._upgrade_cli(context, "upgrade-b", "upgrade", "accept")
        decided = _read_upgrade_context(self.root, context.directory)
        if decided.decision != "accept" or _read_active(self.root) != decided.target:
            raise DeploymentError("accept did not publish the exact B active identity")
        compose = self._compose(decided.environment, overlay=True)
        self._run(
            [*compose, "up", "--detach", "--wait", "--wait-timeout", "180", *SERVICES]
        )
        self._verify_services(compose, decided.target, immutable=True)
        self._cleanup_drill_runtime(compose)
        self._upgrade_cli(decided, "upgrade-b", "upgrade", "cleanup-drill")
        self._assert_drill_absent(decided)
        self._upgrade_cli(decided, "upgrade-b", "upgrade", "finalize")

    def _rollback(self, context: UpgradeContext) -> None:
        overlay = self._compose(context.environment, overlay=True)
        transient = (
            "proxy",
            "backup",
            "vaultwarden",
            "upgrade-vaultwarden-b",
            "drill-vaultwarden",
        )
        self._run(
            [*overlay, "--profile", "upgrade", "--profile", "drill", "stop", *transient]
        )
        self._run(
            [
                *overlay,
                "--profile",
                "upgrade",
                "--profile",
                "drill",
                "rm",
                "--force",
                *transient,
                "drill-copy",
            ]
        )
        self._upgrade_cli(context, "rollback-a", "rollback", "apply")
        decided = _read_upgrade_context(self.root, context.directory)
        if decided.decision != "rollback" or _read_active(self.root) != decided.source:
            raise DeploymentError(
                "rollback did not preserve the exact A active identity"
            )
        self._retag_local(decided.source)
        compose = self._compose(self.root / ".env", overlay=False)
        self._run(
            [*compose, "up", "--detach", "--wait", "--wait-timeout", "180", *SERVICES]
        )
        self._verify_services(compose, decided.source, immutable=False)
        self._cleanup_drill_runtime(overlay)
        self._upgrade_cli(decided, "rollback-a", "upgrade", "cleanup-drill")
        self._assert_drill_absent(decided)
        self._upgrade_cli(decided, "rollback-a", "upgrade", "finalize")

    def _completed_context(self, requested: str) -> UpgradeContext:
        active = _read_active(self.root)
        archive_name = "history" if requested == "accept" else "quarantine"
        archive_root = self.root / "state" / "upgrade" / archive_name
        _private_directory(archive_root)
        matches: list[UpgradeContext] = []
        try:
            entries = list(os.scandir(archive_root))
        except OSError as error:
            raise DeploymentError("upgrade archive is unavailable") from error
        for entry in entries:
            if not entry.is_dir(follow_symlinks=False):
                raise DeploymentError("upgrade archive contains an unsafe entry")
            context = _read_upgrade_context(self.root, Path(entry.path))
            expected = context.target if requested == "accept" else context.source
            if context.decision == requested and expected == active:
                matches.append(context)
        if len(matches) != 1:
            raise DeploymentError(
                "active identity does not have one matching terminal upgrade archive"
            )
        return matches[0]

    def _upgrade_cli(
        self, context: UpgradeContext, service: str, *arguments: str
    ) -> None:
        compose = self._compose(context.environment, overlay=True)
        self._run(
            [
                *compose,
                "--profile",
                "upgrade",
                "--profile",
                "drill",
                "run",
                "--rm",
                "--no-deps",
                service,
                *arguments,
            ]
        )

    def _compose(self, environment: Path, *, overlay: bool) -> list[str]:
        arguments = [
            "docker",
            "compose",
            "--project-name",
            self.project_name,
            "--project-directory",
            os.fspath(self.root),
            "--env-file",
            os.fspath(environment),
            "--file",
            os.fspath(self.root / "compose.yml"),
        ]
        if overlay:
            arguments.extend(("--file", os.fspath(self.root / "compose.upgrade.yml")))
        return arguments

    def _retag_local(self, identity: Identity) -> None:
        for product in PRODUCTS:
            immutable = f"vaultwarden-less-{product}:{identity.release}"
            local = f"vaultwarden-less-{product}:local"
            immutable_document = self._inspect_image(immutable, identity)
            self._run(["docker", "image", "tag", immutable, local])
            local_document = self._inspect_image(local, identity)
            if local_document["Id"] != immutable_document["Id"]:
                raise DeploymentError("local image does not match immutable A")

    def _verify_terminal_runtime(self, context: UpgradeContext, requested: str) -> None:
        identity = context.target if requested == "accept" else context.source
        if _read_active(self.root) != identity:
            raise DeploymentError("active identity does not match completed upgrade")
        compose = self._compose(
            context.environment if requested == "accept" else self.root / ".env",
            overlay=requested == "accept",
        )
        self._verify_services(compose, identity, immutable=requested == "accept")

    def _verify_services(
        self,
        compose: Sequence[str],
        identity: Identity,
        *,
        immutable: bool,
    ) -> None:
        for service, product in zip(SERVICES, PRODUCTS, strict=True):
            result = self._run([*compose, "ps", "--quiet", service], capture=True)
            identifiers = [line for line in result.stdout.splitlines() if line]
            if (
                len(identifiers) != 1
                or CONTAINER_PATTERN.fullmatch(identifiers[0]) is None
            ):
                raise DeploymentError("terminal service does not have one container")
            reference = f"vaultwarden-less-{product}:{identity.release if immutable else 'local'}"
            image = self._inspect_image(reference, identity)
            container_result = self._run(
                [
                    "docker",
                    "container",
                    "inspect",
                    "--format",
                    "{{json .}}",
                    identifiers[0],
                ],
                capture=True,
            )
            container = self._json_object(container_result.stdout)
            configuration = container.get("Config")
            state = container.get("State")
            health = state.get("Health") if isinstance(state, dict) else None
            if (
                container.get("Image") != image["Id"]
                or not isinstance(configuration, dict)
                or configuration.get("Image") != reference
                or not isinstance(state, dict)
                or state.get("Running") is not True
                or not isinstance(health, dict)
                or health.get("Status") != "healthy"
            ):
                raise DeploymentError("terminal service identity or health is invalid")

    def _inspect_image(self, reference: str, identity: Identity) -> dict[str, Any]:
        result = self._run(
            ["docker", "image", "inspect", "--format", "{{json .}}", reference],
            capture=True,
        )
        document = self._json_object(result.stdout)
        configuration = document.get("Config")
        labels = (
            configuration.get("Labels") if isinstance(configuration, dict) else None
        )
        if (
            not isinstance(document.get("Id"), str)
            or IMAGE_ID_PATTERN.fullmatch(document["Id"]) is None
            or not isinstance(labels, dict)
            or labels.get("org.opencontainers.image.revision") != identity.commit
            or labels.get("org.opencontainers.image.version") != identity.release
        ):
            raise DeploymentError("immutable image labels are invalid")
        return document

    def _cleanup_drill_runtime(self, compose: Sequence[str]) -> None:
        self._run([*compose, "--profile", "drill", "stop", "drill-vaultwarden"])
        self._run(
            [
                *compose,
                "--profile",
                "drill",
                "rm",
                "--force",
                "drill-vaultwarden",
                "drill-copy",
            ]
        )
        expected = f"{self.project_name}_drill-data"
        result = self._run(
            [
                "docker",
                "volume",
                "ls",
                "--quiet",
                "--filter",
                f"label=com.docker.compose.project={self.project_name}",
                "--filter",
                "label=com.docker.compose.volume=drill-data",
            ],
            capture=True,
        )
        volumes = [line for line in result.stdout.splitlines() if line]
        if any(volume != expected for volume in volumes) or len(volumes) > 1:
            raise DeploymentError("drill volume selection is not exact")
        if volumes:
            inspected = self._run(
                ["docker", "volume", "inspect", "--format", "{{json .}}", expected],
                capture=True,
            )
            volume = self._json_object(inspected.stdout)
            labels = volume.get("Labels")
            if (
                volume.get("Name") != expected
                or not isinstance(labels, dict)
                or labels.get("com.docker.compose.project") != self.project_name
                or labels.get("com.docker.compose.volume") != "drill-data"
            ):
                raise DeploymentError("drill volume ownership is invalid")
            self._run(["docker", "volume", "rm", expected])

    def _assert_drill_absent(self, context: UpgradeContext) -> None:
        if (self.root / "restore" / "drill").exists() or (
            self.root / "restore" / "drill"
        ).is_symlink():
            raise DeploymentError("verified drill output was not removed")
        compose = self._compose(context.environment, overlay=True)
        for service in ("drill-vaultwarden", "drill-copy"):
            result = self._run(
                [*compose, "--profile", "drill", "ps", "--quiet", service], capture=True
            )
            if result.stdout.strip():
                raise DeploymentError("drill service was not removed")
        result = self._run(
            [
                "docker",
                "volume",
                "ls",
                "--quiet",
                "--filter",
                f"label=com.docker.compose.project={self.project_name}",
                "--filter",
                "label=com.docker.compose.volume=drill-data",
            ],
            capture=True,
        )
        if any(line for line in result.stdout.splitlines() if line):
            raise DeploymentError("drill volume was not removed")

    def _run(
        self,
        arguments: Sequence[str],
        *,
        capture: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        del capture
        try:
            result = self.run_command(
                list(arguments),
                cwd=self.root,
                check=False,
                text=True,
                capture_output=True,
            )
        except OSError as error:
            raise DeploymentError("Docker command could not be executed") from error
        if result.returncode != 0:
            raise DeploymentError("Docker command failed")
        return result

    @staticmethod
    def _json_object(value: str) -> dict[str, Any]:
        try:
            document = json.loads(value)
        except json.JSONDecodeError as error:
            raise DeploymentError("Docker returned invalid JSON") from error
        if not isinstance(document, dict):
            raise DeploymentError("Docker returned invalid JSON")
        return document

    @staticmethod
    def _result(status: str, context: UpgradeContext, decision: str) -> dict[str, Any]:
        identity = context.target if decision == "accept" else context.source
        return {
            "schema_version": SCHEMA_VERSION,
            "status": status,
            "workflow": "upgrade",
            "decision": decision,
            "upgrade_id": context.upgrade_id,
            "active_project_commit": identity.commit,
            "active_release": identity.release,
        }


def parser() -> Parser:
    root = Parser(description=__doc__)
    root.add_argument("--deployment-root", type=Path, default=Path.cwd())
    root.add_argument("--project-name", default="vaultwarden-less")
    workflows = root.add_subparsers(dest="workflow", required=True, parser_class=Parser)
    upgrade = workflows.add_parser("upgrade")
    upgrade.add_argument("decision", choices=("accept", "rollback"))
    return root


def _write_error(code: str, message: str) -> None:
    json.dump(
        {"schema_version": SCHEMA_VERSION, "error": {"code": code, "message": message}},
        sys.stderr,
        separators=(",", ":"),
    )
    sys.stderr.write("\n")


def main(arguments: Sequence[str] | None = None) -> int:
    try:
        parsed = parser().parse_args(arguments)
        if parsed.workflow != "upgrade":
            raise UsageError("unsupported workflow")
        result = Orchestrator(
            parsed.deployment_root, parsed.project_name
        ).terminal_upgrade(parsed.decision)
    except UsageError:
        _write_error("usage_error", "Invalid deployment arguments")
        return 2
    except DeploymentError:
        _write_error(
            "deployment_orchestration_failed", "Deployment orchestration failed"
        )
        return 1
    json.dump(result, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
