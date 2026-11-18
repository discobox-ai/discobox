# 26-11-18-919 — Edge and main have Homebrew formulae, kept current by the workflows that publish them

- **Status**: Proposed (would supersede, in
  [26-11-16-249](26-11-16-249-main-is-a-channel-built-into-its-own-repository-and-latest-is-no-channels-name.md),
  §1's Homebrew column for `edge` and `main`, §6's "reinstall from `--channel
  main`" for a CLI run as `discobox-main`, §7's "There is no Homebrew formula
  for main", and its rejected alternative "A `discobox-main` Homebrew
  formula". The rest of 249 stands. Superseded rather than amended because
  main builds have shipped against 249.)
- **Date**: 2026-11-18
- **Issue**: [#1368](https://github.com/discobox-ai/discobox/issues/1368)
- **Relates to**:
  [0105](0105-a-release-is-cut-as-a-prerelease-and-blessed-stable-later.md)
  §§4–6, whose rule for a second formula (its own name, its own command, one
  shared state directory) this applies twice more;
  [0106](0106-a-consumer-points-at-the-asset-mirror-only-if-it-can-fall-back.md)
  §3, every formula names the release URL alone;
  [0110](0110-the-install-script-is-a-release-asset-and-a-channel-names-which-one-runs.md)
  §3, the installer's channel rules the tap must agree with.

## Context

The owner asked (2026-11-18) whether `edge` and `main` can be installed with
`brew`. ADR 26-11-16-249 said no: §1's table gives both "none", §7 says "There
is no Homebrew formula for main", and its alternatives reject a `discobox-main`
formula because "the tap would commit a dozen times a day, and a formula names
exactly one URL (0106 §3), with no fallback. The installer covers main."

What exists at `7dfa91f71`:

- **The generator.** `scripts/brew-formula.sh [--beta | --dev] <tag> [repo]
  [output]` downloads `discobox-darwin-arm64`, `discobox-linux-amd64` and
  `discobox-linux-arm64` from `<repo>`'s release `<tag>`, hashes them, and
  writes a formula. The channel picks the class, the command (`bin.install …
  => "<cmdname>"`), an alias command, the `desc`, and a caveats paragraph that
  any non-stable channel gets, worded for beta. `homepage` is
  `https://github.com/<repo>`.
- **The tap updates itself, and only when told.**
  `discobox-ai/homebrew-tap`'s `update-formula.yml` runs on
  `workflow_dispatch` alone. It checks out this repository's default branch
  for the generator, resolves each channel with `gh release list` against
  `discobox-ai/discobox` (the newest blessed release for `discobox`, the
  newest `vX.Y.Z` among the last 30 for the beta channel), regenerates both,
  and commits whichever changed. It still writes the beta channel as
  `Formula/discobox-dev.rb` with `--dev`: 249 §2's rename to `discobox-beta`
  has reached the generator (`--beta`) but not the tap. It had a 30-minute cron and dropped it on purpose: "a poll that
  quietly covers for a broken release step is a poll that stops anyone
  noticing the step is broken". `brew:refresh` (`Taskfile.yml`) is the one
  dispatch, with `HOMEBREW_TAP_TOKEN`: a token scoped to the tap that can
  start its workflow and cannot write to it.
- **Who dispatches.** `release.yml` runs `brew:refresh` only when
  `release:dot` says `yes` (a `vX.Y.Z` tag), and fails the release if the
  token is missing. `promote.yml` runs it on every blessing.
  `main-build.yml` never does; its `publish` job publishes into
  `discobox-ai/discobox-main` with `MAIN_RELEASE_TOKEN` and then runs
  `release:prune-main`.
- **How often each channel moves.** Edge moves on every `v*` tag, `-rc` and
  `-alpha` included. Main moves on every coalesced green main build, on the
  order of a dozen a day (249 §3), each a full release in
  `discobox-ai/discobox-main` marked latest (249 §4), deleted after fourteen
  days except the newest (249 §6).
- **The CLI already answers to any `discobox-<suffix>` name.**
  `commandName` (`cli/internal/cli/root.go`) takes it off argv[0], so
  `discobox-edge` and `discobox-main` need no CLI change to name themselves in
  usage and completion (0105 §5).
- **A pruned main build points at the installer.**
  `withDeletedMainBuildHint` (`cli/internal/cli/server_resolve.go`) tells a
  main CLI whose server or images are gone to run `curl -sSfL
  https://main.discobox.ai | sh`. That installs as `discobox`, over the stable
  command, which is the wrong advice for a Homebrew install.
- **Docs say there is none.** README's "Homebrew" section: "There is no edge
  or main formula — use the install script for those."

249's two objections, read against that:

- **One URL, no fallback.** True of every formula, stable included. 0106 §3
  already decided that each formula names GitHub's release URL alone, and that
  holds for `discobox` today. It is not a reason main differs from stable.
- **Tap churn.** A tap commit per main build is real. What it costs is a
  dozen small commits a day in the tap's history and a dozen short tap runs.
  Nobody who has not installed `discobox-main` is offered anything by them:
  `brew update` fetches the tap either way, and `brew outdated` lists only
  installed formulae.

## Decision

### 1. Two more formulae, each its own command

| Channel | Formula | Command | Reads |
| --- | --- | --- | --- |
| `stable` | `discobox` | `discobox` | `discobox-ai/discobox` |
| `beta` | `discobox-beta` (249 §2) | `discobox-beta` | `discobox-ai/discobox` |
| `edge` | `discobox-edge` | `discobox-edge` | `discobox-ai/discobox` |
| `main` | `discobox-main` | `discobox-main` | `discobox-ai/discobox-main` |

They are 0105 §4's shape: separate formulae, the command installed under the
formula's name, no `conflicts_with`, no `keg_only`, so any of the four can be
installed beside the others. No alias command; neither has an old name.

`scripts/brew-formula.sh` gains `--edge` and `--main`, the same way `--beta`
is decided: class `DiscoboxEdge` / `DiscoboxMain`, the command name, `desc`
naming the channel, and the caveats in §5. `homepage` is
`https://github.com/discobox-ai/discobox` for every channel, not the
repository the assets came from: `discobox-ai/discobox-main` holds nothing
but releases.

`brew install discobox-ai/tap/discobox-edge` and
`brew install discobox-ai/tap/discobox-main`: the same tap as the other two.

### 2. The tap resolves each channel by the installer's rules

The tap and the installer must give the same answer for a channel, so the tap
uses the rules `pkg/installer/install.sh` `resolve` uses (0110 §3, 249 §1):

- **The same window.** The installer reads one page of 100 releases; the tap
  reads the same 100 (`--limit 100`, not today's 30) for stable, beta and
  edge, so the tap and the installer read the same releases.
- **edge**: among `discobox-ai/discobox`'s releases whose tag starts `v[0-9]`
  (so never a `vm/*` or `vm-kernel/*` line), not drafts, the newer by version
  core of the newest blessed release and the newest prerelease (newest by
  GitHub's list order), blessed winning a tie. Not simply the first in the
  list: a patch tagged on an older line after a newer release was blessed is
  first in the list, and the blessed release is still edge.
- **main**: `discobox-ai/discobox-main`'s latest release, which 249 §4 makes
  the newest main build. Never paged; the newest is never pruned (249 §6).

### 3. The workflow that publishes a release tells the tap; no poll

The no-cron rule stands. A channel's formula moves because the workflow that
moved the channel dispatched the tap, and a release whose dispatch could not
run fails:

- **`release.yml` dispatches on every tag**, not only a dot release. Every tag
  moves edge. The missing-token check moves with it: any tag without
  `HOMEBREW_TAP_TOKEN` fails the release.
- **`main-build.yml`'s `publish` job dispatches** after `release:publish`, with
  the same `HOMEBREW_TAP_TOKEN`. It is a secret of this repository, so the
  workflow already can read it: no new credential, and none with write access
  to the tap. The dispatch and `release:prune-main` each run once the publish
  has succeeded, whichever of them fails: a failed dispatch does not skip the
  prune, nor a failed prune the dispatch, and either failing fails the run. A
  missing token is handled the way the workflow handles a missing
  `MAIN_RELEASE_TOKEN`: in `discobox-ai/discobox` it fails the build before
  anything is built; a fork, which has no tap, skips the dispatch.
- **`promote.yml` keeps dispatching on a blessing.** It moves stable, and the
  one dispatch recomputes every channel.

`brew:refresh` stays one dispatch that recomputes every channel, so no caller
has to know which formula it is about to move (0105 Consequences). Edge and
main formulae therefore move within minutes of the tag or main build, inside
the issue's hour with no timer.

### 4. One tap commit per main build is accepted; the tap does only the work that changed

Main's formula is committed on every main build. Coalescing (249 §3) already
bounds that at one per build's duration; nothing else rate-limits it.

So that a dozen runs a day stay cheap and one channel cannot block another,
the tap's update changes in two ways:

- **A channel whose formula already names the resolved tag is not
  regenerated.** The run downloads assets only for a channel that moved. Today
  every run downloads and hashes every channel's three binaries.
- **Each channel succeeds or fails alone.** A run regenerates every channel
  it can, commits those, and then fails if any channel could not be resolved
  or generated. A main build whose assets cannot be downloaded does not
  leave stable or beta unwritten, and a broken step still goes red.

### 5. The caveats say what is shared and what goes away

Both new formulae carry 0105 §6's paragraph, per channel rather than the
beta wording they would share today: what the channel installs, that it
installs beside `discobox` and runs as its own command, and that all
channels share one state directory, configuration and server stage, so
only one server runs at a time. Both also say:

- **Going back is a downgrade.** An older CLI leaves a newer running server
  alone (autolaunch, 249 §5), but once that server stops, the older CLI starts
  its own older server against a database the newer one may have migrated
  forward (249 Consequences).

`discobox-main`'s caveats also say:

- **Main builds are deleted after fourteen days** (249 §6). `brew upgrade
  discobox-main` moves to the newest. A `brew reinstall` of a deleted build,
  which happens only with a tap not updated since then, fails to download. That
  is accepted, as 249 §6 accepts it for the installer's `--version`.

### 6. A pruned main build says how its own command is upgraded

`withDeletedMainBuildHint` names the way back that matches how the CLI was
installed: run as `discobox-main` (the only way to get that name is the
formula), it says `brew upgrade discobox-main`. Any other name keeps today's
install-script line. The command name is `commandName()`, the same signal
completion and usage already use.

### 7. The README and `docs/public` list the formulae

The README's "Homebrew" section and `docs/public/getting-started.md` list all
four formulae and their commands, and drop "There is no edge or main formula".

### 8. Open, for implementation

- **Homebrew's version order.** `brew upgrade` moves a keg only to a version
  Homebrew orders higher. `0.19.7-main.4210.g0123abc` → `…-main.4211…` and
  `0.19.7-rc.1` → `0.19.7` are expected to order correctly, by Homebrew's
  numeric and pre-release tokens. Homebrew orders `0.19.7-main.N` above
  `0.19.7`, the opposite of semver; no formula ever holds both, so that does
  not matter. Implementation checks the order against `brew` with generated
  formulae. If one does not, the formula's `version` is what changes,
  never the release's tag.
- **The tap workflow's own change** lands in `discobox-ai/homebrew-tap` after
  the generator's `--edge` and `--main` reach this repository's default
  branch, since the tap checks out the default branch for the generator. It
  writes four formulae: `discobox`, `discobox-beta` (moving the tap off
  `--dev`, with the `formula_renames.json` entry 249 §2 names, unless that has
  landed first), `discobox-edge` and `discobox-main`.

## Consequences

- **Every channel is a `brew install`.** Anyone on macOS (arm64) or Linux can
  hold all four side by side, each under its own command.
- **The tap's history gains a dozen commits a day.** Each is one formula's
  version and checksums. That is what 249 rejected, and is accepted here for
  the reason in Context.
- **A main build in `discobox-ai/discobox` now fails without
  `HOMEBREW_TAP_TOKEN`**, and an `-rc` or `-alpha` tag fails without it too,
  where today only a dot release needs it.
  The secret already exists, so this changes nothing until it is removed.
- **Edge can go backwards in version.** By §2, a patch tagged on an older
  line after a newer `-rc` becomes edge, as it does for the installer. Homebrew
  does not downgrade on `brew upgrade`, so a `discobox-edge` user stays on the
  newer build until edge passes it, where the installer would install the
  older one.
- **Every formula still names GitHub alone** (0106 §3), main's included, even
  once `discobox-ai/discobox-main` has a mirror namespace (249 §4).

## Alternatives rejected

**The tap polls.** A cron in `update-formula.yml` would need no change here and
no token in `main-build.yml`. The tap removed exactly that cron because it hid
broken release steps, and the dispatch path already holds the one token it
needs. Re-adding it for two channels would bring the blind spot back for all
four.

**Rate-limit the main formula** (at most hourly). It needs a timer, a cron or
a debounce, either of which is the poll above under another name, or a delay
that makes the formula lag the build. Coalescing already bounds the rate.

**A separate tap for main** (`discobox-ai/homebrew-main`). It keeps the main
tap's churn out of the stable tap's history, at the cost of a second
repository, a second workflow, a second dispatch token, and a second `brew
tap` for users. The churn is not worth that.

**The workflows push the formula.** A token in this repository with write
access to the tap, able to make a formula say anything. The tap pulls so that
no such token exists (`update-formula.yml`'s header); nothing here changes
that.

**Point main's formula at the asset mirror once it has a namespace.** A
formula names exactly one URL, so that would trade GitHub's availability for
the mirror's (0106 §3).

**Only an edge formula.** Edge moves rarely and fits today's process with a
one-line change, but the owner asked for both, and main's only cost is the
tap churn accepted above.
