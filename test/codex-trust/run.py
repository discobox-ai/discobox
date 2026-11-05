"""Exercise the real harness and pinned Codex TUI without a model request."""
import fcntl
import json
import os
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

ROOT, CODEX, VERSION = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
ANSI = re.compile(rb"\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def tui(argv, cwd, home, expected):
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
        os.kill(pid, signal.SIGKILL)
        os.waitpid(pid, 0)
        os.close(fd)


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
