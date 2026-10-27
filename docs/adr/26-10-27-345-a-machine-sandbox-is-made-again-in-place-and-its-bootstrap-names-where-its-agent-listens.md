# 26-10-27-345 — A machine sandbox is made again in place, and its bootstrap names where its agent listens

- **Status**: Accepted (decided by the maintainer on 2026-10-27, delivering
  #124)
- **Date**: 2026-10-27
- **Narrows**:
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md)
  §1's bootstrap "never rewritten" until the container is rebuilt, for a
  machine, which is rebuilt in place (§1); and
  [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
  §3's "the sandbox agent listens where `guest.ServiceFile` says" (§2).
- **Relates to**: [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md),
  [0021](0021-upgrade-is-a-re-pin-and-preserves-power-state.md),
  [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md),
  disco-vm ADR 0005 and discobox-ai/vm#23.

## Context

A Docker sandbox is rebuilt when its spec moves, or when its primary source
declares a project layer it was not built with (ADR 0055): its container is
removed and a new one is built. Nothing is lost by that, because its durable
tree lives on the pool, in the volumes the container mounts.

A machine sandbox has no tree on the pool. Its disk is the sandbox: its home,
its checkouts, everything it made. Removing the machine to rebuild it would
throw all of that away. A machine could keep its data apart from its disk only
on a volume of its own, and disco-vm gives an instance no volume
(discobox-ai/vm#23).

ADR 26-10-23-551 §3 also has a machine sandbox's agent listen where
`guest.ServiceFile` says. That is disco-vm's file, which the engine writes into
the guest at every start. The sandbox agent reads none of disco-vm's files.
Only its bootstrap says where it listens.

## Decision

### 1. A machine sandbox is made again in place

A machine sandbox runs in one of two modes, by whether its data is on a volume
of its own:

- **No volumes** — every machine today. Rebuilding keeps the machine and its
  disk. The pool places the new bootstrap, the agent, its units and its pins in
  it, and restarts it so that its agent boots on them. The bootstrap is
  rewritten only by this rebuild, as a container's is replaced only when the
  container is.
- **A data volume**, with or without a cache volume. Rebuilding destroys the
  machine and creates a new one with the same volumes, as a container is
  rebuilt on its tree.

The pool keeps the whole bootstrap it placed, so it can also rebuild a machine
when nothing has sent a create, as when settling a started sandbox reads a new
project layer.

### 2. The bootstrap names where the agent listens

The pool asks the driver for the instance's service (`Service.Listen`), which
the driver keeps the same for every boot of an instance. It writes that address
into `sandbox.json` as the agent's listen address. The agent listens there, and
the instance's one URL reaches it. That is the same address `guest.ServiceFile`
holds. Only TCP is taken: no remote driver serves an instance on anything else.
A local driver's vsock stays behind the machine seam (551 §3).

## Alternatives rejected

- **Rebuild a machine as a container is, by destroying it.** The machine's disk
  is all the sandbox has, so every spec change and every new project layer
  would delete it.
- **Never rebuild a machine; refuse a spec that moved.** An upgrade (ADR 0021)
  or a project layer read on first settling (ADR 0055) would then never reach
  a machine.
- **The sandbox agent reads `guest.ServiceFile`.** It would tie the agent to a
  file of disco-vm's for a value the pool already reads from the driver and can
  place in the bootstrap it already writes.

## Deferred

- **The volume mode** (§1). Revisit when disco-vm gives an instance volumes
  that outlive it (discobox-ai/vm#23). The pool switches on whether a
  sandbox's machine keeps its data on one.

## Consequences

- `MachineSandboxRuntime` rebuilds a machine in place, keeps its bootstrap in
  its own record (`machine.json`), and restarts a machine whose placement
  moved.
- `sandbox.json` is rewritten in a machine whenever that machine is rebuilt.
