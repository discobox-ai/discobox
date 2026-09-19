# 0137 — A terminal can be read, typed into, and waited on without attaching

- **Status**: Accepted
- **Date**: 2026-09-15
- **Relates to**: [ADR 0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md),
  a terminal's identity; [ADR 0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md),
  what counts as activity; [ADR 0115](0115-exec-state-converges-on-notifications-not-a-poll.md),
  notifications over polling.

## Context

An agent that leads others drives each worker the way a person would: it reads the worker's
terminal, types into it, and waits for it to finish what it was asked. The only
way to do any of that today is the exec attach stream — a websocket carrying
raw terminal bytes, sized by whoever attached last, replaying escape sequences
a client has to run through a terminal emulator of its own before it can read a
word of them.

That shape fits a person's window. It does not fit a program that runs once and
exits: an agent reading a worker's screen from a one-shot command wants it as
text, and should not be the attacher whose size the worker's terminal takes.
Nor does anything say when a worker is done. A lead that cannot tell polls the
screen and guesses, spending a model call on every guess.

The pieces are already on the sandbox side. Each terminal's shim keeps a
terminal emulator fed from its PTY, so a late attacher can be repainted
(`shimruntime.screenBuffer`), and it writes attach input frames to the PTY.
Harnesses publish lifecycle hook events — Claude Code and Codex both emit `Stop`
when a turn ends — which the sandbox agent records.

## Decision

**A terminal exec gets three routes in the sandbox-agent API, served from its
shim and reached through the control plane like the exec routes beside them:
its screen as text, input written to it, and a bounded wait for something to
happen in it. None attaches, and none changes the terminal's size.**

### 1. The screen is text

`GET /api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/screen`
answers what the shim's emulator holds:

- `rows`, `cols`, and the cursor (`row`, `col`, `visible`);
- `title`, the window title the program last set;
- `lines`, the visible screen, one string per row, trailing blanks trimmed;
- `scrollback`, up to `?scrollback=N` lines above it, oldest first, bounded by
  what the shim keeps (`DefaultScrollbackLines`);
- `outputAt`, when the program last wrote output, and `exited`.

It is rendered from the emulator's cells, not by stripping escape sequences out
of the output stream, so what comes back is what a person looking at the
terminal would read: redrawn regions once, not every frame that drew them.

The shim serves it as `GET /screen` on its socket. An exec with no TTY has no
screen, and the route answers 409.

### 2. Input is written, not attached

`POST .../execs/{execId}/input` takes an ordered list of parts, each either
`text` or `key`:

```json
{"input": [{"text": "fix the failing test in pkg/foo"}, {"key": "Enter"}]}
```

- **`key` names one key**, from a fixed vocabulary: `Enter`, `Tab`, `Escape`,
  `Backspace`, `Delete`, `Up`, `Down`, `Left`, `Right`, `Home`, `End`, `PageUp`,
  `PageDown`, and `C-a` through `C-z`. An unknown name is a 400, never typed as
  text. The sequence a key sends honors the modes the program set — application
  cursor keys among them — which the shim already tracks.
- **`text` is delivered as a paste when the program enabled bracketed paste**,
  and as typed bytes otherwise. A paste keeps a multi-line message one message:
  typed, its first newline would submit it.
- The shim writes the parts, in order, to the PTY through the same path attach
  input frames take, as `POST /input` on its socket.

Input is use. It counts as access to the terminal the way typing into an attach
does, so a lead typing into a worker keeps it awake (ADR 0108). Reading the
screen is not, and does not — nor does it start a discobox that has stopped:
the pool serves a screen or a wait only while the discobox runs, as it serves
its audit trails (ADR 0130), so an orchestrator polling its workers never undoes
an idle stop. Typing into a stopped one starts it, as any use does.

### 3. Waiting is bounded, and the caller names what it waits for

`POST .../execs/{execId}/wait` blocks until one of the conditions it is given
holds, or its timeout passes:

```json
{"until": {"hookEvents": ["Stop", "Notification"], "after": "2026-09-15T12:00:00.123456789Z", "quietSeconds": 10, "exit": true}, "timeoutSeconds": 60}
```

- **`hookEvents`**: a harness hook with one of these event names, from this
  terminal, recorded after the resume point `after` (or after the call began).
  The events are matched where the hooks are read, so the one that ends a turn
  is found behind any number of tool-call hooks.
- **`quietSeconds`**: no output and no input for that long, counted from no
  earlier than the call. Output from before the wait says nothing about
  whether the program has answered what it was just sent, and a program just
  typed to has not had its chance to.
- **`exit`**: the exec has ended.

It answers which condition held — `hook` (with the record), `quiet`, `exit`, or
`timeout` — the terminal's `outputAt`, and `resumeAfter`: where the next wait
resumes. After a hook that is just past it; after anything else it is the point
this wait counted from, so a hook recorded while it returned is found by the
next. `input` answers with a resume point too, taken just before the input is
delivered, so a wait on what the input causes misses nothing recorded before
the wait began. `timeoutSeconds` is at most 60; a client waiting longer asks
again with the resume point it was last given, so nothing recorded between two
calls is missed. A resume point is an opaque string, passed back as given: an
API date-time is whole seconds, and a point rounded down to its second would
find again the hook it resumes after.

The wait is served from notifications the sandbox agent already receives, not
from a loop that polls (ADR 0115): the hook collector's records as they arrive,
and the shim's output and exit as it sees them — the shim is the one process
that holds the program, so its own exit signal is the one a wait answers on,
and a shim already gone reads as an exit.

**The caller names the events.** Which hook ends a turn, or asks a person
something, is a property of the harness — Claude Code's `Stop` and
`Notification`, Codex's `Stop` and `PermissionRequest` — and the orchestrator
driving a worker already knows which harness it started. The server holds no
mapping, and a harness with no hooks is still waited on by `quietSeconds` and
`exit`.

### 4. Where the routes sit

- They are sandbox-agent routes in `api/openapi/server.yaml`, marked
  `x-sandbox-agent`, and proxied by the control plane like the exec routes:
  `screen` and `wait` with `exec:read`, since both only observe the terminal,
  and `input` with `exec:write`.
- None writes state on the control plane.

## Alternatives rejected

**Capture, input, and wait in the client, over the attach stream.** Workable
today, with no server change: attach with replay, run the repaint through an
emulator, write an input frame, detach. Rejected as the design because every
client would re-implement the emulator, and because attaching is not free — the
terminal takes the size of whoever attached last, so a lead that captures a
screen reflows the worker's terminal for everyone watching it.

**Strip escape sequences from the exec log.** The log is every byte ever
written; a TUI redraws the same screen hundreds of times, and what survives
stripping is all of those redraws run together. Only an emulator knows what is
on the screen now.

**The server maps hook events to "turn ended".** ADR 0108 turned the same idea
down for idleness because only one harness had the mapping, and it would be the
server learning each harness's vocabulary. The caller already knows which
harness it is driving; putting the list in the request keeps that knowledge with
it.

**An unbounded long poll, or a streaming wait.** A wait that can hold a request
open forever pins a connection through every hop of the relay, and a stream is
the attach this ADR exists to avoid needing. A bounded wait resumed from where
the last one left off misses nothing and holds nothing.

**Text as keystrokes always.** A message with a newline in it submits at the
first newline in most TUIs. Bracketed paste is what the program asked for when
it enabled it.

## Consequences

- The shim gains `GET /screen` and `POST /input`; the sandbox agent gains the
  three routes and a wait that listens to the hook collector and to the shim's
  output and exit.
- A client needs no terminal emulator to read a terminal, and never reflows one
  it only reads.
- A lead's `wait` is as good as the harness's hooks: a harness that emits none
  is waited on by quiet and exit alone, which is a guess about being done.
- `cp` from inside a discobox is not part of this: it is an exec streaming a tar
  archive over the attach stream, which works through the relay as it is.
