---
name: push-main
description: "Push the current content to obot-platform/discobot main, watch GitHub Actions with gh run watch, fix CI failures, and repeat until main CI passes."
---

# Push Main and Drive CI Green

Push the current repository content to the canonical `obot-platform/discobot`
`main` branch, then watch GitHub Actions with `gh run watch --exit-status`.
If CI fails, diagnose, fix, commit, push, and watch again. Continue until the
latest pushed `main` commit has passing CI.

Treat invoking `/push-main` as authorization to perform the normal end-to-end
push-and-fix loop on `obot-platform/discobot` without pausing for routine
confirmation. Keep the user informed with concise progress updates, but do not
stop on the happy path.

## Core Rules

- Treat `obot-platform/discobot` as the canonical upstream repository.
- Push only to the canonical repository's `main` branch.
- Do not assume the remote is named `origin` or `upstream`; discover it from
  `git remote -v`.
- Use `gh` for GitHub Actions inspection and waiting.
- When a workflow run is queued, waiting, requested, or in progress, wait with:
  `gh run watch <run-id> --exit-status`.
- Avoid polling as the primary wait mechanism. Use `gh run list` to discover
  run IDs, then `gh run watch --exit-status` to block until completion.
- Re-list runs after each watch because new runs can appear for the same commit
  after the first run starts.
- Do not finish until the latest pushed `HEAD` on upstream `main` has all
  relevant GitHub Actions runs completed successfully.
- If CI fails, inspect the failed run with `gh run view` and, when useful,
  `gh run view --log-failed`; fix the issue locally; run the smallest relevant
  local validation; commit; push; and restart CI verification for the new
  `HEAD`.
- Ask the user only when blocked by missing permissions/authentication,
  ambiguous CI failures, a risky or broad remediation, secrets, a non-fast-
  forward push, or an action that would rewrite published history.

## Procedure

### 1. Confirm repository and authentication

1. Run `git status --short --branch`.
2. Run `git remote -v` and select the remote whose URL points to
   `obot-platform/discobot`.
3. Run `gh auth status`.
4. Fetch the canonical refs:
   - `git fetch <remote> main`
   - `git fetch <remote> --tags`
5. Verify the current branch and upstream state:
   - `git branch --show-current`
   - `git rev-parse HEAD`
   - `git rev-parse <remote>/main`

### 2. Commit the current content when needed

1. If the working tree or index is dirty, inspect all changes:
   - `git status --short`
   - `git diff`
   - `git diff --cached`
2. Organize changes into the smallest sensible set of conventional commits.
   - Include a commit body after a blank line.
   - Avoid committing generated binaries, credentials, `.env` secrets, or
     unrelated temporary artifacts.
3. If the requested scope is clear, commit without asking. If changes are
   ambiguous or include suspicious files, ask the user before committing.
4. If there are no local changes, push the current `HEAD`.

### 3. Push to upstream main

1. Rebase/merge only if it can be done safely and without rewriting published
   history. Ask before any non-trivial conflict resolution.
2. Push the local `HEAD` to canonical `main`:
   - `git push <remote> HEAD:main`
3. Fetch again and verify:
   - `git fetch <remote> main`
   - `git rev-parse HEAD`
   - `git rev-parse <remote>/main`
4. Do not continue until local `HEAD` matches `<remote>/main`.

### 4. Watch CI for the pushed commit

1. Resolve the pushed SHA:
   - `git rev-parse HEAD`
2. Discover workflow runs for that SHA:
   - `gh run list --commit <sha>`
3. For each relevant run:
   - If active, run `gh run watch <run-id> --exit-status`.
   - If failed, inspect with `gh run view <run-id>` and
     `gh run view <run-id> --log-failed`.
4. After each watched run completes, run `gh run list --commit <sha>` again.
5. Treat CI as passing only when every relevant run for the pushed SHA has
   conclusion `success`.
6. If no run appears immediately after pushing, wait briefly and re-list a small
   number of times only to discover the run ID. As soon as a run ID exists,
   switch to `gh run watch --exit-status`.

### 5. Fix failures and repeat

When any CI run fails:

1. Identify the failing job/step from `gh run view` and failed logs.
2. Read the relevant code before editing.
3. Make the smallest scoped fix.
4. Run the smallest meaningful local validation first, for example:
   - `pnpm check`
   - `pnpm test`
   - `pnpm ci`
   - `pnpm run ci`
   - `go test ./...` in the affected module
5. Commit the fix with a conventional commit message.
6. Push again with `git push <remote> HEAD:main`.
7. Restart CI verification from step 4 for the new `HEAD`.

## Expected Commands

Use commands along these lines:

```bash
git status --short --branch
git remote -v
gh auth status
git fetch <remote> main
git fetch <remote> --tags
git rev-parse HEAD
git rev-parse <remote>/main
git diff
git diff --cached
git add <paths>
git commit -m '<subject>' -m '<body>'
git push <remote> HEAD:main
gh run list --commit <sha>
gh run watch <run-id> --exit-status
gh run view <run-id>
gh run view <run-id> --log-failed
```

## Completion Criteria

The skill is complete only when:

- the canonical remote for `obot-platform/discobot` has `main` at the local
  `HEAD`,
- all relevant GitHub Actions runs for that exact commit have completed, and
- every relevant run concluded successfully.

End with a concise summary that includes:

- the pushed commit SHA,
- the remote used,
- the successful CI run IDs,
- what changed during the push-main run, and
- why each change was made, including any CI failure fixes.
