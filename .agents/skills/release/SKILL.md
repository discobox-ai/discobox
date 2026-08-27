---
name: release
description: Cut a discobox release — get main green, tag it, let the release workflow build it, and publish the Homebrew formula. Use when the user wants to tag, release, ship a version, or push a formula to the tap.
allowed-tools: Bash, Read, Glob, Grep, AskUserQuestion
---

# Release

A release is cut by pushing a `v*` tag to **GitHub**. Everything after that is
the Release workflow; everything before it is making sure the commit deserves a
tag.

The gate is a green CI run **on the exact commit being tagged**. A tag is
public the moment it is pushed, and the release built from it cannot be
un-published cleanly — so the order is: green, then tag. Never the reverse.

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
run was going:

```bash
git fetch upstream
git log --oneline upstream/main..HEAD    # anything here has NOT been through CI
```

Tag the green commit explicitly — do not assume `HEAD`:

```bash
git tag -l 'v0.1.0-alpha.*' --sort=-v:refname | head -1   # the current alpha
git tag -a v0.1.0-alpha.N -m "v0.1.0-alpha.N" <green-commit>
git push upstream v0.1.0-alpha.N
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
```

Wait for it to complete. The next step downloads the assets it uploads, so
running early just fails.

Every step is a Taskfile target that also runs locally (ADR 0066 §1):
`release:build`, `release:images`, `release:publish`.

## 5. Publish the Homebrew formula

Only after the GitHub release exists — the formula's checksums are of the
assets the release actually uploaded, and the script downloads them to hash
them.

```bash
go tool task brew:publish -- --prerelease v0.1.0-alpha.N   # alpha/rc
go tool task brew:publish -- v1.2.3                        # stable
```

`--prerelease` is required for anything that is not exactly `vMAJOR.MINOR.PATCH`,
and nothing sets it automatically: the tap serves one channel, so a prerelease
reaching `brew install discobox` is a decision somebody makes out loud. The task
refuses first and downloads second, so a missing flag costs nothing.

It writes to `discobox-ai/homebrew-tap` using the operator's own `gh`
credentials. `go tool task brew:formula` generates the formula without pushing,
which is the safe dry run.

## If a tag was pushed on a red commit

Decide immediately, while the release workflow is still building — it is much
cheaper before `publish` creates the GitHub release. Ask the user which:

- **Cancel and delete** (`gh run cancel`, `git push upstream :refs/tags/<tag>`),
  then re-tag that number on the green commit. Nothing red is ever published.
- **Let it finish and supersede it** with the next number. The bad prerelease
  stays in the release list, and only the good tag reaches the tap.

Deleting a pushed tag is outward-facing. Do not choose it unprompted.

## Waiting

`darwin` regularly queues 20+ minutes before it starts. Do not poll it in a
loop — arm a Monitor that exits when the run completes, and keep working.
