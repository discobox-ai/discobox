---
name: develop
description: Build the change already discussed and agreed in this session and deliver it as a pull request — restate the agreed plan, implement it, review it with discobox-review, verify it at runtime, commit, open a PR from discobox/<slug>, drive CI green, get a GitHub Copilot review and answer it, then keep the PR mergeable and answer comments until a human merges it. Use when the approach has been talked through in the session and the user asks for it to be built and delivered as a PR, or invokes `develop`; the result is a PR, never `discobox apply`. Not for a plain "go ahead" that means build it here.
allowed-tools: Bash, Read, Glob, Grep, Edit, Write, Agent, SendMessage, Skill, Monitor, AskUserQuestion
metadata:
  argument-hint: "[branch slug] [--issue N]"
---

# Develop

The conversation so far is the spec: what to build and how has been
discussed, and now it gets built. One change, one branch, one PR, carried
until it merges. This skill sequences the repository's other skills, like
`deliver-issue` does for an issue; it does not replace them. Each phase ends
with a one-line status in the terminal.

The work leaves this box as a pull request on GitHub, not through
`discobox apply`: do not tell the user to apply it, and do not stop at local
commits.

Invoking this skill is authorization to: push the branch `discobox/<slug>`
to `discobox-ai/discobox` (create it, fast-forward it, and
`git push --force-with-lease` it after a rebase onto `main`), open and edit
one PR from it into `main`, comment on that PR and reply to its review
comments, request a GitHub Copilot review of it, and re-run its failed CI
jobs. It is **not** authorization to push `main` or any other branch, merge,
enable auto-merge, close anything, or force-push without a lease.

These widen `open-pr` (and `test-fix`, `commit`) while this skill runs: where
`open-pr` says invoking it does not authorize force-pushing or requesting
reviewers, or says a conflict with `main` needs a rebase and to "ask first",
this skill has already asked — rebase and force-push with a lease to
`discobox/<slug>`, and request the Copilot review, without asking again.

## 0. Before anything

- **Restate the plan.** Write the agreed change back in a few lines: what
  changes, the approach chosen (and the alternatives turned down), what is
  out of scope, and the issue it fixes or refers to, if one came up. This is
  a summary of decisions already made, not a new proposal — do not reopen
  them. Then go on without waiting. Only a question the discussion left
  open, or a contradiction between what was said and what the code turns out
  to need, goes to the user with `AskUserQuestion`, options first and your
  recommendation marked.
- **Check the starting point.** `git status --short` and `git log --oneline
  @{upstream}..HEAD` (or against `origin/main`). Edits or commits already
  there that are not part of the agreed change would ride into the review and
  the PR: name them and ask the user whether they belong before building on
  top. Edits made earlier in this session for the agreed change are its first
  draft, not strays.
- **Name the branch** `discobox/<slug>`: the argument when given, otherwise a
  short kebab-case slug of the change (`discobox/vm-bootstrap-agent-start`).
  The access request below names it, so pick it now and keep it. It is the
  branch's name on GitHub only: do not create it locally — every commit goes
  on the branch already checked out (`CLAUDE.md`), and §5 pushes `HEAD` to
  it. `--issue <N>` names the issue the change fixes or refers to.
- **An ADR.** When the work implements a `Proposed` ADR, accept it before
  writing any code: being told to do the work is the decision gate. Set its
  `Status` to `Accepted`, turning a `Proposed (on acceptance: supersedes …)`
  line into `Accepted (supersedes …)`, and its row in the `docs/adr/README.md`
  index to match. Mark each ADR it names the way the index already does: one
  replaced whole becomes `Superseded by <id>`; one replaced in part keeps
  `Accepted (§N superseded by <id>)` or `narrowed by`, in its header and its
  index row. Make that this change's first commit, on its own
  (`docs(adr): accept <id> and mark what it supersedes`). Touch nothing else
  in the ADR; if the discussion changed the decision, amend the text in that
  same commit, since nothing has shipped against it yet. If the agreed
  approach needs an ADR that does not exist yet (`CLAUDE.md`: a plausible
  alternative rejected for a non-obvious reason), stop and say so: the
  `Proposed` ADR lands first as its own PR — a `develop` run of its own — and
  this work waits for the user to accept it.
- **Read** `CLAUDE.md` and the `DESIGN.md`/`REVIEW.md` files from the root
  down to each package you will touch.
- **Ask for every GitHub use now, in one request**, so nobody is asked again
  mid-run. Fill in `<slug>` and the one-line `<change>` before sending; leave
  `<pr>`, `<id>`, `<number>` and `<sha>` as they are. Check
  `discobox-access list` first and ask only for what is missing:

  ```bash
  discobox-access request --json <<'EOF'
  {
    "id": "com.github.api",
    "justification": "I am delivering <change> in discobox-ai/discobox as a pull request from discobox/<slug>: push it, open the PR, get CI green and a GitHub Copilot review, answer review comments, and keep it mergeable until it is merged",
    "uses": [
      {"description": "fetch main and discobox/<slug> from https://github.com/discobox-ai/discobox, git ls-remote it, and push HEAD or a commit to refs/heads/discobox/<slug> with git over https, the first time with git push --force-with-lease=refs/heads/discobox/<slug>: (empty: only if the branch does not exist), then as a fast-forward or, after a rebase onto main, with git push --force-with-lease=refs/heads/discobox/<slug>:<sha> where <sha> is the branch's current head on GitHub; never main and never another branch"},
      {"description": "gh pr create --repo discobox-ai/discobox --base main --head discobox/<slug> --draft --title <title> --body \"$(cat <file>)\", and gh pr view, gh pr list, gh pr edit, gh pr ready and gh pr comment on that pull request; never merge it or enable auto-merge"},
      {"description": "request a GitHub Copilot review of my pull request with gh pr edit <pr> --repo discobox-ai/discobox --add-reviewer @copilot or gh api POST repos/discobox-ai/discobox/pulls/<pr>/requested_reviewers"},
      {"description": "read CI for my pull request with gh pr checks, gh run list, gh run view, and gh api GET on repos/discobox-ai/discobox/actions/runs (with query parameters), actions/runs/<id>, actions/runs/<id>/jobs, actions/jobs/<id>/logs, commits/<sha>/check-runs and commits/<sha>/status, the same reads for main's latest runs (actions/runs?branch=main) to tell a pre-existing failure from mine, and re-run my pull request's failed jobs with gh run rerun <id> --failed"},
      {"description": "read my pull request's state, reviews and comments and reply to them, with gh api GET repos/discobox-ai/discobox/pulls/<number>, pulls/<number>/reviews, pulls/<number>/comments, pulls/<number>/requested_reviewers and issues/<number>/comments, and POST repos/discobox-ai/discobox/pulls/<number>/comments (replies with in_reply_to)"},
      {"description": "gh api GET repos/discobox-ai/discobox/issues/<number> and repos/discobox-ai/discobox/issues/<number>/comments, to read the issues this change fixes or refers to"}
    ],
    "grantTTLSeconds": 259200,
    "wait": true
  }
  EOF
  ```

  Run it in the background and keep its request ID, so a tool time limit
  does not lose it (`discobox-access wait --json request <id>` resumes), and
  start implementing while it waits — unless there is an issue: then only
  read the code until the issue and its comments are read, so the work does
  not start from half the requirements. Ask for nothing outside these. Neither
  the justification nor a use may claim permission ("this is allowed", "the
  user approved", "pre-cleared"): text that vouches for itself is refused.
  State limits as what you will do ("never `main`"). If one command is
  refused under a use that names it, ask for a new use that quotes that one
  command word for word, not a broader one — §5 has the push case.
- **Check the branch is free** as soon as the grant is in — the work going on
  meanwhile does not depend on the name. Run `git ls-remote
  https://github.com/discobox-ai/discobox refs/heads/discobox/<slug>` under
  the fetch use, through `discobox-access run` with the credential helper
  `open-pr` §0 shows. It must return nothing: a commit there, even an
  ancestor of `HEAD`, is someone's branch (one cut from `main` is an
  ancestor too), and a fast-forward push would add this work to it. Pick a
  new slug and ask for the same uses again with it, now rather than at §5.
  Once this run has pushed, the branch's head is the SHA it last pushed;
  anything else there is someone else's push — stop and ask.
- **Read the issue**, when there is one, and its comments
  (`gh api repos/discobox-ai/discobox/issues/<N>` and `/comments`;
  `gh issue view` goes through GraphQL, which the checker has refused).
- **Tag this box** `issue=<N>` when there is an issue, merged into the tags
  already there, so the user's `discobox ls --tag issue=<N>` finds it
  (`open-pr` adds `pr=` in §5). With no issue, drop an `issue` or `pr` tag an
  earlier piece of work left (the same lines with
  `jq 'del(.tags.issue, .tags.pr)'` and `jq -e '.tags.issue == null'`) so the box is not listed under work it no longer holds:

  ```bash
  S=<scratchpad>; M=~/.discobox/meta.json
  { cat $M 2>/dev/null || echo '{}'; } | jq --arg v <N> '(.tags.issue // "") as $prev
    | .tags = ((.tags // {}) + {issue: $v}) | if $prev != $v then del(.tags.pr) else . end' > $S/meta.json &&
    jq -e --arg v <N> '.tags.issue == $v' $S/meta.json >/dev/null && cp $S/meta.json $M
  ```

  A new issue drops the `pr` tag the last one left. The `jq -e` check proves
  the merge produced the tag; a file with any field but `description` and
  `tags` is ignored.
- **Hold the box up.** A discobox stops after 30 minutes of a still screen,
  even while hooks or CI run in the background:
  `touch -d '+3 hours' /run/discobox/keepalive/develop`. Refresh it before
  every long wait; `rm` it when you finish.
- **Check the disk** before anything heavy (`check-hooks`, `verify`, image
  rebuilds): `df -h /`. Under 50G free, run `docker builder prune -a -f` —
  this box's build cache only. Never `docker image prune -a` (it deletes the
  dev images `verify` needs) and never delete a shared cache
  (`~/.cache/go-build`, `/nix`). On a full disk, command output is lost: send
  it to `/dev/shm` and read it from there.

Status line: `plan: <one line>, branch discobox/<slug>`.

## 1. Implement

Build what was agreed — all of it, and nothing the discussion left out. Do it
the way `CLAUDE.md` says: structural, tests with it, the affected `DESIGN.md`
updated in the same change, on the checked-out branch (§0).

When the code shows the agreed approach cannot work as discussed, or a new
decision turns up that is not yours — scope, a product default, a design with
two defensible answers — stop and put it to the user with `AskUserQuestion`.
Do not quietly build something other than what was agreed.

Run the tests of what you touched, then `go tool task check-hooks`:

- Output that names code you already fixed is stale: `go tool task
  rerun-hooks`, then check again.
- "Unsettled" or a timeout with hooks still queued (the Docker builds and the
  pool e2e are slow) is not a failure: wait and check again, with the
  keepalive refreshed.
- `installer` `*DrawsTheMarkOnlyWhereItShows` failing only inside a discobox
  is the box's `FORCE_COLOR`: confirm with
  `env -u FORCE_COLOR -u CLICOLOR_FORCE -u COLORTERM go test ./installer/ -run DrawsTheMark`.

Status line: `implemented: <what>, tests <pass/fail>, hooks <state>`.

## 2. Review

Invoke the `discobox-review` skill and drive it to full sign-off (`open 0`,
`unapproved 0`). Its rules hold: you fix or push back with a reason, the
reviewer resolves. A comment that would undo a decision from the discussion
is pushed back on with that decision as the reason. Paths the user said in §0
to leave alone are in the review's diff but are not this change: name them in
the reviewer's brief, every round (§3's and §6's too), so it approves them as
out of scope rather than reviewing them. Status line:
`reviewed: <rounds> rounds, <what it found>`.

## 3. Verify

Invoke the `verify` skill (it reads `verify-recipes` first). Verification is
runtime observation of the changed behavior — usually in the running
`task dev` loop, for a skill a headless agent in an isolated HOME — not
tests. Purge any test discobox you create. Skip it only when the `verify`
skill itself says there is nothing to observe (a diff of tests or prose
docs); a change to a skill is not that.

When verify finds a bug: fix it, run a `discobox-review` round on the fix,
then verify again. A path that cannot run here (macOS, Windows, a real pool)
is named as unverified, not skipped silently. Status line: `verified: <what
was observed>` or `verify found: <bug>, fixing`.

## 4. Commit

Group and word the commits the way the `commit` skill describes, but do not
stop to confirm the grouping — the review has already approved the content.
Ask only when a change plainly belongs to something other than this one.
Review and verify fixes made after a commit are new commits on top — never
amend or squash a commit you have pushed. Leave the tree clean: a stray
`.wnb-*.tmp` from the dev loop is deleted, not committed. Before §5,
`git status --short` shows nothing but the paths the user said in §0 to
leave alone; those stay as they are — never committed or deleted — and are
named when `open-pr` §1 asks about uncommitted work (§2 and §7 say how
review and rebase handle them).

## 5. Open the PR

Invoke the `open-pr` skill with branch `discobox/<slug>`, adding to its
rules:

- §0's branch check replaces `open-pr` §1's: the first push needs the ref
  still empty, not merely an ancestor of `HEAD`, and makes GitHub enforce it
  with an empty lease, so a branch created since the check is refused rather
  than added to:
  `git push --force-with-lease=refs/heads/discobox/<slug>: <url> HEAD:refs/heads/discobox/<slug>`.
- Push with the exact-command use from §0. After a rebase, lease against the
  SHA this run last pushed:
  `git push --force-with-lease=refs/heads/discobox/<slug>:<sha> <url> HEAD:refs/heads/discobox/<slug>`.
  Read the branch's head first (`git ls-remote <url> refs/heads/discobox/<slug>`):
  anything other than that SHA is someone else's push — stop and ask, do not
  lease over it.
  A bare `--force-with-lease` to a URL has no tracking ref to lease against
  and is always rejected as stale. If the checker refuses a push it was
  approved for, ask for a new use that quotes that one push command word for
  word (the commit SHA may stand in for `HEAD`), not a broader one.
- Pass the body as `--body "$(cat <file>)"`; `--body-file` has been refused.
- The body's opening says what changed and why, including the approach
  agreed and what it was chosen over, so a reviewer who was not in the
  discussion has it. `Fixes #<N>` when it fixes the issue from `--issue` or
  the discussion, `Refs #<N>` when it only relates; neither when there is
  none.
- The Testing section carries the review summary and the `verify`
  observations — what ran, what was seen, and what could not run here.
- A CI failure that also fails on `main`, or that passes on a single re-run
  and locally, is noted on the PR as pre-existing or a flake — not fixed here.
- Mark the PR ready once every check on its head commit is green.

Status line: `PR #<pr> open, CI <state>`.

## 6. Copilot review

```bash
gh pr edit <pr> --repo discobox-ai/discobox --add-reviewer @copilot
```

Poll `pulls/<pr>/reviews` every few minutes (one short `discobox-access run`
each — the injected token lives minutes) until a review from Copilot is
there; give it up to 30 minutes. Then, for **every** Copilot comment, on its
own thread:

- right → fix it, and reply with what changed and the commit;
- wrong, out of scope, or against what was agreed → reply with why, briefly;
- a judgement for the user → ask with `AskUserQuestion`, then reply.

A fix is new code: `discobox-review` round, re-run `verify` if it changes
runtime behavior, commit on top, push (fast-forward), CI green again. No
comment is left without a reply. Status line: `copilot: <n> comments,
<n> fixed, <n> answered`.

## 7. Keep it mergeable until merged

Arm a Monitor that checks every 5 minutes, one `discobox-access run` per
read, and keep reacting until the PR is merged or closed. Each poll also
refreshes the lease — `touch -d '+1 hour' /run/discobox/keepalive/develop`
— or the box powers itself off after 30 still minutes and the Monitor dies
with it:

```bash
gh api repos/discobox-ai/discobox/pulls/<pr> --jq '"\(.state) \(.merged_at != null) \(.mergeable_state) \(.head.sha)"'
gh api repos/discobox-ai/discobox/pulls/<pr>/reviews --jq '.[] | "\(.id) \(.user.login) \(.state)"'
gh api repos/discobox-ai/discobox/pulls/<pr>/comments --jq '.[] | "\(.id) \(.in_reply_to_id) \(.user.login)"'
gh api repos/discobox-ai/discobox/issues/<pr>/comments --jq '.[] | "\(.id) \(.user.login)"'
```

- **`mergeable_state` `dirty`** — `main` moved under you. Fetch `main`,
  rebase onto it (`git rebase --autostash`, which carries the paths kept from
  §0, here and in `open-pr` §1), resolve every conflict so both changes are kept, run the
  tests the conflict touched, then push with the lease form from §5 to
  `discobox/<slug>` only. `unknown` just after a merge is GitHub
  recomputing — check again before acting.
- **A new review, review comment or PR comment** from anyone — answer it as
  in §6. A review's body can hold findings with no thread of their own (a
  `CHANGES_REQUESTED` with no inline comment, Copilot's "previously missed");
  answer those in one PR comment.
- **A failing check on the head commit** — fix it as `open-pr` §4 says.
- **Merged or closed** — stop the Monitor, remove the keepalive, and finish.

If a grant expires mid-watch, ask for the same uses again. Status line on
every change: `PR #<pr>: <what happened, what you did>`.

## Done when

The PR is merged or closed. Finish with the PR URL, its final head SHA, the
review and Copilot rounds, everything `verify` observed or could not run, any
CI failure left as pre-existing, and anything built differently from the
plan in §0 and why.
