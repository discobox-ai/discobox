# 26-11-13-655 — An unknown image age is started by the reaper, and only a kept repository keeps its newest

- **Status**: Proposed (on acceptance supersedes
  [0040](0040-discobox-images-are-reclaimed-by-label-and-local-age.md) §2's
  "an image whose `LastTagTime` is zero is never reclaimed" and §5's newest
  image of *every* repository, and narrows
  [0143](0143-an-arrival-no-later-than-the-build-is-an-unknown-age.md)'s
  "never reclaimed" to the ordinary pass on the containerd store)
- **Date**: 2026-11-13
- **Issue**: [#1094](https://github.com/discobox-ai/discobox/issues/1094)
- **Relates to**: #1098, a pool whose disk is nearly full reports not-ready,
  which this ADR does not decide.

## Context

Checked against `main` at `be4138204`.

ADR 0040 dates an image by the daemon's `LastTagTime`. It assumes the daemon
sets that time on every arrival: build, pull, load and tag. ADR 0143 found one
exception, a build clamped to `SOURCE_DATE_EPOCH`, and decided that an arrival
no later than `Created` is an unknown age. `imagereap.Reclaimable` never
reclaims an unknown age (`Candidate.knownArrival`,
`pool-agent/imagereap/imagereap.go`).

The premise fails as a whole on the classic image store (overlay2). There,
`LastTagTime` is set only by `docker tag`. A load leaves it zero, and so does a
pull, as #1094's triage reproduced on Docker 29.9.3. Every shipped VM pool runs
that store. The guest the server pins (`guestimage.DefaultVMImage`, `vm/v6`,
2026-09-09) predates `vm-image/docker-daemon.json` turning on
`containerd-snapshotter` (2026-10-21), and no guest with that change has been
pinned since. Images reach such a pool by an image cache load followed by a
pull by digest (`server/providers/dockerworker/image_load.go`,
`recordRegistryDigest`), or by a pull (`image_ensure.go`, `ensureImageRef`; the
pool agent's `ensureImageAvailable`). Either way they arrive with a zero
`LastTagTime`.

Three rules then combine so that nothing is ever reclaimed:

- Every labeled image has an unknown age, so no pass removes it by age.
- `newestPerRepository` compares those same zero times. Every image in a
  repository ties, and a tie protects all of them.
- The pool agent passes no keep set (`runtime.go`, `reclaimImages`;
  `clearcache.go`, `clearCaches`). It has no other signal to tell a current
  image from an obsolete one.

`discobox admin pool clear-cache` calls the same `Reclaim` with a one-hour
floor, so it removes nothing either. The reported pool held 47 images
(42.7 GiB) from every release since v0.5.2, filled its 98 GiB data disk, and
still reported ready.

The newest-per-repository floor (0040 §5) has a second cost, apart from the
unknown ages. 0040 accepted it as one image per repository, kept forever. Since
v0.19 every box runs `discobox-sandbox-base`, so every per-harness and
`discobox-sandbox-agent` repository is obsolete as a whole, and the floor keeps
its newest image on every pool that ever ran one. 0040 §5 was written for one
case only: a keep set naming an earlier build *of the same repository* than the
one being built on. That rationale covers no repository that no keep reference
names.

How the two stores set `LastTagTime` when a tag is applied again, checked on
Docker 29.9.3 while writing this ADR:

| Operation | Classic store | Containerd store |
|---|---|---|
| `load` | zero | the load time |
| `pull` | zero (#1094 triage) | the pull time (ADR 0143) |
| clamped `build` | (not tested) | the clamp epoch (ADR 0143) |
| `tag X X`, an unchanged tag | **now**, every time | no change |
| `tag X Y`, a new tag | now, kept for the image | now, but only while `Y` exists |

## Decision

The label stays the boundary of what may be deleted (0040 §1). Only an image
carrying `io.discobox.reclaimable.v1=true` is ever a candidate, so an image
Discobox did not label, such as a developer's `ubuntu`, is never touched by any
rule below. Usage (0040 §3), retention (§7, §8), the two reapers (§6) and the
derived interval (§9) are unchanged.

1. **A reaper starts an unknown age instead of keeping it forever.** In each
   pass, each labeled image with an unknown age that has a tag is stamped:
   the reaper applies one of its tags to itself again (`ImageTag(t, t)`). On the
   classic store this sets `LastTagTime` to now, so the image's retention window
   starts at the first pass that sees it. The record is still the daemon's own,
   not a private one, which is what 0040 rejected a "first seen" file for.
   Images are stamped oldest `Created` first, so within one repository the
   newest build gets the latest arrival, and §3 keeps the right one. A stamp
   that fails is logged and the image keeps its unknown age until the next
   pass.

   On the containerd store the same call changes nothing. There, an unknown
   age means a clamped build (0143), and the ordinary pass keeps it as before.
   No store check is needed: the daemon's own behaviour splits the two cases.

2. **Discobox's own arrivals are stamped as they land.** The image cache load
   (after `recordRegistryDigest` on the classic store), the server's pull in
   `ensureImageRef`, and the pool agent's pull in `ensureImageAvailable` each
   end by applying the reference the image arrived under to itself again. A
   just-arrived image then has a real age before any pass runs, which §4
   depends on. A failed stamp is logged and never fails the load or pull. An
   unknown age is the safe direction under every rule except §4's, and §4 is an
   explicit clear.

3. **The newest image is protected only in a repository a keep reference
   names.** `newestPerRepository` considers only the repositories of the
   `Keep` references. In each of those, the newest image by known arrival is
   protected. If no image there has a known arrival, the newest by `Created`
   is protected instead. A tie still protects every image in it. A repository
   that no keep reference names has no floor: its images are reclaimed on
   usage and age like any superseded build.

   For this to cover the pool agent, it gets a keep set: its sandbox base image
   (`DISCOBOX_SANDBOX_BASE_IMAGE`, which the engine already delivers in
   `poolPolicyEnv`), passed to both its periodic pass and `clear-cache`. The
   server's keep set is unchanged: the configured sandbox base image and the
   development image manifest. So the case 0040 §5 was written for, a build
   newer than the server's snapshot of the manifest, is still protected,
   because its repository is one the manifest names.

4. **`clear-cache` treats an unknown age as older than its one-hour floor.**
   An explicit clear removes an unused, labeled image that is not kept and has
   no known age at once, tagged or not. The ordinary pass still waits for §1 to
   start its clock. `clearImageFloor` was there so that an image another
   pool's create had just pulled onto a shared daemon was not removed before
   its container existed. Under §2 such an image arrives with a known age, so
   the floor still covers it.

## Alternatives rejected

- **Stamp at arrival only (§2 without §1).** The 47 images already on upgraded
  pools would never be stamped, and neither would anything that arrives some
  other way, such as a `docker pull` in a pool console or an image from before
  this change. The ordinary pass would still never reclaim them.
- **A keep set from the current release instead of age** (only what the
  release or bundle manifest names is current, and everything else goes). The
  keep set answers which images are current, not how long an unused one has
  been unused. A pinned sandbox image whose container was removed would
  disappear at the next pass instead of after the retention window 0040 §7
  sets it, and on a shared daemon so would an image another pool's create had
  just pulled. The keep set is used here only for what it can tell: which
  repositories keep a floor (§3).
- **Treat an unknown age as old in the ordinary pass too.** That brings back
  0143's failure: on the containerd store, a clamped build is removed the
  moment it exists. Starting the clock (§1) costs one retention window and
  removes nothing early.
- **A store-specific stamp that works on containerd too** (tag a temporary
  name, or remove and apply the tag again). On containerd the time only lasts
  while the extra tag exists, so the stamp would leave a name behind or remove
  a tag a create may be resolving at that moment. 0143's keep is the decided
  behaviour there, and nothing in #1094 happens on that store.
- **Keep the floor for every repository on the host daemon, and narrow it only
  on daemons a pool owns.** Each reaper would need a new signal saying whose
  daemon it is, and the pool agent on the local Docker provider shares the host
  daemon. The label already bounds what any reaper may delete, so one rule is
  applied everywhere (owner, on #1094).
- **Drop the floor only in `clear-cache`.** One image per obsolete repository
  would still build up between clears, on every pool that never runs one.
- **Pin the containerd-store guest and stop here.** A guest moving stores
  leaves the classic store's data on the disk, out of sight of every daemon API
  (`vm-image/DESIGN.md`). It does not reclaim what the reported pool holds, and
  any classic-store daemon, a Linux host's included, would still never
  reclaim.

## Consequences

- On an upgraded classic-store pool, the first pass after this ships stamps
  every labeled image, and the pass one retention window later removes each
  one that is unused, not kept, and not the newest of a kept repository.
  `clear-cache` frees them at once.
- A developer's labeled image in a repository the keep set does not name is
  reclaimed one window after it arrived, if no container uses it. Such an
  image is built `FROM` the sandbox base and inherits its label (0040 §1).
  Today only the newest one in its repository is kept forever, so this changes
  only for that newest image.
- `clear-cache` on the local Docker provider acts on the shared host daemon. On
  the containerd store it now removes an unused clamped build that is not
  kept, even one built a moment ago. That is what an explicit clear asks for.
- The snapshot repository (`discobox-snapshot`) is not named by any keep
  reference, so its newest snapshot loses the floor that kept it. Every other
  snapshot was already reclaimed on age, and `clear-cache` already clears all
  of them by name.
- An untagged image with an unknown age cannot be stamped. Only `clear-cache`
  removes it.
- On the containerd store, each pass applies a clamped build's tag to itself
  again for nothing, one cheap call per such image.
- Covering §1 needs a classic-store daemon in CI. Today's integration test
  passes on either store only because `ImageBuild` with tags goes through tag
  (#1094 triage).

## Left to implementation

- Where the classic-store daemon for the integration tests comes from (a
  `docker:dind` service with `containerd-snapshotter=false`, or the pool guest).
- Whether §1's stamp runs inside `labeledImages` or as a separate step of
  `Reclaim` before `Reclaimable`, as long as `Reclaimable` stays the pure,
  table-tested decision.
