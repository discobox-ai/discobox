# 26-10-10-767 — A container sandbox boots systemd as PID 1, and a unit wires its volumes

- **Status**: Accepted (supersedes [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md)'s
  "as PID 1 before systemd starts": the sandbox agent still wires the declared
  volumes, from a unit of the container's init; takes up
  [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md)'s
  deferred "containers on the same model", and replaces its §1 "a container
  keeps its PID-1 `init` flow"; narrows [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md) §§2-3 and
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md) §2, whose PID-1 flow
  becomes that unit)
- **Date**: 2026-10-10
- **Relates to**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md),
  [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md).

## Context

A sandbox now boots one of two ways. A VM guest boots its own init and the
sandbox agent is one of its services, provisioning from its own start once the
bootstrap arrives (26-10-09-143 §1). A container's entrypoint is still
`discobox-sandbox-agent init` (ADR 0007): it runs as PID 1, provisions the
user, wires the config volume, the declared volumes and the sources, and execs
systemd.

26-10-09-143 deferred moving containers onto the VM's model until "the PID-1
flow is the only thing a container still needs that a VM does not", naming
declared volumes as the reason the flow exists. That condition has not been
met — a container still has volumes to wire — and this ADR does not wait for
it, because it conflated two things. What a container needs is the wiring
**before anything that uses the wired paths starts**. Being PID 1 was how ADR
0007 got that ordering; it is not the only way to get it, and it is the part
that makes a container unlike every other sandbox:

- **Two entrypoints into one provisioning.** `boot.Init` and `boot.Provision`
  share `provision` and differ in who calls it, where the identity comes from
  (the container's `DISCOBOX_USER_*` environment against the bootstrap's own
  `user`), and whether systemd is running yet. Each difference is a branch.
- **Code only a PID 1 needs.** Choosing what to exec, dropping a non-init
  command to the sandbox user, and exec'ing init exist so the agent can stand
  in front of systemd.
- **A workaround for standing there.** systemd mounts its tmpfs over `/run`
  after the PID-1 flow has exec'd into it, so the secrets volume cannot be
  bound by the flow and is bound by the agent's server process instead
  (`boot.WireSecrets`).

ADR 0126 §3 holds that "starting the init first and doing the PID-1 work
afterwards is not equivalent" to wiring before it. That is true of work done
*after* init in the sense of unordered against its units — a service the agent
runs once systemd is up races everything that reads a wired path. It is not
true of a unit every such reader is ordered after. What 0126 §3 requires — the
config complete before the wiring runs, and the wiring done before anything
uses it — is what §2 below keeps; only the process doing it changes.

The agent is installed in an image at build time, on every backend; it is
never placed into a running machine and asked to configure it. So everything
that orders a boot — units, their enablement, their dependencies — is in the
image from its first boot, and the ordering ADR 0007 took from being PID 1 can
be declared instead.

## Decision

### 1. systemd is PID 1 in a container

The sandbox image's entrypoint is its init. `discobox-sandbox-agent init` is
removed, with the exec plan behind it: there is no compatibility entrypoint,
and an image built FROM an older base keeps the older agent it was built with.

A bare `docker run <image> bash` therefore boots systemd rather than a login
shell as the sandbox user. A debug session overrides the entrypoint.

### 2. A unit of the image wires the sandbox before `sysinit.target`

`discobox-wire.service` runs the agent's provisioning with wiring on — the
config volume onto `/etc/discobox`, the secrets volume onto
`/run/discobox/secrets`, the declared volumes, the sources, and the user
provisioning 26-10-09-143 §1 lists. It is:

- **Conditional on the config volume's bootstrap**, which only a pool's
  container has. Without it the unit is skipped, not failed: a bare container
  boots with no sandbox in it, as a VM with no bootstrap does.
- **Ordered before, and required by, `sysinit.target`**, and before
  `systemd-tmpfiles-setup.service`. Every unit with default dependencies starts
  after `sysinit.target`, so nothing that uses a wired path — Docker,
  containerd, the Nix daemon, the user's session, the agent — can start ahead
  of the wiring, and tmpfiles creates nothing underneath a mount.
- **Run on every boot.** A container's mounts do not survive a restart, and its
  account does not survive a rebuild, while `/var/lib/discobox` — where a VM
  records that it was provisioned — is a data volume that survives both. That
  record does not gate a container: what `init` did on every boot, the unit
  does on every boot.

The agent's own start is unchanged. `discobox-sandbox-agent.path` fires once
the unit has bound the bootstrap into place, and the agent's `Provision` finds
the config volume and does nothing, as it does today after `init`.

### 3. The identity comes from the bootstrap

The unit resolves the sandbox user from `sandbox.json`'s `user`, as the VM path
does. A unit does not see the container's environment, and the bootstrap is the
same manifest layer `DISCOBOX_USER_*` is rendered from (ADR 0012). The export
container keeps reading the environment, which the pool agent renders from
that same bootstrap (ADR 0129); it has no unit, so nothing changes for it.

### 4. Units bound to the sandbox user are reloaded, on every backend

The session-bus and desktop drop-ins name the sandbox user, which only the
bootstrap knows, so they cannot be in the image. `init` wrote them before
systemd read its units. The unit writes them and reloads the manager — what the
VM path already does after its provisioning — so one sequence serves both.

### 5. A wiring failure ends the container

A sandbox whose wiring failed must not run: every service would write its
state — the home, `/var/lib/docker` — into the image layer, where a rebuild
discards it. Two things make that impossible rather than unlikely:

- `sysinit.target` requires the unit, so its failure fails the target and
  nothing behind it starts.
- The unit's failure action is the manager's exit, with a non-zero status. The
  container stops, as it did when `init` returned an error, and the pool agent
  reports it the way it reports any exited sandbox: the exit code and the tail
  of the container's log. The unit therefore logs to the console as well as the
  journal, which does not outlive the container.

The pool agent sets no restart policy on a sandbox container, so a persistent
failure is one stopped sandbox with a reason, not a loop.

## Alternatives rejected

- **Wait for 26-10-09-143's condition** — keep `init` until a container has no
  volumes to wire. Volumes are not going away (ADR 0007's reasons hold), so
  this keeps two boot models indefinitely for an ordering a unit can declare.
- **Drop to `emergency.target` on failure**, as systemd does for a failed local
  filesystem. It starts a root login on a console nobody is at, and leaves the
  container running with no agent: the pool would wait out a timeout on a
  sandbox that can only be rebuilt, instead of seeing it fail.
- **A systemd generator for the user-bound drop-ins.** It would put them in
  place before units load, with no reload in early boot — but only in a
  container. A VM's bootstrap arrives after boot, so it reloads regardless, and
  the generator would be a second mechanism for the same files.
- **Emit `.mount` units for the declared volumes.** A bind or an overlay is
  expressible; choosing between them by whether the image's directory is empty,
  the cache partition by uid (ADR 0094), and an overlay's upper adopting the
  lower's identity (ADR 0107 §3) are not. The logic would stay in the agent and
  gain a translation into units.
- **Have the pool agent mount each declared path with Docker.** It returns the
  image-specific knowledge ADR 0007 took out of the pool agent, and an overlay
  over the image's own directory cannot be a Docker mount at all, so an
  in-container step would remain.
- **Keep `init` as a thin entrypoint that only execs systemd**, to preserve the
  bare-`bash` debug session. A wrapper kept for one call shape; the entrypoint
  override is one flag.

## Deferred

- **Provisioning a container's home once rather than on every boot.** The
  recursive ownership pass over the home runs on each container boot, as it did
  under `init`. Skipping it needs a record that is reset when the bootstrap
  changes — the git identity and the sources' direnv prefixes are reapplied
  from it — which the VM's sandbox-keyed record is not. Revisit if boot time on
  a large home becomes a complaint.
- **Dropping `DISCOBOX_USER_*` from the sandbox container's environment.** The
  boot no longer reads it; whether anything else in a running sandbox does is
  not decided here.

## Consequences

- Every Linux sandbox boots the same way: its own init is PID 1 and the agent
  is a service of it. A container differs only in having a unit that wires.
- sandbox-agent loses the `init` subcommand, its exec plan and
  `boot.WireSecrets`' place in the server process; `boot` gains the unit's
  entrypoint, and keeps one provisioning with one source of identity.
- The sandbox image gains `discobox-wire.service` and changes its entrypoint;
  every harness image inherits both by being built FROM it.
- Nothing changes in the pool agent, the providers or the API: the same mounts,
  the same bootstrap, the same report of a sandbox that exited.
- A failed boot is reported from the container's console log rather than from
  `init`'s own stderr.
