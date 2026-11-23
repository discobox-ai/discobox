# 26-11-23-993 — Container-runtime state is on the box's root, never on a declared volume

- **Status**: Proposed
- **Date**: 2026-11-23
- **Supersedes in part**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md)'s
  `/var/lib/docker` among the paths a sandbox persists through a declared
  volume; [26-11-02-023](26-11-02-023-every-machine-sandbox-mounts-its-volumes-from-its-own-data-and-cache-roots.md)
  §2's binding of a machine's `/var/lib/docker` and `/var/lib/containerd`
  from its data root.
- **Relates to**: #1604, #32,
  [26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md).

## Context

`sandbox-agent/image.json` declares `/var/lib/docker` and
`/var/lib/containerd` as `data` volumes, so every box binds them from
`/.discobox/data`. On an archil machine that root is the box's Archil disk,
which Archil attaches only over FUSE. containerd's overlayfs snapshotter puts
its upper and work directories there, the kernel refuses the overlay, and no
container starts in an archil box (#1604). Archil documents both halves:
OverlayFS is supported on the sandbox's ext4 root and Docker's data directory
belongs there, and a mounted Archil disk cannot be an OverlayFS upper
directory. A mounted disk has no block-device attach.

A container box's root is the container's own overlayfs, which Docker and
containerd refuse as a data root. That is why the declared volume existed: it
was the only non-overlay place a container box had for this state. Kubernetes
runtimes inside a box (kubeadm, k3s) have the same need and no declaration at
all.

## Decision

1. **No container runtime's state is a declared volume.** `/var/lib/docker`
   and `/var/lib/containerd` leave `image.json`'s `volumes`. Platform and
   harness declarations must not add a runtime's state directory back.
2. **The sandbox base image declares each runtime's state directory as
   `VOLUME`**: `/var/lib/docker`, `/var/lib/containerd`, `/var/lib/kubelet`,
   `/var/lib/etcd` and `/var/lib/rancher`. In a container box each is then an
   anonymous volume of the hosting daemon, on that daemon's own filesystem,
   not overlayfs. On a machine (archil, and any driver that boots the image as
   a root filesystem) `VOLUME` means nothing, and the paths sit on the
   machine's root disk.
3. **This state lives as long as the container or the machine's root, and no
   longer.** It survives stop and start, and sleep and wake. A container
   replaced for a spec change or a rebuild starts with empty runtime state, as
   an archil `Upgrade` does. Every path that removes a sandbox container,
   replacement included, removes its anonymous volumes with it.
4. **A Docker pool's container, which runs the same image, gets the same
   anonymous volumes**, and every path that removes it removes them with it.
   Nothing runs there, so they stay empty. The rule that a pool container
   mounts no *named* volume (#32) is unchanged.
5. **Existing boxes' state on the data root is left as it is.** Boot reads a
   layout record only for a declared volume, so the old
   `/.discobox/data/.../var/lib/docker` and `.../var/lib/containerd` trees and
   their records are no longer read or mounted. They are not moved, and
   nothing deletes them. A box's existing images, containers and Docker
   volumes are gone from its view once it boots on the new image.

## Alternatives rejected

- **Keep the declared volume and make archil's data root a block device.**
  Archil has no block attach for a mounted disk. Its only block device in a
  sandbox is the sandbox's own root.
- **Keep the declared volume and use containerd's `native` snapshotter where
  the data root is FUSE.** It needs no overlay, but every layer is a full
  copy: slow, many times the disk, and still served through FUSE. Docker's own
  storage would differ by provider.
- **Keep the declared volume only on providers whose data root takes an
  overlay.** That leaves two layouts for the same declaration, and every
  consumer of a box's kept data needs a branch for the other. It is the
  arrangement 26-11-02-023 removed.
- **Carry a replaced container's runtime volumes to its replacement.**
  Keeps images across a spec change, but ties the replacement to the old
  container's volumes and adds a hand-off every removal path must get right.
  Runtime state is a cache, which is lost on an archil `Upgrade` already.
- **Cover the pool container's `VOLUME` paths with tmpfs.** Avoids empty
  volumes, but adds pool-only mounts to a create that has none, for paths
  nothing uses. Removing the volumes with the container is enough.
- **Move or delete existing boxes' old state at boot.** Moving copies
  possibly large trees across filesystems with crash-safe resumption, for a
  cache. Deleting removes data a person may still want to recover by hand.
  Leaving it costs only disk.

## Consequences

- Docker, containerd, kind, k3d, k3s and kubeadm run in an archil box, on its
  ext4 root.
- Runtime state no longer survives a container replacement, or a move between
  pools that remakes the container. A Docker volume holding data a person
  cares about must be bound from a path under their home, or another declared
  volume.
- A box made before this keeps orphaned runtime state on its data root until
  a person removes it.
- `excludeFromExport` for these paths goes with their declaration: a snapshot
  or export already leaves a container's volumes out.
