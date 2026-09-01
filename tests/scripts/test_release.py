"""Contract tests for the thin Nix release exporter."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import check_release
import release

B_COMMIT = "b" * 40
TIMESTAMP = 1_788_000_000
INSTANT = dt.datetime.fromtimestamp(TIMESTAMP, dt.UTC)
IDENTITY = release.Identity(
    commit=B_COMMIT,
    timestamp=TIMESTAMP,
    release=f"RELEASE.{INSTANT:%Y-%m-%dT%H-%M-%SZ}-g{B_COMMIT[:12]}",
    created=f"{INSTANT:%Y-%m-%dT%H:%M:%SZ}",
)


def completed(
    arguments: list[str], stdout: str = ""
) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(arguments, 0, stdout, "")


def candidate_document() -> dict[str, object]:
    return {
        "commit": B_COMMIT,
        "started_at": "2026-08-30T10:00:00.000Z",
        "finished_at": "2026-08-30T10:30:00.000Z",
        "gates": list(check_release.EVIDENCE_GATES),
        "status": "passed",
    }


def write_bundle(path: Path) -> dict[str, str]:
    path.mkdir()
    for name in release.ARCHIVES:
        (path / name).write_bytes((name + "\n").encode())
    hashes = {
        name: hashlib.sha256((path / name).read_bytes()).hexdigest()
        for name in release.ARCHIVES
    }
    (path / "SHA256SUMS").write_text(
        "".join(f"{hashes[name]}  {name}\n" for name in release.ARCHIVES),
        encoding="ascii",
    )
    hashes["SHA256SUMS"] = hashlib.sha256(
        (path / "SHA256SUMS").read_bytes()
    ).hexdigest()
    return hashes


class ReleaseScriptTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_cli_is_exactly_one_output_directory(self) -> None:
        command = release.parser()
        parsed = command.parse_args([os.fspath(self.root / "release")])
        self.assertEqual(vars(parsed), {"output_directory": self.root / "release"})
        self.assertFalse(
            any(
                isinstance(action, argparse._SubParsersAction)
                for action in command._actions
            )
        )
        with self.assertRaises(SystemExit):
            command.parse_args([B_COMMIT, os.fspath(self.root / "release")])

    def test_clean_git_identity_is_exact(self) -> None:
        responses = iter(
            (
                completed(["git", "status"]),
                completed(["git", "rev-parse"], B_COMMIT + "\n"),
                completed(["git", "show"], str(TIMESTAMP) + "\n"),
            )
        )
        with mock.patch.object(
            release, "run", side_effect=lambda _arguments: next(responses)
        ):
            self.assertEqual(release.git_identity(), IDENTITY)

        with (
            mock.patch.object(
                release,
                "run",
                return_value=completed(["git", "status"], " M tracked\n"),
            ),
            self.assertRaisesRegex(ValueError, "clean Git worktree"),
        ):
            release.git_identity()

    def test_candidate_evidence_is_private_canonical_and_exact(self) -> None:
        repository = self.root / "repository"
        evidence_root = repository / "tmp" / "release-evidence"
        release_directory = evidence_root / B_COMMIT
        release_directory.mkdir(parents=True, mode=0o700)
        evidence_root.chmod(0o700)
        release_directory.chmod(0o700)
        candidate = release_directory / "candidate.json"
        data = check_release.canonical_json(candidate_document())
        candidate.write_bytes(data)
        candidate.chmod(0o600)
        with (
            mock.patch.object(release, "REPOSITORY", repository),
            mock.patch.object(check_release, "EVIDENCE_ROOT", evidence_root),
            mock.patch.object(
                release, "run", return_value=completed(["git", "check-ignore"])
            ) as run_command,
        ):
            path, digest = release.validate_candidate(B_COMMIT)
            self.assertEqual(path, candidate)
            self.assertEqual(digest, "sha256:" + hashlib.sha256(data).hexdigest())
            self.assertEqual(
                run_command.call_args.args[0][:3],
                ["git", "check-ignore", "--quiet"],
            )

            candidate.write_text(
                json.dumps(candidate_document(), indent=2), encoding="ascii"
            )
            with self.assertRaisesRegex(ValueError, "canonical"):
                release.validate_candidate(B_COMMIT)

    def test_bundle_files_and_checksums_are_exact(self) -> None:
        bundle = self.root / "bundle"
        expected = write_bundle(bundle)
        self.assertEqual(release.validate_bundle(bundle), expected)

        (bundle / "SHA256SUMS").write_text("wrong\n", encoding="ascii")
        with self.assertRaisesRegex(ValueError, "checksum"):
            release.validate_bundle(bundle)

        exact = self.root / "extra"
        write_bundle(exact)
        (exact / "unexpected").write_text("unexpected", encoding="ascii")
        with self.assertRaisesRegex(ValueError, "not exact"):
            release.validate_bundle(exact)

    def test_build_bundle_uses_the_public_nix_package(self) -> None:
        bundle = self.root / "bundle"
        write_bundle(bundle)
        with mock.patch.object(
            release,
            "run",
            return_value=completed(["nix", "build"], os.fspath(bundle) + "\n"),
        ) as run_command:
            self.assertEqual(release.build_bundle(), bundle)
        self.assertEqual(
            run_command.call_args.args[0],
            [
                "nix",
                "build",
                "--no-link",
                "--print-out-paths",
                ".#release",
            ],
        )

    def test_output_must_be_absent_beneath_a_real_parent(self) -> None:
        output = self.root / "release"
        self.assertEqual(release.validate_output(output), output)
        output.mkdir()
        with self.assertRaises(FileExistsError):
            release.validate_output(output)

        real_parent = self.root / "real"
        real_parent.mkdir()
        linked_parent = self.root / "linked"
        linked_parent.symlink_to(real_parent, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "real directory"):
            release.validate_output(linked_parent / "release")

        repository = self.root / "repository"
        (repository / "tmp").mkdir(parents=True)
        ignored_output = repository / "tmp" / "release"
        with (
            mock.patch.object(release, "REPOSITORY", repository),
            mock.patch.object(
                release, "run", return_value=completed(["git", "check-ignore"])
            ) as run_command,
        ):
            self.assertEqual(release.validate_output(ignored_output), ignored_output)
        self.assertEqual(
            run_command.call_args.args[0],
            ["git", "check-ignore", "--quiet", "--", "tmp/release"],
        )

    def test_artifact_evidence_is_canonical_private_and_hash_bound(self) -> None:
        release_directory = self.root / "evidence"
        release_directory.mkdir(mode=0o700)
        candidate = release_directory / "candidate.json"
        candidate.write_bytes(b"candidate\n")
        candidate.chmod(0o600)
        file_hashes = {
            name: hashlib.sha256(name.encode()).hexdigest()
            for name in release.RELEASE_FILES
        }
        release.write_artifact_evidence(
            candidate,
            "sha256:" + "c" * 64,
            IDENTITY,
            file_hashes,
            dt.datetime(2026, 8, 30, 10, 40, 0, tzinfo=dt.UTC),
        )
        artifact = release_directory / "artifact.json"
        expected = {
            "candidate_sha256": "sha256:" + "c" * 64,
            "commit": B_COMMIT,
            "release": IDENTITY.release,
            "files": {
                name: "sha256:" + file_hashes[name] for name in release.RELEASE_FILES
            },
            "verified_at": "2026-08-30T10:40:00.000Z",
            "status": "verified",
        }
        self.assertEqual(artifact.read_bytes(), check_release.canonical_json(expected))
        self.assertEqual(stat.S_IMODE(artifact.stat().st_mode), 0o600)

    def test_create_copies_one_nix_bundle_then_publishes(self) -> None:
        output = self.root / "release"
        bundle = self.root / "bundle"
        write_bundle(bundle)
        candidate = self.root / "candidate.json"
        events: list[str] = []

        def rename(source: Path, destination: Path) -> None:
            events.append("rename")
            source.rename(destination)

        with (
            mock.patch.object(
                release, "git_identity", return_value=IDENTITY
            ) as identity,
            mock.patch.object(release, "validate_output", return_value=output),
            mock.patch.object(
                release,
                "validate_candidate",
                return_value=(candidate, "sha256:" + "c" * 64),
            ) as candidate_check,
            mock.patch.object(release, "build_bundle", return_value=bundle) as build,
            mock.patch.object(
                release,
                "write_artifact_evidence",
                side_effect=lambda *_: events.append("artifact"),
            ),
            mock.patch.object(release, "rename_no_replace", side_effect=rename),
            mock.patch.object(
                release, "sync_directory", side_effect=lambda *_: events.append("sync")
            ),
        ):
            result = release.create_release(
                output,
                dt.datetime(2026, 8, 30, 10, 40, 0, tzinfo=dt.UTC),
            )
        self.assertEqual(result, output)
        self.assertEqual(events, ["sync", "artifact", "rename", "sync"])
        self.assertEqual(identity.call_count, 3)
        self.assertEqual(candidate_check.call_count, 2)
        build.assert_called_once_with()
        self.assertEqual(
            {path.name for path in output.iterdir()}, set(release.RELEASE_FILES)
        )
        self.assertEqual(list(self.root.glob(".release.build.*")), [])

    def test_failure_cleans_only_task_temporary_directory(self) -> None:
        output = self.root / "failed"
        bundle = self.root / "bundle"
        write_bundle(bundle)
        unrelated = self.root / ".failed.unrelated"
        unrelated.mkdir()

        def fail_copy(_source: Path, destination: Path) -> dict[str, str]:
            (destination / "marker").write_text("task", encoding="ascii")
            raise OSError("copy failed")

        with (
            mock.patch.object(release, "git_identity", return_value=IDENTITY),
            mock.patch.object(release, "validate_output", return_value=output),
            mock.patch.object(
                release,
                "validate_candidate",
                return_value=(self.root / "candidate.json", "sha256:" + "c" * 64),
            ),
            mock.patch.object(release, "build_bundle", return_value=bundle),
            mock.patch.object(release, "copy_bundle", side_effect=fail_copy),
            self.assertRaises(OSError),
        ):
            release.create_release(output)
        self.assertFalse(output.exists())
        self.assertTrue(unrelated.is_dir())
        self.assertEqual(list(self.root.glob(".failed.build.*")), [])


if __name__ == "__main__":
    unittest.main()
