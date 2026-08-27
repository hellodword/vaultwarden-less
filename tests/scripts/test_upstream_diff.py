"""Contract tests for binary-safe upstream audit diffs."""

from __future__ import annotations

import argparse
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import upstream_diff


class UpstreamDiffScriptTest(unittest.TestCase):
    def test_name_status_parser_handles_rename_copy_and_special_paths(self) -> None:
        data = (
            b"M\0plain\tname\n\\file\0"
            b"R100\0old/*[name].txt\0new/\xe9\x9b\xaa.md\0"
            b"C75\0source/(copy).txt\0target/#copy!.txt\0"
        )
        changes = upstream_diff.parse_changes(data)
        self.assertEqual([change.status for change in changes], ["M", "R100", "C75"])
        self.assertTrue(upstream_diff.audited(changes[1], ["old/**"]))
        self.assertTrue(upstream_diff.audited(changes[1], ["new/**"]))
        self.assertTrue(upstream_diff.audited(changes[2], ["source/**"]))
        self.assertTrue(upstream_diff.audited(changes[2], ["target/**"]))

        report = "\n".join(upstream_diff.change_line(change) for change in changes)
        self.assertNotIn("\t", report)
        self.assertNotIn("\n\\file", report)
        self.assertNotIn("雪", report)
        for character in "`*_<[]()#!|&":
            self.assertNotIn(character, report)
        self.assertIn("\\t", report)
        self.assertIn("\\n", report)
        self.assertIn("\\\\", report)
        self.assertIn("\\u96ea", report)

    def test_parser_rejects_malformed_or_truncated_records(self) -> None:
        for data in (b"invalid\0path\0", b"R100\0only-old\0", b"M\0\0"):
            with self.subTest(data=data), self.assertRaises(ValueError):
                upstream_diff.parse_changes(data)

    def test_main_uses_binary_safe_diff_options_and_both_rename_paths(self) -> None:
        base = "1" * 40
        candidate = "2" * 40
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            inventory = root / "UPSTREAMS.toml"
            inventory.write_text(
                "\n".join(
                    (
                        "[vaultwarden]",
                        'repository = "https://example.invalid/vaultwarden"',
                        'source_tag = "1.0.0"',
                        f'source_commit = "{base}"',
                        'audit_paths = ["audited/**"]',
                        "",
                    )
                ),
                encoding="utf-8",
            )
            repository = root / "tmp" / "upstream-audit" / "vaultwarden" / "repository"
            repository.mkdir(parents=True)
            calls: list[list[str]] = []

            def fake_git(_repository: Path, arguments: list[str]) -> bytes:
                calls.append(arguments)
                if arguments[:3] == ["remote", "get-url", "origin"]:
                    return b"https://example.invalid/vaultwarden\n"
                if arguments[0] == "rev-parse":
                    return (
                        base if "base" in arguments[1] else candidate
                    ).encode() + b"\n"
                if arguments[0] == "diff":
                    return b"R100\0outside/old\0audited/new\0"
                if arguments[0] == "log":
                    return candidate.encode() + b"\tchange * subject\0"
                return b""

            arguments = argparse.Namespace(
                component="vaultwarden",
                candidate_tag="2.0.0",
                upstreams=inventory,
            )
            previous = Path.cwd()
            try:
                os.chdir(root)
                with (
                    mock.patch.object(
                        upstream_diff, "parse_args", return_value=arguments
                    ),
                    mock.patch.object(upstream_diff, "git", side_effect=fake_git),
                ):
                    self.assertEqual(upstream_diff.main(), 0)
            finally:
                os.chdir(previous)

            diff_call = next(call for call in calls if call[0] == "diff")
            self.assertEqual(
                diff_call[:4],
                ["diff", "--name-status", "-z", "--find-renames"],
            )
            self.assertIn("--find-copies", diff_call)
            report = (repository.parent / "report-2.0.0.md").read_text(encoding="utf-8")
            self.assertIn("outside/old", report)
            self.assertIn("audited/new", report)
            self.assertNotIn("change * subject", report)
            self.assertIn("\\u002a", report)


if __name__ == "__main__":
    unittest.main()
