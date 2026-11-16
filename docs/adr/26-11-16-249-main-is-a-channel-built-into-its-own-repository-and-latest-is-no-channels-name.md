# 26-11-16-249 — Main is a channel, built into its own repository, and `latest` is no channel's name

- **Status**: Proposed
- **Date**: 2026-11-16
- **Would amend**:
  [0105](0105-a-release-is-cut-as-a-prerelease-and-blessed-stable-later.md)
  §2's channel name (`latest` becomes `beta`) and §4's dev formula name
  (`discobox-dev` becomes `discobox-beta`);
  [0110](0110-the-install-script-is-a-release-asset-and-a-channel-names-which-one-runs.md)
  §3's channel table (`latest` becomes `beta`, and `main` is added) and §4's
  addresses (`main.discobox.ai` is added). 0105 §§1, 3, 5 and 6, 0106, and
  0110 §§1, 2, 5 and 6 stand. A main build is otherwise a release in every
  respect those ADRs define.

## Context

A release exists only when someone pushes a `v*` tag:
`.github/workflows/release.yml` runs on that and on a manual dispatch, and
`ci.yml` publishes nothing from `main`. Trying what is on main means building
from source or cutting an `-rc` tag. Main moves fast: about 300 first-parent
commits landed in the seven days before this ADR.

### What a downloadable main build has to fit

- **The installer's channel lookup is one page of one repository's releases.**
  `pkg/installer/install.sh` `resolve` reads
  `api.github.com/repos/discobox-ai/discobox/releases?per_page=100`, keeps the
  `v[0-9]*` tags, and takes `stable` as the first one not marked prerelease.
  `edge` is the newer of that and the newest prerelease. Every release in that
  repository is a prerelease until someone blesses it (0105 §1).
- **The mirror's aliases read the same bit.** `assets.discobox.ai/discobox/`
  `prerelease/` is the newest release marked prerelease, and
  `edge.discobox.ai` derives edge from it (0110 §4).
- **A release CLI is pinned to its own tag.** `release:binary` links the
  version (`pkg/version.Version`), a server manifest whose URLs end in
  `releases/download/<tag>/` (0099 §3, 0106 §2), and the server's image
  references (`DefaultSandboxBaseImage`, `DefaultPoolAgentBundle`,
  `harness.BundleTag`) at `:<tag>`. The CLI fetches exactly the server it was
  cut with.
- **Autolaunch replaces a server only by semantic version.**
  `pkg/endpoint/autolaunch.go` `olderServer` replaces a running local server
  when both versions are valid semver and the server's is lower. Anything
  else, such as a bare commit hash, is never replaced.
- **`:latest` moves on a dot release only.** `release:image` adds the
  `:latest` tag when `DOT_RELEASE` holds, so an explicit prerelease does not
  move it.

### The channel words disagree

| Word | Means in the installer, README and docs | Means elsewhere |
| --- | --- | --- |
| `latest` | the newest `vX.Y.Z`, blessed or not (0110 §3) | the mirror's `latest` alias and GitHub's "Latest" badge: the newest **blessed** release, which is `stable` (0106, 0110 §4); ghcr's `:latest`: the newest dot release |
| `discobox-dev` | Homebrew formula and command for that channel (0105 §4) | reads as a development (main) build, which it is not |
| `prerelease` | explicitly not a channel name (0110 §3) | the mirror's alias for the newest unblessed release |
| `edge` | the newest tagged release of any shape | consistent; it has an address and no formula |

`latest` cannot be made to mean one thing. Two of its meanings belong to
conventions this repository does not own: GitHub's "Latest release" (the
newest release not marked prerelease) and Docker's default tag. The third is
ours.

## Decision

### 1. Four channels, and these are their names everywhere a person reads one

| Channel | Installs | Installer | Address | Homebrew |
| --- | --- | --- | --- | --- |
| `stable` (default) | the newest blessed release | `--channel stable` | `discobox.ai` | `discobox` |
| `beta` | the newest `vX.Y.Z`, blessed or not | `--channel beta` | none | `discobox-beta`, runs as `discobox-beta` |
| `edge` | the newest tagged release of any shape, `-rc` and `-alpha` included | `--channel edge` | `edge.discobox.ai` | none |
| `main` | the newest main build (§3) | `--channel main` | `main.discobox.ai` | none |

`stable` and `edge` keep their names and meanings. `beta` is the channel 0105
and 0110 called `latest`, renamed and otherwise unchanged. `main` is new.

These four words are the vocabulary of the installer and its help, the site's
addresses, the Homebrew formulae and the commands they install, the README and
`docs/public`, and anything the CLI prints about channels.

**`latest` is no channel's name.** It survives only as the name of a mechanism
owned by someone else's convention: GitHub's "Latest release" and the mirror's
`latest` alias that follows it, and Docker's `:latest` image tag. Neither is
ever documented as a channel. The site's Worker still reads the mirror's
`latest` alias to find `stable`, as 0110 §4 decided. That is an implementation
detail of the address, not a name a user is told. ghcr's `:latest` keeps
following the beta channel (0105 §2) because it is only read by a build that
pinned nothing.

`beta` has no address for 0110 §4's reason: no mirror alias can express "newest
dot release", so it resolves on the client. `main` has an address because its
releases live apart (§4), so "newest" is one alias there.

### 2. Old names keep working, and say what replaced them

- **`--channel latest`** and `DISCOBOX_CHANNEL=latest` (`-Channel latest` in
  `install.ps1`) install the beta channel, and the installer prints one line
  saying `latest` is now `beta`.
- **The `discobox-dev` formula** is renamed `discobox-beta` through the tap's
  `formula_renames.json`, so `brew update` migrates an installed keg. The
  `discobox-beta` formula also links a `discobox-dev` command beside
  `discobox-beta`. Run as `discobox-dev`, the CLI prints one line to stderr
  naming `discobox-beta`. Both the alias command and the notice are removed in a
  later release. When is left open (§9).
- **A completion script** installed as `_discobox-dev` is not rewritten. 0105 §5
  still holds: the CLI names itself by argv[0], so `discobox-beta completion`
  writes `_discobox-beta`, and the old file is the user's to remove.

### 3. A main build is a release of each green main commit, coalesced

A new workflow runs when CI finishes green on a push to `main`
(`workflow_run`, gated on the run's conclusion and branch). It runs the same
Taskfile targets as `release.yml`: `release:build`, `release:images`,
`release:publish`. Only the version (§5) and the repository it publishes to
(§4) differ. ADR 0066 §1 holds: the workflow only decides where steps run.

Builds coalesce rather than queue. One concurrency group, with
`cancel-in-progress: false`: a build that has started finishes, and at most one
waits behind it. GitHub replaces a waiting run with a newer one. So main is
built at most once per build's duration, always at the newest green commit, and
not every commit gets a build. At ~40 pushes a day and a build bounded by
`release:images`, that is on the order of a dozen builds a day, not forty.

A main build covers every platform `release:build` covers. It also pushes every
image `release:images` pushes, pinned the way a release's are (§5), so a main
CLI fetches its own server and that server pulls its own images.

### 4. Main builds are published in their own repository

Main builds are GitHub releases in a separate repository,
`discobox-ai/discobox-main`, which holds nothing but those releases. Each one is
created as a full release, not a prerelease, and marked latest, so that
repository's "Latest release" is the newest main build.

The workflow in this repository publishes there with a token scoped to that
repository's contents. The tag each release needs points at that repository's
own placeholder commit, so it carries no meaning. The build's source commit is
in its version (§5) and its release notes. Images go to the same ghcr packages
as releases (`ghcr.io/discobox-ai/discobox-*`) under the main version's tag.
They never move `:latest`, because a main version is not a dot release.

Kept apart, main builds touch nothing that resolves the tagged channels:

- the installer's one-page lookup in `discobox-ai/discobox` still finds
  `stable` among the last hundred tagged releases;
- the mirror's `discobox` namespace and its `prerelease` alias, and therefore
  `edge`, never see a main build;
- GitHub's release list and "Latest" badge on the main repository stay the
  tagged releases.

In `discobox-ai/discobox-main`, GitHub's own `releases/latest` answers "the
newest main build" with no API call. That is what `main.discobox.ai` and
`--channel main` read.

**Mirror.** A mirror namespace for `discobox-ai/discobox-main` is added only
together with its entry in `discobox-ai/infra`'s smoke-test matrix (0106 §5).
Until it exists, a main build's server manifest and its installer name GitHub
alone. Once it does, they name the mirror first and GitHub last (0106 §2, 0110
§5), and `main.discobox.ai` reads the namespace's `latest` alias with GitHub's
`releases/latest/download/` as fallback, the shape `discobox.ai` has.

### 5. A main build's version is a semver prerelease of the next patch

```text
v<MAJOR>.<MINOR>.<PATCH+1>-main.<N>.g<sha7>
```

- `MAJOR.MINOR.PATCH` is the highest `vX.Y.Z` tag in this repository when the
  build runs, and the core is its next patch.
- `N` is the first-parent commit count of the built commit
  (`git rev-list --count --first-parent`). Main is linear, so `N` is unique and
  increasing along it.
- `g<sha7>` names the commit. It never decides an ordering, because `N` already
  differs between any two builds.

So `v0.19.6 < v0.19.7-main.N < v0.19.7`. Every main build after a dot release
sorts above it, and the next dot release sorts above every main build before
it. Tags are never deleted, so the core never decreases, and versions increase
monotonically across builds. Consequences for what already reads versions:

- **Autolaunch** (`olderServer`) works unchanged. A main CLI replaces an older
  running server, stable or main. A stable CLI leaves a newer main server
  running. That is today's rule between `beta` and `stable` (0105 §6), so the
  channels keep sharing one state directory.
- **`DOT_RELEASE`** is false for it, so `release:image` does not move `:latest`
  and `release.yml`'s Homebrew refresh does not run (and is not in this
  workflow anyway).
- **The installer's `--version`** accepts it, since it matches `v[0-9]*`. A
  version containing `-main.` is fetched from `discobox-ai/discobox-main`, and
  any other version from `discobox-ai/discobox`.
- **`discobox version`** prints it, so a bug report names the commit.

The placement of `main` in semver prerelease order (`main` sorts below `rc` and
`alpha`) does not matter. No channel compares a main build with a same-core
`-rc`: they live in different repositories and channels.

### 6. Old main builds are deleted after fourteen days

The publishing workflow prunes main builds older than fourteen days. It always
keeps the newest. A build is pruned whole: its release and tag in
`discobox-ai/discobox-main`, and its ghcr tags. Fourteen days of builds at the
rate in §3 is under two hundred releases. `--channel main` reads `latest` and
`--version` names one tag, so nothing pages through them.

A main CLI whose build was pruned keeps working once its server and images are
staged or pulled. A first stage after the prune fails, and that failure must
say the build is gone and to reinstall from `--channel main`, which installs
the newest build. Main builds are for trying
main, not for staying on a commit.

### 7. Only main gets builds; the installer takes `--channel main`, not `--branch`

Main is a channel like the others, so the installer selects it the same way:
`--channel main`, `DISCOBOX_CHANNEL=main`, `-Channel main`, or
`curl -sSfL https://main.discobox.ai | sh`. A main build installs as `discobox`,
like every channel the script installs (0110 §6). There is no Homebrew formula
for main.

No other branch is built. A branch that needs to be installed the way users
install is cut as an `-rc` tag (edge) or built from a fork, whose release
workflow already publishes into the fork's own namespace (`RELEASE_REPO`,
`REGISTRY`). Revisit when long-lived branches other than `main` exist that
people install from without tagging.

### 8. The site adds `main.discobox.ai`

The site's Worker answers `main.discobox.ai/`, `/install.sh` and `/install.ps1`
with the newest main build's installers, browsers included, the way
`edge.discobox.ai` answers (0110 §4). That change is `discobox-ai/site`'s, on
its `preview` branch. It becomes the place to try an installer change before it
is tagged at all.

### 9. Open, for implementation

- **When the compatibility names go**: `--channel latest`, the `discobox-dev`
  command and its notice (§2). Not before a blessed release has carried the
  notice.
- **A stale VM pin on main.** Whether `release:check-vm-pins` refuses a main
  build the way it refuses a tag, or only records the override in its notes.
- **Pruning ghcr tags**: which token deletes package versions, and whether a
  tag shared by a main build and a release is possible at all (it should not
  be, by §5).

## Consequences

- **Anyone can install main.** `curl -sSfL https://main.discobox.ai | sh` gets
  the newest green main build, with its own server and images, checked against
  the digests it was stamped with (0110 §1).
- **A main build costs a release build's runner time**, on the order of a dozen
  times a day (§3). Coalescing bounds it at one in flight and one waiting.
- **A new repository and a cross-repository token.** `discobox-ai/discobox-main`
  must exist before the first build, and the workflow's token can write only
  there.
- **Installers that predate this ADR do not know `beta` or `main`.**
  `discobox.ai` serves the newest blessed release's installer, so `--channel
  beta` and `--channel main` work from it only once a release carrying them is
  blessed. `edge.discobox.ai` has them as soon as such a release is tagged.
  `main.discobox.ai` has them from the first main build.
- **Going from main back to stable is a downgrade.** It shares the state
  directory and database, as `beta` to `stable` does today (0105 §6). A server
  that a main build has migrated forward is the case to expect there.
- **A pruned build cannot be staged again.** That is accepted (§6).
- **Docs and the CLI change words.** The README's "Release channels", the
  installers' help and errors, `docs/public/getting-started.md`, the Homebrew
  formula script and its caveats, and `commandName`'s comment all move to
  `stable`/`beta`/`edge`/`main`.

## Alternatives rejected

**Publish main builds as prereleases in `discobox-ai/discobox`.** It is the
obvious place, and it breaks every resolver that exists. At a dozen builds a
day the installer's single page of a hundred releases loses `stable` within a
week. The mirror's `prerelease` alias, and with it `edge.discobox.ai`, would
answer with a main build. GitHub's release list would bury the tagged releases.
Fixing that means teaching every resolver and the generic mirror to exclude a
tag shape, which 0106 rejected for `DOT_RELEASE`.

**One rolling `main` release whose assets are replaced each build.** It keeps
one release, but a CLI's linked manifest names
`releases/download/<tag>/<asset>` with a digest. The next build overwrites the
asset, and every main CLI that has not staged its server is stranded within
hours. Naming assets by commit instead fills the release's asset limit in weeks
and still leaves `--version` unable to name a build. A moving tag called `main`
also collides with the branch of that name in every clone.

**Publish main builds to the asset mirror only.** The mirror is a read-through
cache of GitHub releases with no publish step (0106, Consequences). Making it a
store would add the one thing 0106 designed it not to have, and a consumer with
no GitHub fallback, which 0106 §1 forbids.

**Name main builds `main-<sha>`.** They would not be semver. Autolaunch would
never replace a running main server with a newer one. The installer's
`--version` would need a second shape. Releases and builds would have no order.

**Keep `latest`, and make it mean one thing.** Two of its three meanings are
GitHub's and Docker's, and not ours to change. Only the installer's meaning
could move, and it would still disagree with the badge on the repository page.

**Call the newest-dot-release channel `next` or `candidate`.** `candidate`
collides with `-rc` (release candidate), which is an edge-only tag shape. `next`
names when, not how trusted. `beta` is the word people already read as "newer
than stable, not yet blessed".

**Make `edge` mean main builds.** `edge.discobox.ai` already installs
something specific. Changing what an existing address installs, silently,
breaks §2's rule that an old name keeps its meaning or says what replaced it.

**Drop `edge` now that `main` exists.** An `-rc` tag would again reach nothing
but a pinned version, and the step between "newest commit" and "newest dot
release" would have no address.

**A `--branch` flag.** Only main is built (§7), so a branch flag would accept
exactly one value and imply others. `--channel main` keeps one concept.

**A `discobox-main` Homebrew formula.** The tap would commit a dozen times a
day, and a formula names exactly one URL (0106 §3), with no fallback. The
installer covers main.

**Build every green commit.** Forty release builds a day, most of them
superseded before anyone installs them. Coalescing builds the newest commit and
skips nothing anyone would choose.
