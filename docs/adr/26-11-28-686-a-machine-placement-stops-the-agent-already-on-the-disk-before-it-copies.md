# 26-11-28-686 — A machine placement stops the agent already on the disk before it copies

- **Status**: Accepted
- **Date**: 2026-11-28
  Accepted by the owner by merging #1847, 2026-11-28
- **Issue**: [#1846](https://github.com/discobox-ai/discobox/issues/1846)
- **Narrows**:
  [26-11-23-458](26-11-23-458-a-machine-sandbox-is-provisioned-in-place-and-only-docker-and-containerd-restart-around-its-mounts.md)
  §4's "where the agent was running on a placement that moved, apply
  restarts it". The stop half moves to the pool, before the placement's
  first copy (§1 below). Apply still starts the agent on what was placed.
- **Preserves**: the in-box content store's `Apply` and `collect`
  (`sandbox-agent/bundlestore/store.go`), unchanged; `agentlayer.Archive`'s
  entry order; the Docker runtime's placement before start; a snapshot
  build's placement into a fresh base.
- **Relates to**: [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md)
  §1 (the path unit that starts the agent on its bootstrap),
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
  (pins and the content store),
  [26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md)
  §4 (a machine placement needs systemd), #1115, discobox-ai/vm#239.

## Context

A machine sandbox keeps its disk across a stop. A repair keeps it as
well: the box is archived and then made again in place. Every start then
places into the *running* guest (`startLocked`,
`pool-agent/sandboxruntime/machinepower.go`). `place`
(`pool-agent/sandboxruntime/machine.go`) copies `sandbox.json` first,
then streams `agentlayer.Archive`. On a placement that moved, that stream
holds the new agent layer, then `CurrentLink`, `pins.json` and the units
(`pkg/agentlayer/agentlayer.go`). Only after the copy does `place` run
`discobox-provision apply --restart-agent`, and that is when the running
agent is stopped (`applyDeliveredBundles`,
`sandbox-agent/cmd/discobox-sandbox-agent/provision.go`).

By the time the copy starts, the agent from the earlier placement is
already running from the disk. Its units start it at boot, because
`discobox-sandbox-agent.path` sees the `sandbox.json` left on disk. If it
had not started yet, the bootstrap the copy writes first would start it.
That agent provisions the box: `provisionBox` → `fetchBundles` →
`Store.Apply` with the pins it booted with. `Apply` ends in `collect`,
which removes every layer that no kept tree uses, except the layer
`CurrentLink` names. The new layer is in none of the old pins' trees, and
`CurrentLink` names the old layer until the stream's later entries
re-point it. So `collect` removes `layers/<new>` while tar is still
extracting it. GNU tar's deferred `utime`/`chmod` then fail exactly as
#1846 reports. The triage's test in `sandbox-agent/bundlestore` shows the
mechanism.

It is not a second placement. The pool agent serialises create, start and
repair per sandbox (`sandboxLock`). The race is between the pool and the
agent the machine already holds. Any placement that moves the agent layer
on a machine whose disk carries an earlier placement can hit it: a repair,
or the stop and start a create makes when the pins moved. Archil's slow
`CopyTo` widens the window. A Docker sandbox is placed before it starts,
so it does not race.

The deleter is the agent that shipped earlier, read from the machine's
own disk. A fix that changes the agent or its store only helps once the
old agent already carries it. Boxes on today's agent would race on their
next repair.

## Decision

### 1. A placement that moved stops the agent's units before its first copy

When `place` will place something that moved, it stops the agent first,
before it copies `sandbox.json`. "Moved" is what `place` computes today:
no earlier placement on this instance, or another agent layer, other pins
or another bootstrap. It stops the agent through the guest, with
`systemctl stop` on `agentlayer.AgentStartUnit` and
`agentlayer.AgentServiceUnit`, the two units apply stops today. The stop
is the guest's systemd's, not the agent's, so it works on every agent
already on a disk, whatever its version. Each placement already checks
for systemd (`provisioner.RequireSystemd`, 619 §4). A machine with
neither unit loaded has nothing to stop, and that is not a failure.

Once the stop returns, nothing on the machine writes the store. With the
path unit stopped, the bootstrap the copy writes starts nothing. Apply
then starts the agent on what was placed, as it does today. Its own
`--restart-agent` stop finds nothing running, and it stays for agents
pinned older than this.

A placement that moved nothing stops nothing. It copies no layer, and its
running agent is the one it wants.

### 2. The agent stopped part way through its apply loses nothing

The old agent may be part way through `Apply` or a feature step when it
is stopped. The store already takes an apply that was cut short. Its state
is written atomically, and `collect` removes the leftovers of an
interrupted fetch or composition. Once started on what was placed, the
new agent applies the new pins over it. The agent being stopped had only
just booted on a machine that is being started, so nothing reaches it
yet. The restart that 458 §4 already makes ends its terminals anyway.

If the placement fails after the stop, the agent stays stopped and
`startLocked`'s `fail` stops the machine, as 458 §5 has it. On the next
start the machine boots on the old `CurrentLink`, since the stream
re-points it only after the layer. The old agent sweeps the unmarked
leftover layer, which is
`TestApplyFetchesAgainALayerWhosePlacementWasCutShort`'s case. The pool's
placement record has not moved, so the placement moves again and stops
the agent again first.

### 3. The test is the pool agent's

The regression test lives in `pool-agent/sandboxruntime`. A fake guest's
agent applies its pins when the bootstrap lands. A moved placement must
leave the layer it extracts whole, and the fake records that the stop came
before the first `CopyTo`. The triage's store-side test is not added. The
store's behaviour is what this decision keeps.

### Open, left to implementation

- How long the pool waits for the stop. The unit's own stop timeout
  applies inside the guest. Whether the pool's exec bounds it as well, as
  apply's `systemctl` steps are bound to a minute, is left open.
- Whether apply's `--restart-agent` stop goes once no supported pin names
  an agent from before this.

## Alternatives rejected

- **A lock in the in-box store.** `Apply` and `collect` would take a lock,
  and the placement's extraction would hold it. The old agent is the one
  that deletes, and it predates the lock, so it would never take it. Every
  box on today's agent would race on its next repair. Holding the lock
  across the extraction would also mean running the copy as an exec
  (`flock … tar -x`) instead of the driver's `CopyTo`, on every driver.
  And it serialises without removing the old agent's provisioning on the
  old pins, which runs beside the placement and is then killed by apply's
  restart anyway.
- **Extract the layer to a staging path `collect` never sweeps, and
  rename it into `layers/` once whole.** This does survive an old agent,
  which sweeps only `layers/` and `trees/`. But a tar stream cannot
  rename, and the rename cannot be apply's: apply runs from `CurrentLink`,
  which must not name the layer before it is in place. So the pool would
  run a shell `mv` and re-point `CurrentLink` in a second step. A rename
  cut short would leave a staged layer that nothing sweeps. And the old
  agent would still provision on the old pins during the placement.
- **Have `collect` skip layers without their `.complete` marker.** The
  old agent does not have the change. And an unmarked layer is exactly
  what a placement cut short leaves, which `collect` must remove
  (`TestApplyFetchesAgainALayerWhosePlacementWasCutShort`, #1115).
- **Write `CurrentLink` before the layer in `Archive`,** so that `collect`
  keeps the new layer. Then a placement cut short would leave
  `CurrentLink` on a layer that is not whole, and the next boot would run
  an agent that is not there. `Archive` writes the layer first for that.
- **Stop the machine and place before it boots,** as a Docker sandbox is.
  disco-vm places only through a running guest (26-10-09-143 §2). 458
  removed the restart around placement for its cost.

## Consequences

- A repair, or a start whose pins moved, on a machine sandbox no longer
  loses a layer to the agent already on its disk. This holds whatever
  agent version that disk carries.
- A moved placement costs one more exec before its copies.
- The window in which the old agent provisions on pins it is about to
  lose is gone, so its half-finished feature steps no longer run beside a
  placement.
