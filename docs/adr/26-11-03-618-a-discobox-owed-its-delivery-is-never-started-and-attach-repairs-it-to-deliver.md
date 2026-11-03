# 26-11-03-618 — A discobox owed its delivery is never started, and an attach repairs a failed one to deliver it

- **Status**: Proposed
- **Date**: 2026-11-03
- **Issue**: [#70](https://github.com/discobox-ai/discobox/issues/70)
- **Would supersede**: [26-09-24-005](26-09-24-005-attaching-to-a-discobox-awaiting-its-source-delivers-it.md)
  §1's "parked in `awaiting_source`" and §2's "parked **and** has none" as
  the condition for delivering on attach. The condition becomes *owed*
  (§1 below), and a failed discobox that is owed its delivery is repaired
  first (§3). The rest of 26-09-24-005 stands: the attach delivers before it
  dials, only from the origin host, through `sandboxcreate.DeliverSource`, and
  §3's handling of overlapping deliveries is unchanged.
- **Would amend**:
  - [0035](0035-repair-is-one-rebuild-intent-plus-a-start-instruction.md):
    repair is one rebuild intent plus a start instruction, **unless** the
    rebuild leaves the discobox owed its delivery. Then it sends no start (§2).
  - [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md)
    §§1–3: an action, a start or a restart on a discobox owed its delivery
    is refused before the pool is asked to run it. That covers its ensure,
    its automatic repair and the instruction (§2).
- **Relates to**: [26-09-05-008](26-09-05-008-an-attached-client-pushes-the-commits-made-where-it-runs.md)
  §2, the automatic push's exclusion, which this ADR keys on *owed* rather
  than on runtime state (§1);
  [ADR 0058](0058-a-push-delivered-source-has-a-pool-side-origin.md), the
  origin a delivery pushes into.

## Context

A discobox whose source a client delivers parks in `awaiting_source` once it
is provisioned. It starts when the client pushes each such source at the
commit it was pinned to and reports the set complete
(`CompleteSandboxSourcePush`). The server records the report as
`sourceDeliveredAt`. If nobody reports within `sourcePushTimeout` (30 min),
the reconciler fails the discobox
(`parkForSourcePush`, `server/internal/resources/sandboxes/source_delivery.go`).
It stays failed with its container and with `sourceDeliveredAt` unset.

26-09-24-005 made an attach deliver a parked discobox. Issue #70 is a box that
went through the timeout and then a repair, and was left so that nothing in
the CLI could settle it. Every step that went wrong decides by the
discobox's *runtime state*. The fact that matters is whether a delivery is
*owed*. A discobox can be owed its delivery while `failed`, while parked, or
while a repair rebuilds it. Today, at `6f4d33e2`:

- **The automatic push** (`pushableNow`, `cli/internal/cli/push_auto.go`)
  refuses only `awaiting_source` and archived boxes. A failed, owed discobox
  is pushed into, and the push moves the branch the delivery still has to
  land on.
- **`discobox push`** (`cli/internal/cli/push.go`) treats a discobox as
  parked only when it reads `awaiting_source`. Against a failed, owed one it
  does an ordinary push of the current HEAD, not the delivery.
- **Repair** (`RepairSandbox`, `server/internal/resources/sandboxes/service.go`)
  reconciles, and that reconcile re-parks an owed discobox at
  `awaiting_source` with a fresh deadline (`createSandbox` →
  `parkForSourcePush`). Then repair sends `sandboxStart` anyway. The harness
  runs on a workspace the server says is not there, the box reads
  `starting` for good, and the next timeout fails a discobox that is in use.
  `UpgradeHarnessConfigSandboxes` already skips an owed discobox
  (`awaitingSourcePush`). Repair has no such guard.
- **An action** (`acquireForAction`, `attach_wait.go`) waits while
  `sandboxProvisioningPending`. That check is `!Converged() || State ==
  awaiting_source`, so it holds a parked box back. A failed box passes, and
  26-10-12-711 §2 says "a failed sandbox is always asked" to ensure-running.
  So any action on a timed-out, owed discobox can start its harness, by the
  same route as the repair. This was read in the code, not reproduced.
- **The attach** (`deliverableHere`, `cli/internal/cli/push_attach.go`)
  delivers only a discobox that reads `awaiting_source`. Once the timeout has
  failed one, the attach does nothing, and repair is the only way out.
- **The server refuses a completion** unless the discobox has never run
  (`sandboxHasNeverRun`: `pending` or `awaiting_source`). So a failed, owed
  discobox cannot simply be delivered as it stands. It has to be parked
  again first, and a repair is what parks it.

Bug 2 of #70 (a delivery refused as non-fast-forward because the branch is
already ahead) was fixed by `46bdf391`. `pushSource` skips the branch push
when this client's lease already reaches the pinned commit (`originHolds`).
This ADR does not revisit that fix (see Alternatives).

## Decision

### 1. Owed, not parked, is the signal everywhere

A discobox is **owed its delivery** when it has sources a client delivers and
no delivery has been reported: `awaitingSourcePush` on the server. On the
client, `sourceDeliveredAt` is unset and `sandboxcreate.PendingSourcePushes`
is non-empty. Each decision listed in Context keys on this, whatever the
runtime state:

- The automatic push never writes into an owed discobox. Only its delivery
  does.
- `discobox push` against an owed discobox performs the delivery (§3), never
  an ordinary push. Its refusals of `--source`, `--branch` and `--force`
  for a parked discobox apply to every owed one.
- An attach decides whether to deliver by *owed*, not by
  `awaiting_source` (§3).

`awaiting_source` keeps its meaning: provisioned and parked, waiting. It
stops being used as a stand-in for "owed".

### 2. The server never starts a discobox owed its delivery

No path in the server asks a pool to run a discobox that is owed its
delivery.

- **Repair** records the rebuild intent and reconciles as 0035 says. If the
  reconciled row is owed, it returns that row without sending
  `sandboxStart`. The row is parked at `awaiting_source` with a fresh
  `sourcePushTimeout`. The person who repaired it is told it is waiting for
  its source: `discobox repair` prints the parked row, and the launcher's
  repair result says so.
- **An action, a start or a restart** on an owed discobox that is not
  parked (failed, in practice) is refused before ensure-running or the
  instruction reaches the pool. The answer is a `409` saying the discobox
  is still owed its source, and that attaching from its origin host or
  `discobox push` delivers it. An owed discobox that *is* parked is waited on
  as today (`sandboxProvisioningPending`). It is refused rather than repaired
  automatically, because a repair would only park it again. Nothing would
  then deliver it, and the action's wait would run out.
- `CompleteSandboxSourcePush` keeps accepting a report only from a
  discobox that has never run. Once a repair has parked a failed discobox
  again, the report is accepted. A failed one has to be repaired first, and
  §3 does that.

### 3. An attach to a failed, owed discobox repairs it, then delivers, then dials

When a person attaches to a discobox that is owed its delivery, `failed`,
and otherwise deliverable from this machine under 26-09-24-005 §2 (its
origin host, not created by a sandbox), the client:

1. **Repairs it** (`RepairSandbox`). The rebuild converges in the request,
   and by §2 it leaves the discobox parked, not started.
2. **Delivers it**, through the same `deliverParkedSource` /
   `sandboxcreate.DeliverSource` as for a parked discobox.
3. **Dials**, as 26-09-24-005 §1 says.

Each step is narrated on the same busy or status line the delivery already
uses, and repairing is a step of its own. `discobox push` does steps 1 and 2
against a failed, owed discobox. A discobox that is parked and owed skips
step 1, as today.

The repair is not done when the failure cannot be the delivery's to answer.
A discobox created on another machine, or by a sandbox, or whose recorded
directories are gone, gets the refusal 26-09-24-005 §2 already gives, plus
the failure message. It is not rebuilt for a delivery that will not follow.
The check comes before the repair: everything `deliverParkedSource` resolves
and checks before pushing is resolved and checked before repairing.

The repair runs for any failure while owed, not only the timeout. An owed
discobox has never run, so a rebuild loses nothing of a person's. If the
cause is still there, the repair fails with it and the attach reports that,
which says more than the timeout did.

### 4. The beat still never delivers or repairs

26-09-24-005 §4 stands. The automatic push's beat does not deliver, and it
does not repair either. Under §1 it does not touch an owed discobox at all.

## Alternatives rejected

**Repair, then attach, as the documented two-step recovery.** With §2 alone,
this works end to end. The repair parks the box, and the next attach
delivers it under 26-09-24-005. It was rejected for the same reason
26-09-24-005 rejected failing the attach and naming `discobox push`: the
command it names does what the attach could have done, with no decision left
for the person. A failed, owed discobox has only one way forward from its
origin host. Attaching is asking for it by name, and asking to use it.

**Have the server accept a completion from a failed discobox, and let the
report itself re-converge it.** This would avoid the repair round trip. But a
report would have to start a rebuild intent on a failed row, which is a
second author of repair that 0035 and 26-10-12-711 do not have. And
`sandboxHasNeverRun` is the check that stops a stale or duplicated report
from moving a discobox that has gone on to other things. Repair already
re-parks an owed discobox with a fresh deadline, and that is exactly the
state a delivery needs.

**Let the source-await timeout leave the discobox parked instead of failing
it.** The timeout exists so that a discobox whose client never comes back
does not hold its pool's resources and read as provisioning for ever. A
person returning to it later still gets one attach to recover it (§3).

**Have the automatic repair on use (26-10-12-711 §3) repair an owed
discobox too.** Only an attach from its origin host can deliver it. Any
other action would rebuild it into a parked state that nothing then
delivers, and the action's wait would run out. Refusing with the recovery
named says more.

**On a refused delivery push, read the remote tip and accept a branch that
already descends from the pinned commit** (#70's suggestion for bug 2).
`46bdf391` already accepts this client's own later push, by its lease. Under
§1, nothing but a delivery writes into an owed origin, and two deliveries
push the same pinned commit (26-09-24-005 §3). A branch ahead of it with no
lease here would come from a writer the rules say does not exist. Accepting
it would hide that writer, not handle a case.

## Consequences

- Recovering a discobox whose create never delivered is attaching again from
  the machine that created it, whether it is still parked or the timeout has
  failed it. When it was failed, the attach costs one repair before the
  push.
- Repairing an owed discobox leaves it parked and says so. It no longer runs
  a harness on an undelivered workspace or reads `starting` for good.
- `discobox push`, the automatic push and the attach agree on when a
  discobox is owed. None of them writes an ordinary push into an origin that
  is still waiting for its delivery.
- An action against a failed, owed discobox from anywhere but its own
  attach is refused at once, with the way out named, where today it could
  start the harness.
- A discobox already in #70's state (parked, harness running, origin ahead
  from an earlier automatic push) is delivered by the next attach from the
  client whose lease covers that push, through `46bdf391`. Another client
  gets the non-fast-forward refusal and `discobox push --dir`, as today.
