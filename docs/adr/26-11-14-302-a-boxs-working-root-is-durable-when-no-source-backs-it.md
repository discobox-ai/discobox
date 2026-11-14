# 26-11-14-302 — A box's working root is durable when no source backs it

- **Status**: Proposed
- **Date**: 2026-11-14
- **Issue**: [#1156](https://github.com/discobox-ai/discobox/issues/1156)
- **Amends**: [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) §4's
  "including `/workspace` when no source backs it": a rebuild keeps the working
  root whether or not a source backs it; what it still drops is the rest of the
  container's own filesystem
- **Preserves**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md)'s split (the pool supplies the primary
  volumes, the sandbox decides what is wired where);
  [26-11-02-023](26-11-02-023-every-machine-sandbox-mounts-its-volumes-from-its-own-data-and-cache-roots.md)'s
  machine wiring (move in, then bind, never an overlay); disco-vm ADR
  26-11-03-782's rule that only a machine whose agent keeps its data on its
  data volume is given a new root

## Context

A box's durable state is what the pool binds in:
`/.discobox/{data,cache,config,sources,secrets}`
(`prepareSandboxVolumes`, `pool-agent/sandboxruntime/runtime.go`). Boot wires
the image's declared volumes from them (`wireVolumes`,
`sandbox-agent/boot/wire.go`) and binds each source from
`/.discobox/sources/<slug>` onto its target (`wireSources`,
`wireMachineSources`). The primary source's target is the working root,
`/workspace` (`sandboxSources`).

A box created with no source has nothing at its working root. `/workspace` is
the container's own directory (or, on a machine, a directory of its root
disk), and the harness starts there, so it is where the user's work goes.
Every rebuild discards it:

- `admin box upgrade`, and the reprovision error that sends the user to it;
- repair, which tears the runtime down with the same Archive an archive uses
  and builds it again (ADR 0035, 0064);
- a stopped box re-pinned at rest and rebuilt at its next start (ADR 0082);
- archive and unarchive;
- a machine given a new root on a spec change (disco-vm ADR 26-11-03-782).

ADR 0016 §4 accepted this and asked that it be stated at the point of
opt-in. Nothing states it: `admin box upgrade --help` says the box "keeps its
ID, its workspace and sources, its caches, and its secrets", and the console's
offer says "Its work is kept". The reporter lost `/workspace` on v0.19.1 →
v0.19.3 and v0.19.3 → v0.19.4-rc.2 (#1156), while `~` and `/.discobox/data`
survived.

The trade-off was accepted when a no-source box was a scratch shell. It is
now an ordinary way to work (`--no-source`, the Docker-only user's
`Default`), and its working root is where its harness writes. A box that
keeps home but not the directory it was started in is not what anyone
expects.

## Decision

### 1. The working root is a data volume when nothing durable backs it

At boot, before the declared volumes are wired, the sandbox agent adds the
working root to them as a per-sandbox data volume, owned by the sandbox
user, mode `0755`, unless it is already durable:

- a source's target is the working root or one of its parents; or
- a declared volume's path is the working root or one of its parents (a
  working root inside home is home's).

It is the agent's decision, not the pool's: the pool supplies the primary
volumes and never decides an in-sandbox path (ADR 0007). Any runtime whose
boot wires volumes from `/.discobox/data` gets it, containers and machines
alike, with nothing in the runtime-config document, the worker API or the
control plane. It is kept at `/.discobox/data/.working-root`, inside the data
subtree an export carries (ADR 0129). Not at its own path under the data root,
as a declared data path is (`volumeDir`): a machine already keeps a source
nested in the working root at `/.discobox/data/workspace/<slug>`
(`machineSourceDir`), and the working root's mountpoint for it, moved in,
would collide with that tree.

It is not a source. `discobox ls`, the diff stat, service discovery, the
skills sources and direnv see what they see today; a source nested under the
working root (`/workspace/<slug>`) is bound over the volume's directory, as a
nested declared path is.

### 2. It is moved in, never overlaid

The working root is wired the way a machine's paths are (ADR 26-11-02-023
§2): what is at the target is moved into the volume directory, an entry at a
time, the layout recorded as a bind, then the directory is bound — in a
container as well as on a machine. A container's other declared data paths
keep `decideLayout`'s overlay for content their image ships; the working root
does not, because the content at it is the user's, and an overlay would keep
it in the lower layer that the next rebuild drops.

This is also the migration for a box that restarts on the new agent without
being rebuilt (the platform follows its server from the next start, ADR
26-10-16-423): its `/workspace` moves onto the volume at that boot.

### 3. A container rebuilt from one made before this carries its working root over

A container's working root from before this is in its writable layer, which
`ContainerRemove` deletes. So every path where the pool removes a sandbox
container to build another — the spec-change replace (`buildSandboxContainer`),
Archive (and through it repair), and the retire of a container built before
runtime-config delivery — first copies what the old container's layer holds at
the working root out through Docker's archive API, into a stash file in the
sandbox's tree on the pool host. The next container built for the sandbox has
the stash copied into its layer at the working root, through the same API,
before it starts; boot then moves it onto the volume (§2).

- Both copies are Docker's: nothing the box wrote is extracted on the pool
  host, whose agent runs as root.
- A layer with nothing at the working root stashes nothing. One whose working
  root was moved onto the volume is empty, so a box migrates once.
- An existing stash is never overwritten: it holds what no container has been
  given yet. It is removed once copied into the new container, before that
  container starts, so no boot moves the same entries in twice.
- A delete removes the stash with the tree.

### 4. A machine is given a new root only once its working root is on its volume

A machine's record says whether its agent keeps its data on its data volume
(`DataOnVolume`), and only such a machine is given a new root; one that does
not is placed again on the root it has. That line is read from what the agent
wrote at its last start. From this decision on, the agent says, as well,
that it wired the working root (or found it durable, §1), and the pool counts
a machine as keeping its data on its volume only when it did. A machine whose
last start ran an older agent is rebuilt on the root it has; that boot runs
the new agent, which moves the working root onto the volume (§2), and the
rebuild after it may take a new root.

A machine without a data volume keeps everything on its root and is never
given a new one; nothing changes for it.

### 5. What the user is told

`admin box upgrade --help` and the console's "Its work is kept" become true as
written and stay. The reprovision error's pointer to `admin box upgrade` needs
no warning.

## What this does not keep

- **A box already rebuilt before this ships.** Docker deletes a removed
  container's writable layer; disco-vm removes a replaced root "for good"
  (engine `Upgrade`). Neither keeps a copy, and the pool keeps no snapshot of
  a box (its snapshots are of a provisioning plan, made for no sandbox).
  Nothing is recoverable.
- **Writes outside the working root, home, the declared volumes and the
  sources.** The container is still disposable (ADR 0016 §4).
- **A runtime with no data root to wire from** — a non-Linux VM sandbox has
  no declared volumes (`sandboxpath.Volumes`) and keeps everything on its
  disk, which no rebuild replaces.

## Alternatives rejected

**State the loss** (ADR 0016's own answer): correct the help, the console and
the reprovision error to say a no-source box's `/workspace` is cleared.
Rejected by the owner (#1156): it leaves the at-rest rebuild (ADR 0082)
losing work with no command run at all, where no warning can be shown, and
it keeps a box whose home survives a rebuild while the directory its harness
starts in does not.

**Have the pool give a no-source box a source-like directory at the working
root**, in `doc.Runtime.Sources` or as a sixth primary volume. Rejected: a
source is something `ls`, the diff stat, service discovery and the skills
sources act on, and a no-source box has none; and the pool would be deciding
an in-sandbox path, which ADR 0007 leaves to the sandbox. A data volume the
agent adds is the existing mechanism, with its machine wiring, layout
records and export, already built.

**Wire it with `decideLayout` like any other declared data path.** Rejected
(§2): a container booting the new agent on a `/workspace` already full would
choose an overlay, with the user's files as its lower layer in the container,
and lose them at the next rebuild anyway.

**Extract the old container's working root onto the host volume directory**
in place of the stash (§3). Rejected: it untars what the box wrote, as root,
on the pool host — symlinks, hard links and ownership included — where the
stash only hands Docker an archive Docker made.

## Consequences

- A no-source box's `/workspace` survives upgrade, repair, reprovision's
  pointer to upgrade, the at-rest rebuild, archive and unarchive, and a
  machine's new root.
- The first boot that migrates a large `/workspace` on a container copies it
  across filesystems (the layer to the bind), once, before the box is ready.
- A Docker rebuild of a box from before this streams its working root through
  the pool agent twice, once.
- A machine from before this needs one rebuild in place before it takes a new
  root.
