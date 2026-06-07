---
name: release
description: "Run the autonomous release procedure: infer the next version when needed, verify upstream main and CI, create and push the tag, update GitHub release notes, and watch the release workflow to completion."
allowed-tools: Bash(git tag, git log, git describe, git push, git branch, git remote, git fetch, git rev-parse, git ls-remote, git for-each-ref, gh run list, gh run view, gh run watch, gh release view, gh release edit, gh api, pnpm ci, pnpm check, pnpm test, go test), Read, Glob, Grep, Edit, AskUserQuestion
metadata:
  argument-hint: "[version-or-tag]"
---

# Autonomous Release Procedure

Run a release as autonomously as possible. Treat `/release` as authorization to perform the normal release flow end-to-end without pausing for approval on the happy path. Keep the user informed with brief status updates, but continue automatically unless a blocking failure, missing permission, ambiguous repository state, or history-rewriting correction requires a decision.

## Core Rules

- Treat `obot-platform/discobot` as the canonical upstream repository.
- Treat invoking `/release` as approval to perform the standard release actions for the chosen tag: fetching refs, pushing the current `HEAD` to the canonical `main` branch when needed, waiting for CI, creating and pushing the release tag, updating the GitHub release, and watching the release workflow to completion.
- Do not stop to ask for routine confirmations, acknowledgements, or permission to continue during the normal release flow.
- Do not create or push a release tag until the current `HEAD` commit is confirmed to be on that upstream repository's `main` branch.
- Do not create or push a release tag until every CI run for that `HEAD` commit has completed successfully.
- If CI is still running, explicitly wait with `gh run watch <run-id> --exit-status`.
- If CI fails, investigate and fix it autonomously when the cause and remedy are clear, re-run the smallest appropriate local validation, push the fix, and restart CI verification for the new `HEAD`.
- Ask the user only when recovery is not straightforward, the required fix is ambiguous, the remediation would materially change release scope, permissions are missing, or any action would rewrite published history.
- After the tag is pushed, generate changelog/release notes, update the GitHub release, and then watch the release workflow until it succeeds.

## Version Inference

1. If the user provided a version or tag, use it.
   - Normalize it to start with `v`.
2. Otherwise, inspect recent tags with:
   - `git describe --tags --abbrev=0 2>/dev/null || echo "no tags"`
   - `git tag --sort=-v:refname | head -20`
3. Infer the next tag automatically:
   - If the latest tag is an alpha tag like `v1.2.3-alpha4`, use the next alpha tag: `v1.2.3-alpha5`.
   - If the latest tag is a beta tag like `v1.2.3-beta2`, use the next beta tag: `v1.2.3-beta3`.
   - If the latest tag is an RC tag like `v1.2.3-rc7`, use the next RC tag: `v1.2.3-rc8`.
   - If the latest tag is a normal release like `v1.2.3`, assume the next release should start an RC cycle for that same numeric base: `v1.2.3-rc1`.
   - If there are no tags, default to `v0.1.0-rc1`.
4. Before tagging, show the inferred version and the reason it was chosen.

## Full Procedure

### 1. Identify the canonical remote

1. Run `git remote -v`.
2. Select the remote whose fetch URL points to `obot-platform/discobot`.
3. Do not assume the remote is named `origin` or `upstream`; discover it from the URL.
4. Fetch the latest refs before making any decisions:
   - `git fetch <remote> --tags`
   - `git fetch <remote> main`

### 2. Verify that HEAD is the upstream main commit

1. Resolve the local and remote SHAs:
   - `git rev-parse HEAD`
   - `git rev-parse <remote>/main`
2. If `HEAD` is not equal to `<remote>/main`:
   - Explain briefly that the release must be cut from the commit on upstream `main`.
   - Push the current `HEAD` to `<remote> main` automatically.
   - After pushing, fetch again and re-check that `HEAD == <remote>/main`.
3. Do not proceed until this check passes.

### 3. Verify CI for the head commit

1. Determine the commit SHA being released from `git rev-parse HEAD`.
2. Inspect all GitHub Actions runs for that commit with `gh run list --commit <sha>`.
3. For every run associated with that commit:
   - If it is queued, requested, waiting, or in progress, watch it with `gh run watch <run-id> --exit-status`.
   - Re-list runs after each watch until no run for that commit is still active.
4. Treat the commit as releasable only when every relevant run for that commit has concluded successfully.
   - `success` is acceptable.
   - `failure`, `cancelled`, `timed_out`, or `action_required` are blocking.
5. If there is a failure:
   - Inspect the failed run with `gh run view <run-id>` and, when helpful, `gh run view <run-id> --log-failed`.
   - Fix the issue locally when the remedy is clear.
   - Re-run the smallest appropriate local validation first (`pnpm check`, `pnpm test`, `pnpm ci`, `go test`, or narrower commands as needed).
   - Push the fix to upstream `main` automatically and restart the CI verification cycle from the beginning for the new `HEAD`.
   - Ask the user only if the failure is ambiguous, the fix is risky or broad, local validation is inconclusive, or pushing the fix would exceed the normal release flow.

### 4. Review release changes before tagging

1. Determine the comparison base tag:
   - For alpha releases, compare against the previous alpha tag.
   - For beta releases, compare against the previous beta tag.
   - For RC releases, compare against the previous RC tag.
   - For normal releases, compare against the previous normal release tag.
2. Show the commits since that comparison base with:
   - `git log --oneline <previous-tag>..HEAD`
3. If there is no matching previous tag in the same family, explain the fallback you are using.

### 5. Create and push the release tag

1. Create an annotated tag:
   - `git tag -a <tag> -m "Release <tag>"`
2. Record the existing workflow run IDs for the release workflow before pushing the tag.
3. Push the tag to the canonical remote:
   - `git push <remote> <tag>`

### 6. Generate changelog and release notes

1. Generate release notes relative to the correct previous tag.
2. Use GitHub's generated notes API when possible so the notes are based on the actual comparison range:
   - `gh api repos/obot-platform/discobot/releases/generate-notes -f tag_name=<tag> -f target_commitish=<sha> -f previous_tag_name=<previous-tag>`
3. Also capture a concise changelog from git history for the same range.
4. Update the GitHub release for the tag with the generated notes:
   - `gh release edit <tag> --title "Discobot <tag>" --notes-file <file>`
5. If the tag is an alpha, beta, or RC release, mark the GitHub release as a prerelease when updating it.

### 7. Watch the release workflow after tagging

1. After the tag push, identify the new workflow run triggered by that tag.
   - Compare `gh run list` results from before and after the tag push.
   - Focus on the release workflow (`.github/workflows/release.yml`) for the release commit/tag.
2. If the release workflow is queued or running, wait with:
   - `gh run watch <run-id> --exit-status`
3. Re-check until all workflow runs triggered by the tag have completed.
4. If the tagged release workflow succeeds:
   - Confirm that the GitHub release exists and has the expected notes/body.
   - Tell the user the release completed successfully.
5. If the tagged release workflow fails:
   - Report the failing run and the failure summary.
   - Do not rewrite or move the tag without explicit user approval.

## Decision Points That Still Require the User

Ask the user only when:
- a failed CI run cannot be fixed confidently or would require a broad or risky remediation,
- GitHub authentication, authorization, or repository state prevents the standard flow,
- a tag or GitHub release already exists in a conflicting state,
- taking corrective action would rewrite a published release tag or otherwise modify published history.

## Expected Commands

Use commands along these lines during the procedure:

```bash
# Determine canonical remote and current state
git remote -v
git fetch <remote> --tags
git fetch <remote> main
git rev-parse HEAD
git rev-parse <remote>/main
git tag --sort=-v:refname | head -20
git describe --tags --abbrev=0 2>/dev/null || echo "no tags"

# Inspect and wait for commit CI
gh run list --commit <sha>
gh run watch <run-id> --exit-status
gh run view <run-id>
gh run view <run-id> --log-failed

# Review release delta
git log --oneline <previous-tag>..HEAD

# Tag and publish
git tag -a <tag> -m "Release <tag>"
git push <remote> <tag>

# Generate and publish release notes
gh api repos/obot-platform/discobot/releases/generate-notes \
  -f tag_name=<tag> \
  -f target_commitish=<sha> \
  -f previous_tag_name=<previous-tag>
gh release edit <tag> --title "Discobot <tag>" --notes-file <file>

# Watch release workflow after tag push
gh run list --workflow release.yml --commit <sha>
gh run watch <run-id> --exit-status
```

## Output Expectations

Keep the user updated with concise status updates:
- inferred tag,
- upstream remote chosen,
- whether `HEAD` is on upstream `main`,
- CI status for the release commit,
- any local fixes made for failed CI,
- tag creation and push status,
- release notes update status,
- tagged release workflow result.

## Example Usage

```text
/release
/release v1.2.3
/release v1.2.3-rc4
```
