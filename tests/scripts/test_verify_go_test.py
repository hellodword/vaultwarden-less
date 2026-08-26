"""Regression tests for the no-skip Go test verifier."""

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


class VerifyGoTestScriptTest(unittest.TestCase):
    def run_events(
        self, events: list[object], *, return_code: int = 0, required: str = "TestReal"
    ) -> tuple[int, str, str]:
        program = (
            "import json,sys; "
            f"events={events!r}; "
            "[print(json.dumps(event)) for event in events]; "
            f"raise SystemExit({return_code})"
        )
        output = io.StringIO()
        diagnostics = io.StringIO()
        result = verify_go_test.verify(
            [sys.executable, "-c", program, "-json"],
            {required},
            output,
            diagnostics,
        )
        return result, output.getvalue(), diagnostics.getvalue()

    def test_accepts_one_exact_required_pass(self) -> None:
        event = {"Action": "pass", "Package": "example.test", "Test": "TestReal"}
        result, output, diagnostics = self.run_events([event])
        self.assertEqual(result, 0)
        self.assertEqual(json.loads(output), event)
        self.assertIn("verification passed: TestReal", diagnostics)

    def test_rejects_missing_skipped_failed_and_nonzero_runs(self) -> None:
        cases = (
            ([], 0, "required PASS events missing"),
            ([{"Action": "skip", "Test": "TestReal"}], 0, "skipped tests"),
            ([{"Action": "fail", "Test": "TestReal"}], 1, "failed tests"),
            ([{"Action": "pass", "Test": "TestReal"}], 2, "status 2"),
        )
        for events, return_code, expected in cases:
            with self.subTest(expected=expected):
                result, _, diagnostics = self.run_events(
                    events, return_code=return_code
                )
                self.assertEqual(result, 1)
                self.assertIn(expected, diagnostics)

    def test_rejects_non_json_output(self) -> None:
        output = io.StringIO()
        diagnostics = io.StringIO()
        result = verify_go_test.verify(
            [sys.executable, "-c", "print('not json')", "-json"],
            {"TestReal"},
            output,
            diagnostics,
        )
        self.assertEqual(result, 1)
        self.assertIn("not go test JSON", diagnostics.getvalue())


if __name__ == "__main__":
    unittest.main()
