# 26-11-03-986 — A machine pool snapshots a plan by committing a VM it provisioned and shut down

Status: Accepted (supersedes in part [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md) §8's build that powers itself off and leaves its result in `SnapshotDir` for the taker; builds on disco-vm ADRs 26-11-03-606 and 26-11-03-868, see [What it needs of disco-vm](#what-it-needs-of-disco-vm); §§1, 3, 4, 6, What it needs and Consequences amended before implementation landed: the plan on the command's stdin and `SnapshotDir` off the committed disk, the systemd wait and whole-provisioning check, the seal's path and the agent's start disarmed, no committed mark read, only an agent that never answers blames a snapshot, and the provider's base kept as the driver's configuration)
Date: 2026-11-03

## Context

ADR 26-10-16-423 §8 makes a snapshot a cache: a create that finds none for its
plan provisions from scratch, and the snapshot is built behind it. Only the
Docker runtime builds one (`pool-agent/sandboxruntime/snapshot.go`). A build
container boots with an `agentlayer.SnapshotPlan` where a sandbox's bootstrap
would be. `discobox-snapshot.service` provisions it, cleans it, writes
`SnapshotResultFile` and powers it off. The runtime then commits the container,
whose `SnapshotDir` was a bind and so is not committed.

A machine pool (`MachineSandboxRuntime`, ADR 26-10-23-551) builds nothing. Every
sandbox machine boots from the driver's base and provisions everything (Docker,
Nix, Chromium, Go, Homebrew) on every create.

disco-vm is changing underneath:

- **ADR 26-11-03-606 (disco-vm, Accepted) leaves only VMs.** A VM is made fresh
  from the driver's own base, which the caller never names, or `From` a
  committed VM. `Commit` freezes a stopped VM under its own name, and it never
  runs again.
- **ADR 26-11-03-868 (disco-vm, Accepted) makes `Commit` a capability.** A
  driver reports `Capabilities.Commit`, covering both commit and create `From`.
  One that does not report it refuses both with `ErrUnsupported` and changes
  nothing.

boxd shapes the failure modes. It snapshots only a running machine, memory and
disk, so its `Commit` boots the committed disk once more to capture it. Its
restore resumes that guest, flushes it and stops it, so a copy's first start is
a cold boot. A boot the pool did not run lands on the disk it committed, and
disco-vm cannot promise a commit that crashed partway reads as not committed.

## Decision

### 1. One imaging mode, run by the taker

The guest has one way to become an image on every runtime. The taker:

1. starts a box from the base;
2. places the agent layer, units and pins;
3. runs `discobox-sandbox-agent snapshot` as one blocking exec, with the
   `SnapshotPlan` on its standard input;
4. stops the box in order and commits it.

The command:

- waits for systemd to finish booting and has it read the units placed, since
  a VM's are placed in a box already running;
- puts `SnapshotDir` on a tmpfs unless it is a mount already (a container's
  is the taker's bind), then applies the plan's proxy material there. The
  plan holds the build's proxy client key, so neither it nor the material is
  ever written to a disk a snapshot commits: a file deleted there leaves its
  bytes in free blocks every copy carries, and the build's identity has no
  egress policy once the build is done;
- fetches and provisions the pinned bundles, every step blocking;
- refuses a box with any feature not done, or failed, as `discobox-provision
  wait` reads its status: no snapshot is committed that is not provisioned
  whole;
- cleans the box of what is one machine's: empties `/etc/machine-id`, removes
  SSH host keys, untrusts the build's proxy CA;
- removes everything the build put under `SnapshotDir`;
- removes the link that starts the agent when a bootstrap appears (§3);
- writes the **sealed marker** (§3);
- reports its result on the exec's output and exit status.

It no longer powers the box off. `discobox-snapshot.service` and
`SnapshotResultFile` go.

The Docker runtime moves to the same exec. A build container's `SnapshotDir`
stays a bind, so removing it there is harmless.

### 2. A machine pool builds a snapshot only where the driver commits

The pool reads its driver's `Capabilities.Commit` once, before it makes any VM.

- **A driver that does not** gets no snapshot. The pool builds nothing, keeps no
  snapshot records, and every sandbox machine is made fresh and provisions from
  scratch, as now.
- **A driver that does** builds a snapshot per plan behind the first create of
  that plan, as on Docker:
  - the key is `snapshotPlan`'s;
  - the build is anonymous, under the create's `provisionEgress`;
  - at most one build per plan;
  - a build gets the existing 45 minutes.

The build VM is made like a sandbox machine: isolated on the pool's network and
fenced to the pool's hop. A create that finds a snapshot (§4) makes the sandbox
machine `From` it, then starts it and places its bootstrap as it does for a
fresh one.

### 3. A box takes its own identity when its bootstrap arrives

disco-vm does not promise that the disk a copy boots is the one the pool shut
down: boxd's commit boot and restore both run the guest. So the clean in §1 is
not relied on alone.

When `sandbox.json` arrives at a box carrying the sealed marker
(`/etc/discobox-sealed`, in `/etc` since no sandbox volume is mounted there to
hide it), the agent first
gives the box an identity of its own, before anything else:

- a new `/etc/machine-id`, then `systemctl daemon-reexec` and a journald
  restart, so PID 1 and the journal take it up;
- new SSH host keys, where sshd is installed.

Then it removes the marker and boots the sandbox as now. A box with no marker
(fresh from the base) skips this step. The same step runs on Docker.

The re-execution must not run under its maker's own systemctl calls, so a box
made from a snapshot starts its agent as a fresh one does: the snapshot has the
agent's start link removed (§1), and the maker arms it — a VM's by
`discobox-provision apply`, which reloads systemd and starts the path unit, a
container's by placing the link again before its start.

### 4. The pool's done record publishes a snapshot

disco-vm reporting a VM as committed proves nothing to the pool. The pool's own
durable **done record** is what makes a snapshot one.

- **The name is fixed per plan:** `snap-<plan key>`, within the driver's
  name-length limit.
- **The record** lives in the pool's tree, written by rename and synced. It
  holds the instance ID `Commit` returned.
- **A create uses `snap-<key>` only if both hold:**
  - the record exists;
  - the VM of the name exists, with the ID the record holds.

  disco-vm leaves the committed mark to its caller (ADR 26-11-03-606), and the
  record is that mark.

  Otherwise the create makes the machine fresh and asks for a build.

A build of `snap-<key>` always runs the same sequence, and any failure stops it
at the step it reached:

1. **Remove the done record**, durably.
2. **Destroy `snap-<key>` if it exists.** If the destroy fails, or the next
   create still reports `ErrExists`, stop.
3. **Create fresh, place, run the imaging exec, stop in order, `Commit`.**
4. **Write the done record.**

The record is removed before the VM and written after it. A crash anywhere
therefore leaves either no record, so the next pass rebuilds from step 1, or a
record naming a VM that committed. An uncommitted or unrecorded `snap-` VM is
never started, resumed or committed again. The pool cannot know what state it
is in, so it only ever destroys it.

Dropping a snapshot uses the same order, record first and then the VM:

- a snapshot that proved bad (§6);
- a reclaim (§5);
- a cache clear, which drops every snapshot, as on Docker.

### 5. Leftovers are found by name

The VMs live in the pool's disco-vm project, so the pool finds them by name and
needs no memory of its own:

- **A periodic sweep, and one at agent start,** destroys every `snap-` VM with
  no done record and no build under way in this process. The build's egress
  policy, proxy material and tree go after it, in that order, as
  `removeSnapshotBuild` does, since an identity with no egress entry is
  unrestricted.
- **A snapshot whose last use is older than the reclaim age** is dropped, record
  first. The last use is kept beside the record.
- **The orphan reaper keeps to `sbx-` names,** and the sweep keeps to `snap-`
  names.
- **A pool machine made again** has no tree, so its successor reads every
  snapshot as not done and rebuilds what is asked for. The sweep removes the
  rest.
- **A failed plan waits before the next build.** The backoff
  (`snapshotRetryAfter`) is kept in the pool's tree rather than in memory, so an
  agent that keeps restarting does not keep building.
- **Reclaim never races a create.** A create holds the snapshot in memory for
  the length of its `Create` call. A create whose `From` is gone
  (`ErrNotFound`) makes the machine fresh.

### 6. A snapshot that proves bad is dropped, and the sandbox is made again fresh

A snapshot is provisioned whole when it is built (§1), so its platform and
harness steps are known good before any sandbox is made from it. What can still
blame it is a sandbox machine made from it whose agent never answers, its
machine stopping first or the wait running out, on the machine's first start
and before its agent has answered once.
Then, in that same start, the pool drops the snapshot (§4), the plan waits as a
failed build's does, and the machine is destroyed and made again fresh. The
machine never ran its agent for the sandbox, so nothing of the sandbox's is
lost. Nothing else of a start blames the snapshot: a placement that fails can be
the pool's or a passing moment of the box's.

A step that fails later, after the create has returned and the sandbox is in
use, is not taken back: it is reported as any failed step is. A failure in the
sandbox's own phase (its sources, its secrets) is not the snapshot's, since it
would fail the same from scratch.

## What it needs of disco-vm

All of this is on disco-vm `main`. The pool's disco-vm pin moves to it.

- **Only VMs (ADR 26-11-03-606, `247f378`).** The pool uses
  `CreateOptions.From` and `Commit(ctx, inst)`. It does not use images, tags or
  a base the caller names.
- **`Capabilities.Commit` (ADR 26-11-03-868, `e33a525`,
  [discobox-ai/vm#33](https://github.com/discobox-ai/vm/issues/33)).** §2
  decides from it, never from a failed attempt.
- **`Destroy` returns only once the provider no longer has the VM.** §4's step 2
  relies on it. On a driver whose copies chain below the seam (hcs), `Destroy`
  refuses with `ErrInUse` a committed VM that a sandbox machine made from it
  still needs. A drop has already removed the record by then, so the sweep (§5)
  removes the VM once the last such machine is gone.

disco-vm is not asked to make a commit atomic, or to say how a driver captures
a VM.

## Alternatives rejected

- **Freeze the box warm at the bootstrap wait, and resume it per sandbox.** Every
  copy would share the build's memory: its kernel random state, machine-id, boot
  ID, clock and live proxy bridge. Each would need re-identifying under running
  systemd. disco-vm would need memory capture and a create that resumes (it
  chose cold clones, ADR 26-10-15-851). On boxd a restore starts with no egress
  allowlist. The saving is one cold boot against minutes of provisioning.
- **Trust disco-vm's committed mark.** A commit that crashed partway may read as
  committed on some driver, and only the pool can know that the imaging
  succeeded and the stop was clean.
- **A new name per build attempt.** It avoids colliding with a leftover, but
  leaves several candidates to choose among and clean up. A fixed name, cleared
  before every build, makes recovery "build again".
- **Keep a stopped build to commit later.** After a restart the pool cannot tell
  whether its provisioning finished or its stop was clean.
- **The guest powers itself off, and the taker reads `SnapshotResultFile`.**
  boxd cannot read a stopped machine's disk, and on a VM the file would be
  committed.
- **Probe `Commit` by calling it.** Every create on a driver without it would
  run a 45-minute build only to discard it.

## Deferred

- **Warm resume** (the first alternative): revisit if, on a driver that captures
  memory, boot time measures as a large share of a create from a snapshot.
- **A cheaper copy on boxd** (no restore, flush and stop before the cold boot)
  is disco-vm's boxd driver's to make. Nothing here would change.

## Consequences

- `pool-agent/sandboxruntime`:
  - The plan, key, build bookkeeping and backoff move from
    `DockerSandboxRuntime` to `sandboxCore`.
  - `MachineSandboxRuntime` gains the build, the done record, the sweep and the
    fallback.
  - The Docker build runs the imaging exec and stops its container itself.
- The sandbox agent:
  - the `snapshot` command loses its power-off and its result file, and gains
    the `SnapshotDir` cleanup and the sealed marker;
  - the bootstrap path gains §3;
  - `discobox-snapshot.service` leaves `agentlayer.Units`.
- With ADR 26-11-03-606 a fresh VM's base is the driver's configuration
  (`machine.Config.Base`). The discovm provider's `base` option becomes that
  configuration, for the server's driver and the pool agent's alike, rather
  than an ID matched against what the driver lists; `MachineSandboxRuntime`'s
  `baseRef` goes. The base is part of a machine pool's snapshot key.
- On a driver without `Commit` nothing changes, beyond those disco-vm calls.
- `pool-agent/DESIGN.md`'s Snapshots and Machine Sandboxes sections, and the
  sandbox agent's design notes, follow when the work lands.
