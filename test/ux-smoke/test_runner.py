"""Builder readiness regression tests; no Docker daemon required."""
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


class BuildsStarted(Exception):
    pass


class BuilderReadinessTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location("ux_smoke", Path(__file__).with_name("run.py"))
        cls.runner = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.runner)

    @classmethod
    def tearDownClass(cls):
        (cls.runner.ARTIFACTS / "empty.env").unlink()
        cls.runner.ARTIFACTS.rmdir()

    def check_builder(self, containerd, bootstrap_fails=False):
        runner = self.runner
        runner.BUILDER = "ux-smoke-test"
        runner.BUILDER_CREATED = False
        calls = []

        def command(args, **kwargs):
            calls.append(args)
            if args[:2] == ["docker", "info"]:
                return ('[["driver-type", "io.containerd.snapshotter.v1"]]'
                        if containerd else '[]')
            if args[:3] == ["docker", "context", "show"]:
                return "host-context\n"
            if "--bootstrap" in args and bootstrap_fails:
                raise RuntimeError("builder unavailable")
            return ""

        def build_stack():
            expected = "host-context" if containerd else "ux-smoke-test"
            self.assertEqual(calls[-1], ["docker", "buildx", "inspect", "--bootstrap", expected])
            self.assertEqual(runner.BUILDER_CREATED, not containerd)
            raise BuildsStarted()

        with patch.object(runner.shutil, "which", return_value="tool"), \
                patch.object(runner, "command", side_effect=command), \
                patch.object(runner, "build_stack", side_effect=build_stack) as builds:
            if bootstrap_fails:
                with self.assertRaisesRegex(RuntimeError, "builder unavailable"):
                    runner.main()
                builds.assert_not_called()
            else:
                with self.assertRaises(BuildsStarted):
                    runner.main()
                builds.assert_called_once()
        creates = [args for args in calls if args[:3] == ["docker", "buildx", "create"]]
        self.assertEqual(len(creates), 0 if containerd else 1)

    def test_fallback_ready_before_builds(self):
        self.check_builder(containerd=False)

    def test_host_ready_before_builds(self):
        self.check_builder(containerd=True)

    def test_failed_bootstrap_stops_builds(self):
        self.check_builder(containerd=False, bootstrap_fails=True)

    def test_command_keeps_progress_out_of_stdout(self):
        runner = self.runner
        with tempfile.TemporaryDirectory() as directory, patch.object(runner, "ARTIFACTS", Path(directory)):
            output = runner.command([sys.executable, "-c",
                                     "import sys; print('preparing the discobox', file=sys.stderr); "
                                     "print('personal-override')"])
            self.assertEqual(output.strip(), "personal-override")
            self.assertIn("preparing the discobox", (Path(directory) / "commands.log").read_text())


if __name__ == "__main__":
    unittest.main()
