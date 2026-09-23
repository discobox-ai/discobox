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

CI takes about seven minutes end to end: the runners are Depot's and start
without a queue, and `windows` is the critical path, not `darwin`. Still do not
sit in a polling loop: arm a Monitor that exits when the run completes and keep
working.

Six jobs must pass: `check`, `test`, `verify`, `build`, `darwin`, `windows`.

## 4. Read the failure

Follow [../open-pr/ci-failures.md](../open-pr/ci-failures.md): reading a
finished job while the run continues, why one failing module hides the rest,
and what breaks only on the non-Linux runners.

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
