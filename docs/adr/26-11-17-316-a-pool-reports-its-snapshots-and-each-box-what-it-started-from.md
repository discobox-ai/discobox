# 26-11-17-316 — A pool reports its snapshots, and each box what it started from

- **Status**: Proposed (would narrow [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §8's "the control plane never knows which a box started from": the control
  plane is told, and still decides nothing from it)
- **Date**: 2026-11-17
- **Issue**: [#339](https://github.com/discobox-ai/discobox/issues/339)
- **Relates to**: [26-11-03-986](26-11-03-986-a-machine-pool-snapshots-a-plan-by-committing-a-vm-it-provisioned-and-shut-down.md),
  whose builds, done records and failures this reports;
  [0071](0071-resource-accounting-is-a-pool-agent-differenced-report.md) and
  [26-11-14-896](26-11-14-896-pool-storage-accounts-for-the-whole-disk-and-a-purge-takes-a-boxs-recordings.md),
  the resource report this rides and its rule that a snapshot is the plan's,
  never a box's; [26-11-03-815](26-11-03-815-a-pool-waits-on-a-sandboxs-ports-and-reports-a-change-at-once.md),
  the report-at-once path this does not take;
  [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) §10, the state channel the
  per-box fact rides.

## Context

A pool builds a snapshot per plan behind the first create of that plan, and
later creates of the plan start from it (ADR 26-10-16-423 §8, ADR
26-11-03-986). None of that leaves the pool agent. At `8277972c3`:

- **Builds are in memory and in the log.** `requestSnapshot`
  (`pool-agent/sandboxruntime/snapshot.go`) keeps the builds asked for in
  `snapshots.active`, a key and a cancel func each. A build that waits for the
  one-at-a-time lock looks the same as one running. Outcomes reach only
  `slog`: `building a snapshot`, `built a snapshot … took=…`, `build a
  snapshot; its plan provisions from scratch … retryAfter=1h`.
- **A failure keeps its time, not its reason.** `recordSnapshotFailure` writes
  a bare RFC 3339 time to `snapshot-failures/<key>` in the pool's durable
  state, in `readSandboxTombstone`'s format. `snapshotFailedRecently` and
  `sweepSnapshotFailures` read it. The error is logged and lost. The failing
  test on the issue, `TestAFailedSnapshotBuildRecordsItsReason`, shows this.
- **A held snapshot does not say what plan it is for.** On a Docker pool it
  is the image `discobox-snapshot:<key>` (`snapshotdocker.go`), labelled with
  its base image and agent layer only. On a machine pool it is a committed VM
  and a done record `{instance, built, lastUsed}` (`machinesnapshot.go`). The
  key is a digest of `snapshotPlan` (base, project, pool, pins, agent, off
  features, egress). `snapshotPlan` names the bundles and the features off,
  but no record keeps it.
- **A box does not say what it started from.** A machine records the key in
  its sandbox tree (`machine.go`, `Snapshot`; empty is a fresh machine). A
  Docker container records nothing but the image it runs. The server's only
  hint is `provision.json`'s `how: snapshot` (ADR 26-10-16-423 §6). That is
  written by the box, and a box restarted after provisioning from scratch
  reports it too, for every feature that applied nothing on this boot.
- **What the pool reports.** The resource report (`pool-agent/resourcereport.go`,
  every 30 s) carries `PoolResourceReport`. The server stores it verbatim on
  the pool row's `resources` column (`RecordPoolResources`), and serves it as
  `Pool.resources`. The state channel (`statereport.go`, deltas plus a 60 s
  complete sync) carries `PoolSandboxState` per box: its ID and runtime state.
  `PoolSandboxRuntime` has an `image` and `metadata`, but the server keeps
  neither.

So nobody can tell from the API, the CLI or the console whether a plan has a
snapshot, whether one is building or failed and why, or whether a box started
from one.

## Decision

### 1. Snapshot state rides the resource report

`PoolResourceReport` gains `snapshots`, listed on every report, not on the
storage sweep's duty cycle: it is a list of image references or of small
records, plus what is in memory, and costs nothing like a walk.

A change shows on the next report, so up to 30 s late. A build runs for
minutes and fails for an hour (`snapshotRetryAfter`), so nobody is racing it
as a sign-in races a port.

`snapshots` is optional on the wire. Absent means the agent predates it, or
its listing failed on this report; an empty object means the pool holds and
builds nothing. A reader shows "not reported" for the first and "none" for the
second, as ADR 26-11-14-896 §3 does for an image figure.

### 2. The report holds what the pool holds, builds and has failed

| Field | Each entry |
| --- | --- |
| `held` | A snapshot the pool would start a box from: its `key`, its `plan` (§3), `builtAt`, `sizeBytes` where the pool can measure it, `lastUsedAt` where the pool keeps it, and `boxes`, how many of the pool's boxes run it now. |
| `building` | A build asked for and not ended: its `key`, its `plan`, `requestedAt`, and `startedAt` once it holds the build lock. A build with no `startedAt` is queued behind another. |
| `failed` | A plan whose last build failed within `snapshotRetryAfter`: its `key`, its `plan`, `failedAt`, `retryAfter` (when a create may ask again), and `error`. |

A held snapshot that a machine pool drops because a box made from it never
answered (ADR 26-11-03-986 §6) is reported as a failed plan, with that as its
error. That is how the pool already treats it: the plan waits as a failed
build's does.

`sizeBytes` on a Docker pool is the daemon's size of the image. It shares the
base's layers, so the snapshots' sizes do not sum to ADR 26-11-14-896's
`imageBytes`, and the reader is not asked to sum them.

`boxes` is a count, not a list. Which box runs which snapshot is each box's
own fact (§5), kept in one place.

### 3. A plan is named by what it provisions

`plan` is `{base, agent, bundles, off}`. `base` is the plan's `Base`, `agent`
its agent bundle's digest, `bundles` its pins (name and digest, in order: the
platform's, then the harness's), and `off` the features left off. The `key`
stays the snapshot's identity. `plan` is what a person reads to tell one key
from another.

The project, the pool and the egress policy are in the key and not in `plan`.
The project and pool are the reporting pool's own. Two plans that differ only
in egress show the same `plan` under different keys. That is rare, since a
pool's plans share its project's, its own and their harness config's policy,
and the reader is shown both keys.

Every record grows to hold its plan:

- a Docker snapshot image, in a label written at commit;
- a machine pool's done record, in a `plan` field;
- a failure record, which becomes a JSON document `{failedAt, error, plan}`.
  `snapshotFailedRecently` and `sweepSnapshotFailures` read both it and the
  bare time written before it. A bare time reads as a failure with no reason
  and no plan.

A record from before this, with no plan, is reported by its key with `plan`
absent. It is rebuilt with one when it is next built.

The error kept is the build's error as the pool composed it, bounded in
length. Part of it may be the build guest's output: the snapshot command's
exit status and message. Like everything a box writes, the CLI and console
escape it when they show it (`cli/REVIEW.md`). A build holds no secret,
account or source (ADR 26-10-16-423 §8), so its error has none to leak.

### 4. The server keeps it as the pool reported it

`snapshots` is part of the report object the server already stores verbatim on
the pool row (`resources`) and serves as `Pool.resources`. That needs no new
column or migration, and gives no rows of its own. The server reads none of
it. Nothing in scheduling, reconcile or readiness looks at it.

### 5. Each box says what it started from, on the state channel

`PoolSandboxState` gains `startedFrom`: `{"from": "snapshot", "snapshot":
"<key>"}` or `{"from": "base"}`. It is what the box's current container, or
machine, was made from. A restart of the same container keeps it. A box made
again (an upgrade that replaces its container, or ADR 26-11-03-986 §6's fresh
remake) reports what the new one was made from.

- A machine pool already records it (`Snapshot` in the sandbox tree).
- A Docker pool records it on the container, which it makes from the snapshot
  or the base in `deliveredImage`, as a label it can read back on every
  observation.
- A container made before this has no such record. Its `startedFrom` is
  absent ("unknown"), never guessed as `base`.

The server keeps it on the sandbox, as `runtime.startedFrom`, in a column of
its own beside the observed state. An observation that omits it leaves the
column as it was: an older agent says nothing, and a delta whose observer
could not read the record is not the box saying it changed. The complete sync
carries it for every box it can attribute, so a dropped delta heals as the
state does.

The per-box fact and the pool's `held` list name a snapshot by the same key, so
a reader can go from a box to its snapshot's plan.

### 6. The control plane is told, and still decides nothing

Everything here is the pool's own record: its images, its done records, its
failure records, its in-memory builds and the containers or machines it made.
None of it is what a box says about itself (ADR 26-10-16-423 §§6, 13).
`provision.json`'s `how` stays what it is, a fact about this boot's features.

This narrows 423 §8's sentence that the control plane never knows which a box
started from. The rest of §8 stands. The pool alone decides whether to build,
start from, keep or drop a snapshot. The server neither picks, builds nor
removes one, and no API call asks the pool to. ADR 26-11-14-896 §§2 and 6
stand as well: a snapshot is the plan's, charged to the pool, and a purge does
not take it.

### 7. Where it is shown

`discobox admin pool get` shows the pool's snapshots, builds and failed plans
from `Pool.resources.snapshots`. The console's pool view shows the same. A
box's details show what it started from, by the snapshot's key and, when its
pool reports it, its plan.

## Alternatives rejected

- **Report a change at once, as ports are (ADR 26-11-03-815).** It needs a
  route and a write per change. It pays off only for a fact a person is racing.
  A build takes minutes and a failure holds for an hour, so the 30 s report is
  well inside what anyone waits for.
- **Ride the pool's status update.** That body is the pool's readiness and
  schedulability, which the control plane acts on. Snapshot state decides
  nothing there (§6), and mixing it in would make a reader wonder whether it
  does.
- **Rows of their own on the server** (a table of a pool's snapshots). The
  server would keep a lifecycle for state it never acts on: rows to reconcile
  against each report, and to delete with the pool. A report replaced whole on
  every tick cannot disagree with itself.
- **A column of its own on the pool row.** It would split one report into two
  writes that can land apart, which `encodePoolResourceReport` exists to
  avoid. The report object already is the pool's observation, and it is
  optional by field.
- **Report the key alone.** It is a digest. A person cannot tell which
  harness, bundle versions or features off it stands for.
- **List each snapshot's sandbox IDs in the pool report.** It would state the
  per-box fact twice, in two reports on two schedules, and they would disagree
  between them.
- **Carry the per-box fact on the resource report's per-sandbox entry.** That
  entry is consumption figures, and arrives only on the 30 s tick. Where a box
  started from is a fact of the box's runtime, fixed when its container is
  made. The state channel reports it as the box comes up, which is when "the
  next box started from it" is asked.
- **Read the per-box fact from `provision.json`.** It is the box's own word,
  and it cannot tell a restart from a snapshot start. ADR 26-10-16-423 §§6 and
  13 keep the pool's record, not the box's, as the source.
- **Infer an older Docker container's start by comparing its image with the
  pool's snapshots.** A snapshot reclaimed since ([ADR 26-11-13-655](26-11-13-655-an-unknown-image-age-is-started-by-the-reaper-and-only-a-kept-repository-keeps-its-newest.md)) leaves an
  image that names nothing. "Unknown" is honest, and goes as those containers
  are made again.

## Open for implementation

- The field names and nesting on the wire. The shapes above are the content,
  not the spelling.
- The bound on a failure's `error`.
- `sizeBytes` on a machine pool, where the driver may not say how large a
  committed VM is. Absent until it does.
- How the Docker container's start is recorded: a label set at create, or one
  the snapshot image carries and every container made from it inherits. The
  second also marks containers made from the image before the agent changed,
  but only once the image is rebuilt.
- How the CLI and console word and lay out §7.

## Consequences

- The pool agent's snapshot bookkeeping grows. `snapshots.active` holds each
  build's plan, when it was asked for and when it started. The failure record
  becomes JSON with its error and plan. A Docker snapshot image and a machine
  done record carry their plan. A Docker box's container records what it was
  made from.
- The resource report grows by the pool's snapshots and plans: a handful of
  entries on a pool, each a few hundred bytes.
- The state channel's observation grows by one optional field, and the server
  adds one column to the sandbox row, with a migration that leaves it empty.
- `Pool.resources.snapshots` and `Sandbox.runtime.startedFrom` are new in the
  API. Agents from before this report neither, and readers show them as not
  reported.
- It likely lands in two changes: the pool agent and the server's contract
  first, then the CLI and console.
- `pool-agent/DESIGN.md`'s Snapshots section and the server's design notes
  follow when the work lands.
