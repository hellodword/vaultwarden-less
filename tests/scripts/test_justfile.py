"""Positive allowlist and argv contracts for the complete Just interface."""

from __future__ import annotations

import json
import subprocess
import unittest
from pathlib import Path

REPOSITORY = Path(__file__).resolve().parents[2]

GROUPS = {
    "development": {"check", "check-release", "release"},
    "operations": {
        "up",
        "status",
        "backup",
        "maintenance",
        "restore",
        "drill-up",
        "drill-down",
    },
    "upgrade": {
        "upgrade-prepare",
        "upgrade-freeze",
        "upgrade-start",
        "upgrade-verify",
    },
}

DRY_RUNS = {
    "check": (
        [],
        [
            "nix build --no-link .#checks.x86_64-linux.format",
            "nix develop --command go test ./...",
            (
                "nix develop --command python3 -m unittest discover "
                "--start-directory tests/scripts --pattern 'test_*.py'"
            ),
            "nix develop --command go vet ./...",
            "nix develop --command golangci-lint run",
            "nix develop --command ruff check scripts tests/scripts",
            "nix develop --command ruff format --check scripts tests/scripts",
            "nix flake check",
        ],
    ),
    "check-release": (
        ["a" * 40],
        ['nix develop --command python3 scripts/check_release.py "$1"'],
    ),
    "release": (
        ["/tmp/vaultwarden-less-release"],
        ['nix run .#release -- --output-root "$1"'],
    ),
    "up": (
        [],
        [
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-maintenance"
            ),
            (
                "docker compose --env-file .env --file compose.yml up --detach "
                "vaultwarden backup proxy"
            ),
        ],
    ),
    "status": (
        [],
        [
            (
                "docker compose --env-file .env --file compose.yml exec --no-TTY "
                "backup /bin/vaultwarden-less-backup status"
            )
        ],
    ),
    "backup": (
        [],
        [
            (
                "docker compose --env-file .env --file compose.yml exec --no-TTY "
                "backup /bin/vaultwarden-less-backup request-backup"
            )
        ],
    ),
    "maintenance": (
        ["repo", "check", "--target", "primary"],
        [
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-maintenance"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                'maintenance run --rm --no-deps maintenance "$@"'
            ),
        ],
    ),
    "restore": (
        ["primary", "restored", "latest"],
        [
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-maintenance"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile restore "
                'run --rm --no-deps restore restore --target "$1" --snapshot "$3" '
                '--output "$2"'
            ),
        ],
    ),
    "drill-up": (
        [],
        [
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-maintenance"
            ),
            (
                "docker compose --project-name vaultwarden-less-drill --env-file "
                ".env --file compose.yml --profile drill up --detach drill-vaultwarden"
            ),
        ],
    ),
    "drill-down": (
        [],
        [
            (
                "docker compose --project-name vaultwarden-less-drill --env-file "
                ".env --file compose.yml --profile drill down --volumes"
            )
        ],
    ),
    "upgrade-prepare": (
        ["b" * 40, "RELEASE.2026-08-28T00-00-00Z-gbbbbbbbbbbbb"],
        [
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-maintenance"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps maintenance upgrade prepare "
                '--to-commit "$1" --to-release "$2"'
            ),
        ],
    ),
    "upgrade-freeze": (
        [],
        [
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-maintenance"
            ),
            "docker compose --env-file .env --file compose.yml stop proxy",
            "docker compose --env-file .env --file compose.yml stop backup vaultwarden",
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile upgrade run --rm "
                "--no-deps upgrade-a upgrade freeze"
            ),
        ],
    ),
    "upgrade-start": (
        [],
        [
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile maintenance run "
                "--rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile maintenance run "
                "--rm --no-deps config-check-maintenance"
            ),
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile upgrade up --detach "
                "--no-deps --wait --wait-timeout 180 upgrade-vaultwarden-b"
            ),
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile upgrade run --rm "
                "--no-deps upgrade-b upgrade migrate-check"
            ),
        ],
    ),
    "upgrade-verify": (
        [],
        [
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile upgrade stop proxy "
                "backup vaultwarden upgrade-vaultwarden-b"
            ),
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile upgrade run --rm "
                "--no-deps upgrade-b upgrade verify"
            ),
            (
                "docker compose --env-file state/upgrade/current/compose.env --file "
                "compose.yml --file compose.upgrade.yml --profile drill up --detach "
                "--wait --wait-timeout 180 drill-vaultwarden"
            ),
        ],
    ),
}

PRIVATE_DRY_RUNS = {
    "_fmt": ([], ["nix fmt"]),
    "_fmt-check": ([], ["nix build --no-link .#checks.x86_64-linux.format"]),
    "_test": (
        [],
        [
            "nix develop --command go test ./...",
            (
                "nix develop --command python3 -m unittest discover "
                "--start-directory tests/scripts --pattern 'test_*.py'"
            ),
        ],
    ),
    "_vet": ([], ["nix develop --command go vet ./..."]),
    "_lint": (
        [],
        [
            "nix develop --command golangci-lint run",
            "nix develop --command ruff check scripts tests/scripts",
            "nix develop --command ruff format --check scripts tests/scripts",
        ],
    ),
    "_race-release": ([], ["nix develop --command go test -race ./... -count=1"]),
    "_security": ([], ["nix develop --command govulncheck ./..."]),
    "_build": (
        [],
        [
            (
                "nix build --no-link .#backup .#proxy .#backup-image .#proxy-image "
                ".#vaultwarden-image .#moto-image"
            )
        ],
    ),
    "_integration": (
        [],
        [
            (
                "nix develop --command python3 scripts/verify_go_test.py --require "
                "github.com/hellodword/vaultwarden-less/internal/restore:"
                "TestMotoResticIntegration -- go test -json ./internal/restore "
                "-run '^TestMotoResticIntegration$' -count=1 -timeout=20m -args "
                "-run-moto-integration"
            )
        ],
    ),
    "_integration-worker-timing": (
        [],
        [
            (
                "nix develop --command python3 scripts/verify_go_test.py --require "
                "github.com/hellodword/vaultwarden-less/internal/worker:"
                "TestWorkerAcceleratedTargetDegradationIntegration -- go test -json "
                "./internal/worker -run '^TestWorkerAcceleratedTargetDegradationIntegration$' "
                "-count=1 -timeout=5m -args -run-accelerated-worker-integration"
            )
        ],
    ),
    "_e2e-backup": (
        [],
        [
            (
                "nix develop --command python3 scripts/verify_go_test.py --require "
                "github.com/hellodword/vaultwarden-less/tests/e2e:"
                "TestBackupRestoreLifecycle -- go test -json ./tests/e2e "
                "-run '^TestBackupRestoreLifecycle$' -count=1 -timeout=30m -args "
                "-run-backup-e2e"
            )
        ],
    ),
    "_e2e-multitarget-fast": (
        [],
        [
            (
                "nix develop --command python3 scripts/verify_go_test.py --require "
                "github.com/hellodword/vaultwarden-less/tests/e2e:"
                "TestMultiTargetFastCatchUp -- go test -json ./tests/e2e "
                "-run '^TestMultiTargetFastCatchUp$' -count=1 -timeout=25m -args "
                "-run-multitarget-fast-e2e"
            )
        ],
    ),
    "_e2e-import": (
        [],
        [
            (
                "nix develop --command python3 scripts/verify_go_test.py --require "
                "github.com/hellodword/vaultwarden-less/tests/e2e:"
                "TestInitialImportLifecycle -- go test -json ./tests/e2e "
                "-run '^TestInitialImportLifecycle$' -count=1 -timeout=30m -args "
                "-run-import-e2e"
            )
        ],
    ),
    "_e2e-upgrade": (
        ["a" * 40, "b" * 40],
        [
            (
                "nix develop --command python3 scripts/verify_go_test.py --require "
                "github.com/hellodword/vaultwarden-less/tests/e2e:"
                "TestUpgradeAcceptLifecycle --require "
                "github.com/hellodword/vaultwarden-less/tests/e2e:"
                "TestUpgradeRollbackLifecycle -- go test -json ./tests/e2e "
                "-run '^(TestUpgradeAcceptLifecycle|TestUpgradeRollbackLifecycle)$' "
                "-count=1 -timeout=95m -args -run-upgrade-accept-e2e "
                '-run-upgrade-rollback-e2e -upgrade-from-commit "$1" '
                '-upgrade-to-commit "$2"'
            )
        ],
    ),
    "_config-validate": (
        [],
        [
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-backup"
            ),
            (
                "docker compose --env-file .env --file compose.yml --profile "
                "maintenance run --rm --no-deps config-check-maintenance"
            ),
        ],
    ),
    "_image-load": (["/tmp/release"], ['nix run .#image-load -- "$1"']),
    "_schema-capture": (
        ["/tmp/database.sqlite3", "/tmp/schema.json"],
        [
            (
                "nix develop --command python3 scripts/capture_vaultwarden_schema.py "
                '--database "$1" --output "$2"'
            )
        ],
    ),
    "_upstream-diff": (
        ["vaultwarden", "candidate"],
        ['nix develop --command python3 scripts/upstream_diff.py "$1" "$2"'],
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

    def test_public_recipes_and_native_groups_are_exact(self) -> None:
        recipes = self.document["recipes"]
        public = {name for name, recipe in recipes.items() if not recipe["private"]}
        self.assertEqual(public, set().union(*GROUPS.values()))
        self.assertEqual(len(public), 14)

        for group, names in GROUPS.items():
            for name in names:
                self.assertEqual(recipes[name]["attributes"], [{"group": group}])
                self.assertTrue(recipes[name]["doc"])
        self.assertTrue(
            all(
                recipe["private"]
                for name, recipe in recipes.items()
                if name not in public
            )
        )

    def test_no_modules_aliases_or_retired_recipes_exist(self) -> None:
        self.assertEqual(self.document["modules"], {})
        self.assertEqual(self.document["aliases"], {})
        self.assertTrue(self.document["settings"]["positional_arguments"])
        self.assertEqual(
            self.document["settings"]["shell"],
            {"command": "bash", "arguments": ["-euo", "pipefail", "-c"]},
        )
        retired = {
            "down",
            "ps",
            "logs",
            "drill-status",
            "request-backup",
            "repo-init",
            "repo-check",
            "repo-snapshots",
            "upgrade-start-b",
            "upgrade-migrate-check",
            "deployment",
            "upgrade-accept",
            "upgrade-rollback",
            "fmt",
            "test",
            "vet",
            "lint",
            "soak",
        }
        self.assertTrue(retired.isdisjoint(self.document["recipes"]))

    def test_every_public_recipe_has_exact_dry_run_argv(self) -> None:
        self.assertEqual(set(DRY_RUNS), set().union(*GROUPS.values()))
        for recipe, (arguments, expected) in DRY_RUNS.items():
            with self.subTest(recipe=recipe):
                result = run_just("--dry-run", recipe, *arguments)
                actual = (result.stdout + result.stderr).splitlines()
                self.assertEqual(actual, expected)

    def test_every_private_recipe_has_exact_dry_run_argv(self) -> None:
        recipes = self.document["recipes"]
        private = {name for name, recipe in recipes.items() if recipe["private"]}
        self.assertEqual(set(PRIVATE_DRY_RUNS), private)
        for recipe, (arguments, expected) in PRIVATE_DRY_RUNS.items():
            with self.subTest(recipe=recipe):
                result = run_just("--dry-run", recipe, *arguments)
                actual = (result.stdout + result.stderr).splitlines()
                self.assertEqual(actual, expected)


if __name__ == "__main__":
    unittest.main()
