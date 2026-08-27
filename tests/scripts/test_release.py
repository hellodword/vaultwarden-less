"""Contract tests for deterministic two-file image releases."""

from __future__ import annotations

import datetime as dt
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, os.fspath(REPOSITORY / "scripts"))

import release

COMMIT = "0123456789abcdef0123456789abcdef01234567"
IDENTITY = release.Identity(
    commit=COMMIT,
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
            "Labels": {
                "org.opencontainers.image.revision": IDENTITY.commit,
                "org.opencontainers.image.version": IDENTITY.release,
                "org.opencontainers.image.created": IDENTITY.created,
            }
        },
    }


def write_docker_archive(
    path: Path, references: list[str], *, reverse: bool = False
) -> None:
    manifest = json.dumps(
        [{"Config": "config.json", "RepoTags": references, "Layers": []}]
    ).encode()
    members = [("manifest.json", manifest), ("config.json", b"{}")]
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


class ReleaseScriptTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_clean_git_identity_and_dirty_rejection(self) -> None:
        timestamp = 1_788_000_000
        instant = dt.datetime.fromtimestamp(timestamp, dt.UTC)
        expected = release.Identity(
            commit=COMMIT,
            timestamp=timestamp,
            release=f"RELEASE.{instant:%Y-%m-%dT%H-%M-%SZ}-g{COMMIT[:12]}",
            created=f"{instant:%Y-%m-%dT%H:%M:%SZ}",
        )
        responses = iter(
            (
                completed(["git", "status"]),
                completed(["git", "rev-parse"], COMMIT + "\n"),
                completed(["git", "show"], str(timestamp) + "\n"),
            )
        )
        with mock.patch.object(
            release, "run", side_effect=lambda _arguments: next(responses)
        ):
            self.assertEqual(release.git_identity(), expected)

        with (
            mock.patch.object(
                release,
                "run",
                return_value=completed(["git", "status"], " M tracked\n"),
            ),
            self.assertRaisesRegex(ValueError, "clean Git worktree"),
        ):
            release.git_identity()

    def test_six_references_and_labels_are_exact(self) -> None:
        expected = [
            f"vaultwarden-less-{product}:{tag}"
            for product in release.PRODUCTS
            for tag in ("local", IDENTITY.release)
        ]
        self.assertEqual(release.references(IDENTITY.release), expected)
        with mock.patch.object(release, "inspect", side_effect=image_document):
            release.validate_images(expected, IDENTITY)

        wrong = image_document(expected[0])
        labels = wrong["Config"]["Labels"]
        assert isinstance(labels, dict)
        labels["org.opencontainers.image.revision"] = "f" * 40
        with (
            mock.patch.object(
                release,
                "inspect",
                side_effect=lambda reference: (
                    wrong if reference == expected[0] else image_document(reference)
                ),
            ),
            self.assertRaisesRegex(ValueError, "labels"),
        ):
            release.validate_images(expected, IDENTITY)

    def test_canonical_archive_checksum_and_loaded_identity(self) -> None:
        references = release.references(IDENTITY.release)
        first_source = self.root / "first.tar"
        second_source = self.root / "second.tar"
        write_docker_archive(first_source, references)
        write_docker_archive(second_source, references, reverse=True)

        first = self.root / "first"
        second = self.root / "second"
        first.mkdir()
        second.mkdir()
        release.canonicalize_archive(first_source, first / "images.tar.gz")
        release.canonicalize_archive(second_source, second / "images.tar.gz")
        release.write_checksum(first)
        release.write_checksum(second)

        self.assertEqual(
            (first / "images.tar.gz").read_bytes(),
            (second / "images.tar.gz").read_bytes(),
        )
        self.assertEqual(
            (first / "SHA256SUMS").read_bytes(), (second / "SHA256SUMS").read_bytes()
        )
        self.assertEqual(release.verify_directory(first)[1], references)
        with mock.patch.object(release, "inspect", side_effect=image_document):
            release.validate_images(references, None)

    def test_create_is_atomic_repeatable_and_refuses_overwrite(self) -> None:
        references = release.references(IDENTITY.release)
        nix_archive = self.root / "nix-image.tar"
        nix_archive.write_bytes(b"nix")

        def fake_run(
            arguments: list[str], *, check: bool = True
        ) -> subprocess.CompletedProcess[str]:
            del check
            if arguments[:3] == ["docker", "image", "save"]:
                output = Path(arguments[arguments.index("--output") + 1])
                write_docker_archive(
                    output, references, reverse=output.parent.name.endswith("2")
                )
            return completed(arguments)

        def create(path: Path) -> None:
            with (
                mock.patch.object(release, "git_identity", return_value=IDENTITY),
                mock.patch.object(release, "build_image", return_value=nix_archive),
                mock.patch.object(release, "inspect", side_effect=image_document),
                mock.patch.object(release, "run", side_effect=fake_run),
            ):
                release.create_release(path)

        first = self.root / "release-1"
        second = self.root / "release-2"
        create(first)
        create(second)
        self.assertEqual(
            {path.name for path in first.iterdir()}, {"images.tar.gz", "SHA256SUMS"}
        )
        self.assertEqual(
            (first / "images.tar.gz").read_bytes(),
            (second / "images.tar.gz").read_bytes(),
        )
        self.assertEqual(
            (first / "SHA256SUMS").read_bytes(), (second / "SHA256SUMS").read_bytes()
        )
        with self.assertRaises(FileExistsError):
            create(first)

    def test_subprocess_failure_removes_task_temporary_directory(self) -> None:
        output = self.root / "failed-release"
        with (
            mock.patch.object(release, "git_identity", return_value=IDENTITY),
            mock.patch.object(
                release,
                "build_image",
                side_effect=subprocess.CalledProcessError(1, ["nix", "build"]),
            ),
            self.assertRaises(subprocess.CalledProcessError),
        ):
            release.create_release(output)
        self.assertFalse(output.exists())
        self.assertEqual(list(self.root.glob(".failed-release.*")), [])


if __name__ == "__main__":
    unittest.main()
