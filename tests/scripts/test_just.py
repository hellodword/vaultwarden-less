import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class JustTests(unittest.TestCase):
    def test_recipes_forward_arguments_to_nix_without_shell_interpretation(self):
        justfile = Path(__file__).resolve().parents[2] / "Justfile"
        with tempfile.TemporaryDirectory(prefix="just arguments ") as tmp:
            root = Path(tmp)
            nix = root / "nix"
            nix.write_text(
                f"#!{sys.executable}\n"
                "import json, os, sys\n"
                "with open(os.environ['TEST_NIX_LOG'], 'w') as log:\n"
                "    json.dump(sys.argv[1:], log)\n"
            )
            nix.chmod(0o700)
            log = root / "arguments.json"
            env = os.environ | {
                "PATH": str(root) + os.pathsep + os.environ["PATH"],
                "TEST_NIX_LOG": str(log),
            }
            literal = str(root / "spaces ' quotes $(false) ; $HOME `false`")
            cases = [
                (["check"], ["flake", "check"]),
                (["release"], ["build", ".#release"]),
                (
                    ["check-release", "--work-dir", literal],
                    ["run", ".#check-release", "--", "--work-dir", literal],
                ),
            ]
            for args, expected in cases:
                with self.subTest(recipe=args[0]):
                    result = subprocess.run(
                        ["just", "--justfile", str(justfile), *args],
                        env=env,
                        text=True,
                        capture_output=True,
                        check=False,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(json.loads(log.read_text()), expected)


if __name__ == "__main__":
    unittest.main()
