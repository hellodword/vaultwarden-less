"""Contract tests for exact-evidence deterministic two-file releases."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import io
import json
import os
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import check_release
import release

A_COMMIT = "a" * 40
B_COMMIT = "0123456789abcdef0123456789abcdef01234567"
IDENTITY = release.Identity(
    commit=B_COMMIT,
    timestamp=1_788_000_000,
    release="RELEASE.2026-08-29T08-00-00Z-g0123456789ab",
    created="2026-08-29T08:00:00Z",
)


def completed(
    arguments: list[str], stdout: str = ""
) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")


def image_document(reference: str, *, required: bool = True) -> dict[str, object]:
    del required
    product = next(
        product for product in release.PRODUCTS if f"-{product}:" in reference
    )
    digit = str(release.PRODUCTS.index(product) + 1)
    return {
        "Id": "sha256:" + digit * 64,
        "Config": {
            "Entrypoint": release.ENTRYPOINTS[product],
            "Labels": {
                "org.opencontainers.image.revision": IDENTITY.commit,
                "org.opencontainers.image.version": IDENTITY.release,
                "org.opencontainers.image.created": IDENTITY.created,
            },
        },
    }


def write_docker_archive(
    path: Path, references: list[str], *, reverse: bool = False
) -> None:
    manifest_entries = []
    index_entries = []
    members: list[tuple[str, bytes]] = []
    for index in range(0, len(references), 2):
        tags = references[index : index + 2]
        config = f"config-{index // 2}.json"
        manifest_entries.append({"Config": config, "RepoTags": tags, "Layers": []})
        members.append((config, b"{}"))
        for reference in tags:
            index_entries.append(
                {
                    "digest": "sha256:" + f"{index + len(index_entries) + 1:064x}",
                    "mediaType": (
                        "application/vnd.docker.distribution.manifest.v2+json"
                    ),
                    "size": 1,
                    "annotations": {
                        "io.containerd.image.name": f"docker.io/library/{reference}",
                        "org.opencontainers.image.ref.name": reference.rsplit(":", 1)[
                            1
                        ],
                    },
                }
            )
    if reverse:
        manifest_entries.reverse()
        index_entries.reverse()
        for entry in manifest_entries:
            entry["RepoTags"].reverse()
    members.extend(
        [
            ("manifest.json", json.dumps(manifest_entries).encode()),
            (
                "index.json",
                json.dumps(
                    {
                        "schemaVersion": 2,
                        "mediaType": "application/vnd.oci.image.index.v1+json",
                        "manifests": index_entries,
                    }
                ).encode(),
            ),
            ("oci-layout", b'{"imageLayoutVersion":"1.0.0"}'),
        ]
    )
    if reverse:
        members.reverse()
    with tarfile.open(path, "w") as archive:
        for index, (name, payload) in enumerate(members):
            information = tarfile.TarInfo(name)
            information.size = len(payload)
            information.mtime = 1000 + index
            information.uid = 1000 + index
            information.gid = 2000 + index
            archive.addfile(information, io.BytesIO(payload))


def candidate_document() -> dict[str, object]:
    return {
        "from_commit": A_COMMIT,
        "to_commit": B_COMMIT,
        "started_at": "2026-08-30T10:00:00.000Z",
        "finished_at": "2026-08-30T10:30:00.000Z",
        "gates": list(check_release.EVIDENCE_GATES),
        "status": "passed",
    }


class ReleaseScriptTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_cli_is_exactly_a_and_output_with_no_load_subcommand(self) -> None:
        command = release.parser()
        parsed = command.parse_args([A_COMMIT, os.fspath(self.root / "release")])
        self.assertEqual(
            vars(parsed),
            {"from_commit": A_COMMIT, "output_directory": self.root / "release"},
        )
        self.assertFalse(
            any(
                isinstance(action, argparse._SubParsersAction)
                for action in command._actions
            )
        )
        self.assertFalse(hasattr(release, "load_release"))

    def test_clean_git_identity_and_exact_distinct_pair(self) -> None:
        timestamp = 1_788_000_000
        instant = dt.datetime.fromtimestamp(timestamp, dt.UTC)
        expected = release.Identity(
            commit=B_COMMIT,
            timestamp=timestamp,
            release=f"RELEASE.{instant:%Y-%m-%dT%H-%M-%SZ}-g{B_COMMIT[:12]}",
            created=f"{instant:%Y-%m-%dT%H:%M:%SZ}",
        )
        responses = iter(
            (
                completed(["git", "status"]),
                completed(["git", "rev-parse"], B_COMMIT + "\n"),
                completed(["git", "show"], str(timestamp) + "\n"),
            )
        )
        with mock.patch.object(
            release, "run", side_effect=lambda _arguments: next(responses)
        ):
            self.assertEqual(release.git_identity(), expected)

        with (
            mock.patch.object(release, "git_identity", return_value=IDENTITY),
            mock.patch.object(
                release, "run", return_value=completed(["git"], A_COMMIT + "\n")
            ),
        ):
            self.assertEqual(release.release_pair(A_COMMIT), (A_COMMIT, IDENTITY))
        with (
            mock.patch.object(release, "git_identity", return_value=IDENTITY),
            self.assertRaisesRegex(ValueError, "distinct"),
        ):
            release.release_pair(B_COMMIT)

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
        pair = evidence_root / f"{A_COMMIT}..{B_COMMIT}"
        pair.mkdir(parents=True, mode=0o700)
        evidence_root.chmod(0o700)
        pair.chmod(0o700)
        candidate = pair / "candidate.json"
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
            path, digest = release.validate_candidate(A_COMMIT, B_COMMIT)
            self.assertEqual(path, candidate)
            self.assertEqual(digest, "sha256:" + hashlib.sha256(data).hexdigest())
            self.assertEqual(
                run_command.call_args.args[0][:3], ["git", "check-ignore", "--quiet"]
            )

            candidate.write_text(
                json.dumps(candidate_document(), indent=2), encoding="ascii"
            )
            with self.assertRaisesRegex(ValueError, "canonical"):
                release.validate_candidate(A_COMMIT, B_COMMIT)

    def test_six_references_labels_entrypoints_and_ids_are_exact(self) -> None:
        expected = [
            f"vaultwarden-less-{product}:{tag}"
            for product in release.PRODUCTS
            for tag in ("local", IDENTITY.release)
        ]
        self.assertEqual(release.references(IDENTITY.release), expected)
        with mock.patch.object(release, "inspect", side_effect=image_document):
            release.validate_images(expected, IDENTITY)

        wrong = image_document(expected[0])
        config = wrong["Config"]
        assert isinstance(config, dict)
        config["Entrypoint"] = ["/wrong"]
        with (
            mock.patch.object(
                release,
                "inspect",
                side_effect=lambda reference: (
                    wrong if reference == expected[0] else image_document(reference)
                ),
            ),
            self.assertRaisesRegex(ValueError, "entrypoint"),
        ):
            release.validate_images(expected, IDENTITY)

    def test_canonical_archive_checksum_and_standard_load_validation(self) -> None:
        references = release.references(IDENTITY.release)
        first_source = self.root / "first.tar"
        second_source = self.root / "second.tar"
        write_docker_archive(first_source, references)
        write_docker_archive(second_source, references, reverse=True)
        first = self.root / "first"
        second = self.root / "second"
        first.mkdir()
        second.mkdir()
        release.canonicalize_archive(first_source, first / "images.tar.gz", references)
        release.canonicalize_archive(
            second_source, second / "images.tar.gz", references
        )
        release.write_checksum(first)
        release.write_checksum(second)
        release.verify_repeatable(first, second)

        with (
            mock.patch.object(release, "inspect", side_effect=image_document),
            mock.patch.object(release, "validate_upstream_versions") as versions,
            mock.patch.object(
                release, "run", return_value=completed(["docker"])
            ) as run_command,
        ):
            release.load_and_validate(first, IDENTITY)
        self.assertEqual(
            run_command.call_args.args[0],
            ["docker", "image", "load", "--input", os.fspath(first / "images.tar.gz")],
        )
        versions.assert_called_once_with(IDENTITY)

    def test_build_release_creates_only_the_canonical_pair(self) -> None:
        references = release.references(IDENTITY.release)
        nix_archive = self.root / "nix-image.tar"
        nix_archive.write_bytes(b"nix")
        output = self.root / "build"
        output.mkdir()

        def fake_run(
            arguments: list[str], *, check: bool = True
        ) -> subprocess.CompletedProcess[str]:
            del check
            if arguments[:3] == ["docker", "image", "save"]:
                destination = Path(arguments[arguments.index("--output") + 1])
                write_docker_archive(destination, references)
            return completed(arguments)

        with (
            mock.patch.object(release, "build_image", return_value=nix_archive),
            mock.patch.object(release, "inspect", side_effect=image_document),
            mock.patch.object(release, "run", side_effect=fake_run),
        ):
            release.build_release(output, IDENTITY)
        self.assertEqual(
            {path.name for path in output.iterdir()}, {"images.tar.gz", "SHA256SUMS"}
        )
        self.assertEqual(release.verify_directory(output)[1], references)

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

    def test_upstream_versions_are_read_from_images_without_network(self) -> None:
        with (
            mock.patch.object(
                release, "upstream_versions", return_value=("0.19.1", "1.37.2")
            ),
            mock.patch.object(
                release,
                "image_version",
                side_effect=(
                    '{"version":"0.19.1"}',
                    "Vaultwarden 1.37.2\nWeb-Vault Version file missing",
                ),
            ) as version,
        ):
            release.validate_upstream_versions(IDENTITY)
        self.assertEqual(version.call_count, 2)
        self.assertEqual(
            version.call_args_list[0].args,
            (
                f"vaultwarden-less-backup:{IDENTITY.release}",
                "/bin/restic",
                "version",
                "--json",
            ),
        )
        self.assertEqual(
            version.call_args_list[1].args,
            (
                f"vaultwarden-less-vaultwarden:{IDENTITY.release}",
                "/bin/vaultwarden",
                "--version",
            ),
        )

        for invalid in (
            "",
            "vaultwarden 1.37.2",
            "Vaultwarden 1.37.1\nWeb-Vault Version file missing",
            "1.37.2",
        ):
            with (
                mock.patch.object(
                    release,
                    "upstream_versions",
                    return_value=("0.19.1", "1.37.2"),
                ),
                mock.patch.object(
                    release,
                    "image_version",
                    side_effect=('{"version":"0.19.1"}', invalid),
                ),
                self.assertRaisesRegex(ValueError, "Vaultwarden image version"),
            ):
                release.validate_upstream_versions(IDENTITY)

    def test_artifact_evidence_is_canonical_private_and_hash_bound(self) -> None:
        pair = self.root / "pair"
        pair.mkdir(mode=0o700)
        candidate = pair / "candidate.json"
        candidate.write_bytes(b"candidate\n")
        candidate.chmod(0o600)
        built = self.root / "built"
        built.mkdir()
        (built / "images.tar.gz").write_bytes(b"archive")
        (built / "SHA256SUMS").write_bytes(b"checksum")
        release.write_artifact_evidence(
            candidate,
            "sha256:" + "c" * 64,
            A_COMMIT,
            IDENTITY,
            built,
            dt.datetime(2026, 8, 30, 10, 40, 0, tzinfo=dt.UTC),
        )
        artifact = pair / "artifact.json"
        expected = {
            "candidate_sha256": "sha256:" + "c" * 64,
            "from_commit": A_COMMIT,
            "to_commit": B_COMMIT,
            "release": IDENTITY.release,
            "files": {
                name: "sha256:"
                + hashlib.sha256((built / name).read_bytes()).hexdigest()
                for name in ("SHA256SUMS", "images.tar.gz")
            },
            "verified_at": "2026-08-30T10:40:00.000Z",
            "status": "verified",
        }
        self.assertEqual(artifact.read_bytes(), check_release.canonical_json(expected))
        self.assertEqual(stat.S_IMODE(artifact.stat().st_mode), 0o600)

    def test_create_runs_two_builds_then_loads_evidences_and_renames(self) -> None:
        output = self.root / "release"
        candidate = self.root / "candidate.json"
        events: list[str] = []

        def build(path: Path, identity: release.Identity) -> None:
            self.assertEqual(identity, IDENTITY)
            events.append("build")
            (path / "images.tar.gz").write_bytes(b"archive")
            (path / "SHA256SUMS").write_bytes(b"checksum")

        def rename(source: Path, destination: Path) -> None:
            events.append("rename")
            source.rename(destination)

        with (
            mock.patch.object(
                release, "release_pair", return_value=(A_COMMIT, IDENTITY)
            ) as pair,
            mock.patch.object(release, "validate_output", return_value=output),
            mock.patch.object(
                release,
                "validate_candidate",
                return_value=(candidate, "sha256:" + "c" * 64),
            ) as candidate_check,
            mock.patch.object(release, "build_release", side_effect=build) as builds,
            mock.patch.object(
                release,
                "verify_repeatable",
                side_effect=lambda *_: events.append("repeatable"),
            ),
            mock.patch.object(
                release,
                "load_and_validate",
                side_effect=lambda *_: events.append("load"),
            ),
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
                A_COMMIT,
                output,
                dt.datetime(2026, 8, 30, 10, 40, 0, tzinfo=dt.UTC),
            )
        self.assertEqual(result, output)
        self.assertEqual(
            events,
            ["build", "build", "repeatable", "load", "artifact", "rename", "sync"],
        )
        self.assertEqual(builds.call_count, 2)
        self.assertNotEqual(
            builds.call_args_list[0].args[0], builds.call_args_list[1].args[0]
        )
        self.assertTrue(
            all(call.args[0].parent == output.parent for call in builds.call_args_list)
        )
        self.assertEqual(pair.call_count, 3)
        self.assertEqual(candidate_check.call_count, 2)
        self.assertEqual(
            {path.name for path in output.iterdir()}, {"images.tar.gz", "SHA256SUMS"}
        )
        self.assertEqual(list(self.root.glob(".release.build-*")), [])

    def test_failure_cleans_only_task_temporaries_and_never_creates_output(
        self,
    ) -> None:
        output = self.root / "failed"
        unrelated = self.root / ".failed.unrelated"
        unrelated.mkdir()
        calls = 0

        def fail_second(path: Path, _identity: release.Identity) -> None:
            nonlocal calls
            calls += 1
            (path / "marker").write_text("task", encoding="ascii")
            if calls == 2:
                raise subprocess.CalledProcessError(1, ["nix", "build"])

        with (
            mock.patch.object(
                release, "release_pair", return_value=(A_COMMIT, IDENTITY)
            ),
            mock.patch.object(release, "validate_output", return_value=output),
            mock.patch.object(
                release,
                "validate_candidate",
                return_value=(self.root / "candidate.json", "sha256:" + "c" * 64),
            ),
            mock.patch.object(release, "build_release", side_effect=fail_second),
            self.assertRaises(subprocess.CalledProcessError),
        ):
            release.create_release(A_COMMIT, output)
        self.assertFalse(output.exists())
        self.assertTrue(unrelated.is_dir())
        self.assertEqual(list(self.root.glob(".failed.build-*")), [])


if __name__ == "__main__":
    unittest.main()
