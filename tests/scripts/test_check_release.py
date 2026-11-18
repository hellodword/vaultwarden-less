import contextlib
import importlib.util
import io
import os
import socket
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "check_release", Path(__file__).resolve().parents[2] / "scripts/check_release.py"
)
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="release inputs ")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bundle = self.root / "bundle with spaces"
        self.bundle.mkdir()
        (self.bundle / "compose.yml").write_text("services: {}\n")
        self.archive = self.root / "runner archive.tar.gz"
        self.archive.write_bytes(b"synthetic archive")
        self.socket = socket.socket(socket.AF_UNIX)
        self.addCleanup(self.socket.close)
        self.socket_path = self.root / "docker.sock"
        self.socket.bind(str(self.socket_path))
        self.args = [
            "--release-dir",
            str(self.bundle),
            "--runner-archive",
            str(self.archive),
            "--runner-image",
            "test-runner:explicit",
            "--work-dir",
            str(self.root / "work directory"),
            "--docker-socket",
            str(self.socket_path),
        ]

    def test_explicit_artifacts_socket_and_child_failure(self):
        ambient = {
            "DOCKER_HOST": "ssh://wrong-host",
            "DOCKER_CONTEXT": "wrong-context",
            "DOCKER_TLS_VERIFY": "1",
            "DOCKER_CERT_PATH": "/private/certs",
            "DOCKER_CONFIG": "/private/docker",
            "DOCKER_AUTH_CONFIG": "private-auth",
            "COMPOSE_FILE": "/unrelated/compose.yml",
            "PATH": os.environ["PATH"],
        }
        with (
            mock.patch.dict(os.environ, ambient, clear=True),
            mock.patch.object(release.subprocess, "run") as run,
            mock.patch.object(release.subprocess, "Popen") as popen,
            contextlib.redirect_stdout(io.StringIO()) as stdout,
        ):
            popen.return_value.stdout = io.StringIO("synthetic test failure\n")
            popen.return_value.wait.return_value = 23
            self.assertEqual(release.main(self.args), 23)
        load = run.call_args_list[0].args[0]
        setup = run.call_args_list[1].args[0]
        self.assertEqual(setup[-3:], ["test-runner:explicit", "1000:1000", "/work"])
        command = popen.call_args.args[0]
        self.assertEqual(load[:2], ["docker", "--config"])
        self.assertEqual(
            load[3:],
            [
                "--host",
                f"unix://{self.socket_path}",
                "load",
                "--input",
                str(self.archive),
            ],
        )
        self.assertEqual(command[:5], load[:5])
        self.assertIn(
            f"type=bind,source={self.bundle},target={self.bundle},readonly", command
        )
        self.assertIn(
            f"type=bind,source={self.socket_path},target={self.socket_path}", command
        )
        self.assertEqual(
            command[-6:-3], ["--release-dir", str(self.bundle), "--work-dir"]
        )
        self.assertEqual(command[-2:], ["--docker-socket", str(self.socket_path)])
        self.assertIn("test-runner:explicit", command)
        env = popen.call_args.kwargs["env"]
        self.assertEqual(env, {"PATH": ambient["PATH"], "DOCKER_CONFIG": load[2]})
        self.assertEqual(run.call_args.kwargs["env"], env)
        config = Path(load[2])
        self.assertFalse(config.exists())
        self.assertEqual(
            (config.parent / "results.log").read_text(), "synthetic test failure\n"
        )
        self.assertIn(str(config.parent / "results.log"), stdout.getvalue())
        self.assertIn("synthetic test failure", stdout.getvalue())

    def test_invalid_inputs_do_not_invoke_docker_or_create_work(self):
        for extra in (
            ["--docker-socket", str(self.archive)],
            ["--release-dir", str(self.root / "missing")],
            ["--runner-archive", str(self.root / "missing")],
        ):
            with (
                self.subTest(extra=extra),
                mock.patch.object(release.subprocess, "run") as run,
            ):
                with (
                    contextlib.redirect_stderr(io.StringIO()),
                    self.assertRaises(SystemExit) as error,
                ):
                    release.main(self.args + extra)
                self.assertEqual(error.exception.code, 2)
                run.assert_not_called()
                self.assertFalse((self.root / "work directory").exists())

    def test_load_failure_stops_before_runner(self):
        with (
            mock.patch.object(
                release.subprocess,
                "run",
                side_effect=subprocess.CalledProcessError(19, "docker"),
            ),
            mock.patch.object(release.subprocess, "Popen") as popen,
            self.assertRaises(subprocess.CalledProcessError) as error,
        ):
            release.main(self.args)
        self.assertEqual(error.exception.returncode, 19)
        popen.assert_not_called()


if __name__ == "__main__":
    unittest.main()
