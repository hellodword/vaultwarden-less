import contextlib
import importlib.util
import io
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "upstream_diff", Path(__file__).resolve().parents[2] / "scripts/upstream_diff.py"
)
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


class AuditTests(unittest.TestCase):
    def test_explicit_baseline_and_workspace_do_not_invoke_nix(self):
        with tempfile.TemporaryDirectory(prefix="audit inputs ") as tmp:
            root = Path(tmp)
            baseline = root / "baseline with spaces"
            baseline.mkdir()
            (baseline / "go.mod").write_text("before\n")
            content = io.BytesIO()
            with tarfile.open(fileobj=content, mode="w") as archive:
                info = tarfile.TarInfo("go.mod")
                info.size = len(b"after\n")
                archive.addfile(info, io.BytesIO(b"after\n"))
            with (
                mock.patch.object(
                    audit.subprocess,
                    "run",
                    return_value=subprocess.CompletedProcess(
                        [], 0, stdout=content.getvalue()
                    ),
                ) as run,
                contextlib.redirect_stdout(io.StringIO()) as stdout,
            ):
                audit.main(
                    [
                        "go",
                        "candidate-ref",
                        "--baseline",
                        str(baseline),
                        "--work-dir",
                        str(root / "work directory"),
                    ]
                )
            self.assertIn("-before", stdout.getvalue())
            self.assertIn("+after", stdout.getvalue())
            self.assertEqual(len(run.call_args_list), 3)
            for call in run.call_args_list:
                self.assertEqual(call.args[0][0], "git")
            fetch = run.call_args_list[1].args[0]
            self.assertEqual(
                fetch[-2:], ["https://go.googlesource.com/go", "candidate-ref"]
            )
            self.assertTrue(Path(fetch[2]).is_relative_to(root / "work directory"))

    def test_vaultwarden_behavior_schema_and_configuration(self):
        with tempfile.TemporaryDirectory() as tmp:
            baseline, candidate = Path(tmp) / "baseline", Path(tmp) / "candidate"
            for path in [
                "src/api/core/accounts.rs",
                "src/db/models/user.rs",
                "migrations/sqlite/new/up.sql",
                ".env.template",
                ".env.example",
                "src/config.rs",
            ]:
                for root, text in [(baseline, "before\n"), (candidate, "after\n")]:
                    target = root / path
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text(text)
            report = audit.audit_changes(baseline, candidate, "vaultwarden")
            for path in [
                "accounts.rs",
                "user.rs",
                "up.sql",
                ".env.template",
                ".env.example",
                "src/config.rs",
            ]:
                self.assertIn(path, report)
            self.assertIn("-before", report)
            self.assertIn("+after", report)

    def test_added_and_removed_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            baseline, candidate = Path(tmp) / "baseline", Path(tmp) / "candidate"
            for root in [baseline, candidate]:
                (root / "src/api").mkdir(parents=True)
            (baseline / "src/api/deleted.rs").write_text("deleted\n")
            (candidate / "src/api/new.rs").write_text("added\n")
            report = audit.audit_changes(baseline, candidate, "vaultwarden")
            self.assertIn("-deleted", report)
            self.assertIn("+added", report)


if __name__ == "__main__":
    unittest.main()
