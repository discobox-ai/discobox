# WI-01 — Discobox ADR for the managed-resource layer

**Goal:** land the Discobox-side decision record that the managed-resource work
builds against.

Read `00-CONTEXT.md` first. **Start immediately; WI-03 and WI-08 build on the
outcome.**

## Why

`CLAUDE.md` requires an ADR when a plausible alternative was rejected for a
non-obvious reason, drafted as `Proposed`, landed on its own, and flipped to
`Accepted` as the decision gate before implementation. Three decisions in this
integration clear that bar comfortably, and two of them cut against existing
accepted Discobox ADRs. Getting them written down first is what keeps the
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

`docs/adr/0010-deletes-are-hard-deletes.md` is accepted: Discobox hard-deletes.
The managed layer needs the identity to persist through deletion so a repeated
`DELETE` continues the same deletion rather than starting a new resource
generation, and so out-of-band runtime loss can be reconciled instead of
appearing as "already gone". Decide whether this supersedes part of ADR-0010,
narrows it, or sits alongside it as a different resource kind with its own rule.
Do not quietly contradict an accepted ADR.

**3. Overcommit placement for managed sandboxes.**

`Store.SchedulablePoolForSandbox` (`server/internal/store/pools.go:275-307`)
currently refuses placement when a sandbox's requested CPU, memory, or storage
exceeds the pool's *instantaneous, agent-reported* available capacity. The
upstream ADR requires managed placement to be overcommit-only: a pool behaves
like the user's machine, all their agents share it, and starting another one is
not rejected because the sum of requests exceeds the envelope. Contention is
handled by pool QoS, not admission.

The genuine question is whether this becomes managed-only behavior or the model
for all pools. Branching leaves two scheduling semantics in one system, which is
a real cost. The overcommit rationale arguably applies to ordinary pools too —
see `docs/adr/0003-promote-pool-to-a-first-class-primitive.md` and
`docs/adr/0006-pool-is-the-runtime-host.md`, which already describe a pool as a
shared envelope and runtime host. Recommend one and say why the other was
rejected. WI-06 implements whatever this concludes.

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
3. Flip to `Accepted` only once the engineer agrees. That is the gate WI-03 and
   WI-06 wait on.
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
- Overcommit for managed pools only, or for all pools?
- Reject direct lifecycle commands on managed resources, or accept them as
  overridable temporary actions?

## Done when

- The ADR is committed as `Proposed`, reviewed with the engineer, and flipped to
  `Accepted`.
- `docs/adr/README.md` is updated if it maintains an index.
- `go tool task check-hooks` passes.
</content>
</invoke>
<parameter name="description">Write WI-01 ADR brief