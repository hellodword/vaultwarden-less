"""Run one JSON-mode Go test command and prove required tests really passed."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from collections.abc import Sequence
from typing import TextIO


def parse_arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--require", action="append", required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    arguments = parser.parse_args()
    if arguments.command and arguments.command[0] == "--":
        arguments.command = arguments.command[1:]
    if not arguments.command:
        parser.error("a Go test command is required after --")
    if "-json" not in arguments.command:
        parser.error("the Go test command must contain -json")
    return arguments


def verify(
    command: Sequence[str],
    required: set[str],
    output: TextIO,
    diagnostics: TextIO,
) -> int:
    passed: set[str] = set()
    skipped: set[str] = set()
    failed: set[str] = set()
    protocol_errors: list[str] = []
    process = subprocess.Popen(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
    )
    if process.stdout is None:
        raise RuntimeError("failed to capture Go test output")
    for line_number, line in enumerate(process.stdout, start=1):
        output.write(line)
        output.flush()
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            protocol_errors.append(f"line {line_number} is not go test JSON")
            continue
        if not isinstance(event, dict) or not isinstance(event.get("Action"), str):
            protocol_errors.append(f"line {line_number} is not a Go test event")
            continue
        test = event.get("Test")
        if test is not None and not isinstance(test, str):
            protocol_errors.append(f"line {line_number} has an invalid test name")
            continue
        action = event["Action"]
        if action == "pass" and test is not None:
            passed.add(test)
        elif action == "skip" and test is not None:
            skipped.add(test)
        elif action == "fail":
            failed.add(test or "<package>")
    process.stdout.close()
    return_code = process.wait()

    missing = required - passed
    problems: list[str] = []
    if return_code != 0:
        problems.append(f"go test exited with status {return_code}")
    if protocol_errors:
        problems.extend(protocol_errors)
    if skipped:
        problems.append("skipped tests: " + ", ".join(sorted(skipped)))
    if failed:
        problems.append("failed tests/packages: " + ", ".join(sorted(failed)))
    if missing:
        problems.append("required PASS events missing: " + ", ".join(sorted(missing)))
    if problems:
        diagnostics.write("go test verification failed:\n")
        diagnostics.writelines(f"- {problem}\n" for problem in problems)
        return 1
    diagnostics.write(
        "go test verification passed: " + ", ".join(sorted(required)) + "\n"
    )
    return 0


def main() -> int:
    arguments = parse_arguments()
    try:
        return verify(
            arguments.command,
            set(arguments.require),
            sys.stdout,
            sys.stderr,
        )
    except OSError as error:
        print(f"go test verification failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
