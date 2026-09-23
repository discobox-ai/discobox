---
name: open-pr
description: Push the committed work to a branch on GitHub, open a pull request into discobox `main`, watch its CI checks, fix whatever fails, and repeat until every check on the PR's latest commit is green — then leave it for a human to merge. Use when the user wants a PR opened, wants work proven by CI before it lands, or when triage-issue §6 hands off a finished fix.
allowed-tools: Bash, Read, Glob, Grep, Edit, Write, Agent, SendMessage, Skill, Monitor, AskUserQuestion
metadata:
  argument-hint: "[branch-name] [--issue N] [--qa-report path]"
---

# Open a PR and Drive It Green

Put the local commits on a GitHub branch, open a PR into `main`, and keep
working until the PR's head commit passes every check. The PR is where CI
proves the change; a human reviews and merges it.

Invoking this skill is authorization to push **one** branch to
`discobox-ai/discobox` (creating it on GitHub only — the local checkout stays on
its branch), open a PR from it, push further commits to it, edit the PR's body,
mark it ready, and comment on it. It is **not** authorization to merge, enable
auto-merge, push to `main`, force-push, request reviewers, or close anything —
ask for those.

## 0. Credentials

The repo is always `discobox-ai/discobox`; pass `--repo discobox-ai/discobox`
to every `gh` call. Outside a discobox, with `gh` logged in and a GitHub
remote, run the commands below directly and skip this section.

Inside a discobox there is no GitHub remote and no `gh` login: every GitHub
call goes through `discobox-access`. `triage-issue` asks for these uses up
front; check `discobox-access list` and request only what is missing, in one
request:

```bash
discobox-access request --json <<'EOF'
{
  "id": "com.github.api",
  "justification": "the user asked me to open a pull request for my local commits in discobox-ai/discobox and get its CI green",
  "uses": [
    {"description": "Fetch main from discobox-ai/discobox, and push the branch <branch> to it, with git over https using the token; never main, never --force"},
    {"description": "Open a draft pull request from <branch> into main in discobox-ai/discobox, and view, list, edit the body of, mark ready, and comment on that pull request, with gh pr create, gh pr view, gh pr list, gh pr edit, gh pr ready, and gh pr comment"},
    {"description": "Read CI for that pull request with gh pr checks, gh run list, gh run view, and job logs via gh api repos/discobox-ai/discobox/actions/jobs/<id>/logs, and re-run its failed jobs once with gh run rerun --failed"}
  ],
  "grantTTLSeconds": 14400,
  "wait": true
}
EOF
```

Name the branch in the uses (§1 picks it) — the checker reads them literally.
Git takes the token through a one-shot credential helper:

```bash
GH_URL=https://github.com/discobox-ai/discobox
discobox-access run --use <id> -- sh -c 'git -c credential.helper= \
  -c credential.helper="!f() { echo username=x-access-token; echo password=\$GH_TOKEN; }; f" \
  push '"$GH_URL"' HEAD:refs/heads/<branch>'
```

The injected token lives only minutes, so a long `gh run watch` dies with a
401. Make each check a separate short `discobox-access run`.

## 1. What goes in the PR

The work must be committed. `git status --short` shows nothing tracked — an
untracked `wnb-*-failed.txt` from the dev loop is noise — and anything else
uncommitted is a question for the user, not something to sweep in.

Fetch GitHub's `main` (with the credential helper above, `fetch "$GH_URL"
main`) and read what the PR would carry:

```bash
git log --oneline FETCH_HEAD..HEAD
git diff --stat FETCH_HEAD...HEAD
```

Those must be exactly this change's commits. A commit that belongs to someone
else's unpushed work, or to a different change, means stop and ask — never
open a PR that smuggles it in. An empty list means there is nothing to PR.

If `main` moved and `git merge-tree --write-tree FETCH_HEAD HEAD` reports a
conflict, rebase onto `FETCH_HEAD` now — the commits are not published yet, so
this rewrites nothing anyone has — and re-run the tests the conflict touched.

**Branch name:** `issue-<N>` when it fixes an issue; otherwise `<type>/<slug>`
from the first commit's subject (`fix/rm-stopped-box`). Check it is free:
`git ls-remote "$GH_URL" refs/heads/<branch>` (through `discobox-access`)
returns nothing, or a commit that is an ancestor of `HEAD` — that is this
work's earlier push. Anything else is someone's branch: pick another name.

## 2. Push and open

Push `HEAD:refs/heads/<branch>` as in §0. Then find or open the PR:

```bash
gh pr list --repo discobox-ai/discobox --head <branch> --state open --json number,url
gh pr create --repo discobox-ai/discobox --base main --head <branch> --draft \
  --title "<conventional subject>" --body-file <scratchpad>/pr-body.md
```

One PR per branch: if one is open, update its body with `gh pr edit
--body-file` instead. Draft until green, so nobody reviews a PR CI has not
passed.

Title: the commit's subject when there is one commit; otherwise a
conventional subject covering them. Body:

```markdown
<What changed and why, three to six lines. The cause, the fix, anything a
reviewer should look at first.>

Fixes #<N>

## Testing

- <each phase 1 step and its result; pre-existing failures named>

<details><summary>QA: Verified — build <sha></summary>

<the test-fix QA report, verbatim>
</details>

<sub>From `<$DISCOBOX_ADDRESS>`</sub>
```

`Fixes #N` only when it does fix the issue — it closes the issue on merge;
`Refs #N` otherwise. Leave out the Testing section's QA block when `test-fix`
did not run, and say so instead. The footer carries `$DISCOBOX_ADDRESS`
verbatim; leave it off when that is unset. End the body with whatever
attribution line the harness asks PR descriptions to end with.

## 3. Watch the checks

Everything below is about the PR's **head commit**, which must equal local
`HEAD`:

```bash
gh pr view <pr> --repo discobox-ai/discobox --json headRefOid -q .headRefOid
gh pr checks <pr> --repo discobox-ai/discobox --json name,state,bucket,link
```

Arm a Monitor that polls `gh pr checks` (one `discobox-access run` per poll,
every 60s) and exits when no check is in bucket `pending`, and keep working
while it runs. CI takes about seven minutes.

- **Green** — every check is `pass` or `skipping`, and CI's six jobs (`check`,
  `test`, `verify`, `build`, `darwin`, `windows`) are among them. Path-filtered
  workflows (`vm-image`, `libkrun-runtime`, `vm-krun`) appear only when their
  paths changed.
- **`cancel`** — CI cancels a PR's run when a newer push supersedes it. A
  cancelled check on an older commit is not a failure; one on the head commit
  is — re-list, then re-run it.
- **`fail`** — §4.

## 4. Fix, push, repeat

Read the failure: [ci-failures.md](ci-failures.md). Then:

1. Decide whether it is this change's. Failing on `main` too → pre-existing:
   note it on the PR, do not fix it here unless the user asks.
2. Reproduce it locally where you can (Windows- and macOS-only failures often
   reproduce with the tricks in ci-failures.md), fix the cause, and run the
   narrowest local check that covers it.
3. Run the fix through `discobox-review` — a CI fix is new code. The review's
   base has not moved, so only the files you edited need approval again.
4. If the fix changes behavior QA validated, run `test-fix` phase 2 again for
   the affected checks.
5. Commit conventionally, push the branch (fast-forward — never force), and go
   back to §3 against the new head.

After five failed rounds, stop and put the remaining failures to the user.

A conflict with `main` after the PR is open needs a rebase and a force-push:
ask first.

## Done when

The PR's head commit is local `HEAD`, every check on it is green, and the PR is
marked ready (`gh pr ready`). Do not merge.

Finish with: the PR URL, the head SHA, the green run IDs, and every CI failure
met on the way with its fix — or the pre-existing failures left for the user.
