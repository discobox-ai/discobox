# 0143 — An image arrival no later than its build is an unknown age

- **Status**: Accepted
- **Date**: 2026-09-22
- **Supersedes**: [0040](0040-discobox-images-are-reclaimed-by-label-and-local-age.md)
  §2's premise that `LastTagTime` is stamped on every build; the rest of 0040
  stands unchanged.

## Context

ADR 0040 §2 measures an image's local age from `Metadata.LastTagTime`, on the
premise that the daemon stamps it whenever an image arrives, `build` included.
On the containerd image store that premise fails for a build clamped to
`SOURCE_DATE_EPOCH`: the image's `Created` and its `LastTagTime` are both the
epoch. The Nix dev shell sets `SOURCE_DATE_EPOCH=315532800` (1980-01-01), so
every `docker build` run from it — `task build:harness-image`, for one —
produces an image the reaper reads as 46 years old the moment it exists.

The newest-per-repository floor did not save it, because it compares those
same times: a server reclaimed a hand-built `discobox-harness-shell:local`
seconds after it was built, since the watcher's retagged build of the same
repository carried a real, later arrival. Retagging stamps a real time, which
is why the watcher's own images were never affected; pulls stamp one too.

## Decision

An arrival is known only when `LastTagTime` is after `Created`. One that is
not — equal, as the clamp makes it, or earlier — is treated exactly as 0040
already treats a zero `LastTagTime`: an unknown age, never reclaimed.
Nothing arrives before it was built, so this discards no real arrival.

## Alternatives rejected

- **Unset `SOURCE_DATE_EPOCH` for our Docker builds.** It fixes the builds
  this repository makes and none of the others: anyone building their own
  labeled harness image reproducibly gets the same clamp, and the reaper is
  the thing that deletes.
- **Protect the mutable tags (`:local`, `:latest`) by name.** The reaper
  would have to know every naming convention, and a clamped image under any
  other tag would still be reclaimed.
- **A reaper-owned "first seen" record.** Rejected by 0040 for the same
  reason it would be here: it is state kept beside the daemon that the daemon
  can lose or contradict.

## Consequences

- An image built under a clamp is never reclaimed by age until something
  retags it, which stamps a real arrival. The cost is bounded by its names,
  not by its builds: on the containerd image store a rebuild that takes the
  tag drops the superseded image from the image list, even while a container
  still runs from it, so no unnamed clamped build accumulates for the reaper
  to miss. What stays is at most the image each clamped tag currently names —
  a disk cost, not a lost image. The watcher retags every build it makes, so
  its images are unaffected.
- Pulled images are unaffected: a pull stamps the pull time.
