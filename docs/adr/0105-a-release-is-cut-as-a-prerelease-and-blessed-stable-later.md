# 0105 — A release is cut as a prerelease and blessed stable by hand

- **Status**: Proposed
- **Date**: 2026-09-10

## Context

Cutting a tag currently decides everything about a release at once. Push
`v1.2.3` and, within the hour, `brew install discobox` serves it, a winget pull
request is open for it, `ghcr.io/discobox-ai/discobox-pool-agent:latest` points
at it, and GitHub's "Latest release" header names it. Push `v1.2.3-rc1` and none
of that happens: the tag shape is the only input, and `STABLE_RELEASE` — exactly
`vMAJOR.MINOR.PATCH` — is the single test every one of those steps consults.

That works while a tag is only ever cut when it is meant to ship. It stops
working the moment you want to *try* a build. The two things you would want are
both unavailable:

- **Installing a candidate the way users install the real thing.** The
  interesting failures in a release are in the packaging: a formula's checksum,
  a codesigned darwin binary, whether the staged server the CLI downloads
  matches the manifest it was cut with. None of those are reachable from
  `task build`, and reproducing them means either publishing to the one channel
  everybody installs from or testing something that is not what users get.
- **Holding a candidate and the stable build at once.** Homebrew refuses to link
  two formulae that install the same file, so a second channel under the same
  formula name is an upgrade, not a second install. Testing a candidate means
  uninstalling the version you rely on.

The obvious answer — cut `v1.2.3-rc1` tags — was available all along and is not
what anybody does, because it makes the artifact you tested not the artifact you
ship. `v1.2.3-rc1` is built, verified, and then thrown away so that `v1.2.3` can
be built again from the same commit. Every property you just checked belongs to
a binary nobody will install.

So the real change is not another tag shape. It is that **whether a release is
stable is not a property of its tag**. It cannot be known when the tag is
pushed, because it is the answer to "did this build turn out to be good", and
nothing at push time knows that.

## Decision

Three states, decided in three different places.

### 1. Cutting a tag publishes a prerelease, and only that

`release:publish` marks every GitHub release `--prerelease`, dot releases
included. Nothing the release workflow knows makes a release stable, so it
claims nothing. GitHub's own "Latest release" header keeps pointing at the last
blessed release, which is the right answer for somebody landing on the
repository.

### 2. `latest` is the newest dot release, and moves at once

A dot release joins the latest channel the moment it is cut. That is what feeds:

- `ghcr.io/discobox-ai/discobox-*:latest`, which is
  `dockerworker.DefaultPoolImage` — the image a pool boots when nothing named a
  version. A released CLI pins its own tag, so the only thing reading `:latest`
  is something that asked for no version, and the newest release is the right
  answer to that.
- The `discobox-dev` Homebrew formula.

An explicit prerelease tag — `v1.2.3-rc1`, `v1.2.3-alpha.4` — moves neither. It
is not the newest release; it is one you have to name. So the tag-shape test
survives, renamed `DOT_RELEASE`, and it now decides one thing (does this join
the latest channel) rather than four.

### 3. `stable` is a human clearing the prerelease box

Blessing a release is editing it on GitHub and unticking "Set as a pre-release".
`promote.yml` listens for that, and it is the only thing that moves the stable
channels: the `discobox` Homebrew formula and winget.

It listens on the release payload rather than on one activity type.
GitHub names a `released` type — "a pre-release was changed to a release" — but
its documentation does not state plainly that unticking the box *alone* emits
that rather than `edited`, and there are reports of a checkbox-only edit
emitting neither reliably. Betting the mechanism on which one arrives would make
the failure silent and indistinguishable from "nobody has blessed it yet". So
the workflow takes `released`, `edited`, and `published`, and every job is gated
on `github.event.release.prerelease == false`: whichever type arrives, the
answer comes from the release's own state. Duplicates are harmless — the tap
commits only what changed and `winget:publish` refuses to open a second request
for a version winget already serves.

The trigger is a checkbox rather than a moving `stable` git tag or a naming
convention because it is the state itself, not a marker for it. The GitHub API,
the release list, `gh release list --exclude-pre-releases`, and the repository
header all already read that bit correctly; a `stable` tag would be a second
source of truth that every one of them would disagree with.

`promote.yml` refuses any tag that is not a dot release
(`release:require-dot`). winget sorts semantic and string versions differently,
so a package that publishes both `1.2.3` and `1.2.3-rc1` ends up pinned to
whichever comparison wins and the only fix is deleting the odd one out — and a
formula named `discobox` serving an rc is the same mistake in the other package
manager. Unticking the box on an rc is a mis-click, not a plan.

There is no un-promote. Both stable channels are recomputed from whatever the
newest blessed release now is, so backing one out is blessing a different
release rather than undoing this one.

### 4. The two Homebrew formulae are separate formulae with separate commands

The tap serves `discobox` and `discobox-dev`. They are generated from the same
assets by the same script, differing only in name.

`discobox-dev` installs its command as `discobox-dev` rather than `discobox`,
because Homebrew will not link two formulae that install the same file and
holding both at once is the entire point. `conflicts_with` and `keg_only` were
both rejected for that reason: each leaves exactly one of the two reachable.

### 5. The CLI reads its own name off argv[0]

One build is installed under two names, and neither is knowable at link time —
the dev formula installs the identical binary. So `commandName()` takes the base
of `os.Args[0]`, and the usage line, the examples in the long help, and the
generated completion scripts are all written in terms of it. A completion script
from a `discobox-dev` install writes `_discobox-dev`, so it does not overwrite
the stable install's.

A name that is not `discobox` or `discobox-<suffix>` is reported as `discobox`.
A `go test` binary, `go run`'s temporary file, and a copy someone renamed are
all still discobox, and output that changed with where the binary happened to
live would be worse than one that did not.

### 6. The two channels share one state directory

`discobox` and `discobox-dev` share a state directory, a configuration file, and
a server stage, so their servers cannot run at the same time. That is accepted,
not worked around: the point of the dev channel is testing what users get, and a
build running against its own private state is not that. The formula's caveats
say so.

## Consequences

- **Cutting a release is cheaper and blessing one is a deliberate act.** Dot
  releases can be cut as often as is useful; none of them reaches a user who did
  not ask for the dev channel.
- **A release is tested as the artifact that ships.** The binary somebody
  installs from `discobox-dev` and the binary `discobox` later serves are the
  same file with the same checksum, because promotion moves a pointer rather
  than building anything.
- **A release that is never blessed is not a failure.** It stays in the release
  list as a prerelease, which is exactly what it was.
- **`brew install discobox` can now go a while without moving.** That is the
  intent, and it is also the new way to break it: forgetting to bless a release
  leaves the stable channel behind with nothing red anywhere. The release
  workflow cannot detect this, because "nobody has decided yet" and "somebody
  forgot" are the same state.
- **A release is promoted by the Taskfile it was cut with.** A release event
  carries the tag as `GITHUB_REF`, so the workflow file that runs and the tree
  `actions/checkout` fetches are the tagged commit's. That is the right one —
  the formula and manifests describe that release's assets — but it means a tag
  cut before `promote.yml` existed cannot be promoted by the checkbox at all.
  Its `workflow_dispatch` is the way to promote those, and the way to re-run a
  half-failed promotion.
- **The tap's update workflow now writes two formulae** and computes each
  channel itself — the newest release for `discobox-dev`, the newest blessed one
  for `discobox`. `brew:refresh` stays one dispatch that recomputes both, so no
  caller has to know which formula it is about to change.

## Alternatives rejected

- **Cut `-rc` tags and re-cut the final.** Available today, and the reason
  nobody does it: the artifact you verified is not the artifact you ship.
- **A `stable` git tag that gets force-pushed.** A second source of truth that
  GitHub's own release list, header, and API would all disagree with, and a
  force-push is a worse gesture than a checkbox for something this consequential.
- **One formula with a `--devel`-style switch.** Homebrew removed `devel` in
  2019 and has no replacement; two formulae is the supported shape.
- **Separate state directories for the two channels.** Would let both servers
  run at once, at the cost of the dev channel no longer testing what users
  actually run — the upgrade path, the existing database, the staged server —
  which is most of what there is to test.
