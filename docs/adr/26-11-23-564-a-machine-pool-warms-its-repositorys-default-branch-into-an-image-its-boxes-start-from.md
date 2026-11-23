# 26-11-23-564 — A machine pool warms its repository's default branch into an image its boxes start from

- **Status**: Proposed
- **Date**: 2026-11-23
- **Builds on**:
  [26-11-03-986](26-11-03-986-a-machine-pool-snapshots-a-plan-by-committing-a-vm-it-provisioned-and-shut-down.md),
  whose imaging exec, done record, fixed names, sweep and fallback this reuses
  for a second, source-aware layer;
  [26-11-17-316](26-11-17-316-a-pool-reports-its-snapshots-and-each-box-what-it-started-from.md),
  whose report and `startedFrom` this extends.
- **Relates to**:
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §8 (a snapshot holds no source; this adds an image that is built from one
  and keeps none);
  [26-10-10-545](26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md)
  and [26-10-23-948](26-10-23-948-a-pool-pulls-a-cached-source-first-and-a-client-supplies-what-the-pull-could-not.md)
  (the per-remote Git cache that names the commit warmed);
  [26-11-02-023](26-11-02-023-every-machine-sandbox-mounts-its-volumes-from-its-own-data-and-cache-roots.md)
  §2 (a machine's first mount moves what its disk holds into its volumes);
  [0141](0141-a-sandbox-account-is-created-with-an-id-the-guest-gives-accounts.md) (the
  sandbox account may differ per create).

## Context

A plan's snapshot (ADR 26-11-03-986) holds everything the plan's bundles
provision, and nothing of any source. Each new box of a repository therefore
starts with an empty cache of what the repository itself needs. Its first
`go build`, `npm ci`, `nix develop`, `cargo fetch` or `docker compose pull`
downloads and builds everything again. For this repository that is the Nix
shell, the Go module and build caches, and the sandbox base images. It takes
minutes, on every box.

The work depends on the repository and changes slowly: the dependency set at
the default branch's tip. Most boxes of a project start from the same
remote, near that tip.

A machine pool can already capture a box as an image when its driver reports
`Capabilities.Commit`. disco-vm's `Commit` contract also lets a VM be made
`From` a committed one, changed, and committed under a new name
(`engine.Commit`'s doc, ADR 26-11-03-606). What is missing is a step that
runs the repository's own preparation with its source present, and a rule
for when the result is current.

Constraints that shape it:

- **The pool fetches no private remote of its own.** It holds what members
  delivered and what an anonymous pull found (ADR 26-10-23-948). Its record
  of the remote's branches is `refs/cache/heads` in the remote's cache,
  replaced by each member delivery that names them. A sandbox delivering to
  a box it created never moves it (`pool-agent/DESIGN.md`, Git origins).
- **`Commit` keeps the root disk alone.** It removes the VM's data volume
  and cache clone. A build mounts no declared volume (ADR 26-11-03-986 §1),
  so whatever it writes lands on the root disk. A machine's first wiring
  moves what its disk holds at a declared path into its own volume
  (ADR 26-11-02-023 §2).
- **The sandbox account is the create's.** It may mirror the client's name
  and ids (ADR 0141), so one account's home is not another's.

## Decision

### 1. A repository opts in with `.discobox/warm`

A repository warms when the commit at its default branch's tip holds an
executable file at `.discobox/warm`. Its presence is the whole opt-in. No
project setting, server field or API call turns it on. Removing the file
turns it off at the next tip.

The script is the repository's to write. It is run from the checkout's root
as the sandbox account, with the account's login environment and the
build's proxy environment. It fetches and builds whatever the repository
wants a new box to find already done. For example:

```sh
#!/usr/bin/env bash
set -euo pipefail
nix develop -c true
go mod download
go build ./...
```

Its exit status is the warm's result. A non-zero status fails the build (§6).

### 2. Only a machine pool whose driver commits warms, and only its primary source

A warm image is built only on a machine pool whose driver reports
`Capabilities.Commit`, the same gate as ADR 26-11-03-986 §2. Every other pool
reads no `.discobox/warm` and builds nothing.

Only the box's primary source is warmed, and only when it is delivered
through the pool's Git cache (a remote the cache is keyed by). A box with no
source, or a directory source with no remote, has nothing to warm from.

### 3. The commit warmed is the cache's record of the remote's default branch

The pool warms the commit its cache records for the remote's default
branch: the `HEAD` of `refs/cache/heads`, as a member delivery or an
anonymous pull last set it. The pool never fetches the remote to learn it,
and never warms a branch or a commit a box asked for.

Only the remote's own history is trusted to say what the warm image holds.
A box's pushes go to its own namespace and never move `refs/cache`, so a box
cannot choose the code a later box's image was prepared by. A project
member's delivery can, because it reports the remote's branches as that
member fetched them. The project already trusts that member with the
source every box clones.

The pool reads `.discobox/warm` from the cache's object store (a tree
lookup at that commit) to decide whether there is anything to warm. That
runs nothing.

### 4. A warm image is keyed by plan, remote, target and account

The **warm key** is the digest of:

- the plan's snapshot key (ADR 26-11-03-986 §4): base, project, pool, agent,
  pins, features off and `provisionEgress`;
- the primary source's cache key (its normalized remote);
- the primary source's in-sandbox target (`/workspace/<repo>`), since caches
  often key on absolute paths;
- the sandbox account: its name, uid, gid and home.

The commit is not part of the key. One key has at most one published warm
image at a time, and the commit it was warmed at is recorded beside it (§5).

Two people whose accounts differ get two warm images of the same repository.
That is the price of a home that is the person's own.

### 5. A warm image is a committed VM, published by a done record

A warm image reuses ADR 26-11-03-986 §4's discipline, with one change: its
name is per commit as well as per key, so a stale image keeps serving while
its successor builds.

- **The name** is `warm-<24 hex digits>`: the first 24 hex digits of the
  digest of the warm key and the commit. One name per (key, commit) keeps
  recovery as "build again". A key's successive commits never collide.
- **The done record** is `warm-records/<warm key>.json` in the pool's tree,
  written by a synced rename. It names the published VM, its instance ID,
  its commit, `built`, `lastUsed` and the plan, remote, target and account
  it was built for. A create uses the VM only if the record exists and the
  VM of that name has that ID.
- **A build** of (key, commit) runs:
  1. destroy any VM of the build's name and confirm it is gone;
  2. create it `From` the plan's published snapshot (§7), isolated and fenced
     as a snapshot build is, and start it;
  3. place the agent and run the imaging exec with a warm section (§7);
  4. stop it in order and `Commit`;
  5. replace the done record with one naming the new VM;
  6. drop the VM the old record named, as a snapshot is dropped (record
     already moved, then the VM; a VM a create holds, or hcs's `ErrInUse`, is
     left for the sweep).

  A failure stops at its step and destroys the build's VM. The old record,
  and the image it names, stand.
- **The sweep** (ADR 26-11-03-986 §5's, on the same pass) destroys every
  `warm-` VM that no record names and no build under way holds. The orphan
  reaper keeps to `sbx-`, the snapshot sweep to `snap-`, this to `warm-`.
- **A reclaim** drops a warm image unused for `snapshotRetention`, by
  `lastUsed`. A cache clear drops every warm image with every snapshot.

### 6. A warm image is used while it is young enough, and rebuilt behind the create that finds it stale

At create, with a primary source the pool can warm (§2), the pool reads its
cache's default-branch tip and looks up the warm key's record:

| What the record says | The box is made `From` | And the pool |
| --- | --- | --- |
| No record | the plan's snapshot, or the base | asks for a warm build, if the tip holds `.discobox/warm` |
| Built at the tip | the warm image | nothing |
| Built at another commit, under `warmRefreshAfter` (1 h) old | the warm image | nothing yet |
| Built at another commit, `warmRefreshAfter` old or more | the warm image | asks for a rebuild at the tip |
| Any commit, `warmMaxAge` (24 h) old or more | the warm image, if under `warmExpireAfter` (7 d) | asks for a rebuild at the tip |
| `warmExpireAfter` old or more | the plan's snapshot, or the base | asks for a rebuild at the tip |
| The tip holds no `.discobox/warm` | the plan's snapshot, or the base | drops the warm image |

A box made from a stale warm image is not wrong. It clones its own source at
the commit it asked for, as any box does (`pool-agent/DESIGN.md`, Sources).
The warmed caches are older than that source and only cost it a delta. So a
stale image is still used, a rebuild is asked for behind it, and a create
never waits for a warm build.

`warmRefreshAfter` keeps a busy default branch from rebuilding on every
merge. `warmMaxAge` rebuilds an image whose tip has not moved, since what it
fetched (a floating tag, a nightly toolchain) moves without it.
`warmExpireAfter` stops a box from starting a week behind. The values are the
pool's and are not configured per project.

The same periodic pass that sweeps (§5) also asks for a rebuild of any warm
image used within `warmRefreshAfter` whose record is stale by the table
above. The first create after a merge then finds the image current, without
waiting on a create to notice. A warm image nobody has used recently is
never rebuilt on a timer. It ages out by reclaim.

Builds follow the snapshot's limits: one at a time per pool (they share the
build lock), at most one per warm key, `snapshotBuildTimeout` (45 min) for
the build as a whole.

### 7. The warm build is the snapshot's imaging exec with a source and a script

A warm build starts `From` the plan's published snapshot. If the plan has
none, the warm build is not started: the create asks for the plan's
snapshot as now, and a later create asks for the warm image once it exists.
A warm image is always one layer over a known-good snapshot, never a second
way to provision.

The taker places the agent and runs `discobox-sandbox-agent snapshot` as
ADR 26-11-03-986 §1 does. Its `SnapshotPlan` gains an optional `warm`
section:

- the account to create and run as (name, uid, gid, home);
- the source: its target, the commit, and an origin at
  `git.discobox.internal` with a token for the build's identity alone,
  `origin:fetch`, for a namespace the pool pins at the commit for this
  build (`refs/namespaces/<build ID>-warm`) and removes after it;
- nothing else: no secret, gateway, sentinel or credentials endpoint, as for
  a snapshot build. `Validate` keeps refusing every one of them, and accepts
  this one source.

The command, after its plan's provisioning, which finds every feature already
done on a box made from the snapshot:

1. creates the account and its home on the root disk, where a sandbox would
   find them before its wiring moves them (ADR 26-11-02-023 §2);
2. clones the source into its target as the account, at the commit;
3. runs `.discobox/warm` from the checkout's root as the account, under the
   build's `provisionEgress` and its proxy, with its output bounded into the
   result;
4. stops what the script left running: the account's processes, its user
   manager, and any container in the box's Docker;
5. **removes the source**: the whole target directory and the clone's
   origin credential;
6. cleans and seals as a snapshot does (machine-id, SSH host keys, the
   build's proxy CA, `SnapshotDir`, the agent's start link, the seal).

What the script wrote anywhere but the checkout stays: the home's caches,
`/nix/store`, `/var/lib/docker`'s images. A box made from the image wires its
volumes on its first boot, and its first mount moves each declared path's
content off the root disk into the box's own volume. A box clones its source
into an empty target, as a box from a snapshot does.

### 8. A warm image that proves bad falls back to the plan's snapshot

ADR 26-11-03-986 §6 holds, one layer up. A machine made from a warm image
whose agent never answers on its first start drops the warm image, which
then waits `snapshotRetryAfter` as a failed build does. The machine is
destroyed and made again from the plan's snapshot, or the base, in the same
start. The plan's snapshot is not blamed.

A warm build that fails, by the script's exit status, the timeout or any
step of §5, records a failure (ADR 26-11-17-316 §3's JSON record, under
`warm-failures/<warm key>`) with its commit and the error. That key waits
`snapshotRetryAfter` before a create asks again, unless the tip has moved
since: a fix merged to the default branch is tried at once.

### 9. Reported as snapshots are

ADR 26-11-17-316's report grows by the same three lists for warm images:
`held` (key, remote, target, account name, commit, `builtAt`, `lastUsedAt`,
`boxes`, and the plan's snapshot key), `building` (with its commit) and
`failed` (with its commit and the script's error). A box's `startedFrom`
gains `{"from": "warm", "warm": "<warm key>", "commit": "<sha>"}`. The
control plane still decides nothing from either (26-11-17-316 §6).

## Alternatives rejected

- **Keep the checkout in the image, with the script's untracked output
  (`node_modules`, `target/`, `.direnv`).** The largest saving for some
  repositories. But a box's source is a bind onto its target from its own
  volume, cloned by the agent into an empty directory, from its own
  namespace, at its own commit (`pool-agent/DESIGN.md`, Sources). A kept
  checkout would need a second materialization path that adopts a tree at
  another commit, under another origin, and decides which untracked files
  still hold. The image also stays free of the source, which keeps it from
  carrying anything of the repository a box's own clone would not.
- **Warm the commit the creating box asked for.** Every branch would build
  its own image, and a box could choose the code that prepares the next
  box's image. The default branch is shared, slow-moving, and moved only by
  the remote's own history.
- **Fetch the remote from the pool on a timer to find the tip.** The pool
  holds no credential for a private remote (ADR 26-10-23-948 §§4–5). The
  cache's record is as fresh as the project's last delivery, which a busy
  project renews constantly, and a quiet one does not need.
- **Rebuild on every move of the tip.** A busy branch would run a build per
  merge and mostly throw them away. A stale image already saves most of the
  work.
- **A fixed name per key, as a snapshot has.** A rebuild would destroy the
  published image first (ADR 26-11-03-986 §4 step 2), leaving every create
  during the build to start cold. A name per (key, commit) keeps recovery
  deterministic and the old image serving.
- **Build from the base and provision the plan in the warm build.** It
  repeats minutes of provisioning on every refresh, and makes a second path
  to a provisioned box that could disagree with the snapshot's.
- **Declare the script in `.discobox/settings.json` (the project layer).**
  The project layer is read from the box after it clones, and configures a
  box. The warm script prepares an image before any box exists, read by the
  pool at a commit. A file whose presence opts in needs no schema.
- **Run the warm with the project's secrets** (a token for a private
  registry or module proxy). Every box of the project, and every account,
  would start from an image whose caches a secret fetched. The pool would
  hand a build the credentials ADR 26-11-03-986 keeps out of every build.
  See Deferred.
- **Leave out the account and warm as a fixed user.** The home a warm fills
  would be another account's, and a box whose account differs would find it
  owned by a stranger or not at all.

## Deferred

- **A Docker pool.** Its snapshot is a committed image, and declared data
  paths overlay image content (ADR 0007 §3), so the same build would work
  there. Out of scope until it is wanted: revisit when a Docker pool's first
  build in a box is measured as a large share of its time to work.
- **Secrets for the warm**, through a binding scoped to the build. Revisit
  when a repository's warm cannot run without a private registry, and with a
  rule for who may grant it.
- **Keeping the checkout** (the first alternative). Revisit if, measured on
  a repository that warms, the time a box spends rebuilding in-tree output
  dominates what the warm image saves.
- **More than the primary source, or more than the default branch.** Revisit
  when a project's boxes routinely start on a long-lived second branch.

## What it needs of disco-vm

- **A VM made `From` a committed VM can itself be committed**
  (`engine.Commit`'s contract: "create a VM from it, change that, and commit
  it under a new name"). Where a driver chains copies below the seam (hcs),
  dropping a plan's snapshot may now be refused with `ErrInUse` while a warm
  image made from it stands. The sweep already retries such a VM, and a
  cache clear drops the warm images first.

## Consequences

- `pool-agent/sandboxruntime`: `machinesnapshot.go` gains warm records,
  names, the sweep's `warm-` pass, the create's choice (§6) and the fallback
  (§8). `snapshot.go`'s plan gains the warm section and the warm key.
  `gitcache` gains reading the default-branch tip and one file at a commit,
  and a namespace pinned for a build.
- `pkg/agentlayer.SnapshotPlan` gains `warm`, and its `Validate` its one
  source. The sandbox agent's `snapshot` command gains §7's steps 1–5.
- The report and `startedFrom` grow by warm entries (§9). The API, CLI and
  console show them where they show snapshots.
- A box made from a warm image boots slower once, on a driver whose data
  volume is a separate disk: its first mount copies the warmed caches across
  filesystems (ADR 26-11-02-023 §2). That is the cost of a commit that keeps
  only the root disk. It is paid once per box, at disk speed rather than
  network speed.
- A pool holds up to one more committed VM per (plan, remote, target,
  account) in use, plus one in flight per rebuild. Storage is charged to the
  pool, as for snapshots (ADR 26-11-14-896).
- The repository's warm script runs with the project's egress policy and no
  secrets, so a script that needs a private host fails its build. The
  failure is reported, and every box still starts from the plan's snapshot.
- `pool-agent/DESIGN.md`'s Snapshots section gains the warm image when the
  work lands.
