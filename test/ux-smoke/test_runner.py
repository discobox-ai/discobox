"""Smoke runner regression tests; no Docker daemon required."""
import importlib.util
from pathlib import Path
import subprocess
import sys
import urllib.error
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


class PurgeCleanupTest(unittest.TestCase):
    setUpClass = classmethod(BuilderReadinessTest.setUpClass.__func__)
    tearDownClass = classmethod(BuilderReadinessTest.tearDownClass.__func__)
    def failure(self, stderr, code=1):
        result = subprocess.CompletedProcess([], code, stdout="", stderr=stderr)
        return self.runner.CommandFailure("purge failed", result)

    def pending(self):
        return self.failure('failed to purge discobox "box": request failed: '
                            '409 Conflict: sandbox purge did not complete; '
                            'it will be retried in the background')

    def test_pending_purge_waits_for_absence_without_resending(self):
        runner = self.runner
        missing = urllib.error.HTTPError("url", 404, "not found", {}, None)
        state = {"desiredState": "deleted", "errorMessage": "removal in progress"}
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(runner, "ARTIFACTS", Path(directory)), \
                patch.object(runner, "cli", side_effect=self.pending()) as cli, \
                patch.object(runner, "api", side_effect=[state, missing]) as api, \
                patch.object(runner.time, "sleep"):
            runner.purge_box("box")
            cli.assert_called_once_with("admin", "box", "purge", "box", input="", limit=45)
            self.assertEqual(api.call_count, 2)
            self.assertIn("removal in progress", (Path(directory) / "box-purge.json").read_text())

    def test_success_needs_no_wait(self):
        with patch.object(self.runner, "cli"), patch.object(self.runner, "api") as api:
            self.runner.purge_box("box")
            api.assert_not_called()

    def test_other_purge_errors_still_fail(self):
        for error in (self.failure("403 Forbidden"), self.failure("409 Conflict: other conflict")):
            with self.subTest(error=error), \
                    patch.object(self.runner, "cli", side_effect=error), \
                    patch.object(self.runner, "api") as api:
                with self.assertRaises(self.runner.CommandFailure):
                    self.runner.purge_box("box")
                api.assert_not_called()

    def test_background_purge_is_bounded_and_keeps_failure(self):
        runner = self.runner
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(runner, "ARTIFACTS", Path(directory)), \
                patch.object(runner, "cli", side_effect=self.pending()), \
                patch.object(runner, "api", return_value={"errorMessage": "data busy"}), \
                patch.object(runner.time, "monotonic", side_effect=[0, 61]):
            with self.assertRaisesRegex(RuntimeError, "last sandbox:.*data busy"):
                runner.purge_box("box")
            self.assertIn("data busy", (Path(directory) / "box-purge.json").read_text())

    def test_observation_errors_are_not_absence(self):
        runner = self.runner
        for code in (403, 500):
            with self.subTest(code=code), \
                    patch.object(runner, "cli", side_effect=self.pending()), \
                    patch.object(runner, "api", side_effect=urllib.error.HTTPError("url", code, "error", {}, None)):
                with self.assertRaisesRegex(RuntimeError, "background purge.*failed"):
                    runner.purge_box("box")


class DiagnosticCleanupTest(unittest.TestCase):
    setUpClass = classmethod(BuilderReadinessTest.setUpClass.__func__)
    tearDownClass = classmethod(BuilderReadinessTest.tearDownClass.__func__)

    def run_cleanup(self, *, failed=False, teardown_fails=False):
        runner = self.runner
        calls = []
        reset = "recvfrom: connection reset by peer"

        def command(args, **kwargs):
            calls.append(args)
            if args[:2] == ["docker", "ps"]:
                return "first second"
            if args[:2] == ["docker", "logs"]:
                return "container output"
            if args[:2] == ["docker", "inspect"]:
                return "true"
            if args[:3] == ["docker", "exec", "first"]:
                result = subprocess.CompletedProcess(args, 128, stdout="", stderr=reset)
                raise runner.CommandFailure(reset, result)
            if args[:3] == ["docker", "exec", "second"]:
                return "second journal"
            if args[:2] == ["docker", "rm"] and teardown_fails:
                raise RuntimeError("container removal failed")
            return ""

        with tempfile.TemporaryDirectory() as directory, \
                patch.object(runner, "ARTIFACTS", Path(directory)), \
                patch.object(runner, "BOXES", ["box"]), \
                patch.object(runner, "POOLS", ["pool"]), \
                patch.object(runner, "CLI", ["cli"]), \
                patch.object(runner, "SERVER", None), \
                patch.object(runner, "BUILDER_CREATED", False), \
                patch.object(runner, "command", side_effect=command), \
                patch.object(runner, "cli", return_value="screen"), \
                patch.object(runner, "obj", return_value={"pools": []}), \
                patch.object(runner, "purge_box") as purge:
            if teardown_fails and not failed:
                with self.assertRaisesRegex(RuntimeError, "cleanup failed: container removal failed"):
                    runner.cleanup(failed)
            else:
                runner.cleanup(failed)
            self.assertIn(reset, (Path(directory) / "diagnostic-errors.log").read_text())
            self.assertEqual((Path(directory) / "second-journal.log").read_text(), "second journal")
            purge.assert_called_once_with("box")
            self.assertIn(["docker", "rm", "-f", "first", "second"], calls)
            if teardown_fails:
                self.assertIn("container removal failed", (Path(directory) / "cleanup-errors.log").read_text())
            else:
                self.assertFalse((Path(directory) / "cleanup-errors.log").exists())

    def test_journal_reset_is_diagnostic_and_collection_continues(self):
        self.run_cleanup()

    def test_teardown_failure_still_fails_successful_run(self):
        self.run_cleanup(teardown_fails=True)

    def test_cleanup_does_not_replace_original_failure(self):
        original = RuntimeError("original smoke failure")
        with self.assertRaisesRegex(RuntimeError, "original smoke failure"):
            try:
                raise original
            finally:
                self.run_cleanup(failed=True, teardown_fails=True)


if __name__ == "__main__":
    unittest.main()
