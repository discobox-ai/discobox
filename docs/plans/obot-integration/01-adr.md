# WI-01 — Discobox ADR for the managed-resource layer

> Status (checked 2026-09-11): not started — no managed-layer ADR exists in
> `docs/adr/`. Decision 3 below has since been settled by ADR 0029.

**Goal:** land the Discobox-side decision record that the managed-resource work
builds against.

Read `00-CONTEXT.md` first. **Start immediately; WI-03 and WI-08 build on the
outcome.**

## Why

`CLAUDE.md` requires an ADR when a plausible alternative was rejected for a
non-obvious reason, drafted as `Proposed`, landed on its own, and flipped to
`Accepted` as the decision gate before implementation. Three decisions in this
integration cleared that bar when this was written. The second cuts against an
existing Discobox ADR (0010); the third has since been settled for every pool by
ADR 0029 (see below). Getting them written down first is what keeps the
parallel work items from disagreeing with each other.

The upstream Obot ADR is accepted and specifies the cross-system contract. It is
*not* a Discobox ADR and does not record why Discobox chose its internal shape.
That is what this one is for.

## The decisions to record

**1. A managed resource layer over the concrete resources.**

Managed pools and sandboxes are separate persisted resources keyed by an
upstream-owned external ID, mapping to a concrete `Pool`/`Sandbox`. The
alternative — putting `external_id` and `external_revision` columns directly on
the concrete resources — is smaller, and the reason it was rejected is
non-obvious: a concrete sandbox may be *replaced* when an immutable field
changes, and the managed identity has to survive that replacement. Record this.

**2. Managed identity outlives the concrete resource.**

`docs/adr/0010-deletes-are-hard-deletes.md` records that Discobox hard-deletes,
and the code does (the ADR's status line still reads `Proposed`). Sandbox
deletion is also archive-then-confirmed-purge
([ADR 0022](../../adr/0022-sandbox-deletion-is-archive-then-confirmed-purge.md)).
The managed layer needs the identity to persist through deletion so a repeated
`DELETE` continues the same deletion rather than starting a new resource
generation, and so out-of-band runtime loss can be reconciled instead of
appearing as "already gone". Decide whether this supersedes part of ADR-0010,
narrows it, or sits alongside it as a different resource kind with its own rule.
Do not quietly contradict either ADR.

**3. Overcommit placement — settled since this was written; cite, do not re-decide.**

When this plan was written, `Store.SchedulablePoolForSandbox` refused placement
when a sandbox's requested CPU, memory, or storage exceeded the pool's
instantaneous, agent-reported available capacity.
[ADR 0029](../../adr/0029-sandboxes-have-no-per-sandbox-resource-requests.md)
(Accepted) has since removed per-sandbox resource requests and every capacity
comparison in `SchedulablePoolForSandbox` (`server/internal/store/pools.go`),
**for all pools**: placement checks only that the pool is unrevoked, desired
`present`, not `offline`, and agent-reported ready and schedulable. That is the
overcommit model the upstream ADR requires (a pool behaves like the user's
machine; contention is handled by pool QoS, not admission), with one scheduling
semantic rather than a managed-only branch. It also superseded ADR 0003 §4's
per-sandbox limits inside the envelope.

The Discobox ADR should cite ADR 0029 rather than decide this again. The
agent-reported `Available*` figures are still measured from the host
(`runtime.NumCPU()`, `/proc/meminfo` in `pool-agent/agent.go`), not from the
pool's cgroup, but nothing gates on them any more. WI-06 keeps the suspension
and envelope work.

## Also worth recording, if the engineer agrees

- **Managed resources reject direct user lifecycle commands.** The alternative
  is to accept them as temporary actions that a later managed `PUT` overwrites.
  Both are defensible; the choice affects WI-08.
- **Revisions are opaque equality tokens.** Worth one paragraph because the
  natural implementation instinct — comparing or ordering them, or deriving one
  from a hash of normalized config — is wrong and would break correlation.

## Scope

1. Draft `docs/adr/00NN-...md` in Nygard style, following the existing files in
   `docs/adr/` for tone and structure. Keep it short and directive; these are
   read by agents as much as by people.
2. Status `Proposed`. Land it on its own, with no implementation.
3. Flip to `Accepted` only once the engineer agrees. That is the gate WI-03 waits
   on (WI-06's placement half no longer does; see decision 3).
4. Do not write sequencing or implementation plans into the ADR — `CLAUDE.md`
   is explicit that those belong to the task doing the work.

## Out of scope

- Any code change.
- Restating the upstream Obot ADR. Link to it and record only Discobox's own
  decisions and rejected alternatives.
- `DESIGN.md` updates. Those land with the code, in the work items.

## Design questions for the engineer

- Does managed-identity-survives-deletion supersede, narrow, or coexist with
  ADR-0010?
- Overcommit for managed pools only, or for all pools? Settled: all pools
  (ADR 0029).
- Reject direct lifecycle commands on managed resources, or accept them as
  overridable temporary actions?

## Done when

- The ADR is committed as `Proposed`, reviewed with the engineer, and flipped to
  `Accepted`.
- `docs/adr/README.md` is updated if it maintains an index.
- `go tool task check-hooks` passes.
