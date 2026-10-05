"""Build and run the packaged acceptance suite on the current local Docker host."""

import argparse
import json
import os
import subprocess
import tempfile
from pathlib import Path


def build(root: Path, attribute: str) -> Path:
    result = subprocess.run(
        ["nix", "build", "--no-link", "--print-out-paths", ".#" + attribute],
        cwd=root,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    )
    return Path(result.stdout.strip())


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-dir", type=Path)
    parser.add_argument("--work-dir", type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    release = args.release_dir.resolve() if args.release_dir else build(root, "release")
    runner = build(root, "test-runner-image")
    base = (
        args.work_dir.resolve() if args.work_dir else root / "tmp/agent/release-tests"
    )
    base.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="release-", dir=base))
    docker_config = work / "docker-config"
    docker_config.mkdir()
    (docker_config / "config.json").write_text(json.dumps({"auths": {}}))
    env = os.environ | {"DOCKER_CONFIG": str(docker_config)}
    socket = Path("/var/run/docker.sock")
    if not socket.exists():
        parser.error("the current host must provide /var/run/docker.sock")
    docker = ["docker", "--host", "unix:///var/run/docker.sock"]
    subprocess.run(docker + ["load", "--input", str(runner)], env=env, check=True)
    command = docker + [
        "run",
        "--rm",
        "--pull",
        "never",
        "--network",
        "host",
        "--user",
        f"{os.geteuid()}:{os.getegid()}",
        "--group-add",
        str(socket.stat().st_gid),
        "--mount",
        f"type=bind,source={socket},target={socket}",
        "--mount",
        f"type=bind,source={release},target={release},readonly",
        "--mount",
        f"type=bind,source={work},target={work}",
        "--env",
        f"DOCKER_CONFIG={docker_config}",
        "vaultwarden-less-test-runner:local",
        "--release-dir",
        str(release),
        "--work-dir",
        str(work),
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
        raise SystemExit(process.wait())


if __name__ == "__main__":
    main()
