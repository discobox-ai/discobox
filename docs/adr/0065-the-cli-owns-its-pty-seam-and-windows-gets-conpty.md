# 0065 — The CLI owns its pty seam, and Windows gets ConPTY

- **Status**: Accepted
- **Date**: 2026-08-19

## Context

The launcher's workspace screen runs `disco apply` in an overlay pane over the
discobox's live terminals ([ADR 0037](0037-drop-disco-diff-and-disco-status.md)
left apply as the only such command). From the list the same action suspends
Bubble Tea through `tea.Exec` and runs the command in-process on the real
terminal; from the workspace it cannot, because the terminals underneath are
connected and drawing and `tea.Exec` would tear the screen out from under them.
So the workspace re-execs `disco apply` as a child process on a pty of its own
and draws that pty in a pane (`cli/internal/cli/tui_local.go`).

The pty is not decoration. It is what gives the child a *controlling* terminal,
so anything the command starts that opens `/dev/tty` — git, a pager, a
credential prompt — reads its keys from the pane rather than from the real
terminal behind the window. It is also what makes the child believe it is on a
terminal at all, so what it prints is what a shell would show.

That pty comes from `github.com/creack/pty`, which the CLI uses for exactly
three symbols: `StartWithSize`, `Setsize`, `Winsize`. On Windows all three are
a stub — `start_windows.go` in v1.1.24 is `return nil, ErrUnsupported` — so
every local-command overlay fails at the moment it is asked for. `disco` is a
cross-platform CLI whose users are on Windows as much as on Linux, which
[ADR 0053](0053-iroh-is-development-only-until-it-builds-everywhere.md) already
took as a reason not to ship a capability that exists for only part of a team.

Windows has a pseudo-console, but it is not the same object and cannot be
reached through the same door:

- ConPTY is an `HPCON` handle plus two anonymous pipes — keys are written to
  one, VT output is read from the other. There is no master `*os.File`, so
  creack's return type is not merely unimplemented on Windows, it is
  unimplementable.
- `syscall.SysProcAttr` on Windows exposes no thread-attribute list
  (`$GOROOT/src/syscall/exec_windows.go`), and attaching a pseudoconsole to a
  child *is* a thread attribute (`PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE`, carried
  in a `STARTUPINFOEX`). **`exec.Cmd.Start` therefore cannot start a process on
  a pseudoconsole at all**, whatever library wraps it.

Everything needed to do it by hand is already in `golang.org/x/sys/windows` —
`CreatePseudoConsole`, `ResizePseudoConsole`, `ClosePseudoConsole`,
`NewProcThreadAttributeList`, `StartupInfoEx` — which the CLI already depends
on indirectly. No cgo, no new module.

Elsewhere in the tree `sandbox-agent` uses creack far more deeply (`procio`,
`shimruntime`: `GetsizeFull`, size jiggling, winsize plumbing). It runs inside
a Linux container and has no Windows question.

## Decision

### 1. A local command runs on a pty on every platform, or it does not run

The pane path keeps its pty rather than degrading to pipes where one is hard to
get. A command drawn in a pane with no controlling terminal is a different
command: its grandchildren read keys from the real terminal behind the window —
the exact failure the pane exists to prevent — and its output is what a program
prints when it believes nothing is watching. A pane that renders a plausible
but different `apply` is worse than one that says it cannot run.

### 2. The seam is `cli/internal/localpty`, and it starts argv rather than an `exec.Cmd`

```go
// PTY is a command running on a pseudo-terminal of its own.
type PTY interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

type Command struct {
	Path string   // the program, absolute
	Args []string // arguments after the program name
	Env  []string // complete, in os/exec's KEY=VALUE shape
	Dir  string
}

func Start(ctx context.Context, cmd Command, cols, rows int) (PTY, error)
```

`Start` takes a `Command` rather than an `*exec.Cmd` because the Windows
implementation cannot use `os/exec` (see Context), and a signature promising
`exec.Cmd`'s fields would be promising most of them falsely. Read and write are
two handles on Windows and one file on Unix, which is why the interface is
`io.ReadWriteCloser` rather than `*os.File`; `tui.Terminal` — what the pane
already consumes — needs nothing more. `Close` ends the command and releases
the pty. `Read` returns `io.EOF` when the child exits, on both platforms, so a
normal exit never reaches the pane as an error.

### 3. Unix delegates to `creack/pty`; only Windows is ours

The Unix implementation is a thin call to `pty.StartWithSize`. The open/grant/
unlock/ptsname sequence differs per operating system, the `Setsid`+`Setctty`
handoff is the subtle part of getting a *controlling* terminal rather than just
a terminal, and the `TIOCSWINSZ` struct layouts are per-architecture: ~450
lines of platform detail that works and that we have no argument with. Writing
our own would be reimplementing a dependency in order to own the twenty lines
we actually call.

### 4. The Windows implementation is ConPTY, with these constraints

They are the ones that are silent or hanging when got wrong, so they belong in
the decision rather than in a reader's notes:

- The child is started with `windows.CreateProcess` and a `STARTUPINFOEX`
  carrying a one-entry attribute list with the `HPCON`, plus
  `EXTENDED_STARTUPINFO_PRESENT`. The `STARTUPINFO`'s standard handles stay
  zero **and `STARTF_USESTDHANDLES` is set** — the arrangement that reads like
  the wrong one. Without the flag, `CreateProcess` copies the parent's own
  standard handles into the child and they outrank the console it was just
  attached to: the pseudoconsole is created, the child joins it, and everything
  it prints goes to whatever the window's stdout happened to be, with no error
  anywhere. With the flag and no handles the child starts with none, and the
  console it is attached to supplies them. *(Amended during implementation:
  this ADR was drafted saying the opposite, from the MS sample, which works only
  because that sample's parent has console handles to copy.)*
- The command line and the environment block are built here, because `os/exec`
  is not doing it: `windows.ComposeCommandLine` for argv, a UTF-16 double-null
  block for the environment. The API token continues to reach the child through
  the environment rather than the argument list.
- `Close` terminates the child and drains the output pipe before
  `ClosePseudoConsole`, which blocks until that pipe is empty. The wrong order
  hangs the window on the key that dismisses the pane.
- Reads report `ERROR_BROKEN_PIPE` where Unix reports `EIO`. Both map to
  `io.EOF` inside the implementation that knows which platform it is on, never
  in the pane.
- A pseudoconsole does not close when its client exits, the way a pty does when
  its last slave descriptor goes: it holds the write end of the output pipe, so
  a finished command would leave a reader waiting forever and a pane that never
  learns it finished. The process is therefore waited on, and the console closed
  when it exits — which flushes what the command last printed and ends the read.
  *(Amended during implementation; the draft assumed the Unix behaviour.)*
- Cancelling the context kills the child, matching what `exec.CommandContext`
  gives the Unix path.

ConPTY requires Windows 10 1809 (build 17763). Below it, `Start` fails and says
so; there is no fallback.

### 5. The seam is the CLI's, not a shared module

`sandbox-agent` keeps importing `creack/pty` directly. It is Linux-only, it
uses parts of that API this seam has no reason to carry, and generalizing a
two-implementation interface to cover both would make every future change to
either negotiate with the other. If a second cross-platform consumer appears,
promoting the package is a smaller move than un-sharing it.

### 6. A pty that cannot start is reported on the screen that asked for it

Every failure here is a message to a person looking at a screen: an unsupported
Windows version, a `CreatePseudoConsole` that failed, a child that would not
start. The workspace draws the model's status line, so these are legible where
they happen. This is written down because it was not true — the workspace's
frame carried only its key hints, and every report made from that screen went
to a field nothing on it rendered.

## Alternatives rejected

**Adopt a cross-platform pty library (`github.com/aymanbagabas/go-pty` or
similar).** The closest fit and a real option. Rejected because what it saves
is the ~200 lines of Windows code, while what it costs is a new dependency's
interface adopted wholesale into the path that runs our own CLI as a child —
for a caller that needs three functions and already has an interface of its own
in `tui.Terminal`. Any such library must invent the seam this ADR describes,
since no single signature spans both platforms; the choice was whose seam, not
whether there is one.

**Write our own Unix pty as well, and drop creack entirely.** Symmetry for its
own sake. The Unix half is the half that works, the half with the most platform
variance (six operating systems, a dozen architectures), and is already
vendored for `sandbox-agent`. Owning it buys nothing and puts `TIOCSCTTY` in
our maintenance path.

**Use plain pipes on Windows instead of a pseudoconsole.** Cheap, and wrong as
a destination: no controlling terminal for grandchildren, and no tty for the
child to detect, so `apply` renders as it would into a file. It is a smaller
thing wearing the pane's clothes. Whether it appears briefly on the way to
ConPTY is a sequencing question for the branch, not a decision this records.

**Make the workspace suspend like the list does — `tea.Exec`, no pane.** It
would delete the pty requirement outright and leave one code path for apply. It
also breaks the workspace's defining property: the terminals underneath stay
connected, unresized and undrawn, and are exactly where they were when the
command exits. Handing the real terminal over means a harness's output arrives
while nothing is drawing it, and the workspace that comes back is not the one
that left.

**Run apply in-process and render its output into the pane.** No child, no pty,
no Windows problem — but a grandchild that opens the console still reaches the
real terminal, and the pane would stop running the same program the list runs.
Two implementations of one command drift, and the one that drifts is the one
fewer people use.

**Report "not supported on Windows" and stop.** Defensible before the launcher
was the way most people use disco; not after. It leaves a key on the screen
whose only behaviour is to explain itself.

## Consequences

- `golang.org/x/sys` becomes a direct dependency of the CLI module.
- The Windows implementation is exercised only on Windows, so its tests are
  build-tagged and CI needs a Windows job to have covered it. Until it has one,
  the code is unverified on the single platform it exists for.
- `tui_local.go` stops importing `creack/pty` and stops building an `exec.Cmd`;
  `localCommand` holds a `localpty.PTY` and keeps its `tui.Terminal`
  adaptation. Nothing above it changes.
- ConPTY's output is not identical to a Unix pty's — it repaints regions and
  emits its own cursor positioning, particularly across resizes. The pane draws
  what it is sent, so this shows up as redraw noise rather than breakage, and
  is the cost of using the platform's own pseudo-console.
- The CLI gains its first hand-written Windows syscall code. Anything else that
  wants a local pty — a future command drawn in a pane — asks this package
  rather than growing a second one.
