"""Contract tests for the required Go test PASS verifier."""

from __future__ import annotations

import io
import json
import os
import sys
import unittest
from pathlib import Path

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import verify_go_test

PACKAGE = "example.test/integration"
TEST = "TestReal"
REQUIRED = {(PACKAGE, TEST)}


class VerifyGoTestScriptTest(unittest.TestCase):
    def run_events(
        self, events: list[object], *, return_code: int = 0
    ) -> tuple[int, str, str]:
        program = (
            "import json; "
            f"events={events!r}; "
            "[print(json.dumps(event)) for event in events]; "
            f"raise SystemExit({return_code})"
        )
        output = io.StringIO()
        diagnostics = io.StringIO()
        result = verify_go_test.verify(
            [sys.executable, "-c", program],
            REQUIRED,
            output,
            diagnostics,
        )
        return result, output.getvalue(), diagnostics.getvalue()

    def required_pass_events(self) -> list[dict[str, str]]:
        return [
            {"Action": "start", "Package": PACKAGE},
            {"Action": "run", "Package": PACKAGE, "Test": TEST},
            {"Action": "pass", "Package": PACKAGE, "Test": TEST},
            {"Action": "pass", "Package": PACKAGE},
        ]

    def test_accepts_run_then_test_pass_then_package_pass(self) -> None:
        events = self.required_pass_events()
        result, output, diagnostics = self.run_events(events)
        self.assertEqual(result, 0)
        self.assertEqual([json.loads(line) for line in output.splitlines()], events)
        self.assertIn(f"verification passed: {PACKAGE}:{TEST}", diagnostics)

    def test_rejects_skip_fail_nonzero_and_missing_evidence(self) -> None:
        cases = (
            (
                [
                    {"Action": "run", "Package": PACKAGE, "Test": TEST},
                    {"Action": "skip", "Package": PACKAGE, "Test": TEST},
                    {"Action": "pass", "Package": PACKAGE},
                ],
                0,
                "skipped tests/packages",
            ),
            (
                [
                    {"Action": "run", "Package": PACKAGE, "Test": TEST},
                    {"Action": "fail", "Package": PACKAGE, "Test": TEST},
                    {"Action": "fail", "Package": PACKAGE},
                ],
                1,
                "failed tests/packages",
            ),
            (self.required_pass_events(), 2, "status 2"),
            ([], 0, "evidence missing"),
        )
        for events, return_code, expected in cases:
            with self.subTest(expected=expected):
                result, _, diagnostics = self.run_events(
                    events, return_code=return_code
                )
                self.assertEqual(result, 1)
                self.assertIn(expected, diagnostics)

    def test_rejects_bad_order_missing_package_and_cross_package_name(self) -> None:
        cases = (
            [
                {"Action": "pass", "Package": PACKAGE},
                {"Action": "run", "Package": PACKAGE, "Test": TEST},
                {"Action": "pass", "Package": PACKAGE, "Test": TEST},
            ],
            [
                {"Action": "pass", "Test": TEST},
                {"Action": "pass", "Package": PACKAGE},
            ],
            [
                {"Action": "run", "Package": "other/package", "Test": TEST},
                {"Action": "pass", "Package": "other/package", "Test": TEST},
                {"Action": "pass", "Package": "other/package"},
            ],
            [
                {"Action": "pass", "Package": PACKAGE, "Test": TEST},
                {"Action": "run", "Package": PACKAGE, "Test": TEST},
                {"Action": "pass", "Package": PACKAGE},
            ],
        )
        for events in cases:
            with self.subTest(events=events):
                result, _, diagnostics = self.run_events(events)
                self.assertEqual(result, 1)
                self.assertIn("verification failed", diagnostics)

    def test_rejects_non_json_output(self) -> None:
        output = io.StringIO()
        diagnostics = io.StringIO()
        result = verify_go_test.verify(
            [sys.executable, "-c", "print('not json')"],
            REQUIRED,
            output,
            diagnostics,
        )
        self.assertEqual(result, 1)
        self.assertIn("not go test JSON", diagnostics.getvalue())


if __name__ == "__main__":
    unittest.main()
