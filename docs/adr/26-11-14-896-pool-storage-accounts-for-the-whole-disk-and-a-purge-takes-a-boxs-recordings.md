# 26-11-14-896 — Pool storage accounts for the whole disk, and a purge takes a box's recordings

- **Status**: Accepted (supersedes, in part,
  [0071](0071-resource-accounting-is-a-pool-agent-differenced-report.md) §5's
  list of what a per-sandbox report carries, and
  [0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md)'s
  "Retention is 48h and deliberately survives sandbox deletion" and its
  consequence that the proxy's control API is read-only)
- **Date**: 2026-11-14
- Accepted by the owner by merging #1169, 2026-11-14
- **Issue**: [#1096](https://github.com/discobox-ai/discobox/issues/1096)
- **Relates to**: [26-10-08-698](26-10-08-698-the-pool-audit-spool-is-bounded-by-bytes-and-truncates-before-it-deletes.md),
  the spool budget, which this leaves as it is;
  [26-11-13-655](26-11-13-655-an-unknown-image-age-is-started-by-the-reaper-and-only-a-kept-repository-keeps-its-newest.md)
  (#1094), which reclaims the images this makes visible, and owns what
  becomes of a snapshot image after its boxes are gone;
  [0022](0022-sandbox-deletion-is-archive-then-confirmed-purge.md), whose
  purge this extends.

## Context

On a pool whose 97.9 GiB data disk was 99% full, `discobox admin pool
resources` attributed about 55 GiB. The largest consumer, 43–47 GiB of Docker
images, appeared nowhere. Nothing in the report pointed at the gap.

Today, at `975fc1b86`:

- **The sweep walks three kinds of tree.** `walkPoolTrees`
  (`pool-agent/storage.go`) measures each sandbox's data, config, sources,
  secrets and origins trees, the pool cache (`layout.PoolCache`) and the build
  tree (`layout.PoolBuild`). Nothing measures the Docker daemon's images or
  containers' writable layers. Nothing measures the proxy's store either: its
  response cache, its recorded bodies and streams, and `audit.db`
  (`layout.ProxyCache`, `ProxyBodies`, `ProxyStreams`, `ProxyAuditDB`). The
  failing test on the issue, `TestWalkPoolTreesAttributesTheProxyStore`, shows
  this.
- **"pool services" disk is `cacheBytes + buildBytes`**
  (`poolServicesRow`, `cli/internal/cli/resources.go`). The proxy runs in the
  pool container (ADR 0044), and its CPU and memory are in this row, but its
  disk is not.
- **A box's `DISK` is the five trees.** `SandboxStorage.TotalBytes` leaves
  out the container's writable layer, which is where a box's root filesystem
  outside its volumes lives. It also leaves out the box's recordings, which
  the proxy spools under `bodies/<client>/` and `streams/<client>/`
  (`proxy/internal/audit/bodies.go`, `streams.go`), keyed by the sandbox ID.
- **Nothing says what is left over.** The statfs line and the walked lines
  are printed side by side, and their difference is not.
- **A purge leaves the box's recordings.** `DockerSandboxRuntime.DeleteSandbox`
  removes the container, the proxy material and policy, the registry
  namespace and the tree. The proxy's audit has only the age sweep
  (`Recorder.Sweep`); there is no delete by client. ADR 0130 kept a purged
  box's audit trail readable for the 48h window on purpose.
- **A snapshot image is keyed by its plan, not by a box**
  (`snapshotPlan.reference`, `pool-agent/sandboxruntime/snapshotdocker.go`).
  Boxes with the same plan share it.
- **Images may not be this pool's alone.** A Docker provider's pools run on
  the host's daemon (ADR 0003), which also holds images that have nothing to
  do with Discobox, and every local pool sees the same images. A vm pool's
  agent runs its own daemon in its machine, on the same data disk as
  `/var/lib/discobox`. The issue's pool was the second kind.

The issue asks two things: how shared images are charged and what a box's
row includes, and whether a purge removes a box's recordings.

## Decision

### 1. A box's row is everything that is its own

`SandboxStorage` gains two figures, and `totalBytes` includes them:

- `layerBytes`: the writable layer of the box's container (Docker's
  `SizeRw`). A box that has no container (archived, or lost) has none. On a
  machine pool the box's disk is its machine's, and where the driver keeps
  that machine on this pool's disk (`layout.PoolMachines`), its directory is
  the box's `layerBytes`. A remote driver's machines are on its provider, and
  the figure is absent.
- `recordingBytes`: the box's spools, `bodies/<id>/` and `streams/<id>/` under
  the proxy's body and stream trees.

These are the box's own in the same sense as its data tree: nothing else
uses them, and they go when the box is purged (§5). That is the test for
being on the row. `walk.dataBytes` stays the sum of the rows' `totalBytes`,
so it now includes them.

### 2. Shared things are charged once, to the pool

Images are reported as a pool-level figure, never split across the boxes
that use them. This is ADR 0071 §5's reasoning for the cache, applied to
images: splitting a shared image makes each box's figure change when another
box starts or stops, and repeating it on every box makes the column sum to
nothing real.

A snapshot image is an image, and goes in the image figure even when one box
is its only user. Charging an image to a box while it has one user would move
gigabytes between the box and the pool as boxes come and go. It would also
put on the box's row something that purging the box does not free, because
the snapshot belongs to the plan and is reclaimed on the reaper's terms
(ADR 26-11-13-655), not the box's.

### 3. The pool's figures name every consumer

`PoolStorageWalk` gains:

| Field | What |
| --- | --- |
| `proxyCacheBytes` | `layout.ProxyCache`: the proxy's response cache. |
| `proxyAuditBytes` | `layout.ProxyAuditDB`, with its `-wal` and `-shm` files. |
| `proxyRecordingBytes` | Spools under the body and stream trees that no box in the sweep owns: a purged box's, from before §5, and spools of a client that is not a sandbox. Every other spool is on its box's row (§1). |
| `imageBytes` | The daemon's images that are Discobox's: those carrying `harness.ReclaimLabel`, the snapshots, and any image one of this pool's containers runs. Unique and shared layers counted once each. |
| `otherLayerBytes` | Writable layers of this pool's containers that are not a box's, such as a snapshot build's. |
| `daemonShared` | Whether the daemon is shared with anything else (§4). |

The pool services row's disk is cache + build + the three proxy figures.
Images and other layers are a separate line, because they are neither a
service the pool runs nor a box.

All of these are measured in the sweep, on its duty cycle (ADR 0071 §7).
Docker's disk usage call computes every container's `SizeRw` and costs about
what a walk does, so it belongs on the expensive schedule, not on every
report. A sweep whose daemon call fails reports the image and layer figures
as absent, not as zero, for the same reason a canceled walk is discarded.

Every new field is optional on the wire, like `dataBytes`: an agent that
predates them writes a sweep without them, and a reader shows what it was
given.

### 4. What is left over is shown, where it can be computed

The report shows `filesystem.usedBytes` minus everything attributed, as
"not attributed", clamped at zero.

On a pool whose daemon is its own (`daemonShared` false: a vm pool), the
images and layers are on the same disk as `/var/lib/discobox`, and they are
subtracted. On a shared daemon they are shown but not subtracted: the
daemon's storage may be on another filesystem entirely (Docker Desktop's VM),
and when it is on the same one, the remainder also holds whatever else the
host keeps there. There the line reads as the filesystem's, not the pool's,
which the CLI says.

How the agent learns that its daemon is shared is open to implementation: it
follows from the provider that made the pool, and the agent is not told that
today.

### 5. A purge removes the box's recordings and audit rows

This supersedes ADR 0130's retention past deletion. A purge is the command
that destroys a box's data, and the issue's 1.8 GiB of recordings outliving
it is the case it should cover. After a purge, a box's egress cannot be
audited from its pool. That is the owner's decision on #1096.

- The proxy's control API gains a delete of one client's audit: every HTTP,
  SOCKS and DNS row whose `client_id` is the box, and its body and stream
  spools. It needs its own scope, `audit:purge`. The pool agent holds the
  control key (`proxyagent.ReadControlKey`) and signs every control token
  itself; it signs one with `audit:purge` only for a delete, and its audit
  reader (`proxy.ControlClient`) keeps signing `audit:read` alone, so a read
  relayed from the control plane can never delete.
- `DeleteSandbox` calls it, on both the Docker and the machine runtime, after
  the container is gone. It is part of the durable removal, as the proxy
  material is: if the proxy cannot be reached the delete fails, and the
  purge's reconcile retries it (ADR 0022 §3). A repeated call finds nothing
  and succeeds.
- A spool still open when the call arrives (an upgraded stream of the box's
  that the proxy has not yet seen close) is closed by the proxy before its
  file is deleted. The recorder already tracks open spools by file; it now
  closes the client's.
- Rows go before files, the order the age sweep uses, so nothing is left
  that names a deleted file.
- Archiving a box, and the reaper of a box lost out of band, leave the
  recordings to the age sweep and the budget as now. Only a purge is the
  owner's word that the box's data should go.

The response cache is not touched. Its entries are content-addressed and
pool-shared (ADR 26-10-08-698 §1), and a row that referred to one is deleted
with the rest.

### 6. The snapshot is not a purge's

A purge does not remove a snapshot image. It belongs to a plan, which other
boxes may use now or later. Its reclaim is ADR 26-11-13-655's, and the
issue's snapshot that never went away on a vz pool is #1094.

## Alternatives rejected

- **Split a shared image across its users.** Each box's figure would move as
  other boxes start and stop, and summing the rows would still need the
  pool's figure to explain an image no running box uses.
- **Charge a snapshot to a box while it is the only user**, as the issue
  suggested. The figure jumps by gigabytes when a second box with the same
  plan starts, and the row would promise bytes a purge does not free (§6).
- **Measure images with `du` over the daemon's data root.** The agent does
  not see the daemon's filesystem on a shared daemon, and on overlay2 a walk
  double counts shared lower layers. The daemon's own accounting is what
  `docker system df` shows, which is what an operator will compare it with.
- **Keep the recordings and truncate them to their heads on purge.** It
  keeps ADR 0130's audit trail readable and frees nearly all the bytes. The
  owner chose a purge that destroys the box's data, which the heads and rows
  are part of.
- **Have the pool agent delete the files and rows itself.** The proxy owns
  `audit.db` and holds it open, writes spools concurrently, and keeps the
  running total the budget depends on (ADR 26-10-08-698 §2). A second writer
  would race all three.
- **Make the recordings' removal best-effort in a purge.** A purge that
  reports success while the box's recorded traffic is still on disk is the
  same defect ADR 0022 §3 fixed for the tree.
- **Report the remainder on a shared daemon too, subtracting images.** It
  would subtract bytes that may be on another filesystem and present a
  number that is wrong in either direction.

## Open for implementation

- How the agent learns its daemon is shared (§4).
- The route's path and the CLI's wording for the new lines and for the
  remainder on a shared daemon.
- Whether `audit.db` is vacuumed after a large purge. The age sweep does not
  vacuum, and ADR 26-10-08-698 keeps the database outside the budget.

## Consequences

- `pool resources` names the images, the proxy's store and the remainder,
  and a box's `DISK` grows by its layer and recordings. Totals from earlier
  releases are not comparable with these.
- The sweep makes one Docker disk usage call per pass, which on a large
  daemon takes seconds. The duty cycle absorbs it.
- `discobox audit` returns nothing for a purged box. Its pool trail stays
  readable after an archive, as before.
- The proxy's control API is no longer read-only. The delete route is new
  surface, behind its own scope, on the loopback listener ADR 0130 bound.
- An agent from before this answers the delete route with its router's 404.
  A purge on a pool not yet reconciled onto the new agent fails and is
  retried until it is.
