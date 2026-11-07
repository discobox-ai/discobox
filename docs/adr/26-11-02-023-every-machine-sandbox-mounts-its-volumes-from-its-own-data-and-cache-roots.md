# 26-11-02-023 — Every machine sandbox mounts its volumes from its own data and cache roots

- **Status**: Proposed
- **Date**: 2026-11-02; revised 2026-11-07, before it was accepted, onto the
  data-volume mounting `main` gained meanwhile (acf9ff62)
- **Amends**:
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §2's "where it has none, the declared-volume mechanism does not apply at
  all"; [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md)
  §1's "Declared volumes are not wired"; and
  [26-10-10-767](26-10-10-767-a-container-sandbox-boots-systemd-and-a-unit-wires-its-volumes.md)
  §2's unit conditional on a config volume "which only a pool's container
  has".
- **Relates to**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md),
  [0107](0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md),
  [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md),
  disco-vm ADR 26-11-03-782 (a machine's data and cache volumes), #509 and
  #775.

"Mounting the volumes" here is the boot step the code calls *wiring*
(`boot.Wire`, `discobox-wire.service`): creating each declared path, owned
as declared, and bind-mounting it from the box's data or cache root, and
binding each source onto its target.

## Context

A harness and the platform declare volumes (ADR 0007): paths such as `%HOME%`,
`/var/lib/docker`, `~/.cache` and Claude Code's `/var/lib/claude-code`, each
`data` or `cache`, with an owner and a mode. Every box but one kind mounts
them at boot from `/.discobox/data` and `/.discobox/cache`, and that mount is
also what *creates* each path, owned as declared:

- **A container**: the pool mounts both roots into it, and
  `discobox-wire.service` mounts the declared paths and the sources before
  `local-fs.target`, on every boot.
- **A machine whose driver attached a data volume** (disco-vm ADR
  26-11-03-782): since acf9ff62 its declared paths are mounted from that
  volume as a container's are, and each source is kept at its target's own
  path on it (`wireMachineSources`). The agent's start mounts them first;
  every later boot, the unit does, where `/.discobox/data` is a mount point.
- **A machine without one** — every boxd machine — mounts nothing
  (`wireNone`). Nothing creates a path no other step makes:

  - Claude Code sets `CLAUDE_CODE_TMPDIR=/var/lib/claude-code` and declares
    it a `data` volume owned by the sandbox user; on boxd `claude` fails with
    `EACCES: permission denied, mkdir '/var/lib/claude-code'`.
  - A box with more than one source puts each at its own subdirectory of the
    working root. Nothing creates those, and the agent's clone refuses a
    missing target outside the home, so every multi-source box on boxd fails
    with `materialize source "primary": source target: stat
    /workspace/<repo>: no such file or directory`.

  Its kept data is spread over its disk rather than under `/.discobox/data`,
  so clearing the cache, the box's own `discobox` skill, the verify recipes
  and any export each need a third case.

## Decision

### 1. Every Linux machine has `/.discobox/data` and `/.discobox/cache`

A machine whose driver attached volumes has them there, as now. One that has
none gets both as directories on its own disk, made at its first mount. From
then on the two are mounted the same way, the way a data-volume machine is
today: every declared path as `volumeDir` lays it out (data per target, cache
partitioned by uid under `.users/<uid>`, a `scope: shared` path at the cache
root), with the owner and mode its declaration names, parents before
children; and every source bound onto its target from the target's own path
under `/.discobox/data`. There is no machine that mounts nothing.

Roots on the disk do not make a machine one that keeps its data on a volume.
Only a machine whose `/.discobox/data` is a mount point says so
(`sandboxconfig.MachineDataWiredFile`); one without stays a machine the pool
rebuilds in place (26-10-27-345), since its disk is still all it has.

The cache stays the machine's own (0126 §2 stands). Source-scoped pool data
(`/.discobox/data-per-source`) stays unavailable on a machine.

### 2. A machine's existing content is moved into its volume and bound

Every path is a bind on a machine, never an overlay. The first time a path
is mounted, whatever the machine already has there is moved into its volume
directory — data or cache, into the volume its declaration names — and the
path is then bound. Each entry is moved by itself (a rename, a copy only
across filesystems), so a boot cut short leaves the rest for the next one; a
name the volume already has is refused rather than overwritten; and the
layout is recorded only once all of it has moved. Parents go first, so a
cache path inside the home (`~/.cache`) moves with the home and then, at its
own turn, out of it into the cache volume, and nothing is left hidden under
a bind.

A machine's disk is its own, not an image shared with other boxes, so there
is no lower layer to keep. A container keeps ADR 0107's rule: an overlay over
what its image ships.

### 3. The unit mounts them at boot, every boot, and the first placement restarts the machine

`discobox-wire.service` runs on every machine that has a sandbox: its
condition becomes the bootstrap the box has — the config volume's copy in a
container, the placed `/etc/discobox/sandbox.json` on a machine — rather
than the config volume or a mounted `/.discobox/data`. It keeps its ordering
(before `local-fs.target`, `sysinit.target` and tmpfiles), so nothing that
uses a mounted path — the provider's own dockerd and containerd on boxd among
it — starts ahead of the mount.

A machine boots before its bootstrap is placed, so its own services are
already running on the paths the mount would cover. The first placement
therefore places the bootstrap and the agent layer, starts nothing, and
restarts the machine; the unit mounts at that boot, and the agent and
feature provisioning start after it. Placement already restarts a machine
whose bootstrap moved (26-10-27-345); a first placement is one more reason.
The agent's start still mounts where the unit did not this boot (an agent
layer placed by an older pool), as it does today; the per-start marker
(`sandboxconfig.MachineWiredFile`) keeps it to once.

A failure stops the box, on every kind: a container's manager exits
(26-10-10-767 §5), and a machine powers off, which the pool reports as a
stopped sandbox. The unit no longer passes a machine's failure to the
agent's start.

## Alternatives rejected

- **Create each declared path and source target, owned as declared, without
  mounting.** The smallest fix for both failures above. Rejected because it
  keeps two layouts, and every consumer of a box's kept data (cache
  clearing, export, the box's own skill) still needs a machine-without-a-volume
  branch; it is also a third set of rules for the same declarations.
- **Overlay a machine's existing content, as a container overlays its image.**
  A boxd machine's `/var/lib/docker` and `/var/lib/containerd` always hold
  content by then — its own dockerd ran on the boot before the placement —
  and Docker's `overlay2` driver and containerd's overlayfs snapshotter refuse
  a data root on overlayfs: Docker in the sandbox would stop working. A bind
  over the content without moving it would hide it, and with it an existing
  machine's home.
- **Mount on the running machine at the agent's start, as a data-volume
  machine's first mount does today.** Avoids the first-placement restart.
  Rejected because the pool cannot know every unit of a provider's image that
  holds a declared path open, a mount laid under a running process splits its
  view of the path silently, and moving dockerd's data root out from under a
  running dockerd loses what it writes meanwhile.
- **Keep a machine's unit from failing, leaving the agent's start to try
  again** (acf9ff62's rule). Rejected because a boot that goes on without
  its mounts starts the provider's services on the bare paths, where they
  write to the disk the mount later covers — the very split the restart
  avoids.

## Deferred

- **Exporting and importing a machine sandbox's tree.** Still refused
  (`ErrUnsupported`). This makes `/.discobox/data` the tree to export on
  every box; revisit when a machine pool needs export.

## Consequences

- `wireNone` goes: a Linux machine is mounted as a data-volume machine is,
  whatever holds its roots, and its multi-source boxes and declared paths
  outside the home work on boxd.
- `discobox-wire.service`'s condition is the box's bootstrap, and a machine's
  mount failure powers it off instead of passing to the agent's start.
- The machine runtime's first placement restarts the machine before the agent
  starts, adding one guest boot to a machine sandbox's create.
- A machine provisioned before this is mounted at its next start: its
  content at each declared path — the home, `/var/lib/docker`, `~/.cache` —
  is moved into its volume and bound back, on its own disk.
- The sandbox's `discobox` skill and the verify recipes describe one layout.
