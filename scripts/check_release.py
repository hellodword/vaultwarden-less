"""Run prebuilt acceptance artifacts on an explicitly selected local Docker socket.

The Nix app supplies the artifacts and tools; this script never invokes Nix.
The host and runner must see each bind source at the same absolute path.
"""

import argparse
import errno
import json
import os
import stat
import subprocess
import sys
import tempfile
from pathlib import Path


def docker_environment(config: Path) -> dict[str, str]:
    # A caller's remote context, TLS settings or credentials must not redirect
    # an acceptance run away from the explicitly selected local socket.
    env = {
        key: value
        for key, value in os.environ.items()
        if not key.startswith(("DOCKER_", "COMPOSE_"))
    }
    return env | {"DOCKER_CONFIG": str(config)}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-dir", type=Path, required=True)
    parser.add_argument("--runner-archive", type=Path, required=True)
    parser.add_argument("--runner-image", required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--docker-socket", type=Path, required=True)
    args = parser.parse_args(argv)
    release, runner = args.release_dir.resolve(), args.runner_archive.resolve()
    base, socket = args.work_dir.resolve(), args.docker_socket.resolve()
    if not (release / "compose.yml").is_file():
        parser.error("release-dir must contain the deployment bundle's compose.yml")
    if not runner.is_file():
        parser.error("runner-archive must be an existing image archive")
    if not args.runner_image or args.runner_image.startswith("-"):
        parser.error("runner-image must be an image reference")
    if not socket.exists() or not stat.S_ISSOCK(socket.stat().st_mode):
        parser.error(f"docker-socket must be a local Unix socket: {socket}")
    # Docker --mount uses comma-separated fields. Spaces are supported; commas
    # would otherwise silently change the meaning of a mount specification.
    if any("," in str(path) for path in (release, base, socket)):
        parser.error("shared bind paths must not contain commas")
    base.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="release-", dir=base))
    runtime = work / "runtime"
    runtime.mkdir(mode=0o700)
    try:
        with tempfile.TemporaryDirectory(dir=work) as config:
            docker_config = Path(config)
            (docker_config / "config.json").write_text(json.dumps({"auths": {}}))
            env = docker_environment(docker_config)
            docker = ["docker", "--config", config, "--host", f"unix://{socket}"]
            subprocess.run(
                docker + ["load", "--input", str(runner)], env=env, check=True
            )
            # Prepare only this run's empty bind mount for the packaged runner.
            subprocess.run(
                docker
                + [
                    "run",
                    "--rm",
                    "--pull",
                    "never",
                    "--network",
                    "none",
                    "--read-only",
                    "--user",
                    "0:0",
                    "--entrypoint",
                    "/bin/chown",
                    "--mount",
                    f"type=bind,source={runtime},target=/work",
                    args.runner_image,
                    "1000:1000",
                    "/work",
                ],
                env=env,
                check=True,
            )
            command = docker + [
                "run",
                "--rm",
                "--pull",
                "never",
                "--network",
                "host",
                "--group-add",
                str(socket.stat().st_gid),
                "--mount",
                f"type=bind,source={socket},target={socket}",
                "--mount",
                f"type=bind,source={release},target={release},readonly",
                "--mount",
                f"type=bind,source={runtime},target={runtime}",
                args.runner_image,
                "--release-dir",
                str(release),
                "--work-dir",
                str(runtime),
                "--docker-socket",
                str(socket),
            ]
            print(f"Acceptance results: {work / 'results.log'}", flush=True)
            with (work / "results.log").open("w") as log:
                process = subprocess.Popen(
                    command,
                    env=env,
                    text=True,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.STDOUT,
                )
                for line in process.stdout:
                    print(line, end="", flush=True)
                    log.write(line)
                    log.flush()
                return process.wait()
    finally:
        try:
            runtime.rmdir()
        except OSError as error:
            if error.errno != errno.ENOTEMPTY:
                raise


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except subprocess.CalledProcessError as error:
        print(f"Docker command failed (exit {error.returncode}).", file=sys.stderr)
        raise SystemExit(error.returncode)
    except OSError as error:
        print(f"Acceptance setup failed: {error}", file=sys.stderr)
        raise SystemExit(1)
