---
name: release
description: Cut a discobox release — infer the next version, get main green, tag it, watch the release workflow, publish the Homebrew formula, and open the winget pull request. Use when the user wants to tag, release, ship a version, push a formula to the tap, or submit a version to winget.
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
git tag -l 'v*' --sort=-v:refname | head -5
```

The scheme is plain `vMAJOR.MINOR.PATCH`. Ignore the `vm/vN` tags entirely:
those version the VM guest image, not the CLI.

- Patch (`v0.2.0` → `v0.2.1`) is the default for ordinary work, and the one to
  take without asking.
- Minor (`v0.2.0` → `v0.3.0`) when the release carries a feature worth naming.
  Whether it does is a product decision, not an arithmetic one; ask.
- Nothing to go on → ask rather than invent a base.

The `v0.1.0-alpha.N` tags below `v0.1.0` were a temporary scheme and are not a
pattern to continue. A prerelease is now a deliberate exception, and everything
downstream treats it as one: neither the tap nor winget serves a prerelease, so
reaching either with one takes `--prerelease` typed out.

## Remotes

In this working copy `upstream` is GitHub (`ibuildthecloud/discobox`, which
redirects to `discobox-ai/discobox`) and `origin` is a Depot mirror. Releases,
tags, CI, and the `gh` CLI all mean **upstream**. Push there.

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

Run the CI test half locally first. It is the same target CI runs and it fails
in minutes rather than after a runner queue:

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
time, since `darwin` runs long — fetch it from the API:

```bash
J=$(gh run view <run-id> --repo discobox-ai/discobox --json jobs -q '.jobs[]|select(.name=="windows")|.databaseId')
gh api --allow-escape-sequences repos/discobox-ai/discobox/actions/jobs/$J/logs \
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

Wait for it to complete rather than polling; `binaries` has a darwin leg, so
this is another 20-plus-minute wait. The next step downloads the assets it
uploads, so running early just fails.

Do not write the release notes by hand. `release:publish` creates the release
with `--generate-notes`, and marks it `--prerelease` for anything that is not
exactly `vMAJOR.MINOR.PATCH` — the same test `release:image` uses to decide
whether `:latest` moves, so the release object and the image tags cannot
disagree about what a tag is. Confirm the result rather than reproducing it:

```bash
gh release view vX.Y.Z --repo discobox-ai/discobox \
  --json isPrerelease,assets -q '"prerelease=\(.isPrerelease) assets=\(.assets|length)"'
```

Every step is a Taskfile target that also runs locally (ADR 0066 §1):
`release:build`, `release:images`, `release:publish`.

## 5. The Homebrew tap

**For a stable tag this is automatic.** The release workflow's `publish` job
dispatches `update-formula.yml` in `discobox-ai/homebrew-tap` as soon as the
GitHub release exists (`brew:refresh`, using `HOMEBREW_TAP_TOKEN`). The tap then
takes the newest non-prerelease release, regenerates the formula with this
repository's `scripts/brew-formula.sh`, and commits if the result changed.

**That dispatch is the only thing that updates the tap.** It had a 30 minute
cron; that was deleted on purpose. A backstop that quietly covers for a broken
release step is a backstop that stops anyone noticing the step is broken, so the
release fails instead — a missing or expired `HOMEBREW_TAP_TOKEN` turns the
`publish` job red rather than leaving `brew install discobox` a version behind.

The consequence for this skill: **a red `publish` step here is a real failure and
the tap is genuinely stale.** Do not wait it out. Fix the token, then run the
dispatch by hand.

Confirm it landed rather than assuming:

```bash
gh api repos/discobox-ai/homebrew-tap/contents/Formula/discobox.rb \
  -q '.content' | base64 -d | grep -m1 version
```

If it has not, send the dispatch by hand — the same one the release workflow
sends, and safe to repeat:

```bash
go tool task brew:refresh
```

And the override, which regenerates and pushes the formula directly rather than
asking the tap to, for a tag the tap's own rule will not take at all:

```bash
go tool task brew:publish -- vX.Y.Z                        # stable
go tool task brew:publish -- --prerelease vX.Y.Z-rc1       # prerelease
```

`--prerelease` is required for anything that is not exactly `vMAJOR.MINOR.PATCH`,
and nothing sets it automatically: the tap serves one channel, so a prerelease
reaching `brew install discobox` is a decision somebody makes out loud. The task
refuses first and downloads second, so a missing flag costs nothing.

It writes to `discobox-ai/homebrew-tap` using the operator's own `gh`
credentials. `go tool task brew:formula` generates the formula without pushing,
which is the safe dry run.

## 6. The winget pull request

**For a stable tag this is automatic.** The release workflow's `winget` job runs
after `publish` and opens the request itself. Nothing to do but watch it.

It skips anything that is not exactly `vMAJOR.MINOR.PATCH`, so a prerelease
needs the target by hand — and saying so out loud, exactly as the tap does,
because winget has no notion of a channel and whatever is published is what
`winget install discobox` gives everyone:

```bash
go tool task winget:publish -- --prerelease vX.Y.Z-rc1
```

Run it by hand for a stable tag too when the job skipped for a missing token,
or when a first attempt failed — it is idempotent, and says `winget already
serves <version>` rather than opening a second request.

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
else's. Without the secret the job warns and ends green rather than failing a
release that otherwise succeeded.

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

## If a tag was pushed on a red commit

Decide immediately, while the release workflow is still building — it is much
cheaper before `publish` creates the GitHub release. Ask the user which:

- **Cancel and delete** (`gh run cancel`, `git push upstream :refs/tags/<tag>`),
  then re-tag that number on the green commit. Nothing red is ever published.
- **Let it finish and supersede it** with the next number. The bad prerelease
  stays in the release list, and only the good tag reaches the tap and winget.

Deleting a pushed tag is outward-facing. Do not choose it unprompted.

## Waiting

`darwin` regularly queues 20+ minutes before it starts. Do not poll it in a
loop — arm a Monitor that exits when the run completes, and keep working.
