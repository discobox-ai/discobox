# 0106 — A consumer points at the asset mirror only if it can fall back

- **Status**: Proposed
- **Date**: 2026-09-10

## Context

`assets.discobox.ai` is a Cloudflare Worker that fronts this repository's
Release assets, caching each one in R2 on first request. It gives a short,
stable, CORS-enabled, range-capable URL for something GitHub serves awkwardly:
`latest` is a redirect chain, there is no CORS header, ranges are unreliable,
and release downloads are not built for programmatic traffic. The component is
generic and lives in `discobox-ai/release-asset-mirror`; the deployment that
serves our namespaces, its scheduled smoke tests, and the secrets it needs
belong to `discobox-ai/infra`.

Nothing in this repository points at it. Every consumer names
`github.com/discobox-ai/discobox/releases/download` directly:

| Consumer | Where | Fed by |
| --- | --- | --- |
| Server manifest linked into a release CLI | `Taskfile.yml` `-base-url`, ADR [0099](0099-the-cli-downloads-the-server-it-starts.md) §3 | the release that built it |
| `discobox` Homebrew formula | `scripts/brew-formula.sh` | `promote.yml`, newest blessed release |
| `discobox-dev` Homebrew formula | `scripts/brew-formula.sh --dev` | `brew:refresh`, newest dot release |
| winget manifest | `scripts/winget-manifests.sh` | `promote.yml` |

That is not an oversight to correct wholesale. The consumers differ in one
respect that decides the question, and it is not integrity.

**Integrity is already settled everywhere.** All four carry a SHA-256 alongside
the URL: the server manifest by ADR 0099 §3, which is emphatic that the
manifest states a URL and a digest or neither; Homebrew in each formula's
`sha256`; winget in `InstallerSha256`. A mirror serving the wrong bytes fails
the digest check, not the install. Where the bytes come from is therefore a
question about *availability*, and only that.

**What differs is whether a wrong answer can be taken back.** Each consumer
writes the URL into a record with a different lifetime:

- The **server manifest** is base64-linked into a released binary. Every
  `discobox` already on a machine carries the URL it was cut with, forever, and
  no later release can change what an installed one does.
- A **winget manifest** is merged into `microsoft/winget-pkgs`. That repository
  is not ours, every published version's `InstallerUrl` is permanent, and
  amending a historical manifest is a moderated pull request against someone
  else's policy.
- Both **Homebrew formulae** are generated into our own tap from this
  repository's public releases. They are the records we can regenerate from
  scratch at any time, which is what `brew:refresh` already does on every dot
  release.

So the mirror is a single point of failure of three different durabilities. If
`assets.discobox.ai` lapses — the domain, the Cloudflare account, R2 billing —
the tap can be regenerated in minutes, winget cannot be fixed at all, and every
shipped CLI is stranded unless the binary itself knows somewhere else to look.

### The mirror's aliases are defined against the bit ADR 0105 moved

[0105](0105-a-release-is-cut-as-a-prerelease-and-blessed-stable-later.md) made
every release a GitHub prerelease at push time, with stable becoming a human
clearing that box. Two of the mirror's three path shapes read exactly that bit,
so its aliases acquired meanings nobody chose:

- `/{ns}/latest/` resolves through GitHub's `releases/latest/download` redirect,
  which is the newest release that is *not* a prerelease. Under 0105 that is
  the newest **blessed** release — so the mirror's `latest` became the stable
  channel, correctly and for free.
- `/{ns}/prerelease/` is the newest release flagged prerelease: the newest
  **unblessed** release. That is *not* the dev channel. `discobox-dev` is the
  newest **dot** release whether or not it has been blessed, and the mirror
  cannot express that — it has no notion of `DOT_RELEASE`, only of the
  prerelease bit.

The two already disagree. The newest dot release is `v0.5.2`, which is blessed,
so `discobox-dev` installs it; the mirror answers `prerelease` with
`v0.6.0-alpha.3`, an explicit tag the dev channel deliberately excludes. A dot
release also *leaves* the mirror's `prerelease` alias the moment it is blessed,
while staying on the dev channel until the next one is cut.

## Decision

### 1. A consumer may name the mirror only if it can fall back

The rule, stated once so the next consumer does not need this re-litigated: a
record that names the mirror must also name the release URL, or be regenerable
by us on demand. Anything else stays on `github.com` regardless of how much
nicer the mirror's URL is.

### 2. The server manifest carries an ordered list of URLs and one digest

`serverstage.Asset` gains `urls []string` in place of `url string`. Staging
tries them in order and stops at the first whose bytes match `sha256`; the
digest is unchanged and still what verification rests on, so an extra source
adds no trust. `assets.discobox.ai` goes first and the release URL goes last.

This is the consumer the mirror was built for. Staging a server is ~94 MB per
machine on the autolaunch path, which is the one place our traffic is heavy,
programmatic, and repetitive enough for a read-through cache to matter — and
the one record that cannot be corrected after the fact, which is why it gets a
fallback rather than a swap.

`Taskfile.yml`'s `-base-url` already pins a concrete tag
(`releases/download/{{.PIN_TAG}}`), so nothing here depends on a release's
prerelease bit and 0105 changes none of it.

ADR 0099 §3's reasoning is preserved exactly. It rejected "a base URL plus a
name convention" because a derived URL for another platform still has no
digest — "the manifest states both or neither". A list of URLs against one
digest states both for every entry; it is more locations for the same verified
artifact, not a location the binary has to trust.

### 3. Both Homebrew formulae and winget keep naming the release URL

None of the three formats has a fallback to express. A Homebrew `url` and a
winget `InstallerUrl` are each exactly one location, and the mirror would
replace GitHub's availability with ours rather than adding to it. For winget the
record is also permanent and not ours to amend, which turns an outage into a
class of version that can never be installed again.

The tap is regenerable, so pointing either formula at the mirror later is
reversible and can be reconsidered on its own — `discobox-dev` especially, since
it is recomputed on every dot release and its users have opted into a moving
channel. Doing it now would trade a real dependency for a cosmetic URL.

### 4. The mirror's aliases are not wired to the release channels

Nothing here publishes `assets.discobox.ai/discobox/latest/` or `/prerelease/`
as the address of a channel, and no documentation should describe them as one.
`latest` happening to equal the stable channel is a consequence of both reading
GitHub's prerelease bit, not a contract; `prerelease` is not the dev channel and
cannot be made into one without teaching a generic mirror what a dot release is.

A consumer that wants a channel names the channel's own mechanism — `brew
install discobox` or `discobox-dev`, or a concrete tag. A consumer that wants
*an asset* names a concrete tag, which is what every consumer in the table above
already does.

### 5. A namespace nothing watches is not added

`discobox-ai/infra` smoke-tests every path shape of every namespace on a
schedule, because this mirror served concrete tags and `latest` correctly for a
day while `prerelease` returned 500 and nothing noticed. A namespace added
without a matching entry in that matrix is unmonitored by construction, and an
unmonitored mirror is worse than no mirror: it fails silently, and it fails at
install time on someone else's machine.

## Consequences

A release's assets are published to GitHub exactly as they are now; the mirror
fills from them on first request and is never something the release pushes to.
There is no publish step to break, and a cold mirror is a slow first download
rather than a failed one.

`serverstage` gains a list where it had a string — a manifest schema change, so
a CLI decoding a manifest and the tool encoding one move together, and
`EncodeManifest`'s validation covers each entry.

The mirror's `latest` alias now tracks blessings rather than tags, so it stands
still through every unblessed dot release. That is the correct behaviour for a
stable channel and a surprise for anyone expecting "most recent"; §4 exists so
nothing is built on either reading.

If the mirror is retired, the change is deleting the first entry from the
manifest's URL list. Nothing else in this repository names it.

## Rejected alternatives

**Point every consumer at the mirror.** The obvious reading of "we built a
mirror, use it". It puts an install path we cannot repair — winget — behind
infrastructure with one operator, one domain, and one billing relationship, to
gain a shorter URL. The asymmetry is the point: the benefit is cosmetic for
winget and real for server staging.

**Serve the dev channel from `/{ns}/prerelease/`.** Tempting once 0105 made
unblessed releases the normal state, and it is the shape people will assume.
The alias cannot exclude explicit `-rc1` tags or include a blessed dot release,
so it would be a channel that silently disagrees with `discobox-dev` — which it
already does today. Teaching the mirror `DOT_RELEASE` would put a discobox
release convention inside a component whose whole value is being generic.

**Mirror-only in the server manifest, no fallback.** Simpler, and it keeps
`Asset.URL` a string. It also makes every binary we have ever shipped depend on
a Worker staying up forever, which is precisely the failure the fallback exists
to prevent, and it cannot be fixed later for binaries already released.

**Have the mirror redirect to GitHub when R2 misses.** Would let a consumer name
only the mirror and still reach GitHub. It moves the single point of failure
rather than removing it: a DNS or Worker outage takes the redirect down too, and
the client never learns the release URL.

**Keep the mirror unused.** Defensible — it carries no traffic today, so
retiring it costs nothing. Rejected because server staging is a real recurring
cost on GitHub's release bandwidth, and because the mirror is now watched and
deployed from CI, which is what made it trustworthy enough to put on a user
path.
