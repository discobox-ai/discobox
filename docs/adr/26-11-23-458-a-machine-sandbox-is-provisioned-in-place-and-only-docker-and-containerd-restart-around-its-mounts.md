# 26-11-23-458 — A machine sandbox is provisioned in place, and only Docker and containerd restart around its mounts

- **Status**: Proposed (would supersede in part [26-11-02-023](26-11-02-023-every-machine-sandbox-mounts-its-volumes-from-its-own-data-and-cache-roots.md)
  §3's first-placement restart and its rejection of mounting on the running
  machine, and [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md)
  §1's restart of a machine onto what was placed)
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
  §4,
  [26-11-23-993](26-11-23-993-container-runtime-state-is-on-the-boxs-root-never-on-a-declared-volume.md),
  discobox-ai/vm#218, discobox-ai/vm#219, #1060.

## Context

A machine sandbox is provisioned by placing what its agent boots on through
the running guest and running `discobox-provision apply`
(`pool-agent/sandboxruntime/machine.go`, `place`). Suppose the machine was
never placed, or was placed with another agent layer, other pins or another
bootstrap. Then `place` copies the new layer, starts nothing and reports
`moved`. `startLocked` (`machinepower.go`) then stops the machine, starts it
again and places a second time. That makes `discobox-wire.service` mount
the declared volumes at boot, ahead of everything that uses them.

ADR 26-11-02-023 §3 chose this because a machine boots before its bootstrap
is placed, so its own services are already running on the declared paths.
Mounting under them would split their view of a path, and they would lose
whatever they write while their data is moved. That ADR rejected mounting on
the running machine because "the pool cannot know every unit of a
provider's image that holds a declared path open".

The cost is a full power cycle on every new machine sandbox, and on every
start whose placement moved. On archil it is the third start of a new box.

The owner (2026-11-23) set the scope:

> "the provisioning should be smart in that if it installs the data disk
> mounting stuff that it would know that it should restart service. but we
> will just reserve it to the known ones which I believe are just docker and
> containerd and don't worry about the rest."

Today, the services that run on a declared path before the first placement
are Docker's and containerd's:

- boxd's base runs `dockerd` and `containerd` on `/var/lib/docker` and
  `/var/lib/containerd`.
- discobox's sandbox base, which is archil's default, enables
  `docker.socket`, so `docker.service` and `containerd` start on first use
  (`server/providers/dockerworker/sandbox-image/Dockerfile`).

## Decision

### 1. The pool places once, and never restarts a machine to provision it

`place` always ends in `discobox-provision apply`, and no longer reports
`moved`. `startLocked` starts the machine at most once per start. These go:

- the stop-and-start after a placement;
- the second placement;
- `restartedInOrder` and `restartedForced`.

A start still stops a machine for a reason other than provisioning: to
give it a new root (`upgradeMachine`, which needs a stopped machine), or
because a failed start's `fail` stops it. This ADR changes neither.

### 2. Apply mounts in place only when the mounts change

On a Linux machine, `discobox-provision apply` mounts the declared volumes
and sources itself, in place, when either is true:

- This start has no mounts yet (`sandboxconfig.MachineWiredFile` is absent),
  as on a first placement.
- The placed bootstrap declares a path, or a source target, that this
  start's mounts do not have, or one that now comes from another volume.

It mounts with the boot's mode (`wireMachine`): content is moved into the
volume and bound as 26-11-02-023 §2 says. It writes `MachineWiredFile` and,
on a data volume, `MachineDataWiredFile`, as a boot's wiring does.

If the mounts are unchanged, apply mounts nothing and restarts nothing.
A path the bootstrap no longer declares stays bound until the machine's next
boot.

### 3. Docker and containerd are stopped before the mounts and started after

Docker and containerd are the only services apply restarts. It does so
only when it mounts (§2):

1. It records which of `docker.socket`, `docker.service` and
   `containerd.service` are active, in `/run/discobox/`.
2. It stops them. The socket goes first, so nothing reactivates Docker
   while its data moves.
3. It mounts.
4. It starts again exactly the ones that were active, then removes the
   record.

The mounting runs as a oneshot systemd unit that apply starts and waits for,
so its binds land in PID 1's mount namespace, whatever namespace the driver's
exec runs in. The unit's name is left to implementation.

No other service is stopped, restarted or looked for. There is no general
discovery of what holds a path, and no dependency graph.

### 4. The agent starts on what was placed

Apply starts the agent as it does today. Where the agent was running on a
placement that moved, apply restarts it on the new one. That is the restart
of the machine this replaces, narrowed to the agent itself. A restarted
agent ends its terminals, as the machine restart did.

### 5. A failure part way leaves what the next apply finishes

- The moves are per entry, and the layout is recorded last (26-11-02-023
  §2). An apply run again converges.
- An apply that finds §3's record starts the units it names once its
  mounting is done.
- A mount that fails leaves Docker and containerd stopped. Starting them on
  a bare path would write data the mount later hides. The pool's `fail`
  stops the machine as today. The next start either boots it, and
  `discobox-wire.service` mounts at boot, or runs apply again on a running
  machine.

### 6. A machine pinned to an agent that cannot mount in place is restarted as today

Pins may name an agent older than this. Before applying, the pool asks the
placed agent whether it can mount in place, the way
`discobox-wire-machine.conf` asks `wire --can-mount-machine`. An agent that
cannot is placed and restarted as before.

## Alternatives rejected

- **Find every holder of a path from `/proc` and restart its unit.** It
  answers 26-11-02-023's objection in general. The owner kept the restart
  to the known services, Docker and containerd. That is smaller, and covers
  what runs on today's bases.
- **Keep the restart and make it cheaper** (discobox-ai/vm#218,
  discobox-ai/vm#219). Those cut the other two starts of a new archil box.
  This one is discobox's own, and a power cycle on a remote driver costs
  tens of seconds however it is tuned.
- **`systemctl soft-reboot`.** It stops every service, the guest exec the
  pool places through among them. It also needs systemd 254 or later. It is
  the reboot made smaller.
- **Mount under running services, moving nothing** (`wireMachineStarted`).
  26-11-02-023 rejected it, and the reason stands.

## Deferred

- **Any service other than Docker and containerd on a declared path.** One
  example is `nix-daemon.socket`, which listens under
  `/nix/var/nix/daemon-socket`. Such a service is not restarted, and keeps
  its view from before the mount until the machine's next boot. Revisit
  when a box is seen broken by one, adding that unit to §3's list.
- **Removing the restart for older agents (§6).** Revisit when the oldest
  agent a pool can be pinned to mounts in place.

## Consequences

- A new machine sandbox starts once: on archil fresh, on archil from a
  snapshot, and on boxd. The create loses one guest boot, and gains a stop
  and start of Docker and containerd where they were running.
- A start whose placement moved restarts the agent, not the machine. Docker
  and containerd restart only if the mounts changed.
- These go, except on §6's path:
  - `place`'s `moved` result;
  - `placeAfter`;
  - the forced-stop record (`recordForcedPlacement`).
- `discobox-wire.service` and its machine drop-in are unchanged. They mount
  at every boot after the first placement.
- `pool-agent/DESIGN.md` (Machine Sandboxes) and `sandbox-agent/DESIGN.md`
  describe placement as one start, with mounting in place.
- Once 26-11-23-993 takes `/var/lib/docker` and `/var/lib/containerd` out of
  the declared volumes, Docker and containerd hold no path apply mounts.
  §3's stop and start then protect nothing, and the implementation drops
  them rather than restart two services for no reason. A machine's mount in
  place then stops no service at all.
