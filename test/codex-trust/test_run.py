"""Regression coverage for the PTY runner's process cleanup."""
import os
from pathlib import Path
import runpy
import signal
import sys
import tempfile
import time
import unittest
from unittest.mock import patch


class TUICleanupTest(unittest.TestCase):
    def test_tui_stops_descendants_before_returning(self):
        root = Path(__file__).resolve().parents[2]
        with patch.object(sys, 'argv', ['run.py', str(root), sys.executable, 'test']):
            tui = runpy.run_path(str(root / 'test/codex-trust/run.py'))['tui']
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            pidfile = tmp / 'writer.pid'
            heartbeat = tmp / 'heartbeat'
            # Model Codex launching a daemon in a separate session.
            # A process-group kill must not accidentally satisfy this test.
            script = '''
import os, pathlib, signal, time
signal.signal(signal.SIGHUP, signal.SIG_IGN)
if os.fork() == 0:
    os.setsid()
    pathlib.Path('writer.pid').write_text(str(os.getpid()))
    while True:
        pathlib.Path('heartbeat').write_text(str(time.monotonic_ns()))
        time.sleep(0.005)
while not pathlib.Path('heartbeat').exists():
    time.sleep(0.005)
print('Trust this folder?', flush=True)
time.sleep(60)
'''
            try:
                tui([sys.executable, '-c', script], tmp, tmp, 'trust')
                before = heartbeat.read_text()
                time.sleep(0.05)
                self.assertEqual(heartbeat.read_text(), before,
                                 'descendant still writes temporary state after tui() returns')
            finally:
                if pidfile.exists():
                    try:
                        os.kill(int(pidfile.read_text()), signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_scenario_failure_survives_cleanup_failure(self):
        root = Path(__file__).resolve().parents[2]
        with patch.object(sys, 'argv', ['run.py', str(root), sys.executable, 'test']):
            runner = runpy.run_path(str(root / 'test/codex-trust/run.py'))
        def fail_scenario(*args):
            raise AssertionError('original scenario failure')
        def fail_cleanup():
            raise OSError('secondary cleanup failure')
        scope = runner['tui'].__globals__
        with patch.dict(scope, run_tui=fail_scenario, stop_children=fail_cleanup):
            with self.assertRaisesRegex(AssertionError, 'original scenario failure') as caught:
                runner['tui']([], root, root, 'prompt')
        self.assertTrue(any('secondary cleanup failure' in note
                            for note in caught.exception.__notes__))

    def test_cleanup_does_not_reap_unrelated_child(self):
        import subprocess
        root = Path(__file__).resolve().parents[2]
        with patch.object(sys, 'argv', ['run.py', str(root), sys.executable, 'test']):
            runner = runpy.run_path(str(root / 'test/codex-trust/run.py'))
        sibling = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
        try:
            with tempfile.TemporaryDirectory() as tmp:
                with self.assertRaises(AssertionError):
                    runner['tui']([sys.executable, '-c',
                                   "print('Trust this folder?', flush=True)"],
                                  Path(tmp), Path(tmp), 'prompt')
            self.assertIsNone(sibling.poll())
        finally:
            sibling.kill()
            sibling.wait()


if __name__ == '__main__':
    unittest.main()
