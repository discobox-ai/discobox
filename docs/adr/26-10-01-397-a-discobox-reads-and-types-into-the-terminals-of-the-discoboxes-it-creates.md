# 26-10-01-397 — A discobox reads and types into the terminals of the discoboxes it creates

- **Status**: Accepted (§1's authority by creator superseded by [26-10-14-320](26-10-14-320-a-discobox-acts-on-any-discobox-in-its-project-and-the-judge-decides-which.md): the terminals of any discobox in the project)
- **Date**: 2026-10-01
- **Supersedes**: in [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  for the discoboxes the caller created and only for the four routes of §1:
  §4's refusal of execs and terminals; its deferral of "terminal screen,
  input, and wait … for a lead that drives its workers"; and its rejection of
  authority by creator, which
  [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md)
  lifted for source delivery only. 0140's other deferrals — start, stop,
  archive, exec create, and port tunnels — stand.

## Context

A discobox holding `ai.discobox.sandbox` can create workers, list them, and
answer their credential requests (ADRs 0140, 26-09-24-630, 26-09-30-782). It
cannot see what a worker is doing. `admin box ls` shows a worker's state, its
Git status, and whether its harness is busy, but not why a worker stopped: a
worker idle with uncommitted work is either between steps or waiting on a
question, and the lead cannot tell which, nor answer it. Every such stop goes
back to a person.

The person can already do this from their machine with
`discobox admin terminal screen`, `input`, and `wait` (ADR 0137). Those calls
are refused to a discobox by the sandbox role: `403 not in the sandbox role`.

## Decision

### 1. The role reads and types into the terminals of a discobox the caller created

The sandbox role gains, each allowed only when the named discobox's recorded
creator is the calling sandbox (the `createdSandbox` ownership of
ADR 26-09-24-630 §2):

- `GET sandboxes/{id}/execs` — list its terminals, so one can be named.
- `GET sandboxes/{id}/execs/{execId}/screen` — read what a terminal shows.
- `POST sandboxes/{id}/execs/{execId}/wait` — wait for a hook, quiet, or exit.
- `POST sandboxes/{id}/execs/{execId}/input` — type keys and text into it.

Each is one bounded request that the pool proxy judges against the lead's use,
as every other call to this API is.

The sandbox service's own scope check, which let a sandbox caller request only
the push's `sandbox:write`, also lets it request `exec:read` and `exec:write`,
the scopes these routes lease. `exec:write` also covers creating, starting,
and ending an exec, so the role's route list is what keeps those out; the
service still refuses a sandbox caller the legacy terminal, tunnel, and
sandbox-HTTP scopes, and refuses it the waiting acquire outright, which is how
an attach (and a create that waits to be used) reaches a sandbox.

### 2. Creating, ending, and attaching to execs stay out

`POST sandboxes/{id}/execs` (create), `DELETE`, `start`, and `attach` are not
added.

## Alternatives rejected

**Add `attach`.** It is a WebSocket: one judged handshake, then an unjudged
stream of keystrokes for as long as it stays open. Screen, input, and wait give
a lead the same reach one call at a time, and the judge sees each one.

**Add exec create and start.** Starting a new process in a worker is a second
way in beside its harness, with its own arguments and environment, and nothing
a lead driving the worker's agent needs.

**Allow input only to the primary (harness) terminal.** The primary is the one
a lead talks to, but a terminal is named by an exec ID or the virtual
`primary`, and the role decides by route, not by resolving which exec a name
means. The worker is the lead's own and holds only what the lead approved, so
another terminal in it reaches nothing more.

**Keep sending every stop to a person.** The person approved the lead to create
and run workers; making them attach to each worker to say "continue" defeats
the orchestration they asked for.

## Consequences

- A lead can see why a worker stopped and answer it, without a person.
- Input reaches whatever the terminal runs; through a worker's harness, a lead
  can have that worker run anything it could run itself. The worker holds no
  credential the lead did not approve for it.
- A discobox a person created records no creator, so no discobox can read or
  type into it.
- The in-box skills and the well-known credential's description say a lead may
  read and type into its own discoboxes' terminals.
