---
name: release
description: Cut a discobox release — infer the next version, get main green, tag it, watch the release workflow, and land it on the dev Homebrew channel; separately, mark an already-cut release stable so it reaches `brew install discobox` and winget. Use when the user wants to tag, release, ship a version, push a formula to the tap, promote or mark a release stable, or submit a version to winget.
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

The glob matters. `'v*'` also matches `vm/vN` — the VM guest image tags, which
sort above every CLI tag, so it returns a screen of the one thing this decision
must ignore and no CLI version at all. `'v[0-9]*'` excludes them.

The scheme is plain `vMAJOR.MINOR.PATCH`.

- Patch (`v0.2.0` → `v0.2.1`) is the default for ordinary work, and the one to
  take without asking.
- Minor (`v0.2.0` → `v0.3.0`) when the release carries a feature worth naming.
  Whether it does is a product decision, not an arithmetic one; ask.
- Nothing to go on → ask rather than invent a base.

Dot releases are now cheap: every one publishes as a GitHub prerelease and only
reaches people who installed `discobox-dev`, so cutting one does not need the
ceremony it used to. Take the patch bump without asking.

**The newest tags are alphas, and the alpha scheme is retired.** `v0.6.0-alpha.1`
through `.3` sit above `v0.5.2` in that list. They are exactly what the dev
channel replaces (ADR 0105): an alpha moves nothing at all — not `:latest`, not
`discobox-dev` — and cannot be promoted to stable. Do not continue the series,
and do not bump from it as though it were a release. **The next tag is the dot
release those alphas were heading for**: after `v0.6.0-alpha.3` that is
`v0.6.0` — not `v0.6.1`, and not `v0.6.0-alpha.4`.

Reaching any channel with an `-rc`/`-alpha` tag takes `--prerelease` typed out.

## Remotes

Confirm with `git remote -v` rather than assuming; this varies by checkout.
Where both exist, `upstream` is GitHub (`ibuildthecloud/discobox`, which
redirects to `discobox-ai/discobox`) and `origin` is a Depot mirror. Releases,
tags, CI, and the `gh` CLI all mean **upstream**. Push there.

**Inside a discobox there may be no GitHub remote at all** — `origin` is
`/.discobox/origins/primary`, the sandbox's own mirror, and `gh` is not logged
in. Add the remote, and get a credential with the `discobox-access` skill rather
than assuming one exists; a push needs the token named explicitly and the URL
spelled out, or the access judge will refuse it:

```bash
git remote add upstream https://github.com/discobox-ai/discobox.git
discobox-access run --use <id> -- git -c credential.helper= \
  -c 'credential.helper=!f() { if test "$1" = get; then echo username=x-access-token; echo "password=$GH_TOKEN"; fi; }; f' \
  push https://github.com/discobox-ai/discobox.git HEAD:main
```

One consequence: `Taskfile.yml`'s `RELEASE_REPO` reads only `origin` and only
matches github.com URLs, so it resolves empty here and
`scripts/brew-formula.sh` falls back to its own `discobox-ai/discobox` default.
That is correct by accident, not by design — if the formula ever points at the
wrong repo, this is why.

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
`pool-agent`, `sandbox-agent` — and stops at the first one that fails. Every
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
git log --oneline v0.2.0..HEAD
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

**Not part of cutting a release.** winget is a stable channel, so its submission
belongs to `promote.yml` (§7) and does not run here. Cutting a tag never opens a
winget pull request, and its absence from the release run is correct.

Run it by hand only when promotion's `winget` job skipped for a missing token or
a first attempt failed — it is idempotent, and says `winget already serves
<version>` rather than opening a second request:

```bash
go tool task winget:publish -- vX.Y.Z
```

It refuses anything that is not exactly `vMAJOR.MINOR.PATCH`. An rc reaching
winget takes `--prerelease` typed out, because winget has no notion of a channel
and whatever is published is what `winget install discobox` gives everyone.

Only after the GitHub release exists, either way: the checksum is of the
`discobox-windows-amd64.zip` the release uploaded.

Unlike the tap, **opening the request is not the end**. The Windows Package
Manager Community Repository is Microsoft's; their validation pipeline runs the
submission and a moderator merges it. Report the URL and say the version is
pending review, not published. Two things need a human on that thread:

- The account's *first ever* pull request has to sign the CLA, which is a reply
  on the thread. Once only — after that it submits unattended.
- A `Needs-Author-Feedback` label means a moderator asked something. Threads go
  stale after 5 days and close after 8.

### The token

The job needs `WINGET_TOKEN`: a **classic** PAT with `public_repo` (fine-grained
tokens are not accepted), for an account that has forked
`microsoft/winget-pkgs`. `GITHUB_TOKEN` cannot do this — it may only write to
this repository, and the submission is a pull request from a fork of somebody
else's. Without the secret the `winget` job fails: nothing else submits, so a
missing token is a version winget silently never gets, and that is worth a red
run.

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
`release:require-dot` and then updates `brew install discobox` and opens the
winget request.

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

That second option is cheaper than it used to be, and usually right. Deleting a
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
