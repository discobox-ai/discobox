"""Exercise the real harness and pinned Codex TUI without a model request."""
import ctypes
import fcntl
import json
import os
from multiprocessing import Pipe
from pathlib import Path
import pty
import re
import select
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time
import tomllib
import traceback

ROOT, CODEX, VERSION = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
ANSI = re.compile(rb"\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def stop_children():
    """Kill and reap children until even detached, reparented descendants are gone."""
    children = Path(f"/proc/self/task/{os.getpid()}/children")
    while True:
        pids = [int(pid) for pid in children.read_text().split()]
        if not pids:
            return
        for pid in pids:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        for pid in pids:
            os.waitpid(pid, 0)


def tui(argv, cwd, home, expected):
    """Give each scenario its own subreaper, including daemons in new sessions."""
    receiver, sender = Pipe(duplex=False)
    worker = os.fork()
    if worker == 0:
        receiver.close()
        failure = None
        try:
            # Linux PR_SET_CHILD_SUBREAPER: orphaned grandchildren become ours,
            # rather than PID 1's. This worker owns no unrelated subprocesses.
            libc = ctypes.CDLL(None, use_errno=True)
            if libc.prctl(36, 1, 0, 0, 0) != 0:
                errno = ctypes.get_errno()
                raise OSError(errno, os.strerror(errno))
            run_tui(argv, cwd, home, expected)
        except BaseException as exc:
            failure = exc
            failure.add_note(traceback.format_exc())
        finally:
            try:
                stop_children()
            except BaseException as exc:
                if failure is None:
                    failure = exc
                else:
                    failure.add_note(f"Process cleanup also failed: {exc!r}")
            sender.send(failure)
            sender.close()
            os._exit(0)
    sender.close()
    try:
        failure = receiver.recv()
    finally:
        receiver.close()
        os.waitpid(worker, 0)
    if failure is not None:
        raise failure


def run_tui(argv, cwd, home, expected):
    """Answer terminal queries, but never send Enter or accept a trust dialog."""
    pid, fd = pty.fork()
    if pid == 0:
        env = {k: v for k, v in os.environ.items()
               if not k.startswith(("CODEX_", "OPENAI_"))}
        env.update(HOME=str(home), CODEX_HOME=str(home / ".codex"),
                   TERM="xterm-256color", PATH=str(CODEX.parent) + ":" + os.environ["PATH"])
        os.chdir(cwd)
        fcntl.ioctl(1, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 160, 0, 0))
        os.execvpe(argv[0], argv, env)
    output = b""
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if select.select([fd], [], [], 0.1)[0]:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output += chunk
                if b"\x1b[6n" in output[-len(chunk)-8:]:
                    os.write(fd, b"\x1b[1;1R")
                text = ANSI.sub(b"", output).decode(errors="replace")
                compact = re.sub(r"\s+", "", text)
                if "Trustthisfolder?" in compact:
                    assert expected == "trust", text
                    print(f"PASS control: folder trust dialog in {cwd}", flush=True)
                    return
                # The composer is painted before onboarding. The configured
                # reasoning-effort footer appears only after initialization;
                # require that positive marker rather than a timed absence.
                if expected == "prompt" and "AskCodextodoanything" in compact and "low·" in compact:
                    (ROOT / ".tmp/codex-trust/last-screen.log").write_text(text)
                    print(f"PASS {argv[0]} {argv[1:]} in {cwd}: initialized prompt, no trust dialog", flush=True)
                    return
        raise AssertionError(f"did not reach {expected}: {ANSI.sub(b'', output).decode(errors='replace')}")
    finally:
        os.close(fd)
        # The scenario worker stops and reaps the whole tree, including this
        # PTY child, before reporting the scenario's result.


def main():
    assert command(str(CODEX), "--version") == f"codex-cli {VERSION}"
    # Propagation must be private before replacing any mount in this namespace.
    subprocess.run(["mount", "--make-rprivate", "/"], check=True)
    with tempfile.TemporaryDirectory(prefix="discobox-codex-trust-") as tmp:
        tmp = Path(tmp)
        etc = tmp / "etc"
        libexec = tmp / "libexec"
        etc.mkdir()
        (libexec / "harness-capture").mkdir(parents=True)
        # These mountpoints exist in a discobox; create them on an empty CI host.
        Path("/etc/codex").mkdir(parents=True, exist_ok=True)
        Path("/usr/local/libexec/discobox").mkdir(parents=True, exist_ok=True)
        subprocess.run(["mount", "--bind", str(etc), "/etc/codex"], check=True)
        subprocess.run(["mount", "--bind", str(libexec), "/usr/local/libexec/discobox"], check=True)
        (libexec / "harness-capture/settings.cjs").write_bytes((ROOT / "harness/capture/settings.cjs").read_bytes())
        (libexec / "codex-trust.cjs").write_bytes((ROOT / "harness/codex-cli/trust.cjs").read_bytes())
        baseline = (ROOT / "harness/codex-cli/system-config.toml").read_text()
        # Explicit approval/sandbox policies can suppress trust onboarding.
        # Remove those defaults in this sensitivity test so only trust can pass.
        baseline = re.sub(r'^(approval_policy|sandbox_mode) = .*\n', "", baseline, flags=re.M)
        (libexec / "codex-system-config.toml").write_text(baseline)
        (etc / "config.toml").write_text(baseline)
        launcher = tmp / "launch.sh"
        launcher.write_text((ROOT / "harness/codex-cli/launch.sh").read_text().replace(
            "/.discobox/data-per-source/primary", str(tmp / "no-source-data")))
        for name in ("repo", 'other.cwd with "quotes" $dollars and apostrophe\'', "no-source", "nested-repo/subdir"):
            cwd = tmp / name
            cwd.mkdir(parents=True)
            # Even a no-source box can later acquire a repository.
            repo = cwd.parent if name == "nested-repo/subdir" else cwd
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            home = tmp / (name + "-home")
            config = home / ".codex"
            config.mkdir(parents=True)
            preferences = 'model_reasoning_effort = "low"\n'
            (config / "config.toml").write_text(preferences)
            (config / "auth.json").write_text(json.dumps({"OPENAI_API_KEY": "sk-test-not-a-real-key"}))
            # Old quoted override key must fail against this very binary and
            # environment, or our positive result could be masked by policy.
            old = f'projects.{json.dumps(str(cwd))}.trust_level="trusted"'
            tui([str(CODEX), "-c", old], cwd, home, "trust")
            tui(["sh", str(launcher)], cwd, home, "prompt")
            tui(["sh", str(launcher), "--resume"], cwd, home, "prompt")
            tui([str(CODEX)], cwd, home, "prompt")
            saved = tomllib.loads((config / "config.toml").read_text())
            assert saved.get("model_reasoning_effort") == "low", saved
            assert "projects" not in saved, "trust leaked into user preferences"
            projects = tomllib.loads((etc / "config.toml").read_text())["projects"]
            assert projects[str(cwd)]["trust_level"] == "trusted", projects
        print(f"PASS pinned Codex {VERSION}: launch, restart, direct start; portable preferences unchanged")


if __name__ == "__main__":
    main()
