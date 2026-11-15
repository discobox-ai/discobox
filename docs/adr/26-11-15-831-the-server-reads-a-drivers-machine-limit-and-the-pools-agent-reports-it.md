# 26-11-15-831 — The server reads a driver's machine limit, and the pool's agent reports it

- **Status**: Proposed (would supersede [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md) §4's "boxd's org quota is not reported by disco-vm", and the Context line that boxd "does not report the org's machine quota"; its "a create past it fails at the provider and is reported as that failure" stands)
- **Date**: 2026-11-15
- **Issue**: [#1196](https://github.com/discobox-ai/discobox/issues/1196)
- **Depends on**: discobox-ai/vm#160 (disco-vm ADR 26-11-15-274), which adds `machine.Capabilities.MaxMachines`; this is built once discobox's disco-vm pin is past it
- **Preserves**: [26-11-11-080](26-11-11-080-each-disco-vm-driver-is-a-provider-type-of-its-own-name-configured-by-its-own-fields.md) §9 (a pool's machine authenticates in-machine where its driver can, and the provider's key is never placed on it); [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md) §4; [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md) §5 (a placement that moved is applied in place); the pool agent as the one writer of its conditions.

## Context

discobox-ai/vm#160 reports boxd's org machine limit as a new seam field,
`Capabilities.MaxMachines`: how many VMs of an OS may exist at once, stopped
ones and every org member's counted, with a create past it refused as
`ErrCapacity`. It is not `MaxRunning`: boxd's `max_vms` refuses a create,
never a boot. Unlike every other capability, it is learnt at `Check`, the
one seam call that does I/O; `Capabilities` stays I/O-free and returns it
only after a `Check` has read it. A credential the provider refuses the read
(boxd's machine token, which boxd does not document for this call) leaves no
cap. vm#160 is open; the pin in `server/go.mod` and `pool-agent/go.mod`
(`v0.0.0-20261115042309-1de8a13579e2`) has no such field.

Checked against `main` at `292a7021d`:

- **The `runtime` condition is the agent's.** `machinePoolStatus`
  (`pool-agent/machines.go`) builds it from `machines.Driver.Capabilities()`,
  read once at start in `pool-agent/agent.go`, and reports `maxRunning`
  beside the driver, hop, isolation, egress and commit. Nothing in the
  server reads `maxRunning`: it is for whoever reads the pool.
- **The agent's driver never runs `Check`.** The only `Check` is the
  server's, in `poolMachine.create`
  (`server/providers/discovm/poolmachine.go`), before it makes the pool's
  machine. A limit read there is on the server's driver only, and is gone
  after a server restart until the next create.
- **A boxd pool's agent authenticates in-machine.** boxd declares
  `inMachine`, so `poolKeys` (`server/providers/discovm/poolkey.go`) places
  no key and the bootstrap's `DriverConfig` turns the in-machine field on.
  The agent's driver holds a machine token, the credential vm#160 says may
  not read the limit.
- **Conditions are reported whole.** `UpdatePoolStatus`
  (`server/internal/store/pools.go`, from
  `server/internal/resources/pools/agent_service.go`) replaces the stored
  conditions with each report; the server writes nothing into them.
- **The placement is computed on every converge pass.**
  `poolMachine.converge` builds the bootstrap (`placement`) and compares its
  fingerprint (`poolruntime.PlacementFingerprint`, which hashes the whole
  `poolagent.Bootstrap`) with the one recorded in the pool's runtime state;
  a moved fingerprint is provisioned in place and restarts the pool's units.

So the value is readable only where the provider's key is, the server, and
shown only where the agent writes, the pool's `runtime` condition.

## Decision

### 1. The server reads the limit at `Check`, on every converge pass

`poolMachine.placement` runs the driver's `Check` before it builds the
bootstrap, not only `create`, and reads `Capabilities().MaxMachines` after
it. A `Check` that fails here does not fail the pass, and does not take a
running pool down: the limit read last stands (§2). `create`'s `Check`
still refuses a machine it cannot vouch for, as today; whether a running
pool should act on a `Check` that now fails (a host-bound secret added to the
org, say) is not this decision.

### 2. The value read is recorded in the pool's runtime state

`poolRuntimeState` gains the limit read (`maxMachines`, by guest OS, omitted
when none). A pass whose `Check` fails, or that runs before any `Check` has
answered since the server started, uses the recorded value, so a server
restart does not lose it. A `Check` that succeeds and reports none records
none: the provider said there is no cap, or that this credential may not
read one.

### 3. The bootstrap carries it, and the agent reports it

`poolagent.Bootstrap` gains `MaxMachines`, by guest OS, `omitempty`, set
from §2 for a machine pool. The agent puts it in its `runtime` condition as
`maxMachines` beside `maxRunning`, for the pool's sandbox OS, and leaves the
key out when the bootstrap has none. The agent's own driver does not run
`Check` for it.

It is in the placement's fingerprint like every bootstrap field. A pool on a
driver that reports none keeps the fingerprint it has. A boxd pool's
fingerprint moves once, on the first pass that reads its limit, and again
only when the org's limit changes: each is an in-place re-placement and a
restart of the pool's agent units, which leave its sandboxes' machines
running.

### 4. What it means

`maxMachines` is the provider's count, not the pool's: for boxd, every
machine billed to the org, stopped ones and every member's and every pool's
counted. It is reported, not enforced: nothing in discobox places or
schedules on it, as nothing does on `maxRunning`. A create past it is
refused by the provider and reported as that failure, as 26-10-09-106 §4
has it.

The pool agent's `DESIGN.md` documents the condition's `maxMachines`: read
by the server at the driver's `Check` with the provider's credential, absent
when the driver reports none or the credential may not read it.

## Open, for implementation

- **How often `Check` runs.** §1 runs it on every pass that computes the
  placement. If passes come often enough that two provider API calls each
  (boxd reads the limit and lists the org's secrets) matter, bounding it to
  an interval, with §2's record between, is the implementation's choice.
- **Which OS the condition names.** §3 reports the pool's sandbox OS's
  limit as one number, as the issue asks; if the implementation finds a
  reader that needs every OS's, it reports the map instead.

## Alternatives rejected

- **The pool's agent runs `Check` at start.** Simplest: the agent already
  reads its driver's capabilities. But every boxd pool's agent authenticates
  in-machine (26-11-11-080 §9), with a token vm#160 says may not read the
  limit, so it would report none on the one driver this is for. And boxd's
  `Check` now also refuses an org with host-bound secrets: an agent that ran
  it would either fail its start on them, a policy this issue does not ask
  for, or have to ignore a `Check` failure only to read a side effect of it.
- **The server writes `maxMachines` into the stored `runtime` condition.**
  No re-placement and no agent change. But the agent reports its conditions
  whole, so every report would need a merge of a server-owned key into an
  agent-owned object, in the store's report path, with the server's value
  surviving each report and an agent's never overriding it. That makes the
  condition two writers' for one field. The bootstrap is already how the
  server tells the agent what the provider's configuration says (`Driver`,
  `DriverConfig`, `MachineBase`).
- **Report it on the provider rather than on the pool.** The limit is the
  account's, so it arguably belongs there. But a provider has no reported
  status of its driver's capabilities today, and the issue asks for it beside
  `maxRunning`, itself a host's limit reported per pool. Revisited if a
  provider gains a status of its own.
- **Keep it out of the fingerprint.** A bootstrap field the fingerprint
  ignores never reaches a placed agent, which reads the bootstrap from its
  box; it would show only on the pool's next re-placement for another reason.
- **Place on it.** Closing a pool's scheduling at the org's cap would count
  machines discobox does not own (other members', other tools'), which no
  pool can see. Deferred below.

## Deferred

- **Scheduling against `MaxMachines`, and a create refused with
  `ErrCapacity` reported as a capacity wait rather than a failure.**
  Revisited when the provider can say how many of its machines exist now,
  not only the cap, so a pool can tell whether a create would fit.

## Consequences

- A boxd pool shows its org's machine limit in its `runtime` condition once
  the pin carries vm#160, and keeps showing it across a server restart.
- The server makes one more provider round trip (`Check`) per pool converge
  pass on a machine pool, unless the implementation bounds it.
- A change to the org's limit restarts each of its pools' agents once, in
  place.
- The issue's Done when holds for any driver: one that reports
  `MaxMachines[linux] == N` after `Check` shows N; one that reports none
  shows none.
