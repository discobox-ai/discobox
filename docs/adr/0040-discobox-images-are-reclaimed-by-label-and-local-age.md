# 0040 — Discobox images are reclaimed by label and local tag age

- **Status**: Accepted
- **Date**: 2026-08-14

## Context

Nothing removes a Discobox image once it stops being needed, and two different
flows keep producing them.

`task dev` rebuilds pool, sandbox, and harness images on every input change and
tags each build with a content-addressed `dev-<hash>` reference (see
`cmd/discobox-docker-image-watch`). The previous build keeps its own distinct
tag, so a day of editing leaves a daemon holding every intermediate image that
was ever built. Those images then travel: `DevelopmentImageSynchronizer` loads
them onto each pool's Docker daemon, whose `/var/lib/docker` is a named volume
that outlives the pool container it is mounted into, so the copies accumulate
there too and survive pool replacement.

Deployments accumulate the same way at a slower rate. Every upgrade pulls a new
pool or sandbox image, and the superseded one stays on the daemon forever.

Docker offers no per-owner reclamation. The closest built-in,
`docker image prune -a --filter label=... --filter until=...`, cannot express
what is wanted here: its `until` filter compares an image's **`Created`**
timestamp, which is when the image was *built by whoever published it*, not when
it arrived on this daemon. A release image built three weeks before it ships is
eligible for pruning the instant it is pulled.

## Decision

Discobox images carry a label saying they are Discobox's to reclaim, and a
reaper on each daemon removes labeled images that no container uses and that
have not arrived locally within a retention window.

1. **The label is applied at build time.** `io.discobox.reclaimable.v1=true`
   (`harness.ReclaimLabel`) is set in `pool-agent/Dockerfile` and
   `sandbox-agent/Dockerfile`. Harness images, the test harness images, and any
   image built `FROM` the sandbox base inherit it through the OCI image config,
   so no harness Dockerfile repeats it.

   Build time is the only time it *can* be applied. A label is part of the image
   config, so it cannot be added to an image after it is pulled without
   rebuilding it into a different image. Labeling what we publish is therefore
   what makes a *pulled* image identifiable later: the label ships inside the
   image and survives the pull.

2. **Local age is `Metadata.LastTagTime`, never `Created`.** The daemon stamps
   `LastTagTime` whenever an image is tagged locally, which covers every way an
   image arrives: `build`, `pull` (including a re-pull that only re-applies an
   unchanged tag), `load` (how image sync delivers development images), and
   `tag`. It is the "when did this image arrive here" clock that `Created` is
   not. An image whose `LastTagTime` is zero has no knowable local age and is
   never reclaimed.

3. **Used means referenced by any container, running or stopped.** A stopped
   sandbox keeps its container, and that container's image must survive; power
   state is not a statement about images. Containers are enumerated with
   `All: true` and their resolved `ImageID`s form the in-use set.

   Usage is then re-checked per image immediately before removal, scoped with
   the `ancestor` filter. The daemon guards the two removal steps unequally: it
   refuses to delete an image a container is using, but it will **untag** one
   without complaint. Since removal untags before deleting, the daemon is only a
   backstop for half the sequence, and a container created after the pass-level
   snapshot would otherwise have the references stripped from its image — the
   container keeps running while nothing can name its image again, and the
   delete then fails, leaving it dangling and unnamed. The up-front set keeps
   the pass cheap; the re-check makes it safe.

4. **A keep set covers what is needed but not yet running.** The host reaper
   keeps the engine's configured pool image and every reference and ID in the
   development image manifest. This is what protects a base image that only
   *other images* build `FROM`: on a dev host nothing runs
   `discobox-sandbox-agent:dev-<hash>` directly, so the container check alone
   would reclaim the base that the next harness build needs.

5. **The newest image of a repository is never reclaimed.** A keep set is a
   snapshot somebody else took, and it can be stale in both directions: the
   server loads the development manifest once at startup, so an image built
   after that is in no keep set at all, and during a rebuild pass — which takes
   far longer than the development window — the manifest on disk still names
   the *previous* build. Age then makes the freshly built image look like
   garbage precisely because it is the one being built on.

   The newest image in a repository is the current one by construction: it is
   what the mutable tag (`:local`, `:latest`) points at and what the next build
   layers on. Keeping it costs one image per repository — the one you would keep
   anyway — and every superseded build is still reclaimed. This is a floor under
   the other rules, not a replacement for them, because deletion is irreversible
   and the keep set is only as fresh as whoever published it.

6. **Two reapers, one per daemon owner.** The pool agent reaps its own pool's
   Docker daemon, because it owns that daemon and keeps running when the control
   plane cannot reach it. The server reaps the host daemon through the Docker
   provider, matching the existing rule that drivers provide connectivity while
   the engine owns the daemon's contents.

7. **Retention is 24h, overridable with `DISCOBOX_IMAGE_RETENTION`.** It matches
   `sandboxVolumeRetention`, the window the pool agent already applies to a dead
   sandbox's volume tree, for the same reason: a window long enough that an
   accidental removal and a same-day recreate cost nothing. The server
   propagates a configured value into the pool container's environment so one
   setting governs both daemons.

8. **A daemon the image watcher drives gets 15 minutes instead.** `task dev`
   supersedes a multi-gigabyte image every few minutes, so a day of grace is a
   day of images — the window has to be shorter than the loop producing them or
   it reclaims nothing that matters. The signal is
   `Config.DevelopmentImageSync`, which is non-nil only when the watcher's
   manifest was loaded, so no new mode flag exists to get out of sync. An
   explicit `DISCOBOX_IMAGE_RETENTION` still wins.

   The window can be this short safely because "superseded" is unambiguous
   here: anything still wanted is named by a container or by the current
   development manifest, and both are checked before age is.

9. **The sweep interval is derived from the window, not configured.** Half of
   it, clamped to [1m, 1h]. The two are useless apart — a 15-minute window swept
   hourly reclaims on the hour anyway — and deriving it is also what carries the
   development cadence into a pool, which has no other way to know it is one:
   the pool agent is handed a retention, not a mode.

## Alternatives considered

- **`docker image prune` with `until` and `label` filters.** Rejected: `until`
  compares `Created`, so it deletes freshly pulled release images built before
  the window. It also cannot express a keep set.
- **A reaper-owned "first seen" state file.** Rejected: `LastTagTime` already is
  that record, maintained by the daemon itself. A private file would have to be
  placed on a durable path in every pool, be reconciled against images the
  daemon reclaimed on its own, and would still be wrong for an image that was
  re-pulled.
- **Reclaiming unlabeled or dangling images generally.** Rejected: on a
  developer's machine the host daemon is shared with images Discobox did not
  create. The label is the boundary of what Discobox is entitled to delete.
- **Counting only *running* containers as usage.** Rejected: it would reclaim
  the image of every stopped sandbox, and the sandbox could then fail to start
  when its pinned digest is no longer present.
- **Reaping from the dev image watcher only.** Rejected: it would fix `task dev`
  churn and nothing else. Images also accumulate on long-running deployments
  that never run the watcher.
- **One retention window everywhere.** Rejected: the production window is set by
  how long an accidental removal stays recoverable, and the development one by
  how fast a rebuild loop produces garbage. A single value has to lose one of
  those, and 24h in a rebuild loop reclaims nothing before the disk fills.
- **A separate `DISCOBOX_DEV` style mode flag.** Rejected: the presence of the
  development image manifest already states it, and a second flag is one more
  thing that can disagree with the first.

## Consequences

- A pinned sandbox image whose container was removed out of band becomes
  reclaimable after the retention window, and a later recreate must re-pull it
  (or fail, if the pin is no longer published). This matches the existing volume
  reaper, which reclaims that same sandbox's data on the same window.
- Under the classic graph driver, removing an image that other images were built
  `FROM` fails while a child exists. The reaper tolerates the conflict and the
  next pass reclaims it once the child is gone. Under the containerd image
  store, untagging the last reference already reclaims the image.
- An image with several tags cannot be deleted by ID without force. The reaper
  removes each reference first, then the ID, so it never needs `Force` and can
  never yank an image out from under a container the daemon knows about. That
  ordering is also why usage is re-checked per image first: untagging is the one
  step the daemon does not refuse for an image in use.
- Two Docker provider instances sharing one host daemon each run a reaper. They
  are idempotent, but each keeps only its own configured pool image, so an
  instance with no pools may have its (unused) pool image reclaimed and re-pulled
  on next use.
- Reclamation makes a built image disappear without any source file changing,
  which the development image watcher had no way to notice: it rebuilt on file
  changes only, so a reclaimed image was never rebuilt while `.env` and the
  manifest went on naming it, and every pool reconcile failed against an image
  that could not come back. The watcher therefore also rebuilds a spec whose
  image has left the daemon. That covers `docker system prune` and a manual
  `rmi` too, which could always have caused this and simply never had a reason
  to happen.
- One image per repository survives forever, including repositories nothing uses
  any more (a harness that was removed, or the throwaway `discobot-dockerfile-test`
  builds). That is the price of never deleting the current build of anything, and
  it is bounded by the number of repositories rather than by build churn.
