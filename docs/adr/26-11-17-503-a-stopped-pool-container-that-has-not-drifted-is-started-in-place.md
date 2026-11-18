# 26-11-17-503 — A stopped pool container that has not drifted is started in place

- **Status**: Accepted
- **Date**: 2026-11-17
  Accepted by the owner by merging #1312, 2026-11-18.
- **Issue**: [#1301](https://github.com/discobox-ai/discobox/issues/1301)
- **Relates to**: [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md) §5 (a pool is provisioned once and keeps what it provisioned), which this carries to a stopped Docker pool container; [26-11-15-294](26-11-15-294-a-report-after-the-reconcile-started-the-pools-agent-is-the-new-agents.md) §1 (its row for a provider that starts a box that was not running); [26-11-15-602](26-11-15-602-an-in-place-restart-is-recorded-before-it-begins-and-the-new-agents-first-report-counts.md) §§1, 3.
- **Preserves**: every reason a pool container is rebuilt today other than being stopped (a moved image, a moved `configRevision` label, a repair); the moved-bundles path for a running box (`applyMovedPoolBundles`); the upgrade watch and its boundaries; the VM lifetime rule that a VM dies with the server and its disks do not (`server/providers/DESIGN.md`, VM Lifetime).

## Context

On a plain server restart with a libkrun pool, `upgrading_pools` holds for
about 37s after the pool's VM starts (#1301, release log `server-kr200.log`).
Triage at `8529dd3f` found no missed report. The upgrade watch logged no
`pools still upgrading` line, so the pool waited on its own reconcile and
cleared when that settled. The time goes into rebuilding the pool container.

Checked against `main` at `8277972c3`:

- A libkrun VM stops with the server (`server/providers/libkrun/driver.go`).
  Its disks are kept, so the guest's Docker comes back with the pool's
  container present and **stopped**. No restart policy is set on it.
- `shouldRemoveExistingContainer` (`server/providers/dockerworker/engine.go`)
  reads a container that is not running as drift, alongside a moved image or
  label. `ensurePoolContainer` then removes it with `RemoveVolumes: true` and
  `createPoolContainer` makes and provisions a new box from scratch:
  `ContainerCreate`, `ContainerStart`, `poolruntime.ProvisionBox`, then the
  health wait. That took about 15–19s of the restart in two runs of the
  triage, and most likely the bulk of the release run's 37s.
- No ADR chose to rebuild a stopped container. The rule dates from the
  engine/driver refactor (116c4c509). 26-10-24-953 §5 says the opposite of
  the box: "A pool box is made rarely, and boots fast afterwards from its own
  disk or volume".
- A VM pool's machine already works that way. `poolMachine.converge`
  (`server/providers/discovm/poolmachine.go`) starts a stopped machine after
  `gate`, marks the pool started (`markStarted`), records
  `sandbox.AgentStartBox`, and provisions only if the placement moved. The
  Docker engine is the one provider that rebuilds a stopped box.
- The box is built to boot from its disk. The pool bundle's units
  (`discobox-pool-agent`, `discobox-proxy`, BuildKit, the registry, the
  mediator) are enabled (`WantedBy=multi-user.target`), so systemd (PID 1 in
  the container) starts them on boot. The agent writes the proxy's
  environment, CA and control key, and the builder's and registry's
  configuration, before it sends `READY=1`
  (`discobox-pool-agent.service`), and those units are ordered after it.
  BuildKit clears its own `net` directory before it starts.
- What the container is created with: `/run`, `/run/lock` and `/tmp` are
  tmpfs (`poolContainerSpec`), so a start begins them empty. The pool's
  identity key and its state trees are bind mounts from the host state root
  (`layout.MountRoots`, `PoolIdentityKey`), so the key outlives even a
  rebuild. The provisioned bundles, the bootstrap
  (`/etc/discobox/pool-agent/bootstrap.json`) and the policy environment are
  on the container's own disk. The `docker` named volume is the one thing a
  rebuild removes.
- The engine keeps a record of what it last placed in the box
  (`RuntimeState.Box`, `recordedBox`), with the container's ID
  (`RuntimeState.ContainerID`). `applyMovedPoolBundles` compares that record
  with what the release now names, and applies only a moved bundle or a
  moved placement.
- The local Docker driver's pool watcher already sends a stopped pool
  container to reconcile (`server/providers/docker/pool_watcher.go`). So a
  Docker pool whose daemon restarted, or whose host rebooted, takes the same
  rebuild path today.

## Decision

### 1. A stopped container that has not drifted is started, not rebuilt

In `ensurePoolContainer`, a pool container that is not running is **started
in place** when all of these hold:

- it is on the resolved image, and carries the desired labels (its
  `configRevision` among them), the same test a running container passes;
- it is the container the pool's runtime state names
  (`RuntimeState.ContainerID`), and that state has a recorded box
  (`RuntimeState.Box.Agent` set). That box was provisioned through to its
  record. A container left by a pass that died between create and record has
  no such record, and is rebuilt;
- Docker reports it `exited` or `created`. Any other state that is not
  running (`dead`, `removing`) is rebuilt;
- the reconcile is not a repair. `RepairPool` still forces a rebuild.

`shouldRemoveExistingContainer` stops treating "not running" as drift. A
stopped state is now a reason to start, and only the checks above decide
between a start and a rebuild.

### 2. What a start does, in order

1. **Close placement** with `begin`, as before any agent is replaced. The
   old agent's heartbeats can no longer admit work, and the pool row goes to
   `registering`.
2. **Mark the pool started** as a new box is (`recordPoolRuntime`'s
   `recreated` branch, the Docker twin of the VM pool's `markStarted`):
   `Ready`, `Schedulable` and `Degraded` false until the agent reports. The
   container keeps its ID.
3. **Record the start** with `sandbox.ReportAgentStarted(…,
   sandbox.AgentStartBox)` just before `ContainerStart`, as 26-11-15-294 §1's
   row for "starts a box that was not running" already says. No agent runs in
   a stopped container, so any ready report after the start counts
   (26-11-15-294 §2).
4. **Start it** (`ContainerStart`). It is already on the pool's sandbox
   network with its proxy alias, so nothing is reconnected.
5. **Wait for its agent** as for a new container: the healthcheck when the
   agent listens on IP, and container state otherwise
   (`waitContainerReady` with its wait). Systemd starts the units the box
   last provisioned.
6. **Apply what moved** through `applyMovedPoolBundles`, exactly as for a
   running box. A moved bundle or placement is applied in the started box and
   records its own in-place start (26-11-15-602 §1), which then becomes the
   latest start and is judged by 26-11-15-602 §3. Nothing moved means nothing
   is run, and the start in step 3 stays the boundary.

The pool is not re-enrolled on a start. Its key is on the host's identity
tree and unchanged, and the server's record of its public half is unchanged.
A start whose placement moved re-enrolls through `ProvisionBox`, as a
running box's apply does.

### 3. What a stop leaves stale, and how a start clears it

| State | After a stop | Cleared by |
| --- | --- | --- |
| `/run`, `/run/lock`, `/tmp` (tmpfs) | gone | the start: they begin empty |
| systemd units and their processes | stopped | systemd's boot, which starts every enabled pool unit |
| the proxy's environment, CA and control key; the builder's and registry's configuration | possibly from an older control-plane run | the agent, which rewrites them before `READY=1`, and before the units ordered after it start |
| bundles, bootstrap, policy environment on the container's disk | what the box last provisioned | step 6, when the release or placement now names something else |
| the identity key and the pool's state trees (bind mounts) | kept | nothing to clear: they outlive a rebuild too |
| the `docker` named volume | kept | nothing: a rebuild was what removed it |

The container's own create-time settings (its environment, mounts, ports,
healthcheck) are compared no more than they are for a running container.
Whatever changes those either moves `configRevision` and so rebuilds, or
travels in the placement and so is applied in step 6.

### 4. A start that fails falls back to a rebuild, in the same pass

The start is a fast path, never the only one. Placement is already closed
(step 1), so the pass goes on to remove the container, as today, and build a
new one, when:

- Docker refuses the start;
- the container is not running by the end of the health wait, or its
  healthcheck reports `unhealthy` by then.

A container whose healthcheck is still `starting` when the wait ends is
reported as `sandbox.ErrPoolNotReachable`, as an existing container's is
today, so the reconciler requeues it rather than repairing a host that is
still coming up. A start that fails is logged with its reason before the
rebuild, so the time it cost can be seen.

A step-6 apply that fails keeps the started box running what it last
provisioned and reports `sandbox.ReportBoxKept`, as for a running box
(26-10-24-953 §5). It is never a reason to rebuild.

### 5. Scope

This is the Docker engine's container path (`server/providers/dockerworker`),
shared by every provider built on it: the local Docker driver, and the
drivers that run the pool in Docker on a machine (`libkrun`, `vz`, `wslc`,
`execvm`, `digitalocean`). The VM pool machine
(`server/providers/discovm`) already starts a stopped machine, and is not
changed. The upgrade watch, the pool reconciler, the pool agent and the
bundles are not changed.

## Alternatives rejected

- **Keep rebuilding, and make provisioning faster.** Provisioning work only
  gets so cheap: fetching and verifying the bundles and laying every feature
  will always cost something, and a start does none of it. It is also the
  path that removes the `docker` volume. 26-10-24-953 §5 already names a fast
  boot from the box's own disk as the design. Its deferred snapshot of a
  provisioned pool is the other way to get one, and it is heavier than
  starting the disk that is already there.
- **Set a restart policy (`unless-stopped`) and let Docker start it.** A
  libkrun guest's dockerd would start the container on boot, before the
  server reconciles. The start would happen outside the reconcile, so it
  could not be recorded before it began (26-11-15-294 §1), and its old
  agent's first report could land before placement closed. It would also
  start a container that has drifted, only for the reconcile to remove it,
  and it would take back a stopped container from someone who stopped it on
  purpose.
- **Start every stopped container, and rebuild only one that then fails.**
  A container with no recorded box was never fully provisioned, so systemd
  would boot a half-laid box. The record (§1) costs one comparison and
  sends that case straight to the rebuild that will be needed anyway.
- **Repair a failed start in a later pass, not this one.** A pool with no
  sandboxes assigned is not repaired on a failed reconcile, it is retried,
  and a retry would start the same broken container again and again. The
  rebuild is what the pass would have done before this ADR, so falling back
  to it within the pass never leaves a pool worse off than today.
- **Re-provision the box on every start, whether or not anything moved.**
  It is the cost this ADR removes. The record already says whether the box
  is current, as it does for a running box.

## Consequences

A plain restart with a libkrun (or `vz`, `wslc`) pool, whose VM stops with
the server, waits for the VM to
boot, the container to start and systemd to bring up the agent, then for its
first report. It no longer waits for a fresh box to be provisioned. The
release-validation warm-stopped check is where the effect is measured
(#1301's Done when).

The same holds for a Docker pool whose container stopped because its daemon
restarted or its host rebooted.

A stopped pool keeps its `docker` volume across a server restart. Before,
the rebuild removed it.

A container that fails to start costs one failed attempt and the health wait
before it is rebuilt. That wait is bounded by the healthcheck (about 35s of
start period and retries) and happens only when the start has already
failed. It is logged.

`server/providers/DESIGN.md` says, with the code, that a Docker pool's
stopped container is started in place, and which cases rebuild it.

## Open

Left to implementation, not decided here:

- Whether the start lives in `ensurePoolContainer` beside the running-box
  branch or in a function of its own that `createPoolContainer`'s wait and
  record steps are shared with.
- How long the health wait in §4 is for a started container: the
  healthcheck's own bound, or `agentAfterApplyTimeout`.
- Whether an `exited` container whose last exit code says systemd failed
  (rather than being stopped) goes straight to the rebuild, without trying
  the start.
