# 0027 — Harness terminals run as a shell's typed-in job, not as the exec's own process

- **Status**: Accepted
- **Date**: 2026-07-31

## Context

A harness terminal (`terminal.Service.Create`) ran the resolved harness command
(e.g. `claude`) directly as the exec's own argv: `execs.Manager` PTY-exec'd it
with `Setsid`, making the harness process itself the exec's session leader. That
process group has no parent in its own session — it is permanently *orphaned* —
and the kernel unconditionally discards `SIGTSTP`, `SIGTTIN`, and `SIGTTOU` sent
to an orphaned group (`sandbox-agent/DESIGN.md`, "Signal frames..."). Ctrl-Z
typed by an attached user therefore did nothing at all: not "no shell to return
to," but the kernel refusing to stop the process in the first place.

This did not affect a plain `shell: true` exec, which already runs the login
shell as its own argv: pressing Ctrl-Z there stops a child job of that shell,
whose process group has a parent (the shell) in the same session and so is
never orphaned. The gap was specific to harness terminals, which never had a
shell in front of the harness at all.

The obvious-looking fix — resolve the login shell and pass the harness
invocation via `sh -c "harness ..."` (optionally forcing `-i` for interactivity)
— was tested empirically against a real PTY before being ruled out. Bash's
last-command exec optimization replaces the shell process outright whenever the
`-c` string reduces to one simple command, **even under `-i`**: `bash -i -c
'sleep 100'` leaves a single process (`sleep`, PID unchanged from the forked
bash) that is still its own session leader with an orphaned group — the exact
failure this change exists to fix, just reached a different way. Verified with
`pty.fork()`: `ps -o pid,ppid,pgid,sid` showed `sleep` as PID/PGID/SID all
equal, parented outside its session, and Ctrl-Z produced no state change at all.

## Decision

A harness terminal now launches the resolved login shell as its actual OS
process (`execs.CreateRequest.Shell: true`, the same mechanism a plain `shell:
true` exec already uses) and separately carries the harness invocation as
`StartupCommand`. Immediately after the shell process starts, the exec shim
writes that command — rendered as a single shell command line via
`execs.QuoteShellCommand` (POSIX single-quoting: the one quoting form bash,
zsh, dash, ksh, and fish all agree on) and terminated with a newline — into the
process's PTY input, exactly as if a user had typed it at the prompt.

This is deliberately *not* argv or `-c`: typed input goes through the shell's
normal interactive command-reading path, so the harness becomes a real foreground
job of an interactive shell rather than a `-c` command list that bash may
optimize away. The child's process group has the shell as a parent in the same
session, so it is never orphaned, and Ctrl-Z gets genuine job control — stop,
`jobs`, `fg`, `bg` — with the shell surviving underneath and handing back a live
prompt on suspend, same as a local terminal.

No readiness handshake is needed before injecting: a PTY in cooked mode queues
input at the kernel line-discipline layer until the reading process (the shell)
issues its next read, so writing immediately after the process starts is safe
regardless of profile/rc timing — the same reason a person can paste ahead into
a terminal before its prompt has drawn.

`StartupCommand` is a generic capability on the `execs` primitive, not a
harness-specific one — `execs.Manager` still never learns what a harness is.
The `shell` fallback harness (a plain login shell with no wrapped command) sets
no `StartupCommand`, since it already is the shell being launched.

Because `Command` now reports the shell (the literal argv executed) rather than
the harness invocation, `Exec`/`SandboxExec` gained a separate `StartupCommand`
field so a harness terminal is still self-describing: `command` answers "what
process is the exec," `startupCommand` answers "what is running in it."
CLI/API display prefers `startupCommand` when set.

## Consequences

**Consequence: terminal startup now pays login-shell cost.** A harness terminal
sources the shell's profile/rc files before the harness starts, where it
previously PTY-exec'd the harness directly. This is the same cost a plain
`shell: true` exec already pays and was judged acceptable for real job control.

**Consequence: `SandboxExec.command` changed meaning for harness terminals.**
It now reports the login shell argv instead of the harness command for any
terminal except the `shell` fallback. Existing consumers reading `command` to
learn "what harness command is this terminal running" must switch to
`startupCommand`; the CLI table and the sandbox-agent HTTP handler were updated
in the same change.

**Consequence: shell-quoting is POSIX, not shell-specific.** `QuoteShellCommand`
assumes the resolved shell accepts POSIX single-quote escaping. This covers
every shell `execs.ResolveShell` can currently resolve to (bash, zsh, dash, ksh,
fish); a shell with genuinely incompatible quoting would need dispatch logic
this design does not have.

**Consequence: startup-command injection failure fails the exec.** If writing
the typed-in command to the PTY fails, the shim terminates the just-started
shell and reports the exec as failed, rather than leaving a bare, unconfigured
shell running silently in place of the harness.

## References

- `sandbox-agent/DESIGN.md`, "Signal frames act on the exec's process group" —
  the orphaned-process-group rule this decision routes around, and the updated
  description of how a harness terminal now avoids it.
- `sandbox-agent/execs/shim_test.go`,
  `TestRunShimStartupCommandGetsRealJobControl` — pins the end-to-end behavior:
  a raw Ctrl-Z byte stops the typed-in child while the shell survives.
