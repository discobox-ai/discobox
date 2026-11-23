# 26-11-23-458 — A machine sandbox is provisioned in place, and only what holds a path it mounts is restarted

- **Status**: Proposed
- **Date**: 2026-11-23
- **Issue**: #1602
- **Would supersede in part**
  [26-11-02-023](26-11-02-023-every-machine-sandbox-mounts-its-volumes-from-its-own-data-and-cache-roots.md)
  §3's "the first placement … restarts the machine" and its rejection of
  "Mount on the running machine at the agent's start"; and
  [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md)
  §1's restart of a machine with no volumes "so that its agent boots on"
  what was placed.
- **Relates to**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md),
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md),
  [26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md)
  §4, discobox-ai/vm#218, discobox-ai/vm#219, #1060.

## Context

A machine sandbox is provisioned by placing what its agent boots on through
the running guest and running `discobox-provision apply`
(`pool-agent/sandboxruntime/machine.go`, `place`). When the machine was
never placed, or was placed with another agent layer, other pins or another
bootstrap, `place` copies the new layer, starts nothing and reports
`moved`. `startLocked` (`machinepower.go`) then stops the machine, starts
it again, and places a second time, so that `discobox-wire.service` mounts
the declared volumes at boot, ahead of everything that uses them.

ADR 26-11-02-023 §3 chose this because a machine boots before its bootstrap
is placed: its own services are already running on the declared paths.
Mounting under them would split their view of a path and lose what they
write while their data is moved. It rejected mounting on the running machine
because "the pool cannot know every unit of a provider's image that holds a
declared path open".

The cost is a full power cycle on every new machine sandbox and on every
start whose placement moved. On archil it is the third start of a new box.
The owner (2026-11-23): "We should not reboot a machine to provision it.
The provisioning should be aware of if it needs to take action and restart
any services."

Today, on the bases discobox runs:

- The declared paths are the platform's (`sandbox-agent/image.json`): the
  home, `/var/lib/docker`, `/var/lib/containerd`, `/var/lib/discobox` and
  `harness-capture` under it, `/nix` and three directories under it,
  `/home/linuxbrew/.linuxbrew`, and the home's caches. A harness adds its
  own, such as Claude Code's `/var/lib/claude-code`.
- What runs on them before the placement:
  - boxd's base runs `dockerd` and `containerd`.
  - discobox's sandbox base (archil's default) enables `docker.socket`, so
    `docker.service` and `containerd` start on first use.
  - `nix-daemon.socket` listens under `/nix/var/nix/daemon-socket`.
- On a start whose placement moved, the disk was already wired at boot. The
  old agent, its terminals and the sandbox user's processes are running too.

## Decision

### 1. The pool places once, and never restarts a machine to provision it

`place` always ends in `discobox-provision apply`. It no longer reports
`moved`. `startLocked` starts the machine at most once per start, and drops
the stop-and-start after a placement, the second placement, and
`restartedInOrder`/`restartedForced` with it.

A start still stops a machine for a reason other than provisioning: the
machine is given a new root (`upgradeMachine`, which needs a stopped
machine), or a failed start's `fail` stops it. This ADR does not change
either.

### 2. Apply works out what changed, and acts on that alone

`discobox-provision apply` on a Linux machine compares what was placed
with what is running, in three parts:

| Part | Changed when | What apply does |
|---|---|---|
| Mounts | A declared path or a source target has no mount this start (`sandboxconfig.MachineWiredFile` absent), is new to the bootstrap, or now comes from another volume (data/cache, scope) | Wires that path live (§3) |
| Agent layer | The layer under `/opt/discobox/current`, or a unit file it installs, differs from what the running units were started from | `systemctl daemon-reload`, then restarts each active unit whose file or binary changed (§5) |
| Agent | Its layer, pins or bootstrap differ from what the running agent started on, or it is not running | Restarts `discobox-sandbox-agent.service`, or starts its path unit as today |

Nothing changed: apply starts nothing, stops nothing and restarts nothing.
It still exits 0, so the pool records the placement as it does today.

A path the bootstrap no longer declares stays bound until the machine's next
boot. Its content is in the volume either way, and unbinding it would mean
stopping its holders for no gain.

### 3. Live wiring is a unit, ordered by what holds the paths

The live mount runs as a oneshot systemd unit that apply starts and waits
for, not as apply's own process. So it acts in PID 1's mount namespace,
whatever namespace the driver's exec runs in, and logs to the journal and
the console as `discobox-wire.service` does. The unit's name is open (Open).

It wires with the boot's mode (`wireMachine`): content is moved into the
volume and bound as 26-11-02-023 §2 says, entry by entry, with the layout
recorded last. It is not `wireMachineStarted`, which moves nothing. It
writes `MachineWiredFile` and, on a data volume, `MachineDataWiredFile`, as
a boot's wiring does. The next start finds the machine exactly as a boot
would have left it.

For the set of paths it is about to wire, in order:

1. **Find the holders.** Every process whose cwd, root, executable, an
   open file or a mapped file is at or under one of those paths (from
   `/proc`). Each is mapped to its unit by its cgroup. Every active socket
   or path unit that listens or watches under one of the paths is a holder
   too, with the service it activates.
2. **Stop them**, sockets and path units before their services, and
   dependents first as systemd orders a stop. The set is written to
   `/run/discobox/` before the first stop (§6).
3. **Check again.** Any process still holding a path fails the unit, and
   nothing is moved.
4. **Wire**: move and bind each path, parents before children.
5. **Start exactly the units it stopped**, in systemd's order, then remove
   the record.

The paths are known before anything is stopped, so a path with no holder
costs no stop. That is the common case on a new machine for everything but
Docker's and Nix's.

### 4. The services it may stop

What it may stop is whatever §3 finds, not a fixed list. On today's bases
that is, and only these are expected:

| Unit | Why it holds a declared path |
|---|---|
| `docker.socket`, `docker.service` | `/var/lib/docker`: boxd runs dockerd at boot; the sandbox base socket-activates it |
| `containerd.service` | `/var/lib/containerd` |
| `nix-daemon.socket`, `nix-daemon.service` | `/nix`, and its socket under `/nix/var/nix/daemon-socket` |
| `discobox-sandbox-agent.service` and `.path` | `/var/lib/discobox`, and the home through its terminals, on a start whose placement moved |
| `discobox-proxy-bridge.service` | runs the agent's binary; stopped with the agent when the layer changes (§5) |
| `user@<uid>.service` and the sandbox user's session scopes | the home, where a session outlives the agent |

It never stops:

- **The unit apply itself runs in**, or the driver's guest exec through
  which the pool reaches the machine. Either one holding a path fails the
  wiring, naming the unit and the path.
- **PID 1.**

Anything else found holding a path is stopped and restarted the same way,
and logged by name. That is the answer to 26-11-02-023's "the pool cannot
know every unit": it does not need to. The guest's kernel knows, and the
ordering is the units' own.

### 5. A changed agent layer restarts what runs from it

Units are installed from the layer (`agentlayer.MachineUnits`). After a
`daemon-reload`, apply restarts each long-running unit that is active and
whose unit file or `ExecStart` binary changed. Today that means:

- `discobox-sandbox-agent.service`
- `discobox-proxy-bridge.service`

A oneshot whose file changed (`discobox-render-proxy-env.service`,
`discobox-trust-ca.service`) is run again only if it had run this boot.
`discobox-wire.service` is never restarted live, because §3 is its live
counterpart. A restarted agent ends its terminals, as the machine restart
it replaces did.

### 6. A failure part way leaves what the next apply finishes

- Each step is idempotent: the moves are per entry, and the layout is
  recorded last (26-11-02-023 §2). An apply run again on the same placement
  converges.
- What was stopped is recorded in `/run/discobox/` before the first stop. An
  apply that finds the record starts those units once its wiring is done,
  even if it stopped none of them itself. On a tmpfs, the record goes with
  a reboot, and a boot starts every unit in its own order anyway.
- A wiring that fails leaves its holders stopped. Starting dockerd on a
  bare path would write the data the mount later hides, which is the split
  26-11-02-023 §3 avoids. Apply fails with the holder or the path that
  failed. The pool's `fail` stops the machine as today. The next start
  either boots it, and `discobox-wire.service` mounts at boot because the
  bootstrap is now placed, or applies again on a running machine.

No state is one only a reboot recovers.

### 7. A machine pinned to an agent that cannot wire live is restarted as today

Pins may name an agent older than this. Before applying, the pool asks the
placed agent whether it can wire live, the way `discobox-wire-machine.conf`
asks `wire --can-mount-machine`. An agent that cannot is placed, the machine
is restarted as before, and the pool places again. That path is removed once
no agent a pool can pin predates this (Deferred).

## Alternatives rejected

- **Keep the restart and make it cheaper** (discobox-ai/vm#218,
  discobox-ai/vm#219). Those cut the other two starts of a new archil box.
  This one is discobox's own, and a power cycle on a remote driver costs
  tens of seconds however it is tuned.
- **`systemctl soft-reboot`.** It restarts userspace without the kernel, so
  `discobox-wire.service` would mount in its boot order. Rejected:
  - It stops every service, and the guest exec channel the pool is placing
    through goes with them.
  - It needs systemd 254 or later, which a provider's own base need not
    have.
  - It is the reboot the owner asked to drop, made smaller.
- **A fixed list of units to stop around the mount** (docker, containerd,
  nix-daemon). Simple, and it is what §4 expects to find. Rejected as the
  rule because it is exactly 26-11-02-023's objection: a provider's base, or
  a feature, can run anything else on a declared path, and a missed holder
  silently splits its view. The kernel's answer is complete. The list is
  documentation of what it finds today.
- **Keep the provider's services off until a sandbox is placed**, with
  drop-ins that make docker and containerd wait for `sandbox.json`. Then
  the first placement would need no stop at all. Rejected:
  - The pool can place nothing in a machine before its first boot.
  - The base is the provider's: on boxd, its dockerd is up before any
    placement.
  - It does nothing for a start whose placement moved, where the old agent
    and its sessions hold the paths.
- **Mount under running services, moving nothing** (`wireMachineStarted`).
  26-11-02-023 rejected this, and the reason stands: content at a path
  would be hidden, and a running dockerd keeps writing to the disk under
  its bind.

## Open

- The live wiring unit's name, and whether it is a templated or transient
  unit, or a second `ExecStart` mode of an existing one.
- How apply records what the running units were started from (§2's "Agent
  layer" row): a digest beside the placed layer, or systemd's own
  `NeedDaemonReload` and the unit's start time against the layer's.
- Whether a holder outside any unit (a process systemd does not track) fails
  the wiring or is signalled directly. Nothing on today's bases is one.

## Deferred

- **Removing the restart for older agents (§7).** Revisit when the oldest
  agent a pool can be pinned to wires live.

## Consequences

- A new machine sandbox starts once. On archil fresh, archil from a
  snapshot, and boxd, the create loses one guest boot. On a new machine it
  gains at most a stop and start of Docker's and Nix's units.
- A start whose placement moved restarts the agent and the units that
  changed, not the machine. One with nothing moved restarts nothing.
- `place`'s `moved` result, `placeAfter` and the forced-stop record
  (`recordForcedPlacement`) go, except on the §7 path while it remains.
- `discobox-provision apply` gains a live wiring step on Linux machines.
  `wireMachineStarted` stays for an agent started by an older pool that
  placed no drop-in.
- `discobox-wire.service` and its machine drop-in are unchanged. They still
  mount at every boot after the first placement.
- `pool-agent/DESIGN.md` (Machine Sandboxes) and `sandbox-agent/DESIGN.md`
  describe placement as one start, with provisioning in place.
