# 26-11-23-004 — A snapshot build reports its progress, and an admin reaches it through its pool

- **Status**: Proposed (would extend [26-11-17-316](26-11-17-316-a-pool-reports-its-snapshots-and-each-box-what-it-started-from.md)
  §2's `building` and `failed` entries, and narrow its §6 "never what a box
  says of itself" for a build's progress alone)
- **Date**: 2026-11-23
- **Issue**: [#1597](https://github.com/discobox-ai/discobox/issues/1597)
- **Relates to**: [26-11-03-986](26-11-03-986-a-machine-pool-snapshots-a-plan-by-committing-a-vm-it-provisioned-and-shut-down.md),
  the build this makes visible, and its §5 sweep;
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §8, a build holds no secret, account or source;
  [26-11-18-571](26-11-18-571-a-server-routes-path-says-who-answers-it-and-nothing-is-under-api.md),
  where the new routes go.

## Context

At `97b22818`, a pool builds a snapshot of a plan in a box of its own. On a
Docker pool that box is a `discobox-snapshot-<pool>-<id>` container
(`snapshotdocker.go`, `buildSnapshot`). On a machine pool it is a
`snap-<key>` VM (`machinesnapshot.go`, `buildMachineSnapshot`). What
discobox shows of it:

- **One row in the pool's report.** `snapshotBuildsReport`
  (`pool-agent/sandboxruntime/snapshot.go`) puts each entry of
  `snapshots.active` on the resource report's `building` list. An entry
  carries `{key, plan, requestedAt, startedAt}` (`BuildingSnapshot`,
  `snapshotreport.go`). It doesn't say what the build is doing, or which box
  it runs in.
- **One blocking exec.** `runSnapshotCommand` runs the agent's `snapshot`
  command through `provisioner.Provision`, which buffers the command's stdout
  and stderr until it exits. The pool agent learns the exit status and the
  stderr tail at the end, and nothing before. The command's stdout carries
  nothing today: `enterCommand` (`sandbox-agent/cmd/discobox-sandbox-agent/snapshot.go`)
  passes it through, and nothing writes to it.
- **What the build knows, it keeps.** In the build, `provision.Apply` keeps
  a `provision.Tracker` at `provisionspec.StatusPath`: per feature, its
  state, current step and error (`provisionspec.Status`). It writes each
  feature's output to `/var/log/discobox/provision/<feature>.log`
  (`provision.LogDir`). Nothing outside the build reads either while it
  runs.
- **Nothing reaches the build's box through discobox.** A build has no
  sandbox row, by design (ADR 26-11-17-316 §4,
  `layout.SnapshotIDPrefix`), and runs no sandbox agent: the snapshot
  command disarms the agent's start. So the box's exec and logs routes
  (`/projects/{p}/sandboxes/{s}/agent/…`) can't address it, and the pool
  agent's router (`pool-agent/server`) has no route for a build.
  `discobox admin pool console` reaches the pool's host through the provider
  driver. On a Docker pool, that console can `docker exec` into the build. On
  archil it is a shell in the pool's machine, which can't reach the build's
  machine.
- **The pool agent does reach it.** Every build holds a `machine.Guest` for
  its box: a `provisioner.ContainerGuest` on Docker, `engine.Guest` on a
  machine pool. That is how the snapshot command runs on every runtime.
  `machine.Guest` offers `Exec` (with a TTY), `CopyFrom` and `CopyTo`.
- **A failure keeps its error, and loses its box.** Both build functions
  remove the box in a `defer`, whatever the outcome. The failure record
  (`snapshot-failures/<key>`) keeps `{failedAt, error, plan}` for
  `snapshotRetryAfter` (an hour). The error is bounded to 2 KiB.

On 2026-11-23 an archil build stayed `building` for 17 minutes or more, and
nothing in discobox could tell whether it was progressing or stuck (#1585).

## Decision

### 1. The build streams its progress to the pool on stdout

The snapshot command writes its provisioning status to stdout as it
changes: one line per change, holding what `provisionspec.Status` holds of
each feature (state, step, error). It writes nothing else there, since the
steps write to their logs.

`runSnapshotCommand` reads the command's stdout as it comes, instead of
buffering it. It keeps the latest status on the build's `snapshotBuild`.
A line it can't parse is dropped, never an error: the build's result stays
its exit status and stderr, as today.

The pool agent learns about a change as the build makes it, with no exec of
its own and no poll of the guest. That holds on every runtime, including a
machine pool whose guest is a remote machine, at no cost per report.

### 2. The report carries the build's box and progress

`BuildingSnapshot` gains:

| Field | Holds |
| --- | --- |
| `box` | What the build runs in, as its runtime names it: the container's name, or the machine's. Absent while queued. |
| `progress` | The build's latest status (§1): the overall state, and per feature its state, step and error. Absent while queued, and from a build whose agent predates §1. |

It rides the resource report as the rest of `snapshots` does (ADR
26-11-17-316 §1), so it shows up to 30 s late. A build runs for minutes. A
person who is watching closely follows its logs (§4), which are live.

`FailedSnapshot` gains `progress` too, the last status the build reported
before it failed, kept in the failure record. That tells which feature
failed and at which step. A failure recorded before this has none.

### 3. Progress is the build's word, and decides nothing

ADR 26-11-17-316 §6 reports only the pool's own records, never what a box
says of itself. `progress` is the build guest's word. This narrows §6 for
`progress` alone, as `error` already carries the build's stderr (316 §3).
That is safe for three reasons:

- A build is the pool's own box. It holds no account, secret or source (ADR
  26-10-16-423 §8), and nobody else's code runs in it.
- Nothing acts on `progress`: no scheduling, reconcile, readiness, or the
  pool's own decision to keep, retry or drop. The build's exit status alone
  decides whether it is committed, as now.
- The CLI and console escape it when they show it (`cli/REVIEW.md`), as they
  do `error`.

### 4. An admin reads a build's logs, and opens a shell in it, through its pool

The pool agent serves two routes for a build. They address it by its key,
which `building[]` already lists, and act on the `machine.Guest` the build
holds:

- **Logs.** Read the feature logs under `provision.LogDir`, one feature or
  all, as a tail, or followed until the build ends. The pool agent runs the
  read itself, through the guest. It is a fixed read of fixed paths, never
  a command the caller names.
- **Exec.** A command, or an interactive shell with a TTY, in the build's
  box, as `machine.Guest.Exec` runs one. The session ends when the build
  ends, since the build removes its box.

The server forwards each from an admin route under the pool, beside
`console` and `logs` (ADR 26-11-18-571 §1):
`/projects/{p}/pools/{pool}/snapshot-builds/{key}/{logs,exec}`. It takes the
same admin authorization as `admin pool console`. The server keeps nothing
of a build. It forwards to the pool agent the build's key and the caller's
request, as it already forwards `clear-cache`.

A build that is queued has no box yet, so both routes refuse it as queued. A
key that names no build under way gets the failed build's kept logs (§6) if
there are any, and otherwise is not found.

This works the same on every runtime. It needs no provider credential and no
provider console, because the pool agent already reaches the build's guest.

### 5. A shell keeps a build from being committed

A snapshot is a cache of what provisioning makes, and every box of its plan
starts from it. If a person changed a build by hand, every box of that plan
would carry the change, and nothing would record it.

So the first exec into a build marks it as opened. The build runs on, so the
person can watch it and debug it, but its result isn't committed. It ends as
a build ended by a clear or by `errSnapshotHeld` does: no snapshot, no
failure record, and the next create of the plan asks again. The logs route
doesn't mark it, because it is the pool agent's own fixed read (§4).

### 6. A failed build keeps its logs, not its box

When a build fails, the pool agent copies out what it needs before the
deferred removal takes the box: the status (§2), and each feature log's
tail. It copies them with `CopyFrom`, bounded per feature. It keeps them
beside the failure record, and the logs route serves them (§4). They live as
long as the failure: `sweepSnapshotFailures` and a cache clear's
`forgetSnapshotFailures` remove them with it.

The box itself is still removed, as now. Keeping it is deferred (see
Deferred).

### 7. Where it is shown

- **A listing of builds beside boxes.** `discobox admin box ls` can list each
  pool's running builds with its boxes, from each pool's
  `Pool.resources.snapshots.building`: the pool, the plan, the state (queued
  or building), when it started, its box, and its current feature and step.
  No new list route. The server keeps no lifecycle for builds (ADR
  26-11-17-316 §4). `discobox ls`, the user's own boxes, doesn't list them:
  a build is the pool's.
- **A build's progress** in `discobox admin pool get` and the console's pool
  view, beside its row.
- **Its logs and a shell**, from a CLI command under `discobox admin pool`
  on §4's routes.

## Alternatives rejected

- **The pool agent reads the build's `provision.json` on each report.** It
  costs an exec or a copy through the guest every 30 s for each build, and
  on a remote machine pool that is a round trip to the provider. The status
  would also be up to a tick behind before the report adds its own tick. The
  stream in §1 costs nothing while nothing changes.
- **The build streams its feature logs to the pool agent, which keeps a
  buffer.** Logs would then need no second exec. But the pool agent would
  hold every byte of every build's output in case someone asks. A read on
  demand (§4) reads only when someone asks, and the kept tail (§6) covers a
  failure.
- **Give a build a sandbox-like identity, so the box routes reach it.** It
  would need a row and a lifecycle on the server for something the server
  never acts on, which ADR 26-11-17-316 §4 rejected. It would also need a
  sandbox agent running in the build, which the snapshot command disarms so
  that boxes made from it start theirs on cue.
- **Reach the build through the provider console.** That already works on a
  Docker pool. It doesn't work on archil, and on boxd it needs the
  provider's own key. The pool agent's guest reaches every build.
- **Let a shell's changes be committed.** It is the quickest way to unstick
  a build. But it would hand every box of the plan whatever was typed, with
  no record of it (§5).
- **Refuse exec into a build, and offer logs alone.** That is safer, but
  some stuck builds can't be explained from logs: a hung step that writes
  nothing, or a network the step waits on. Since §5 makes a shell
  harmless to the snapshot, there is nothing to refuse.
- **Keep a failed build's box, stopped, for a while.** See Deferred.

## Deferred

**Keeping a failed build's box.** Not now, for three reasons:

- On a machine pool a kept box is a machine, either running or stopped.
  Running, it costs the provider's rate and quota, and keeps its egress
  identity and client certificate live. Stopped, it can't be reached:
  `machine.Guest` needs a running guest.
- A build's box is named by its plan (`snap-<key>`, `snapshotBuildID`), so
  the retry after `snapshotRetryAfter` would destroy the kept box to build
  again. Keeping a box for longer than an hour would need a name of its own.
- Both sweeps (`sweepSnapshotBuilds`, `sweepSnapshots`) would need a third
  kind of build box, neither under way nor left over, with its own expiry.

Revisit when a failure the kept logs (§6) and the reported status can't
explain needs a shell after the fact.

## Open for implementation

- The wire form of §1's stream: its framing, how the pool agent knows it
  from stray output, and whether it is the whole `provisionspec.Status` or a
  trimmed form.
- The field names and nesting of `box` and `progress` on the wire.
- How the logs route reads through the guest: a `tail` in the guest's base,
  or a subcommand of the agent the build already holds. Also the tail's
  default length, and the per-feature bound on what §6 keeps.
- The exec stream's protocol between the CLI, the server and the pool
  agent, and whether it matches `admin pool console`'s.
- Whether each runtime's guest allows a second exec beside the build's own.
  Docker does. Each disco-vm driver's guest must be checked, boxd and archil
  first.
- Whether an exec into a build is audited, and where, as an admin's console
  is or is not today.
- How the CLI words and lays out §7: the flag on `admin box ls`, and the
  command names under `admin pool`.

## Consequences

- The snapshot command gains a stdout contract with the pool agent, and
  `runSnapshotCommand` reads output as a stream instead of buffering it.
  An agent bundle from before this writes no progress, and its builds report
  none.
- `snapshotBuild` holds the build's box name, its guest, its latest status,
  and whether a shell has opened it. The failure record gains the last status
  and a directory of log tails.
- The pool agent's API gains two routes for a build, and the server two
  admin routes that forward to them. `Pool.resources.snapshots` gains `box`
  and `progress` (optional, absent from older agents).
- A build someone opened a shell in costs a rebuild: the next create of its
  plan builds it again.
- It splits cleanly: §§1–3 and the listing first, then §4–§5's routes, then
  §6.
- `pool-agent/DESIGN.md`'s Snapshots and Reporting sections, and the
  server's design notes for the new routes, follow when the work lands.
