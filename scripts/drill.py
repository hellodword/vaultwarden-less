"""Operate the drill under a separate Compose project and disposable volume."""

from __future__ import annotations

import argparse
import subprocess
import sys


def compose(*arguments: str) -> None:
    subprocess.run(
        [
            "docker",
            "compose",
            "--project-name",
            "vaultwarden-less-drill",
            "--env-file",
            ".env",
            "--file",
            "deploy/compose.yml",
            "--profile",
            "drill",
            *arguments,
        ],
        check=True,
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("up", "status", "down"))
    action = parser.parse_args().action
    try:
        if action == "up":
            compose("down", "--volumes", "--remove-orphans")
            compose("up", "--detach", "drill-vaultwarden")
        elif action == "status":
            compose("ps", "drill-vaultwarden")
        else:
            compose("down", "--volumes", "--remove-orphans")
    except subprocess.CalledProcessError as error:
        print(f"drill {action} failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
