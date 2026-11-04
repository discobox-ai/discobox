---
name: release
description: Cut a discobox release — infer the next version, get main green, tag it, watch the release workflow, and land it on the dev Homebrew channel; separately, mark an already-cut release stable so it reaches `brew install discobox`, and submit it to winget by hand. Use when the user wants to tag, release, ship a version, push a formula to the tap, promote or mark a release stable, or submit a version to winget.
allowed-tools: Bash, Read, Glob, Grep, Edit, Write, AskUserQuestion
metadata:
  argument-hint: "[version-or-tag]"
---

# Release

A release is cut by pushing a `v*` tag to **GitHub**. Everything after that is
the Release workflow; everything before it is making sure the commit deserves a
tag.

The gate is a green CI run **on the exact commit being tagged**. A tag is
public the moment it is pushed, and the release built from it cannot be
un-published cleanly — so the order is: green, then tag. Never the reverse.

## Three states, and only one of them is this skill

Cutting a tag does **not** ship to users (ADR 0105). It publishes a GitHub
prerelease and moves the *latest* channel — ghcr's `:latest` and
`brew install discobox-dev`. `brew install discobox` and winget do not move.

They move when a human opens the GitHub release and unticks **"Set as a
pre-release"**. That fires `promote.yml`, and it is a separate, deliberate act.

So this skill ends at a published prerelease. Say so when you hand back: name
the version, say it is on the dev channel, and say that promoting it is the
user's call. Do not untick that box for them and do not run `promote.yml`
unless they ask for it in those terms — see §7.

## Running this

Invoking the skill is authorization for the happy path end to end: fetching,
pushing `main`, waiting on CI, fixing a CI failure whose remedy is clear,
creating and pushing the tag, and watching the release workflow. Report
progress in short updates; do not stop for routine confirmation.

Stop and ask when:

- CI fails in a way whose fix is ambiguous, broad, or risky;
- `gh` auth or repository permissions block the flow;
- the tag or its GitHub release already exists in a conflicting state;
- the remedy would rewrite published history — a pushed tag included;
- the version to cut is genuinely unclear after the rules below.

The one thing never done without asking: **pushing the tag is the point of no
return.** If the user did not name a version, say which one you inferred and
why before you push it.

## Which version

If the user named one, use it, normalized to a leading `v`.

Otherwise infer it from the tags this repository already has:

```bash
git fetch upstream --tags
git tag -l 'v[0-9]*' --sort=-v:refname | head -6
```

The glob matters. `'v*'` also matches `vm/vN` and `vm-kernel/vN` — the VM guest
image and libkrun kernel tags, on their own release lines, which sort above every
CLI tag, so it returns a screen of the one thing this decision must ignore and no
CLI version at all. `'v[0-9]*'` excludes them.

The scheme is plain `vMAJOR.MINOR.PATCH`.

- Patch (`v0.2.0` → `v0.2.1`) is the default for ordinary work, and the one to
  take without asking.
- Minor (`v0.2.0` → `v0.3.0`) when the release carries a feature worth naming.
  Whether it does is a product decision, not an arithmetic one; ask.
- Nothing to go on → ask rather than invent a base.

### The tag says how much you trust the build

Three levels, each reaching one step further (ADR 0105). The shape of the tag
picks the first two; a human picks the third.

| tag | reaches | when |
| --- | --- | --- |
| `v0.6.0-alpha.2`, `-beta.1`, `-rc.1` | that tag's own assets and images, and the installer's opt-in `edge` channel — **nothing else**: neither brew formula, no `:latest`, no winget, no default install | lowest confidence. You want a real release build that lands in front of nobody who did not ask for edge. |
| `v0.6.0` | GitHub prerelease, ghcr `:latest`, `brew install discobox-dev`, the installer's `latest` channel | the normal case, and what a dot release is for |
| the same release, blessed | `brew install discobox`, `curl -sSfL https://discobox.ai \| sh`; winget by hand (§6) | §7 — a human's decision, later |

A dot release is the default; take the patch bump without asking. It is cheap
now, because it only reaches people who went and installed `discobox-dev`.

**An `-alpha`/`-beta`/`-rc` tag is a deliberate choice, not a lesser one.** It is
how you get a genuine release build — signed darwin binary, multi-arch images
pushed, assets uploaded — that reaches no `brew install` of either name. You
reach it by pinning the version, and nothing promotes it later: when you trust
it, cut the dot release. Overriding that to push one at a channel anyway takes
`--prerelease` typed out.

**One consequence right now:** that listing puts `v0.6.0-alpha.1` through `.3`
above `v0.6.0`, because git's version sort ranks a suffixed tag above the bare
one. They are older: they come from before the dev channel existed, when an
alpha was the only way to try a build, and `v0.6.0` is the release they were
heading for. Do not continue that series and do not bump from it as though it
were a release. Bump from the newest dot release — after `v0.6.0` that is
`v0.6.1`, not `v0.6.0-alpha.4`.

## Remotes

Confirm with `git remote -v` rather than assuming; this varies by checkout.
Where both exist, `upstream` is GitHub (`ibuildthecloud/discobox`, which
redirects to `discobox-ai/discobox`) and `origin` is a Depot mirror. Releases,
tags, CI, and the `gh` CLI all mean **upstream**. Push there.

**Inside a discobox no remote is known to be GitHub**, and `gh` is not logged
in. `origin` is the host checkout's own remote, which is the Depot mirror as
readily as GitHub, and `discobox` is the pool's
`https://git.discobox.internal/...` route. In a discobox created before ADR
26-10-10-545, `origin` is that pool route and there may be an `upstream`: the
remote the host's branch tracks. Point `upstream` at GitHub whether or not it
exists. `discobox-ai/discobox` is public, so `git fetch` and
`git ls-remote` need no credential; every push and every `gh` call runs under
the token asked for once, up front (below), which arrives in `GH_TOKEN`. A
push needs the token named explicitly and the URL spelled out:

```bash
git remote add upstream https://github.com/discobox-ai/discobox.git 2>/dev/null ||
  git remote set-url upstream https://github.com/discobox-ai/discobox.git
git -c credential.helper= \
  -c 'credential.helper=!f() { if test "$1" = get; then echo username=x-access-token; echo "password=$GH_TOKEN"; fi; }; f' \
  push https://github.com/discobox-ai/discobox.git HEAD:main
```

The tag goes the same way, with `refs/tags/vX.Y.Z` in place of `HEAD:main`.

The Dagger constructor takes an explicit repository and defaults to
`discobox-ai/discobox`. For a fork, pass `dagger api call --repository=OWNER/REPO`;
source transport remotes do not choose release destinations.

### Inside a discobox: ask for access once, up front

Every GitHub step in this skill runs on one `GH_TOKEN`: `git push` over https
to `github.com`, and every `gh` call, which goes to `api.github.com`. A host
is exact, so the request names both, each narrowed to what the release does
there: pushing `main` and the tag to this repository, and reading it, its CI
and the tap. Ask for all of it in one request, so the human answers once and
the release then runs without them.

1. **Settle the version first.** The justification names it, and a version
   changed after it is granted is a second request. If the user did not name one and it is
   not a plain patch bump, ask the patch-or-minor question now, not at the tag.
2. **Request before anything else, in the background**, and run the local
   `ci:test` and `ci:check` while the human answers:

```bash
discobox-access request --json <<'EOF'
{
  "name": "github",
  "envVar": "GH_TOKEN",
  "routes": [
    "github.com/discobox-ai/discobox.git/**,/discobox-ai/discobox/**",
    "GET api.github.com/repos/discobox-ai/discobox/**",
    "GET api.github.com/repos/discobox-ai/homebrew-tap/**",
    "api.github.com/graphql graphql=query"
  ],
  "justification": "Cutting discobox release vX.Y.Z with the /release skill: push main of discobox-ai/discobox, watch CI until it is green, create tag vX.Y.Z on the green commit, then read the release and the dev Homebrew formula in discobox-ai/homebrew-tap. git uses the token over https to github.com and gh uses it against api.github.com. I will not force-push, delete or move a branch or tag, change a release, write to the tap, or read secrets. Needed for about two hours, since CI can take several rounds.",
  "grantTTLSeconds": 7200,
  "wait": true,
  "timeoutSeconds": 3600
}
EOF
```

Each of these is shaped by something that went wrong on the release that wrote
it down:

- **The justification names a version, never a commit.** The release commit
  moves whenever CI needs a fix or `main` moves under you — one release went through
  three candidate commits — and an approval naming one commit cannot honestly
  be stretched to another. §3's gate is what picks the commit.
- **Ask for how long it takes.** The approver picks the binding's lifetime, from
  `grantTTLSeconds` as the default. One lasted an hour, less than a
  release with a CI fix in it takes.
- **Never two requests in flight.** One filed while another is still pending
  comes back as that one — same request ID — and what it asked for is never
  shown to anyone. If something turns out to be missing, wait until nothing is
  pending, then ask.
- **`unavailable` with a control-plane 503 is a restart, not an answer.** The
  human never saw the request. Retry once before telling them to approve
  anything.

Deliberately left out: Homebrew refresh or publication, deleting a pushed tag,
and promoting (§5). Each is a failure path or a human's decision, and the moment
one is needed is a moment the human should be looking anyway. Ask then. None
has a route here: every API route is a `GET` or a GraphQL query, so `gh`
writes nothing, though the GraphQL queries read every repository the token
can see. The git route is held to this repository but not to a ref: a push
can write, force or delete any branch or tag of it, so which refs a push
writes is the justification's — `main` and the tag, never a delete or a
force — and `main`'s branch protection covers `main`.

## 1. Current pipeline boundary

The repository now uses `.dagger/modules/pipeline` (ADR 26-10-26-225).
Before local release, preview or publication work, start the fresh release
engine described in `docs/dagger.md` and select it with `DAGGER_ENGINE`.
Never use the engine/cache scope that checks unmerged PR code. Actions does
this through `.github/actions/release-engine`; native release Go caches must
also be separate from PR caches.
Native macOS and Windows commands run conventionally in Actions or on a local
machine of that operating system. Linux builds portable executables/helpers;
Mac consumes the helpers and supplies signed source-bound artifacts. A Linux
pass does not verify native execution. Never fake a native receipt. Missing
Mac artifacts still block assembly and publication. Rehearse locally instead:

```bash
dagger check
dagger api call ci-artifacts export --path=build/ci
dagger api call preview --version=v0.0.0-preview --images export --path=build/release/preview
```

Inspect `preview.json`, `native/macos.json`, and `native/windows.json`. The
native reports describe external commands; they are not successful test results. VM image and
kernel releases remain separate, executable Dagger operations.

## 2. Source and validation

Follow the checked-out branch policy. Build and test the exact release source.
Building and publishing product artifacts requires a clean checkout whose HEAD
matches the remote version tag. Promotion operates on an already published
release and uses the current pipeline without rebuilding historical source.
The operator passes the repository explicitly when targeting a fork:

```bash
dagger api call --repository=OWNER/REPO ci-artifacts export --path=build/ci
```

Actions triggers Dagger and native repository commands, supplies credentials,
and transports native artifacts. CI jobs are `pipeline`, `ux-smoke`,
`native-macos`, and `native-windows`. The first includes portable checks and
cross-builds; `ux-smoke` drives real Docker boxes on its host runner.
A green job therefore does not establish native coverage. Verify locally first;
use GitHub's run status only as evidence that its event adapter also ran.

## Documentation gate

Before tagging, run `go tool task docs:check` and review `docs/public/`
against the exact release CLI and console (including workflows and keys).
After publication creates the tag, follow `docs/public-import.md`: resolve
that docs-containing release tag to its full SHA and require a reviewed site
import PR recording the allowlist, file mapping and exporter version/command
before promoting its docs as current. Normal site builds consume committed
snapshots only. Keep prerelease labels until stable promotion is explicitly
approved; do not move the site stable target just because a tag exists.
Before marking a release stable, require that reviewed site import and confirm
its exact product version and SHA match the release being promoted.

## 3. Build and assemble

```bash
dagger api call release-build --version=vX.Y.Z export --path=build/release/bin
dagger api call release-assemble --version=vX.Y.Z --artifacts=build/release/bin export --path=build/release/assembled
```

The second command requires the real Mac files and receipt to have been placed
in the artifact directory. It verifies matching platform receipts and digests,
merges release.json, and stamps installers from the final binaries. Never fake
a receipt or substitute a cross-compiled unsigned Darwin executable.

## 4. Explicit publication

When native artifacts are available and publication is authorized, the full
entry point is:

```bash
dagger api call release --version=vX.Y.Z --native-artifacts=build/native --username=USER --registry-password=env:REGISTRY_PASSWORD --token=env:GH_TOKEN --tap-token=env:TAP_TOKEN
```

Credentials are Dagger Secrets. Do not print them or embed them into shell
command strings. A local `gh` credential can be supplied with
`--token=cmd:'gh auth token'`. Use discobox-access for credentials not already
granted; the initial route grant above does not grant write access to the tap.

Publication creates a GitHub prerelease. A dot version also advances registry
latest aliases and the Homebrew dev formula. Explicit semver prereleases
advance neither. `release-publish` consumes assembled artifacts; it never
rebuilds them. The source's real commit and final-server-digest-before-CLI
ordering must survive signing and publication unchanged.

## 5. Homebrew and promotion

Homebrew refresh now writes formula contents directly, with no cross-repo
workflow dispatch. Ask explicitly for tap contents write permission when it is
needed; the former dispatch-only permission is insufficient.

```bash
dagger api call brew-refresh --token=env:TAP_TOKEN
```

Stable promotion remains a separate human decision and only accepts a dot
version. When explicitly asked to promote:

```bash
dagger api call promote --version=vX.Y.Z --token=env:GH_TOKEN --tap-token=env:TAP_TOKEN
```

This updates the prerelease state and recomputes the Homebrew channels. It does
not submit winget. Re-running a release/promotion step must inspect current
external state; do not assume a previous successful Dagger build means the
publication destination is still correct.

## 6. winget remains manual

Automatic winget submission is disabled. Do not open a submission as an extra
release or promotion step. When explicitly authorized:

```bash
dagger api call winget-manifests --version=vX.Y.Z --token=env:GH_TOKEN export --path=build/winget
dagger api call winget-publish --version=vX.Y.Z --token=env:WINGET_TOKEN
```

Submission needs a classic PAT with public_repo for the account that owns the
fork. It opens/updates a PR in microsoft/winget-pkgs, whose validation and
moderator control publication. Report the PR as pending; never claim it merged
or Windows validation ran unless their native jobs actually succeeded.

## 7. VM artifact lines

```bash
dagger api call vm-check --kind=guest
dagger api call vm-check --kind=kernel
dagger api call vm-publish --kind=guest --version=vX.Y.Z --registry=ghcr.io/OWNER --username=USER --password=env:REGISTRY_PASSWORD
```

`vm/v*` tags address guests; `vm-kernel/v*` tags address kernels. Their digest
outputs are pins for the server, not implicit edits to it. Verification inspects
the actual kernel and filesystem configuration; it does not claim a VM boot.
