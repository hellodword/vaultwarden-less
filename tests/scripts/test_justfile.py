"""Positive allowlist and final argv contracts for the three Just recipes."""

from __future__ import annotations

import json
import subprocess
import unittest
from pathlib import Path

REPOSITORY = Path(__file__).resolve().parents[2]
COMMIT = "a" * 40

RECIPES = {
    "check": ([], ["nix flake check"]),
    "check-release": (
        [COMMIT],
        [f'nix develop --command python3 scripts/check_release.py "{COMMIT}"'],
    ),
    "release": (
        [COMMIT, "/tmp/vaultwarden-less-release"],
        [
            (
                "nix develop --command python3 scripts/release.py "
                f'"{COMMIT}" "/tmp/vaultwarden-less-release"'
            )
        ],
    ),
}


def run_just(*arguments: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["just", *arguments],
        cwd=REPOSITORY,
        check=True,
        text=True,
        capture_output=True,
    )


class JustfileContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        result = run_just("--dump", "--dump-format", "json")
        cls.document = json.loads(result.stdout)

    def test_exact_public_allowlist_has_no_indirection(self) -> None:
        recipes = self.document["recipes"]
        self.assertEqual(set(recipes), set(RECIPES))
        self.assertEqual(self.document["modules"], {})
        self.assertEqual(self.document["aliases"], {})
        for name, recipe in recipes.items():
            with self.subTest(recipe=name):
                self.assertFalse(recipe["private"])
                self.assertTrue(recipe["doc"])
                self.assertEqual(recipe["dependencies"], [])
                self.assertEqual(recipe["attributes"], [])

    def test_each_recipe_has_one_exact_final_argv(self) -> None:
        for recipe, (arguments, expected) in RECIPES.items():
            with self.subTest(recipe=recipe):
                result = run_just("--dry-run", recipe, *arguments)
                self.assertEqual((result.stdout + result.stderr).splitlines(), expected)


if __name__ == "__main__":
    unittest.main()
