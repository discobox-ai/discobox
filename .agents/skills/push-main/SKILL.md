---
name: push-main
description: Push the current work to discobox `main` on GitHub, watch the CI run, fix whatever fails, and repeat until the pushed commit is green. Use when the user wants to push to main, get main green, or drive CI to passing.
allowed-tools: Bash, Read, Glob, Grep, Edit, Write, AskUserQuestion
---

# Push Main and Drive CI Green

Push the checked-out work to `main` on GitHub, then keep working until the
exact commit that landed has every CI job passing. If a job fails: diagnose,
fix, commit, push, watch again.

Invoking this skill is authorization to run that loop without pausing for
routine confirmation. Report progress concisely; do not stop on the happy path.

Ask the user only when blocked: missing auth or permissions, a non-fast-forward
push, anything that rewrites published history, a fix that is broad or risky,
secrets, or a CI failure whose right remediation is genuinely ambiguous.

## Remotes

`upstream` is GitHub (`ibuildthecloud/discobox`, redirecting to
`discobox-ai/discobox`); `origin` is a Depot mirror. CI, `gh`, and "main" all
mean **upstream**. Do not assume the names — confirm with `git remote -v` and
pick the remote whose URL is the GitHub one.

`gh` does not infer the repo from `upstream`, so pass
`--repo discobox-ai/discobox` to every `gh` call.

## 1. Commit what is outstanding

Follow the repository git workflow: commit on the branch already checked out,
never create a branch. This skill pushes to `main`, so the branch is normally
`main` — if it is not, ask before pushing someone's feature branch to `main`.

```bash
git status --short --branch
git diff
git diff --cached
```

Organize the changes into the smallest sensible set of conventional commits
with a body. Do not sweep in generated binaries, credentials, or scratch files.
If the working tree is clean, push the current `HEAD`.

Before pushing, run `go tool task check-hooks` — the background hooks are what
catches formatting, tidy, codegen, and lint drift, and every one of those is a
CI job. If its output looks stale, `go tool task rerun-hooks` and check again.

## 2. Run CI's own targets locally first

These are the same targets CI runs, and they fail in minutes instead of after a
runner queue:

```bash
go tool task ci:test    # every module's tests, the way CI runs them
go tool task ci:check   # lint plus the windows/amd64 cross type-check
go tool task verify     # fmt, go.mod, generated files, Mermaid are current
```

## 3. Push and watch

```bash
git fetch upstream
git log --oneline main..upstream/main    # rebase if upstream moved ahead
git push upstream HEAD:main
git rev-parse HEAD upstream/main         # must match before going on
```

Commits already applied upstream are patch-identical and drop out of a rebase
silently; `git cherry -v upstream/main main` shows a `-` for those.

Then find the run for that exact SHA and block on it:

```bash
SHA=$(git rev-parse HEAD)
gh run list --repo discobox-ai/discobox --commit $SHA
gh run watch <run-id> --repo discobox-ai/discobox --exit-status
```

Prefer `gh run watch --exit-status` over polling. If no run exists yet, re-list
a few times only to discover the ID, then switch to watching. Re-list after
each watch returns — more runs can appear for the same commit.

`darwin` regularly queues 20+ minutes before it starts. Do not sit in a polling
loop for it: arm a Monitor that exits when the run completes and keep working.

Six jobs must pass: `check`, `test`, `verify`, `build`, `darwin`, `windows`.

## 4. Read the failure

`gh run view --log-failed` refuses while the run is still in progress. To read a
finished job's log while its siblings are still going — which is most of the
time, because of `darwin` — go through the API:

```bash
J=$(gh run view <run-id> --repo discobox-ai/discobox --json jobs \
  -q '.jobs[]|select(.name=="windows")|.databaseId')
gh api --allow-escape-sequences repos/discobox-ai/discobox/actions/jobs/$J/logs \
  | sed 's/\x1b\[[0-9;]*m//g' > /tmp/win.log
grep -nE "(--- FAIL|FAIL\s+github|panic:)" /tmp/win.log
```

### One failure hides the rest

`test:all` runs the modules in order — root, `cli`, `termpane`, `server`,
`pool-agent`, `sandbox-agent` — and stops at the first that fails. Every
package *within* a module still runs, but no later module does. A green job
after a fix is not proof the fix was the last problem. Expect to iterate; do
not promise a single round trip.

### What actually breaks on the non-Linux runners

- **Unix socket paths over ~108 bytes.** CI's `TMPDIR` is long and `t.TempDir()`
  adds the test name. Use `shorttmp.Dir(t)` for any directory a socket binds
  under. Reproduce with `TMPDIR=/some/deliberately/long/path go test ./...`.
- **Windows has no POSIX file mode.** `os.Chmod(dir, 0o000)` leaves it readable
  and there is no executable bit. Skip such assertions on
  `runtime.GOOS == "windows"` and say why.
- **`filepath` vs `path`.** Guest paths — anything inside a sandbox or pool, so
  all of `layout` and the sandbox agent's workdirs — are Linux paths on every
  host. Build and compare them with `path`; `filepath.Dir` cleans to backslashes
  on Windows and quietly stops matching.
- **Host paths fed to guest-path code.** The exec manager resolves workdirs as
  guest paths, so a `C:\...` source is read as relative and joined onto the
  working root. Such a test is POSIX-only; skip it on Windows.

Fix these properly rather than skipping wholesale — one of them (a home
directory resolved before checking whether there was anything to install) was a
real launch failure that only Windows exposed.

## 5. Fix, push, repeat

Read the code before editing. Make the smallest scoped fix, re-run the narrowest
local validation that covers it (the affected module's `go test ./...`, then the
relevant `ci:` target), commit conventionally, push to `upstream HEAD:main`, and
restart step 3 against the new SHA.

Architecture-changing fixes update the affected `DESIGN.md` in the same commit.

## Done when

`upstream/main` is at the local `HEAD` and every CI run for that exact commit
concluded `success`.

Finish with a short summary: the pushed SHA, the successful run IDs, what
changed during the loop, and why — including each CI failure and its fix.
