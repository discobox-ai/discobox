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
| `v0.6.0-alpha.2`, `-beta.1`, `-rc.1` | that tag's own assets and images, and **nothing else** — neither brew formula, no `:latest`, no winget | lowest confidence. You want a real release build that cannot land in front of anyone. |
| `v0.6.0` | GitHub prerelease, ghcr `:latest`, `brew install discobox-dev` | the normal case, and what a dot release is for |
| the same release, blessed | `brew install discobox`; winget by hand (§6) | §7 — a human's decision, later |

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

**Inside a discobox there may be no GitHub remote at all** — `origin` is
`/.discobox/origins/primary`, the sandbox's own mirror, and `gh` is not logged
in. Add the remote. `discobox-ai/discobox` is public, so `git fetch` and
`git ls-remote` need no credential; every push and every `gh` call runs under
the token asked for once, up front (below). A push needs the token named
explicitly and the URL spelled out, or the access judge will refuse it:

```bash
git remote add upstream https://github.com/discobox-ai/discobox.git
discobox-access run --use <id> -- git -c credential.helper= \
  -c 'credential.helper=!f() { if test "$1" = get; then echo username=x-access-token; echo "password=$GH_TOKEN"; fi; }; f' \
  push https://github.com/discobox-ai/discobox.git HEAD:main
```

The tag goes the same way, with `refs/tags/vX.Y.Z` in place of `HEAD:main` and
the tag's own use ID.

One consequence: `Taskfile.yml`'s `RELEASE_REPO` reads `GITHUB_REPOSITORY` and
otherwise only `origin`, matching only github.com URLs, so it resolves empty here and
`scripts/brew-formula.sh` falls back to its own `discobox-ai/discobox` default.
That is correct by accident, not by design — if the formula ever points at the
wrong repo, this is why.

### Inside a discobox: ask for access once, up front

Every GitHub step in this skill runs on one `GH_TOKEN` with host `github.com`:
`git push` over https, and every `gh` call, which goes to `api.github.com` and
is covered by the same grant. Ask for all of it in one request, so the human
answers once and the release then runs without them.

1. **Settle the version first.** The tag's use names it, and a version changed
   after the grant is a second request. If the user did not name one and it is
   not a plain patch bump, ask the patch-or-minor question now, not at the tag.
2. **Request before anything else, in the background**, and run the local
   `ci:test` and `ci:check` while the human answers:

```bash
discobox-access request --json <<'EOF'
{
  "name": "github",
  "envVar": "GH_TOKEN",
  "host": "github.com",
  "justification": "Cutting discobox release vX.Y.Z with the /release skill: push main, watch CI until it is green, tag the green commit, then check the release and the dev Homebrew formula. git uses the token over https to github.com and gh uses it against api.github.com. Needed for about two hours, since CI can take several rounds.",
  "uses": [
    {"description": "git push the local main branch to https://github.com/discobox-ai/discobox.git main"},
    {"description": "git push the annotated release tag vX.Y.Z, created on the main commit CI verified green, to https://github.com/discobox-ai/discobox.git"},
    {"description": "Read GitHub Actions workflow runs, jobs, and job logs for discobox-ai/discobox with gh run list, gh run view, gh run watch, and gh api repos/discobox-ai/discobox/actions"},
    {"description": "Read the GitHub release for vX.Y.Z in discobox-ai/discobox with gh release view"},
    {"description": "Read Formula/discobox-dev.rb from discobox-ai/homebrew-tap with gh api repos/discobox-ai/homebrew-tap/contents"}
  ],
  "wait": true,
  "timeoutSeconds": 3600
}
EOF
```

Each of these is shaped by something that went wrong on the release that wrote
it down:

- **The tag's use names a version, never a commit.** The release commit moves
  whenever CI needs a fix or `main` moves under you — one release went through
  three candidate commits — and an approval naming one commit cannot honestly
  be stretched to another. §3's gate is what picks the commit.
- **The justification says how long.** The approver picks the grant's lifetime
  and the request has no field for it. One grant lasted an hour, less than a
  release with a CI fix in it takes.
- **Never two requests in flight.** One filed while another is still pending
  comes back as that one — same request ID, same uses — and what it asked for
  is never shown to anyone. If a use turns out to be missing, wait until
  nothing is pending, then ask.
- **`unavailable` with a control-plane 503 is a restart, not an answer.** The
  human never saw the request. Retry once before telling them to approve
  anything.

Deliberately left out: `brew:refresh`, `brew:publish`, deleting a pushed tag,
and promoting (§7). Each is a failure path or a human's decision, and the moment
one is needed is a moment the human should be looking anyway. Ask then.

## 1. Land the work

Follow the repository's git workflow: commit on the branch already checked out,
do not create branches.

If `upstream/main` has commits you do not (`git log --oneline main..upstream/main`),
rebase rather than merge:

```bash
git fetch upstream
git rebase upstream/main
```

Commits already applied upstream — a rebased copy of your own work, which is
common here — are patch-identical and drop out silently. Confirm with
`git cherry -v upstream/main main`: a `-` means upstream already has that patch.

## 2. Get it green

Run the CI test half locally first. It is the same target CI runs, and finding a
failure here costs seconds rather than a seven-minute round trip:

```bash
go tool task ci:test    # every module's tests, the way CI runs them
go tool task ci:check   # lint plus the windows/amd64 cross type-check
```

Then push and watch the run:

```bash
git push upstream main
gh run list --repo discobox-ai/discobox --workflow ci.yml --branch main --limit 1
gh run view <run-id> --repo discobox-ai/discobox \
  --json status,conclusion,jobs -q '.status+" "+(.conclusion//"-"), (.jobs[]|.name+" "+.status+" "+(.conclusion//"-"))'
```

Six jobs must pass: `check`, `test`, `verify`, `build`, `darwin`, `windows`.

### Reading a failure

`gh run view --log-failed` refuses while the run is in progress. To read a
finished job's log while its siblings are still going — which is most of the
time, since `windows` is the longest job — fetch it from the API:

```bash
J=$(gh run view <run-id> --repo discobox-ai/discobox --json jobs -q '.jobs[]|select(.name=="windows")|.databaseId')
gh api repos/discobox-ai/discobox/actions/jobs/$J/logs \
  | sed 's/\x1b\[[0-9;]*m//g' > /tmp/win.log
grep -nE "(--- FAIL|FAIL\s+github|panic:)" /tmp/win.log
```

### One failure hides the rest

`test:all` runs the modules in order — root, `cli`, `termpane`, `server`,
`pool-agent`, `sandbox-agent`, `access` — and stops at the first one that fails. Every
package *within* a module still runs, but no later module does. So a green
`windows` job after a fix is not evidence the fix was the last problem; it may
just be the first. Expect to iterate, and do not promise a single round trip.

### What actually breaks on the non-Linux runners

Real failures found this way, all worth checking before pushing:

- **Unix socket paths over ~108 bytes.** CI's `TMPDIR` is long; `t.TempDir()`
  adds the test's own name. Use `shorttmp.Dir(t)` for any directory a socket
  gets bound under. Reproduce locally with
  `TMPDIR=/some/deliberately/long/path go test ./...`.
- **Windows has no POSIX file mode.** `os.Chmod(dir, 0o000)` leaves it readable;
  there is no executable bit to carry over. Skip those assertions with
  `runtime.GOOS == "windows"` and say why.
- **`filepath` vs `path`.** Guest paths — anything inside a sandbox or pool,
  which is all of `layout` and the sandbox agent's workdirs — are Linux paths on
  every host. Build and compare them with `path`. `filepath.Dir` cleans to
  backslashes on Windows and quietly stops matching.
- **Host paths fed to guest-path code.** The exec manager resolves workdirs as
  guest paths, so a `C:\...` source is read as relative and joined onto the
  working root. Such a test is POSIX-only; skip it on Windows.

Fix these properly rather than skipping wholesale — one of them (a home
directory resolved before checking whether there was anything to install) was a
real launch failure that only Windows exposed.

## 3. Tag the commit CI verified

Check what CI actually ran against. Local `main` may have moved on while the
run was going, and the commit being tagged must be one that is on upstream
`main`:

```bash
git fetch upstream
git rev-parse HEAD upstream/main         # the release commit is on main
git log --oneline upstream/main..HEAD    # anything here has NOT been through CI
```

Then read what is going out, against the previous release tag:

```bash
git log --oneline <previous-tag>..HEAD
```

Say how many commits that is and what is notable in them before tagging. It is
the last moment anyone can notice that a release is carrying something it
should not.

Tag the green commit explicitly — do not assume `HEAD`:

```bash
git tag -a vX.Y.Z -m "vX.Y.Z" <green-commit>
git push upstream vX.Y.Z
```

Untested commits sitting on top of the green one go out in a later tag, after a
CI run of their own. Say so rather than sweeping them in.

## 4. The release workflow

Pushing the tag triggers `.github/workflows/release.yml`: `binaries` (linux and
darwin legs — darwin because cgo, the macOS SDK, and codesigning are
native-only), `images` (multi-arch, pushed to ghcr), then `publish`, which
creates the GitHub release and uploads `build/release/bin`.

```bash
gh run list --repo discobox-ai/discobox --workflow release.yml --limit 1
gh run watch <run-id> --repo discobox-ai/discobox --exit-status
```

Wait for it to complete rather than polling — about seven minutes, `images`
being the long pole rather than the darwin leg of `binaries`. The next step
downloads the assets it uploads, so running early just fails.

Do not write the release notes by hand. `release:publish` creates the release
with `--generate-notes` and `--prerelease`, always — every release is cut as a
prerelease and blessed later (ADR 0105). `isPrerelease: true` here is the
expected result, not a problem to fix. Confirm it rather than reproducing it:

```bash
gh release view vX.Y.Z --repo discobox-ai/discobox \
  --json isPrerelease,assets -q '"prerelease=\(.isPrerelease) assets=\(.assets|length)"'
```

`release:image` separately decides whether ghcr's `:latest` moves, from
`release:dot` — exactly `vMAJOR.MINOR.PATCH`. A dot release moves it; a
`-rc`/`-alpha` tag does not.

Every step is a Taskfile target that also runs locally (ADR 0066 §1):
`release:build`, `release:images`, `release:publish`.

## 5. The Homebrew tap

**For a dot release this is automatic, and it feeds `discobox-dev` only.** The
release workflow's `publish` job dispatches `update-formula.yml` in
`discobox-ai/homebrew-tap` as soon as the GitHub release exists (`brew:refresh`,
using `HOMEBREW_TAP_TOKEN`). The tap regenerates **both** formulae with this
repository's `scripts/brew-formula.sh` and commits whichever changed:

- `discobox-dev.rb` from the newest dot release — which is what you just cut.
- `discobox.rb` from the newest release a human has marked stable — which this
  is not, so it does not move here.

Check the one that should have moved:

```bash
gh api repos/discobox-ai/homebrew-tap/contents/Formula/discobox-dev.rb \
  -q '.content' | base64 -d | grep -m1 version
```

**That dispatch is the only thing that updates the tap.** It had a 30 minute
cron; that was deleted on purpose. A backstop that quietly covers for a broken
release step is a backstop that stops anyone noticing the step is broken, so the
release fails instead — a missing or expired `HOMEBREW_TAP_TOKEN` turns the
`publish` job red rather than leaving `brew install discobox-dev` a version
behind.

The consequence for this skill: **a red `publish` step here is a real failure and
the tap is genuinely stale.** Do not wait it out. Fix the token, then send the
dispatch by hand — the same one the release workflow sends, and safe to repeat:

```bash
go tool task brew:refresh
```

And the override, which regenerates and pushes one formula directly rather than
asking the tap to, for a tag the tap's own rule will not take at all:

```bash
go tool task brew:publish -- --dev vX.Y.Z                   # discobox-dev
go tool task brew:publish -- vX.Y.Z                         # discobox (stable)
go tool task brew:publish -- --dev --prerelease vX.Y.Z-rc1  # an rc, said out loud
```

`--dev` picks the formula; without it you are writing `discobox.rb`, which is
the stable channel and not this skill's to move. `--prerelease` is required for
anything that is not exactly `vMAJOR.MINOR.PATCH`, and nothing sets it
automatically: neither channel serves an rc, so one reaching either is a
decision somebody makes out loud. The task refuses first and downloads second,
so a missing flag costs nothing.

It writes to `discobox-ai/homebrew-tap` using the operator's own `gh`
credentials. `go tool task brew:formula -- [--dev] vX.Y.Z` generates a formula
without pushing, which is the safe dry run.

## 6. The winget pull request

**Switched off, and nothing automatic opens one.** winget is a stable channel,
so its submission belonged to `promote.yml` — and that job is currently removed.
[microsoft/winget-pkgs#432935](https://github.com/microsoft/winget-pkgs/pull/432935),
the first Discobox package, is open and waiting on the account's one-time CLA
signature and a moderator. A pull request per release while that sits there puts
the package at the back of the review queue each time rather than stacking
versions behind it. Nothing is lost by waiting: winget has never served
discobox, so no published version is going stale.

So its absence from both the release run *and* the promote run is correct right
now. Do not treat it as a failure, and do not open one to be helpful.

Once 432935 merges, restore the `winget` job in `promote.yml` — the comment
where it was says exactly what it contained.

The by-hand path still works and is the only one: it is idempotent, and says
`winget already serves <version>` rather than opening a second request:

```bash
go tool task winget:publish -- vX.Y.Z
```

It refuses anything that is not exactly `vMAJOR.MINOR.PATCH`. An rc reaching
winget takes `--prerelease` typed out, because winget has no notion of a channel
and whatever is published is what `winget install discobox` gives everyone.

Only after the GitHub release exists, either way: the checksums are of the
`discobox-windows-*.zip` archives (amd64 and arm64) the release uploaded.

Unlike the tap, **opening the request is not the end**. The Windows Package
Manager Community Repository is Microsoft's; their validation pipeline runs the
submission and a moderator merges it. Report the URL and say the version is
pending review, not published. Two things need a human on that thread:

- The account's *first ever* pull request has to sign the CLA, which is a reply
  on the thread. Once only — after that it submits unattended.
- A `Needs-Author-Feedback` label means a moderator asked something. Threads go
  stale after 5 days and close after 8.

### The token

`winget:publish` needs a **classic** PAT with `public_repo` (fine-grained
tokens are not accepted), for an account that has forked
`microsoft/winget-pkgs`, and checks the token's scopes before it writes
anything. By hand that is your own `gh` credential; the `winget` job, once
restored, takes it from the `WINGET_TOKEN` secret. `GITHUB_TOKEN` cannot do
this — it may only write to this repository, and the submission is a pull
request from a fork of somebody else's. The restored job should fail without
the secret: nothing else submits, so a missing token is a version winget
silently never gets, and that is worth a red run.

`go tool task winget:manifests` generates the three manifests without opening
anything, which is the safe dry run. Nothing about the submission is validated
on Windows — `winget validate` and `winget install --manifest` need Windows and
this release runs on Linux and macOS — so the pull request body says so rather
than ticking those boxes.

Once the *first* submission merges, add the install line to `README.md` beside
the `brew install` one — it is deliberately not there yet, because until a
version is published `winget install Discobox.Discobox` finds nothing:

```
winget install Discobox.Discobox
```

The identifier is `Discobox.Discobox` and the version string is the tag without
its `v`. Neither may drift: winget sorts semantic and string versions
differently, so a package that publishes both forms ends up pinned to whichever
version wins the wrong comparison, and the only fix is deleting the odd one out.

## 7. Marking a release stable

**This is not part of cutting a release, and it is not yours to do.** A tag you
just pushed is on the dev channel; it becomes stable when a human decides it
did. Hand back at the end of §5 and say so.

When the user does ask for it in those terms — "mark v0.3.1 stable", "promote
it", "push it to the real brew" — the act is on GitHub: open the release and
untick **"Set as a pre-release"**. That runs `promote.yml`, which gates on
`release:require-dot` and then updates `brew install discobox`. It does **not**
open a winget request — that job is switched off (§6).

```bash
gh release edit vX.Y.Z --repo discobox-ai/discobox --prerelease=false --latest
gh run list --repo discobox-ai/discobox --workflow promote.yml --limit 1
```

**Confirm the run actually started**, and do not report the promotion as done
until it has. `promote.yml` listens on three activity types and gates on the
release's own `prerelease` field, precisely because which type a checkbox-only
edit emits is not something GitHub documents plainly. If no run appears within a
minute, that is the case it was hedged against — dispatch it by hand rather than
waiting:

```bash
gh workflow run promote.yml --repo discobox-ai/discobox -f tag=vX.Y.Z
```

Four things to know before running any of it:

- **It is outward-facing and effectively one-way.** Both stable channels
  recompute from the newest blessed release, so backing it out means blessing a
  different release, not undoing this one — and a winget pull request, once
  opened, is Microsoft's to merge. Confirm the version out loud first.
- **Only a `vMAJOR.MINOR.PATCH` tag can be promoted.** `promote.yml` refuses an
  rc, because winget sorts semantic and string versions differently and a
  package serving both forms ends up pinned to the wrong one.
- **`--latest` is a separate GitHub flag** from `--prerelease=false`, and it is
  the repository header rather than any channel. Pass it unless the user is
  blessing something older than the current stable release.
- **A release is promoted by the Taskfile it was cut with.** A release event
  carries the tag as `GITHUB_REF`, so the workflow file that runs is the tagged
  commit's — which means a tag cut before `promote.yml` existed cannot be
  promoted by the checkbox at all, and no run will appear. Use the dispatch.

The `workflow_dispatch` above is also how to re-run a half-failed promotion
without touching the release again.

## If a tag was pushed on a red commit

Decide immediately, while the release workflow is still building — it is much
cheaper before `publish` creates the GitHub release. Ask the user which:

- **Cancel and delete** (`gh run cancel`, `git push upstream :refs/tags/<tag>`),
  then re-tag that number on the green commit. Nothing red is ever published.
- **Let it finish and supersede it** with the next number. The bad release stays
  in the release list as a prerelease, which is what every release is until
  somebody blesses it — so nothing about it reaches `brew install discobox` or
  winget, and the only channel to correct is `discobox-dev`, which the next tag
  moves anyway.

That second option is cheap, and usually right. Deleting a
pushed tag is outward-facing; do not choose it unprompted.

## Waiting

Measured over the last ten runs of each: **CI takes about 7 minutes and the
release workflow about 7 minutes**, and queue time is effectively zero — these
are Depot runners and they start immediately. Earlier revisions of this file
claimed a 20-plus-minute `darwin` queue. That is not what happens, and budgeting
for it wastes real time.

Where CI's seven minutes go, slowest first — every job runs in parallel, so the
wall clock is just the top row:

| job | median | |
| --- | --- | --- |
| `windows` | 7.2m | the critical path; 230s of it is `cd cli && go test ./...` |
| `darwin` | 5.7m | over half is Nix install, cache restore, and `nix develop` |
| `test` | 3.2m | |
| `check` | 1.9m | |
| `build` | 1.0m | |
| `verify` | 0.7m | |

Release, same shape: `images` 4.7m and `binaries` (darwin) 3.5m in parallel,
then `publish` 1.0m.

Still do not sit in a polling loop — arm a Monitor that exits when the run
completes and keep working. Just expect it to fire in about seven minutes, not
twenty.
