import importlib.util
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "upstream_diff", Path(__file__).resolve().parents[2] / "scripts/upstream_diff.py"
)
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


class AuditTests(unittest.TestCase):
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
