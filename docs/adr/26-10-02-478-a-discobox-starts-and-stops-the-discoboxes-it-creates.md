# 26-10-02-478 — A discobox starts and stops the discoboxes it creates

- **Status**: Accepted (§1's authority by creator, and §2's exclusion of repair and upgrade, superseded by [26-10-14-320](26-10-14-320-a-discobox-acts-on-any-discobox-in-its-project-and-the-judge-decides-which.md))
- **Date**: 2026-10-02
- **Supersedes**: in [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  for the discoboxes the caller created and only for the three routes of §1:
  §4's refusal of start and stop; its deferral of "start, stop … for a lead
  that drives its workers"; and its rejection of authority by creator, which
  [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md)
  and [26-10-01-397](26-10-01-397-a-discobox-reads-and-types-into-the-terminals-of-the-discoboxes-it-creates.md)
  lifted for source delivery and terminals only. 0140's other deferrals —
  archive, exec create, and port tunnels — stand.

## Context

A lead discobox creates workers, answers their credential requests, and reads
and types into their terminals (ADRs 0140, 26-09-24-630, 26-09-30-782,
26-10-01-397). It cannot bring a stopped worker back. When the host's disk
filled, the workers stopped, and the lead had to ask a person to start each
one: `discobox admin box start` from a discobox answers
`403 not in the sandbox role`.

Typing into a worker's terminal, or listing its terminals, already starts a
stopped worker on use (ADR 0017 §12). That is a side effect, not a start: a
start that fails there is swallowed and the call fails on other terms, nothing
tells a lead it works that way, and it cannot stop anything.

## Decision

### 1. The role starts, stops, and restarts a discobox the caller created

The sandbox role gains, each allowed only when the named discobox's recorded
creator is the calling sandbox (the `createdSandbox` ownership of
ADR 26-09-24-630 §2):

- `POST sandboxes/{id}/start`
- `POST sandboxes/{id}/stop`
- `POST sandboxes/{id}/restart`

Each is an instruction the pool carries out in the request and answers with
its error, if any (ADR 0017 §9). Each is one call the pool proxy judges
against the lead's use, as every other call to this API is.

`restart` is in because it is exactly a stop and then a start, under the
pool's per-sandbox power lock: it reaches nothing the other two do not, and it
spares a lead unwedging a worker two judged calls with a gap between them.

The sandbox service needs no change for these. Unlike the push and the
terminals, a power instruction leases no scope on the caller's behalf: the
control plane signs it to the pool under its own `sandbox:write`, so the
service's scope check (`authorizeRequestedScopes`) is never consulted, and the
route's ownership is the whole check.

### 2. Archive, purge, repair, upgrade, and port tunnels stay out

They change what exists — the container, the image, the data — rather than
whether it runs. Repair and upgrade rebuild the container and discard
everything outside its durable tree; archive and purge remove it. Those are a
person's decisions about their discobox.

## Alternatives rejected

**Start only, not stop: a lead could stop a worker mid-task.** It already can:
typing Ctrl-C or an exit command into the worker's harness (26-10-01-397) ends
its work as surely, and the judge weighs that call as it would a stop. A stop
keeps the workspace and its uncommitted changes; a start resumes it. Leaving
stop out takes away nothing harmful and keeps a lead from putting down a
worker it has finished with or found wedged.

**Leave restart out.** It would send a lead to two calls for what the pool
already does as one, with no reach saved.

**Rely on on-demand start through the terminal routes.** It cannot report a
failed start, which is exactly the case — a full disk — that sent this back to
a person, and it gives no way to stop.

**Add repair too.** A worker left in an error state by the full disk needs it,
not a start. But a repair tears the container down and rebuilds it on a new
image, losing anything installed outside the workspace; it stays a person's
call. Revisit when a lead must recover workers that a start cannot.

## Consequences

- A lead brings back the workers that stopped under it, sees why one would
  not start, and stops the ones it is done with, without a person.
- A discobox a person created records no creator, so no discobox can start or
  stop it, nor itself.
- The in-box skills and the well-known credential's description say a lead
  may start and stop its own discoboxes.
