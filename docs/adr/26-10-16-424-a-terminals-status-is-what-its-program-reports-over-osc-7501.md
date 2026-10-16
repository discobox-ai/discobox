# 26-10-16-424 — A terminal's status is what its program reports over OSC 7501

- **Status**: Accepted
- **Date**: 2026-10-16
- **Supersedes**: [ADR 0124](0124-a-terminal-is-active-while-what-it-shows-changes.md)
  §1, for an exec while it holds a reported status (§4 below). Execs that never
  report keep 0124 unchanged.

## Context

Nothing in Discobox knows whether the agent in a box is working, waiting for
its next instruction, or blocked on a person. A derived per-session `state`
(`running|idle|needs_input|…`) existed and was removed (`817895bd`). Nothing
read it, it cost a hook query per terminal per poll, and it was the last
harness-specific Go code: a table of Claude Code's hook events. The console's
"blocked" today means only a waiting credential request or a blocked pull
request (`cli/internal/tui/data.go` `standing`). The idle stop guesses activity
from screen changes (ADR 0124).

[OSC 7501](https://www.superlogical.com/rex/docs/build/program-status) (draft
0.4, Mitchell Hashimoto's Program Status Protocol) lets the program say it
itself. A report is `OSC 7501 ; state=…:kind=…:app=…:msg=<b64> ST`. The states
are `idle`, `working`, `done`, `blocked` (with `kind` `permission`, `question`
or `auth`) and `error`, plus `clear`. Each report replaces its record whole.
Records may nest under `/`-separated ids. A program detects support with
`OSC 7501 ; ? ST`, which a supporting terminal answers with `?`.

Claude Code (2.1.300) emits it, but only after that probe is answered. With no
reply it records "no reply to OSC 7501 ; ?" and never reports. It probes when
it starts. A primary terminal usually starts with nobody attached, where the
only terminal it has is the shim's screen emulator. That emulator answers DA1
and friends only while nobody is attached, and does not know 7501. Codex,
opencode and others have proofs of concept only. The protocol is the same for
every program, so supporting it is no harness's code.

## Decision

### 1. The shim answers the probe, always

The shim's screen emulator handles OSC 7501. It answers `OSC 7501 ; ? ST` with
`OSC 7501 ; ? ST` on the PTY whether or not a client is attached, unlike the
emulator's other query answers (`shimruntime/runtime.go`
`pumpScreenResponses`). A client attached at the time may have a terminal that
answers too. A second `?` reply after the probe has settled is harmless to the
program, and an unanswered probe is not. Termpane does not answer; the shim
already has.

### 2. The shim keeps the records, by the spec's rules

The shim parses every report from the output it already absorbs, applying the
spec's grammar, limits and discard rules. A report that breaks one, or whose
`msg`/`title` decodes to a control character, is dropped whole. Records live
in the shim beside the title, so they survive a sandbox-agent restart. They
are capped at the spec's minimum of 64 per exec. `clear` removes the addressed
record and those under it, or every record when it has no `id`. RIS removes
every record.

The spec ties a record's lifetime to the attached process and the shell
prompt. A harness terminal is a shell's typed-in job (ADR 0027), so the exec
outliving the harness is the normal case. The shim therefore samples the PTY's
foreground process group (`TIOCGPGRP`) when a report arrives, and again when
status is read:

- When the group that reported is no longer in the foreground, its `working`,
  `blocked` and `idle` records are dropped. That happens when the harness
  exited back to the shell, was suspended, or was killed.
- `done` and `error` stay until replaced or cleared, as the spec requires.
- An OSC 133 `A` prompt drops the same three, for a shell that marks its
  prompts.
- A new run of the exec (revive) starts with no records.

### 3. A reported status is the terminal's status

`SandboxAgentSessionStatus` (both `sandbox.yaml` and `server.yaml`) gains
`programStatus`, present only while the session holds a record:

- `state`, `kind`, `progress`, `app`, `title` and `msg` (decoded), with
  `reportedAt`.
- It is the root record. With no root, it is the most pressing child, in the
  order `blocked`, `error`, `working`, `done`, `idle`.

It reaches the server in the agent status the pool already pushes (ADR 0030),
as `runtime.agentStatus.sessions[]`. Where it is present it is the definitive
status of that terminal and its harness: nothing derived from screens, titles
or hooks overrides or supplements it. Where it is absent there is no agent
status, and nothing is guessed.

### 4. The idle stop reads it: only `working` is activity

For an exec holding a record, the record replaces 0124's screen change as its
activity:

- While the effective state is `working`, the exec counts as active now.
- `idle`, `blocked`, `done` and `error` are idle. **A box blocked on a person
  is idle**, so it stops at the timeout like any other quiet box.
- Every report is activity at the time it arrived, so a fall from `working`
  starts the idle clock then.

Attachers, last access, connections and leases count exactly as before. An exec
with no record keeps 0124.

### 5. `discobox ls` and the console list show it

- `discobox ls` gains an `AGENT` column holding the primary terminal's state:
  `working`, `blocked:<kind>` (or `blocked`), `idle`, `done` or `error`, and
  `-` with no record. JSON output already carries it through
  `runtime.agentStatus`.
- The console list draws it as a second glyph after the state dot, so both
  stay columns the eye runs down:

  | State | Glyph | Color |
  | --- | --- | --- |
  | `working` | `▶` | name |
  | `idle` | `◇` | dim |
  | `blocked` | `◆` | error |
  | `done` | `✓` | add |
  | `error` | `×` | error |

  No record draws a blank. Without color the glyph gives way to the word, as
  the state dot does. Under the cursor, a `blocked` or `error` record's `msg`
  takes the name's place, the way a box's error message does today.
- `blocked` makes the box's standing `blocked` (`Sandbox.standing`), beside a
  waiting credential request: an agent waiting on a permission prompt is as
  stuck as one waiting on a credential.

### 6. The console re-emits the focused box's status

The console probes its own terminal for OSC 7501 when it starts. If the
terminal answers, the console emits one root record on it: the status of the
focused box's primary terminal.

- It emits again on every change of focus. A box with no record is emitted as
  `state=clear`.
- It re-emits as that box's status changes. The console reads a change from
  the stream it is attached to when it has one, so it lags nothing, and from
  the polled status otherwise.
- The record is the console's own: it is re-encoded from the parsed fields,
  never relayed as bytes, with the box's display name as `title`.
- On exit the console clears it, beside the title reset in `termguard`.

Plain `discobox attach` passes the raw stream through as it does today, so a
supporting outer terminal sees the program's own reports.

## Alternatives rejected

- **Derive status from harness hooks.** This is what `817895bd` removed. It
  needs a per-harness event table and a query per terminal per poll, and it
  is a guess the program does not need anyone to make.
- **Read status from the title or the screen.** Codex puts its activity in the
  title, Claude Code does not. Scraping is per-harness and breaks on a
  restyle. It stays only as 0124's fallback for programs that report nothing.
- **Answer the probe only while nobody is attached, as with DA1.** A program
  started under an attached console or a non-supporting terminal would never
  report for its whole run, because it probes once.
- **Answer in termpane.** It covers only the console. The headless start, the
  common case, would still go unanswered.
- **Count `blocked` as activity.** That keeps a box running while nobody
  answers it. It is a box waiting on a person, and the auto-start latch brings
  it back when the person returns.
- **Re-emit every box as a nested record (`id=<box>`).** Records belong to a
  terminal, and the console's terminal is showing one box. Twenty boxes'
  records in one tab is an inbox, not a status.
- **Expose every record, not one effective record.** No consumer here draws a
  tree. The shim keeps them all, so the API can grow a list when one does.

## Deferred

- **Sleeping instead of stopping.** ADR 26-10-16-337 introduces sleep. Once
  it lands, a box that is idle because its agent is `blocked` or `idle` can
  sleep rather than stop at the timeout. That would also remove §4's cost of a
  lost prompt. It is a separate change; this one stops as the idle stop does
  today.
- **Pushing status on change.** The list and `ls` see a change at the pool's
  15s poll. Revisit if that lag shows in use, for example if a box sits
  visibly `working` in the list after it has blocked.
- **Notifications.** Spec-required rate limiting, and whether a `blocked` box
  out of focus should ring or notify, wait until re-emitting the focused box
  has been used.

## Consequences

- Claude Code reports from its first run under this shim. Other harnesses
  light up as they adopt the protocol, with no change here.
- A box stopped while `blocked` loses that prompt if its harness does not
  restore it on relaunch. This is the cost of §4, until sleep (see Deferred)
  replaces the stop.
- A program that reports `working` and hangs keeps its box running, because
  the spec has no heartbeat. Process exit and leaving the foreground still
  drop the record, so this is limited to a live, hung process.
- `msg` and `title` are program-controlled text shown in the list and
  re-emitted outward. They pass the spec's control-character check, and are
  drawn with bidirectional and invisible formatting disarmed.
