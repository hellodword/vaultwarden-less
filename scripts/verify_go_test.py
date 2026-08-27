"""Run one JSON-mode Go test command and prove required package tests passed."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from collections.abc import Sequence
from dataclasses import dataclass
from typing import TextIO


@dataclass
class RequiredState:
    run_at: int | None = None
    pass_at: int | None = None


def required_identity(value: str) -> tuple[str, str]:
    package, separator, test = value.rpartition(":")
    if not separator or not package or not test:
        raise argparse.ArgumentTypeError("required identity must be <package>:<test>")
    return package, test


def parse_arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--require", action="append", required=True, type=required_identity
    )
    parser.add_argument("command", nargs=argparse.REMAINDER)
    arguments = parser.parse_args()
    if arguments.command and arguments.command[0] == "--":
        arguments.command = arguments.command[1:]
    if not arguments.command:
        parser.error("a Go test command is required after --")
    if "-json" not in arguments.command:
        parser.error("the Go test command must contain -json")
    if len(set(arguments.require)) != len(arguments.require):
        parser.error("required identities must be unique")
    return arguments


def verify(
    command: Sequence[str],
    required: set[tuple[str, str]],
    output: TextIO,
    diagnostics: TextIO,
) -> int:
    states = {identity: RequiredState() for identity in required}
    package_pass: dict[str, int] = {}
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
        package = event.get("Package")
        test = event.get("Test")
        if not isinstance(package, str) or not package:
            protocol_errors.append(f"line {line_number} is missing Package")
            continue
        if test is not None and (not isinstance(test, str) or not test):
            protocol_errors.append(f"line {line_number} has an invalid test name")
            continue
        action = event["Action"]
        identity = (package, test) if test is not None else None
        if action == "run" and identity in states:
            state = states[identity]
            if state.run_at is not None or state.pass_at is not None:
                protocol_errors.append(
                    f"line {line_number} repeats required test run {package}:{test}"
                )
            else:
                state.run_at = line_number
        elif action == "pass" and identity in states:
            state = states[identity]
            if state.run_at is None or state.pass_at is not None:
                protocol_errors.append(
                    f"line {line_number} has out-of-order required PASS {package}:{test}"
                )
            else:
                state.pass_at = line_number
        elif action == "pass" and test is None:
            package_pass[package] = line_number
        if action == "skip":
            skipped.add(f"{package}:{test}" if test is not None else package)
        elif action == "fail":
            failed.add(f"{package}:{test}" if test is not None else package)
    process.stdout.close()
    return_code = process.wait()

    missing: list[str] = []
    for (package, test), state in states.items():
        final_pass = package_pass.get(package)
        if (
            state.run_at is None
            or state.pass_at is None
            or final_pass is None
            or final_pass <= state.pass_at
        ):
            missing.append(f"{package}:{test}")
    problems: list[str] = []
    if return_code != 0:
        problems.append(f"go test exited with status {return_code}")
    problems.extend(protocol_errors)
    if skipped:
        problems.append("skipped tests/packages: " + ", ".join(sorted(skipped)))
    if failed:
        problems.append("failed tests/packages: " + ", ".join(sorted(failed)))
    if missing:
        problems.append(
            "required run/PASS/package PASS evidence missing: "
            + ", ".join(sorted(missing))
        )
    if problems:
        diagnostics.write("go test verification failed:\n")
        diagnostics.writelines(f"- {problem}\n" for problem in problems)
        return 1
    names = sorted(f"{package}:{test}" for package, test in required)
    diagnostics.write("go test verification passed: " + ", ".join(names) + "\n")
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
