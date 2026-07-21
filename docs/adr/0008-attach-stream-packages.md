# 0008 — Attach stream is one protocol with two roles

- **Status**: Accepted
- **Date**: 2026-07-21

## Context

Exec and terminal attach is a duplex framed stream between a client's stdio and
a process inside a sandbox. It crosses four hops, but only the two ends speak
the protocol: the control-plane proxy and the sandbox-agent's websocket bridge
are byte pipes that never decode a frame.

The two ends are structured badly for their job:

- **The protocol is declared twice.** `sandbox-agent/terminal/frame` and
  `cli/internal/cli/attach_protocol.go` carry the same constants, the latter
  with a comment saying it mirrors the former. Renumbering the frames required
  editing both in lockstep; nothing but that comment prevents skew.
- **The roles are welded to their hosts.** Session logic lives in
  `internal/cli` against Cobra and `App`; host logic lives in `shimruntime` and
  `execs/shim.go` against `http.ResponseWriter` and a live process. Neither can
  be exercised without its embedder.
- **There is already a second implementation.** `internal/cli/tui.go` has its
  own frame read loop that silently ignores frame types it does not know.

Four bugs were found in one session, each in a different layer, none of which a
unit test could have caught in the current structure:

- `cmd.Wait` racing the output readers, discarding a fast command's entire
  output (`a7d504e`).
- The attacher joining the broadcast set after the `101`, losing anything
  written in that window (`384ed2b`).
- `SIGTSTP` discarded because every exec's process group is orphaned by
  `Setsid` (`4dbcc87`).
- A self-suspend that never stopped the process (`4dbcc87`).

Confirming them needed a live sandbox, `script` to allocate a PTY, `ps` polling,
and a 200-iteration stress harness. Nothing about the protocol requires that.

## Decision

Structure the stream as **one protocol, two roles, platform adapters
underneath**.

Root module — shared contracts, dependency-light:

| Package | Owns |
| --- | --- |
| `execstream/frame` | Wire codec, frame types, payload structs |
| `execstream` | `Conn` — the duplex frame contract |
| `execstream/client` | Attach stdio: raw mode, demux, resize, signals, suspend |
| `execstream/host` | Serve N attachers: fan-out, buffering, replay, exit retention |

Sandbox-agent — platform:

| Package | Owns |
| --- | --- |
| `procio` | Process I/O: PTY vs pipes, signal mapping, exit status |
| `…/screen` | vt-backed repaint, implementing `host.Replayer` |

Contracts:

```go
// The one duplex seam. Websocket, unix socket, and net.Pipe are interchangeable.
type Conn interface {
	ReadFrame() (frame.Frame, error)
	WriteFrame(typ byte, payload []byte) error
	Close() error
}

// Ready is called once the attacher is registered and buffering, before any
// frame reaches the wire. The HTTP adapter writes its 101 there.
func (s *Stream) Attach(ctx context.Context, conn Conn, opts AttachOptions) error

// What a new attacher is shown. TTY streams have one; pipe streams do not.
type Replayer interface {
	Observe(payload []byte)
	Snapshot() []byte
}

// Every OS-terminal interaction the client makes, in one place.
type Console interface {
	IsTerminal() bool
	MakeRaw() (restore func(), err error)
	Size() (cols, rows int, ok bool)
	Suspend() error
	NotifySignals(ch chan<- os.Signal, sigs ...os.Signal)
	StopSignals(ch chan<- os.Signal)
}
```

- **`Attach` takes a `Conn` and a `Ready` callback, not an
  `http.ResponseWriter`.** Registration happens inside `Attach` and the
  announcement is a callback it invokes, so "register before announcing" cannot
  be expressed wrongly. The bug in `384ed2b` becomes unrepresentable.
- **`Replayer` is an optional capability.** This is the exception the repo rule
  on optional interfaces allows: the capability genuinely may or may not exist
  at runtime, and today it is the `screen == nil` check that distinguishes TTY
  execs from pipe execs. It is not a shim to avoid updating implementations.
- **Reconnect is a `Conn` decorator**, not a `Conn` feature.
- **Policy stays at the edges**: the `-t` decision in `disco exec`, Ctrl-P
  Ctrl-Q detach, reconnect backoff, auth and pool routing, harness-configure
  sequencing. Moving policy inward is what turns a library into a framework.

## Alternatives rejected

- **Keep the duplicated frame constants.** Ten lines that look cheap to mirror.
  Rejected because the duplication is invisible to the compiler: the two
  declarations can disagree indefinitely and the first symptom is a corrupted
  stream at runtime, in a different module from the edit that caused it.
- **Put the new packages in a new nested module.** There is precedent — `cli`,
  `server`, `gormdb`, `hooks`, `pool-agent`, and `sandbox-agent` are all nested
  modules. Rejected because a module is the unit of independent release, and
  these packages have no release story of their own: the cost is a `go.mod`,
  `replace` directives in every consumer, and CI wiring, for no versioning
  benefit. The root module is already the shared-contract module and every
  consumer imports it. Revisit if root's dependency surface becomes a problem.
- **Put `host` and `procio` in the root module too.** Rejected because
  `creack/pty` and `charmbracelet/x/vt` would become dependencies of the module
  the CLI and server both import. `Replayer` and the `procio` split exist
  precisely so the shared module stays light; collapsing them would defeat the
  placement above.
- **Unify the audit log and the live stream behind one `Sink` interface.** They
  are two consumers of the same chunk, already decoupled — the disk log no
  longer backs replay. Rejected because it would shrink a two-line fan-out
  while hiding the invariant that actually matters: exit must not be observable
  until both sinks have drained.
- **Extract `client` first, since it is the most duplicated.** Rejected as
  ordering, not as a goal: it carries the highest user-visible risk, and the
  `frame` and `host` moves are mechanical and make the client extraction
  smaller when it happens.

## Consequences

- The root module gains `golang.org/x/term` as a direct dependency; it is
  already there indirectly.
- A wire-protocol change edits one file. The frame numbering and the CLI's view
  of it can no longer disagree.
- The TUI keeps its own frame loop, and should: `tui.Terminal` is an
  `io.ReadWriteCloser` for an embedded pane, which must not take raw mode,
  proxy signals, or suspend the process — Bubble Tea owns the terminal. It is a
  consumer of the protocol, not of the session. What it loses is the ability to
  drift: it decodes with the shared frame package instead of its own constants.
- The bug classes above become unit tests: `procio` covers pipe ownership, exit
  status, and signal mapping with no sockets; `host` covers registration
  ordering, buffering, and replay over `net.Pipe`; `client` covers suspend and
  raw-mode handling against a fake `Console` — including on macOS and Windows,
  where those paths can currently only be compile-checked.
- Breaking-change coordination is unchanged: the protocol still has no version
  negotiation, so the CLI and the sandbox-agent image continue to ship together.

## Deferred

- **The `client` extraction** is sequenced last, behind `frame`, `host`, and
  `procio` — not because its value is in doubt, but because it carries the only
  failure mode that tests do not catch: interactive terminals feeling wrong.
  The earlier steps shrink it.

  The evidence for doing it is already partly in: two of the four bugs above
  were client-side, and confirming them took a live sandbox, `script` to
  allocate a PTY, `ps` polling, and an A/B of three suspend implementations.
  The macOS and Windows paths cannot be exercised at all today — they are
  compile-checked and nothing more. A fake `Console` turns both into table
  tests on any host. Revisit at the latest when the next client-side bug lands
  or when a second consumer of the *session* appears; the TUI is not one.

- **A separate package for signal proxying and job control** is deferred with
  the `client` step, and is the part of it most likely to deserve its own
  boundary rather than living inside the session.

  The argument for splitting it: the platform half — which signals are
  forwarded, their wire names, and how a process actually stops and resumes —
  is small, forked per platform, and load-bearing in a way that is not
  self-evident. Two plausible implementations of the stop are silently wrong
  here. Restoring `SIGTSTP` to its default and re-raising does not stop a Go
  process that has notified it; that is also what `charm.land/bubbletea`'s
  `suspendProcess` does, so this repository already contains an implementation
  that would not work for this case. Only `SIGSTOP` stops, and only because it
  cannot be caught and is never discarded for an orphaned process group. Code
  whose correctness rests on facts like these earns isolated tests.

  The argument against splitting it further: the *sequencing* is attach-specific
  and must not move — stop the remote job, hand back the terminal in its
  pre-attach mode, stop, then retake the terminal, resume the remote, and
  re-send the window size. That ordering is a property of the stream, not of
  the platform, and belongs in the session next to the frames it writes.

  So the likely shape is a narrow platform package (signal set, wire names,
  suspend/resume primitive) with the orchestration staying in `client`. Decide
  when the `client` extraction is actually done, with the real seam in view.

- **Version negotiation on `Conn`** is deferred until mixed-version attach is a
  real requirement rather than a hypothetical one.

- **Audit-log capture policy** — input keystrokes are recorded unconditionally
  and `--include-input` gates only display — is a separate decision. It is
  noted here because the `execlog` boundary is where it would be settled, not
  because this ADR settles it.
