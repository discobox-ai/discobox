# 0109 — The install script is a release asset, and a channel names which one runs

- **Status**: Proposed
- **Date**: 2026-09-11
- **Amends**: [0105](0105-a-release-is-cut-as-a-prerelease-and-blessed-stable-later.md)
  §2 in one respect — an explicit prerelease tag reaches one channel, `edge`,
  which no package manager serves and which you only get by naming it.
- **Supersedes**: [0106](0106-a-consumer-points-at-the-asset-mirror-only-if-it-can-fall-back.md)
  §4 for the installer's addresses — `discobox.ai` serves the stable installer
  through the mirror's `latest` alias, and `edge.discobox.ai` derives the edge
  channel from both of its aliases. §§1–3 and §5 stand.

## Context

Homebrew is the only install path. It does not cover a Linux machine without
Homebrew or any Windows machine, and winget is waiting on a moderator
(microsoft/winget-pkgs#432935). The expected answer is a one-liner:
`curl -sSfL https://discobox.ai | sh`, and `irm https://discobox.ai/install.ps1
| iex` on Windows.

A script served from a URL has to answer three questions every time it runs:
which version to install, where to get it, and how to know the bytes are right.
The pieces that answer them already exist, but none of them was built to serve
a script:

- **Channels** (0105). *Stable* is the newest release a human has blessed, which
  is `brew install discobox`. *Latest* is the newest dot release, which is
  `brew install discobox-dev`. An explicit prerelease tag (`-alpha`, `-rc`)
  reaches neither and is had only by pinning its version.
- **The asset mirror** (0106). `assets.discobox.ai/discobox/{tag}/{asset}` has a
  GitHub fallback for every consumer that names it. Its `latest` alias is the
  newest blessed release and its `prerelease` alias is the newest unblessed
  one. §4 declined to treat either alias as a channel: `prerelease` is not the
  latest channel, and once every release is blessed it drifts back to whatever
  alpha came last.
- **The site.** `discobox.ai` is a static-assets Worker in `discobox-ai/site`
  and returns HTML to every client, curl included. `preview.discobox.ai` is
  already taken by the site's own staging deploy.

The script also has to be changeable without breaking every install at once,
and it has to be possible to run an installer change before it reaches
everyone.

## Decision

### 1. Each release uploads its own installer, stamped with what it installs

Every release uploads `install.sh` and `install.ps1` beside its binaries. The
copies it uploads are **stamped** with two things: the release's tag, and the
SHA-256 of every CLI binary in that release. `release:publish` stamps them from
the files it is about to upload, through `internal/cmd/discobox-installers`. So
every digest in an installer describes a file that release actually uploaded.

Run with no arguments, a stamped installer installs its own release and nothing
else. The copy in the source tree is unstamped and never installs anything
itself; it can only delegate (§2).

### 2. Anything else is delegation to that release's own installer

A pinned version (`--version v1.2.3`) or a channel (`--channel latest`) resolves
to a tag. Unless that tag is the installer's own, the script downloads the
installer that release uploaded and runs it with `--version <tag>`, passing
every other argument through. Two things follow:

- The code that installs a release is always the code that release shipped.
- Every digest checked was stamped by that release.

A release cut before this ADR has no installer and cannot be installed this
way. The script says so and names the release page.

### 3. Three channels, resolved by the script

| channel | installs | also served by |
| --- | --- | --- |
| `stable` (default) | the newest blessed release | `brew install discobox` |
| `latest` | the newest `vMAJOR.MINOR.PATCH` release, blessed or not | `brew install discobox-dev` |
| `edge` | the newest release of any shape, `-alpha` and `-rc` included | nothing else |

The script resolves a channel from one unauthenticated request to the GitHub
Releases API. It applies the same rules the tap applies to its two formulae.
`edge` is the newer of the newest blessed release and the newest prerelease,
compared by version core, with the blessed one winning a tie. A blessed release
is always a dot release (`release:require-dot`), so comparing cores is enough.
This is also what can be computed from the mirror's two aliases alone (§4).

`edge` is what this ADR amends in 0105. An explicit prerelease tag still
reaches no package manager and no default. It now reaches one channel, and
only a person who asks for that channel by name gets it.

`prerelease` is not a channel name. Every dot release is a GitHub prerelease
until someone blesses it, so the word already names that bit, and a channel
called `prerelease` would sometimes contain releases the bit does not mark and
sometimes miss ones it does.

### 4. `discobox.ai` serves the stable installer and `edge.discobox.ai` the edge one

The site gains a Worker in front of its static assets:

- `discobox.ai/` answers a client that does not ask for HTML with the stable
  release's `install.sh`, or with `install.ps1` if it is PowerShell. Browsers
  get the site exactly as they do now. `/install.sh` and `/install.ps1` answer
  every client.
- `edge.discobox.ai` answers every path with the edge release's installer,
  browsers included. It has no site. It is the place to run an installer change
  before it is blessed.

The Worker does not call the GitHub API. Unauthenticated requests from shared
Worker egress would be rate-limited, and the mirror already holds the token that
avoids that. So stable is `assets.discobox.ai/discobox/latest/install.sh`, with
GitHub's `releases/latest/download/install.sh` behind it as the fallback. Edge
reads the tag each mirror alias redirects to and applies §3's rule, which makes
the `prerelease` alias's drift harmless: an old alpha never beats a newer
blessed release.

This supersedes 0106 §4 for these two addresses. The mirror's `latest` is now
relied on as the stable channel. That is safe because it and the tap's rule
both read GitHub's release state, which is exactly what blessing sets (the
release skill passes `--latest` when blessing). The mirror stays generic: the
channel rules live in the site and the script, never in the mirror's
configuration. `latest` (the channel) has no host, because no alias can express
"newest dot release". `--channel latest` resolves on the client.

### 5. Every download goes to the mirror first, then GitHub

Binaries and delegated installers are both fetched from
`assets.discobox.ai/discobox/<tag>/` first and
`github.com/discobox-ai/discobox/releases/download/<tag>/` second, which is
0106 §1's shape. A binary is checked against the digest its release stamped, so
a second source adds no trust. A delegated installer has no digest. It comes
from the same sources, over the same TLS, as the script that is already
running, so delegation trusts nothing the first `curl | sh` did not.

### 6. Where it installs, and under what name

- **`install.sh`** installs to `--dir` or `$DISCOBOX_INSTALL_DIR`. Otherwise it
  uses `/usr/local/bin` when run as root and `~/.local/bin` for anyone else. It
  never edits a shell profile; if that directory is not on `PATH`, it says so.
- **`install.ps1`** installs to `%LOCALAPPDATA%\Programs\Discobox` and adds that
  directory to the user's `PATH`, since Windows has no per-user bin directory
  that is already on it. `-NoModifyPath` opts out. Under PowerShell 7 on Linux
  or macOS it installs what `install.sh` would, to the same place.
- **One name on every channel.** The command is `discobox` whatever the channel.
  `discobox-dev` exists only because Homebrew will not link two formulae that
  install the same file. A script picks its own directory, so it has no such
  constraint.
- **Platforms.** Linux on amd64 or arm64 with glibc: the script checks for the
  loader `release:binary` names and refuses a musl system. Apple Silicon macOS,
  including a shell running under Rosetta. Windows on amd64 or arm64. An Intel
  Mac is refused, per ADR 0062.

## Consequences

- **A release carries two more assets.** Nothing downstream reads them except
  the installers themselves and the site.
- **An installer is as immutable as the release it ships in.** A bug in one is
  fixed by the next release, not by editing an old one. `curl discobox.ai | sh`
  picks up an installer fix only when a release carrying it is blessed, and
  `edge.discobox.ai` runs it before then. That lag is intended: it is the stable
  channel's own lag.
- **`curl discobox.ai | sh` fails until a release carrying an installer is
  blessed.** Until then the mirror has no `latest/install.sh` to serve, so the
  Worker answers curl with an error while browsers see no change.
  `edge.discobox.ai` works as soon as the first such release is cut.
- **`--channel` and `--version` spend GitHub API quota; the default does not.**
  A stamped script with no arguments needs no lookup at all. A resolved channel
  costs one request against the 60 an hour GitHub allows each address
  unauthenticated, and the script reports a rate limit as one, suggesting a
  pinned version.
- **The site stops being static.** The Worker runs only for `/`, the two script
  paths, and the edge host; every other path is still served straight from the
  static assets.

## Rejected alternatives

**One unversioned script, served from the site or from `main`.** This is the
usual shape for `curl | sh`. It makes the installer the one thing a release does
not ship. Any change reaches everyone at once with nowhere to try it first. It
also has no digest to check without a separately published checksum file.

**A `SHA256SUMS` asset instead of stamped digests.** This is a second fetch from
the same place the binary came from, and it splits the record 0099 §3 insists
on keeping whole: an asset's URL and digest travel together or not at all.

**Teach the mirror the channels.** 0106 rejected putting `DOT_RELEASE` in a
generic mirror, and that still holds. A configurable per-alias tag pattern
would be generic, but it would move the latest channel's definition out of this
repository and into `discobox-ai/infra`'s deploy configuration, which is one
more place to keep in step with the tap.

**Resolve channels in the site Worker from the GitHub API.** The Worker would
need a token secret of its own, because shared egress exhausts the
unauthenticated limit. The mirror already has that token, which is why edge is
read from its aliases instead.

**Define `edge` as the most recently created release.** This is simpler in the
script, but the Worker cannot compute it from redirects, and two definitions
would disagree the first time an older line is tagged after a newer one.

**Install the `latest` channel as `discobox-dev`.** That reproduces a Homebrew
constraint that does not exist here. It would also imply two independent
installs, which 0105 §6 says they are not: they share a state directory and
cannot run servers at the same time.

**`preview.discobox.ai` or `prerelease.discobox.ai` for the edge installer.**
The first is the site's staging deploy and would tie "staging the website" to
"newest release". The second repeats the reason `prerelease` is not a channel
name.
