# 0038 — Terminal identity is the exec id, and terminals revive in place

- **Status**: Accepted
- **Date**: 2026-08-13

## Context

A terminal is an exec created in harness mode (ADR 0032), and identity lives at
the exec-run level: every launch of a terminal mints a fresh exec id, a fresh
`discobox-exec-<id>` transient unit, and a fresh durable record. When a
sandbox restarts, the previous primary's transient unit does not survive the
reboot, reconciliation demotes its record to `lost`, and `EnsurePrimary`
launches the harness resume command as a *new* exec. The old record stays
behind ("deletion is what removes it", `agentstatus.ComputeSessionStatus`).

The consequences clients observe:

- `agentStatus.sessions` accumulates one dead `primary: true` entry per
  sandbox boot. A sandbox restarted three times reports four sessions for what
  a user thinks of as one terminal.
- A terminal's id is not a stable handle. The virtual `"primary"` exec id
  papers over this for the primary only — and only on the attach/start path;
  status reporting knows nothing of the alias, and a *non-primary* terminal
  that dies is unrecoverable under its own id: the client must create a new
  terminal and lose the old identity entirely.

The expectation this collides with: a terminal has an identity the way a tmux
session does. A client creates it, attaches to it, and if it dies, attaches to
the same id again to resume it. One-shot execs are legitimately fire-and-forget
runs; terminals are not. The per-run records are still wanted — but as an
audit trail, not as the session list.

Three things are conflated into the exec id today: the durable terminal
identity, the individual process run, and the audit/transcript record.

## Decision

The exec id of a terminal-mode exec **is** the terminal's durable identity, and
it is stable across process runs. A dead terminal is *revived in place* — under
the same exec id — rather than replaced by a new record.

1. **Revive reuses the record.** When a terminal-mode exec is `exited`,
   `failed`, or `lost` and a client attaches to or starts it, the terminal
   layer relaunches it: the harness's relaunch/resume command becomes the new
   `StartupCommand`, typed into a fresh login shell (ADR 0027), running as a
   new transient unit under the same exec id. The record's status returns to
   `starting`/`running`; its socket and runtime paths are unchanged (both are
   keyed by exec id).

2. **Each run gets its own transient unit.** Unit names carry a generation
   suffix (`discobox-exec-<id>-g<N>`); the record's `Unit` field always names
   the current generation. Reusing one unit name across runs was rejected:
   systemd retains transient-unit state after exit (a failed unit needs
   `reset-failed` before the name is startable again), and per-run units keep
   the audit trail's unit references unambiguous. The `discobox-exec-*`
   listing glob still matches.

3. **Revive is a terminal-mode behavior, owned by the terminal layer.**
   `execs.Manager` gains a generic relaunch primitive — restart an existing
   record with a new unit and (optionally) a new `StartupCommand` — and still
   never learns what a harness is, mirroring the `StartupCommand` precedent
   (ADR 0027). Plain one-shot execs keep today's semantics exactly: an ended
   exec replays while its shim lives and is `ErrSessionGone` after; it is
   never revived.

4. **The `"primary"` virtual id becomes a pure alias.** It resolves to *the*
   primary terminal record — there is only one, kept across boots — and
   attach/start through it revives that record like any other terminal id.
   `EnsurePrimary` on boot revives the existing primary record instead of
   creating a sibling. The durable launched-once marker
   (`PrimaryStateStore`) stays: it is what distinguishes the initial prompt
   from the resume command even if the primary record itself is deleted.

5. **Exec fields describe the current run.** `startedAt`, `exitedAt`,
   `exitCode`, `pid`, and `unit` report the latest generation. Per-run history
   lives where the audit trail already lives: the append-only event log
   (`exec.created` / `exec.started` / `exec.exited`) and the transcript store
   (ADR 0028), both keyed by the stable exec id and now spanning runs.

6. **`agentStatus.sessions` needs no filtering.** One record per terminal
   identity means the session list is exactly one primary entry plus one per
   manually created terminal, each showing its current run's state. A `failed`
   or `lost` session entry now means "not running, revivable by attach", not
   "gone forever".

## Alternatives rejected

**A separate terminal-identity layer** (`term_…` ids mapping to a current
`ex_…` plus a history of past ones). Rejected: it introduces a second id
namespace every client and the control plane must join across, directly
against the planned collapse of exec+terminal into a single `/execs` surface;
and the mapping layer's only job would be protecting `execs` from a change
that belongs in it.

**Reporting-time collapse** (filter superseded generations out of
`agentstatus`). Rejected: it fixes only the status symptom. Attach identity
stays broken — a non-primary terminal still cannot be resumed under its own
id — and clients that saw a terminal id before a reboot would find it renamed
after.

**Reusing the same unit name across runs** via `systemctl reset-failed`.
Rejected: see Decision §2.

## Consequences

**Consequence: `Unit` is no longer immutable on an exec.** Anything that
derives the unit name from the exec id instead of reading the record's `Unit`
field must be found and corrected as part of implementation.

**Consequence: reviving must fence the previous run.** The shim outlives its
command to serve replay; revive stops the old unit/shim (freeing the socket
path) before launching the new generation, so a replay-attacher's socket is
never yanked out from under a *live* run — only from an ended one.

**Consequence: existing sandboxes keep their historical duplicate records.**
Revive picks the newest primary record; older dead primaries from before this
change linger until deleted. Sandbox-local stores are disposable with the
sandbox, so no migration or backfill is performed.

**Consequence: transcripts and hook history span runs.** ADR 0028 transcript
chunks and harness hook records for one terminal id now cover every run of
that terminal. Session-state derivation already scans backward to the most
recent state-defining event, so it is unaffected; transcript readers gain
cross-restart continuity.

**Consequence: client guidance changes.** "Create a new exec to run it again"
stops being the recovery for a dead terminal; attaching to the same id is.
The attach-path error message and CLI behavior around dead sessions follow.
