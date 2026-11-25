# 26-11-25-133 — A box its own egress restricts is provisioned before it is a box

- **Status**: Proposed (would narrow [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §8's "a create finding none for its plan … provisions from scratch" and
  [26-11-03-986](26-11-03-986-a-machine-pool-snapshots-a-plan-by-committing-a-vm-it-provisioned-and-shut-down.md)
  §2's "a driver that does not gets no snapshot … provisions from scratch",
  for a box its own level restricts)
- **Date**: 2026-11-25
- **Issue**: #1763
- **Relates to**:
  - [26-10-01-032](26-10-01-032-a-sandboxs-egress-is-denied-when-any-of-its-levels-says-so.md)
    §5, a new box restricted from its first request;
  - [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
    §1's machine-scoped layers and §5's boot that never waits on provisioning;
  - [26-11-17-316](26-11-17-316-a-pool-reports-its-snapshots-and-each-box-what-it-started-from.md)
    and [26-11-23-004](26-11-23-004-a-snapshot-build-reports-its-progress-and-an-admin-reaches-it-through-its-pool.md),
    what a pool reports of its builds;
  - [26-11-07-519](26-11-07-519-a-running-box-keeps-its-reconcile-failure-as-a-warning.md),
    a box's reconcile warning.

## Context

A box made with its own level already restricted (`discobox run
--egress-deny-by-default --egress-allow github.com …`, `admin box create`
with the same flags) comes up with `failed to provision: brew, go, mise, nix,
rust`, its feature downloads refused by its own allow list. "Locked from the
first second" (#1591) costs the box its features, and nothing says which hosts
they would need. `scripts/live-run.sh` works around it by narrowing a box only
after it has provisioned.

What exists today, at `d160df48`:

- **The server sends a create two policies**
  (`server/internal/resources/sandboxes/reconciler.go:703-710`). `egress` is
  every level merged, the box's own included (`store.EffectiveSandboxEgress`).
  `provisionEgress` is every level but the box's own, with no passthrough
  (`store.ProvisionEgress`, `server/internal/store/egress.go:42`;
  `egress.Provision`).
- **A snapshot build already runs under `provisionEgress`**, anonymously
  (26-10-16-423 §8; `newSnapshotPlan`,
  `pool-agent/sandboxruntime/snapshot.go:176`). A box started from its plan's
  snapshot has its features whatever its own level says.
- **With no snapshot yet, the box provisions in place under its own level.**
  `deliveredImage` (`pool-agent/sandboxruntime/snapshotdocker.go:96-118`)
  starts it from the sandbox base image and asks for the build behind it
  (`requestSnapshot`). The agent inside then fetches through the box's own
  proxy identity, whose policy is `req.Egress`
  (`pool-agent/sandboxruntime/runtime.go:909`).
- **A machine pool whose driver reports no `Capabilities.Commit`** never has a
  snapshot (`MachineSandboxRuntime.snapshotFor`,
  `pool-agent/sandboxruntime/machinesnapshot.go:161-174`), so every box there
  provisions in place.
- **Levels only widen one another** (`egress.Merge`): `DenyByDefault` is any
  level's, and `Allow` is the union. So a box's own level can make its
  effective policy narrower than `provisionEgress` in exactly one way: it turns
  restriction on (`egress.DenyByDefault`) where no other level does
  (`!provisionEgress.DenyByDefault`). An own level that only adds hosts, or one
  under a pool or project that is already restricted, leaves the box at least
  as wide as its provisioning, and in-place provisioning reaches what a build
  would.
- **The box's account has passwordless sudo**
  (`sandbox-agent/boot/user.go:150`). Anything the box's agent holds as root,
  the box's programs can read.
- **No shipped feature says `snapshot: false`**
  (`sandbox-agent/features/*/provision.yaml`, `harness/*/provision.yaml`), so
  today a snapshot holds every step a box would run.

The bug, then, is a box restricted by its own level whose plan has no
snapshot: on the dev loop after every agent rebuild (each changes the plan
key), on any pool's first box of a plan, and on every box of a pool that
cannot commit.

## Decision

### 1. The rule: a box its own level restricts is never provisioned under that level

A create is **own-restricted** when `egress.DenyByDefault &&
!provisionEgress.DenyByDefault`. The pool reads this from the two policies the
create already carries; nothing new is sent. Its machine-scoped layers
(26-10-16-423 §1) are provisioned before the box exists, under
`provisionEgress` and an anonymous identity, never in place under its own
level. Its own programs run only once that is done, and only ever under its
own policy, from their first request.

A create that is not own-restricted is unchanged: it starts from a snapshot
when there is one, and otherwise from the base, provisioning in place with the
build behind it (26-10-16-423 §8).

### 2. Where the pool commits, the create waits for its plan's snapshot

On Docker, and on a machine pool whose driver commits (26-11-03-986 §2), an
own-restricted create that finds no snapshot for its plan:

- asks for the plan's build, or joins the one already running (one build per
  plan, as now);
- does not start the box from the base;
- answers that it is waiting for the plan's snapshot (§4);
- once the snapshot is published, starts the box from it, as any create that
  finds one does.

The snapshot is the plan's, so it serves every later box of the plan,
restricted or not. Nothing about the build changes: anonymous, under
`provisionEgress`, at most `snapshotBuildTimeout`.

### 3. Where it cannot commit, the box's own machine is its build

On a machine pool whose driver does not commit, an own-restricted create:

1. makes the box's machine fresh, as now;
2. runs the snapshot build in it, exactly as a committing pool's build runs
   (26-11-03-986 §1): the build's anonymous identity and proxy material, the
   plan on the imaging command's stdin, every step blocking, under
   `provisionEgress`, sealed and stopped in order;
3. removes the build's proxy material and policy, as a build's cleanup does;
4. starts the same machine and places the box's bootstrap. The box takes its
   own identity from the seal (26-11-03-986 §3), as one copied from a snapshot
   does, and runs only under its own policy.

It answers that it is waiting (§4) while 1–3 run. Nothing is committed or
kept for another box; the next own-restricted box of the plan builds again in
its own machine. A box that is not own-restricted on such a pool is
unchanged: made fresh, provisioning in place.

### 4. Waiting is a condition the server requeues on, not a held request

The pool does not hold the create request open for the build. It answers an
own-restricted create still waiting with a distinct condition. The server
classifies it as one (beside `ErrPoolNotReachable`), records nothing as
failed, and requeues the reconcile shortly after
(`reconcile.RequeueAfter`). The create stays idempotent: a create asked again
for a box that is waiting finds its build (§2: the plan's; §3: the box's) and
answers the same way until it is done.

The box stays `starting`. Its status says it waits for its plan's snapshot,
and the build itself is visible where a pool's builds already are
(26-11-17-316 §2, 26-11-23-004 §7). Attach and `run` wait for it the way
they wait for any `starting` box.

### 5. A build that fails starts the box from the base, and says why

If the build fails (§2: the plan's build fails, or failed within
`snapshotRetryAfter`; §3: the box's own), the create starts the box from the
base and provisions in place under its own level, as today. Its features may
then fail as they do now, and the box is still usable for what its level
allows. The box carries a warning (26-11-07-519) saying it was started without
its features' snapshot because the build failed, with the build's reason, and
that its own egress restricts what in-place provisioning can fetch. A
snapshot is still a cache, never a requirement (26-10-16-423 §8): a failed
build never leaves a box that cannot start.

### 6. What still runs under the box's own level

Some provisioning runs inside a running box, under its own policy, and this
decision does not move it:

- a step or feature marked `snapshot: false` (none ships today);
- a reprovision of failed features, by hand (ADR 26-11-11-560) or the box's
  own retries (ADR 26-11-19-797).

A platform or harness pin that moves remakes the box through a create, so §1
applies to it.

## Alternatives rejected

- **In-place provisioning gets its own, wider policy.** The agent would fetch
  feature downloads through a provisioning identity under `provisionEgress`.
  But the box's programs run while it provisions (26-10-16-423 §5), and its
  account has passwordless sudo, so whatever credential the agent holds, the
  box's programs can use. The box would reach, from its first second, every
  host its own level was set to refuse. That breaks the restriction #1591
  asked for.
- **Features declare their download hosts, and those join the box's level**
  the way a harness image's declared hosts do (`ImageEgressHosts`,
  `egress.go:67`). Every restricted box would then reach brew's, nix's, Go's
  and Docker's hosts for its whole life, long after provisioning, which is
  most of what a person restricting it means to refuse. The lists would also
  have to be kept exact by hand for every feature and every mirror it tries.
- **The CLI warns at create and names the hosts.** It tells the user why, but
  the box still comes up without its features, and the user must either widen
  the box for its whole life or narrow it after it provisions, which is what
  `live-run.sh` already does by hand. With §§2–3 every pool kind provisions
  the box properly, so a warning would only be noise. What still goes wrong
  (§5) is said on the box itself, where it is true.
- **The create request blocks until the build is done.** A build may take up
  to 45 minutes. Holding a pool create that long holds the reconcile and the
  request over a remote pool's tunnel, and a dropped connection loses the
  answer. A condition the server requeues on (§4) costs nothing while it
  waits.
- **A failed build fails the create.** A build commits only a box whose whole
  provisioning succeeded (26-11-03-986 §1), so one flaky feature download
  would leave a restricted box unable to start at all, until the next build
  succeeded. Starting from the base (§5) keeps the box usable and says why.
- **Only Docker and committing pools; a non-committing pool keeps
  provisioning in place.** The triage's first recommendation. It leaves the
  bug on every box of such a pool, and §3 costs one build per box, using the
  machinery a committing pool already runs.
- **Wait for the snapshot for every create of a plan with none.** Boxes that
  are not own-restricted lose nothing by provisioning in place, and making
  them wait would make every plan's first box slower for no gain.

## Deferred

- **Reprovisioning a restricted box under `provisionEgress`** (§6). Revisit
  when a reprovision of a restricted box is seen failing on a refused
  download, or when a shipped feature first says `snapshot: false` and fetches
  something. It needs a way to fetch on the box's behalf that its programs
  cannot use, which §1's reasoning on sudo rules out inside the box.

## Consequences

- A box made with `--egress-deny-by-default` and a narrow allow list comes up
  with its features, on every pool kind, and its own programs never reach a
  host its own level refuses.
- The first own-restricted box of a plan with no snapshot starts only once
  the build is done: minutes, not seconds. On a pool that cannot commit, every
  own-restricted box pays that.
- `scripts/live-run.sh` no longer has to narrow a box after it provisions; it
  can create it narrowed.
- The pool gains one create answer (waiting) and the server one condition to
  requeue on; the box's status gains a waiting reason and §5's warning.

## Open, for implementation

- The waiting answer's shape on the pool API (a status code and error code,
  or a field of the instance) and the requeue interval.
- The exact wording of §4's status and §5's warning, which name no ADR.
