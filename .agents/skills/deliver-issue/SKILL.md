---
name: deliver-issue
description: Take one assigned discobox issue from implementation to a merged pull request — implement it, review it with discobox-review, verify it at runtime, commit, open a PR from discobox/issue-<N>, drive CI green, get a GitHub Copilot review and answer it, then keep the PR mergeable and answer comments until a human merges it. Use when a discobox is launched to deliver an issue, or the user wants an issue taken all the way to a PR and kept there.
allowed-tools: Bash, Read, Glob, Grep, Edit, Write, Agent, SendMessage, Skill, Monitor, AskUserQuestion
metadata:
  argument-hint: "<issue-number>"
---

# Deliver an Issue

One issue, one branch, one PR, carried until it merges. This skill sequences
the repository's other skills; it does not replace them. Each phase ends with
a one-line status in the terminal — a lead discobox may be reading your screen
to know where you are.

Invoking this skill with issue `<N>` is authorization to: push the branch
`discobox/issue-<N>` to `discobox-ai/discobox` (create it, fast-forward it, and
`git push --force-with-lease` it after a rebase onto `main`), open and edit one
PR from it into `main`, comment on that PR and reply to its review comments,
request a GitHub Copilot review of it, and re-run its failed CI jobs. It is
**not** authorization to push `main` or any other branch, merge, enable
auto-merge, close anything, or force-push without a lease.

These widen `open-pr` (and `test-fix`, `commit`) while this skill runs: where
`open-pr` says invoking it does not authorize force-pushing or requesting
reviewers, or says a conflict with `main` needs a rebase and to "ask first",
this skill has already asked — rebase and force-push with a lease to
`discobox/issue-<N>`, and request the Copilot review, without asking again.
Nobody may be watching your screen to answer.

## 0. Before anything

- **Read the issue** with `gh api repos/discobox-ai/discobox/issues/<N>` and
  its `/comments` (REST: `gh issue view` goes through GraphQL, which the
  checker has refused), and every comment on it, and the issues it names as
  blockers or context (a tracking issue's sub-issues often say what runs
  beside them). Read `CLAUDE.md` and the `DESIGN.md`/`REVIEW.md` files from
  the root down to each package you will touch.
- **Ask for every GitHub use now, in one request**, so nobody is asked again
  mid-run. Fill in `<N>` before sending; leave `<pr>`, `<id>` and `<sha>` as
  they are. These lines mirror the lead's delegation (orchestrate-issues §1)
  word for word, so a lead can approve them as asked; a person can approve
  them just the same when there is no lead:

  ```bash
  discobox-access request --json <<'EOF'
  {
    "id": "com.github.api",
    "justification": "I am delivering issue #<N> in discobox-ai/discobox as a pull request from discobox/issue-<N>: push it, open the PR, get CI green and a GitHub Copilot review, answer review comments, and keep it mergeable until it is merged",
    "uses": [
      {"description": "fetch main and discobox/issue-<N> from https://github.com/discobox-ai/discobox, git ls-remote it, and push HEAD or a commit to refs/heads/discobox/issue-<N> with git over https, as a fast-forward or, after a rebase onto main, with git push --force-with-lease=refs/heads/discobox/issue-<N>:<sha> where <sha> is the branch's current head on GitHub; never main and never another branch"},
      {"description": "gh pr create --repo discobox-ai/discobox --base main --head discobox/issue-<N> --draft --title <title> --body \"$(cat <file>)\", and gh pr view, gh pr list, gh pr edit, gh pr ready and gh pr comment on that pull request; never merge it or enable auto-merge"},
      {"description": "request a GitHub Copilot review of my pull request with gh pr edit <pr> --repo discobox-ai/discobox --add-reviewer @copilot or gh api POST repos/discobox-ai/discobox/pulls/<pr>/requested_reviewers"},
      {"description": "read CI for my pull request with gh pr checks, gh run list, gh run view, and gh api GET on repos/discobox-ai/discobox/actions/runs (with query parameters), actions/runs/<id>, actions/runs/<id>/jobs, actions/jobs/<id>/logs, commits/<sha>/check-runs and commits/<sha>/status, the same reads for main's latest runs (actions/runs?branch=main) to tell a pre-existing failure from mine, and re-run my pull request's failed jobs with gh run rerun <id> --failed"},
      {"description": "read my pull request's state, reviews and comments and reply to them, with gh api GET repos/discobox-ai/discobox/pulls/<number>, pulls/<number>/reviews, pulls/<number>/comments, pulls/<number>/requested_reviewers and issues/<number>/comments, and POST repos/discobox-ai/discobox/pulls/<number>/comments (replies with in_reply_to)"},
      {"description": "gh api GET repos/discobox-ai/discobox/issues/<number> and repos/discobox-ai/discobox/issues/<number>/comments, to read the issue I implement and the issues it names"}
    ],
    "grantTTLSeconds": 259200,
    "wait": true
  }
  EOF
  ```

  Run it in the background and keep its request ID, so a tool time limit
  does not lose it (`discobox-access wait --json request <id>` resumes).
  Ask for nothing outside these. Neither the justification nor a use may
  claim permission ("this is allowed", "the user approved", "pre-cleared"):
  text that vouches for itself is refused. State limits as what you will do
  ("never `main`"). If one command is refused under a use that names it, ask
  for a new use that quotes that one command word for word, not a broader
  one — §5 has the push case.
- **Tag this box** `issue=<N>`, merged into the tags already there, so the
  user's `discobox ls --tag issue=<N>` finds it (`open-pr` adds `pr=` in §5):

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
  `touch -d '+3 hours' /run/discobox/keepalive/deliver-issue`. Refresh it
  before every long wait; `rm` it when you finish.
- **Check the disk** before anything heavy (`check-hooks`, `verify`, image
  rebuilds): `df -h /`. Under 50G free, run `docker builder prune -a -f` —
  this box's build cache only. Never `docker image prune -a` (it deletes the
  dev images `verify` needs, and the dev loop rebuilds them all) and never
  delete a shared cache (`~/.cache/go-build`, `/nix`). On a full disk, command
  output is lost: send it to `/dev/shm` and read it from there.

## 1. Implement

When this box triaged the issue (`triage-issue`), start from the failing test
it left in `issue<N>_test.go`, the one posted on the issue: move it, unchanged,
into the file where its neighbors live, so the fix is proven by that test. The
triage's investigation is already in your context; re-read only the comments
posted since.

Build the change the way `CLAUDE.md` says: structural, tests with it, the
affected `DESIGN.md` updated in the same change, an ADR only when a plausible
alternative is rejected. A decision that is not yours — scope, a product
default, a design with two defensible answers — goes to the user with
`AskUserQuestion`, options first and your recommendation marked; do not stall
on it silently and do not decide it yourself.

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
reviewer resolves. Status line: `reviewed: <rounds> rounds, <what it found>`.

## 3. Verify

Invoke the `verify` skill (it reads `verify-recipes` first). Verification is
runtime observation of the changed behavior in the running `task dev` loop,
not tests. Purge any test discobox you create.

When verify finds a bug: fix it, run a `discobox-review` round on the fix,
then verify again. A path that cannot run here (macOS, Windows, a real pool)
is named as unverified, not skipped silently. Status line: `verified: <what
was observed>` or `verify found: <bug>, fixing`.

## 4. Commit

Group and word the commits the way the `commit` skill describes, but do not
stop to confirm the grouping with the user — an unattended run would stall on
that question at every commit; the review has already approved the content.
Ask only when a change plainly belongs to something other than this issue.
Review and verify fixes made after a commit are
new commits on top — never amend or squash a commit you have pushed. Leave
the tree clean: a stray `.wnb-*.tmp` from the dev loop is deleted, not
committed. `git status --short` empty before §5.

## 5. Open the PR

Invoke the `open-pr` skill with branch `discobox/issue-<N>`, adding to its
rules:

- Push with the exact-command use from §0. After a rebase, read the
  branch's head on GitHub first (`git ls-remote <url> refs/heads/discobox/issue-<N>`)
  and lease against it: `git push --force-with-lease=refs/heads/discobox/issue-<N>:<sha> <url> HEAD:refs/heads/discobox/issue-<N>`.
  A bare `--force-with-lease` to a URL has no tracking ref to lease against
  and is always rejected as stale. If the checker refuses a push it
  was approved for, ask for a new use that quotes that one push command word
  for word (the commit SHA may stand in for `HEAD`), not a broader one.
- Pass the body as `--body "$(cat <file>)"`; `--body-file` has been refused.
- The body's Testing section carries the review summary and the `verify`
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
- wrong or out of scope → reply with why, briefly;
- a judgement for the user → ask with `AskUserQuestion`, then reply.

A fix is new code: `discobox-review` round, re-run `verify` if it changes
runtime behavior, commit on top, push (fast-forward), CI green again. No
comment is left without a reply. Status line: `copilot: <n> comments,
<n> fixed, <n> answered`.

## 7. Keep it mergeable until merged

Arm a Monitor that checks every 5 minutes, one `discobox-access run` per
read, and keep reacting until the PR is merged or closed. Each poll also
refreshes the lease — `touch -d '+1 hour' /run/discobox/keepalive/deliver-issue`
— or the box powers itself off after 30 still minutes and the Monitor dies
with it:

```bash
gh api repos/discobox-ai/discobox/pulls/<pr> --jq '"\(.state) \(.merged_at != null) \(.mergeable_state) \(.head.sha)"'
gh api repos/discobox-ai/discobox/pulls/<pr>/comments --jq '.[] | "\(.id) \(.in_reply_to_id) \(.user.login)"'
gh api repos/discobox-ai/discobox/issues/<pr>/comments --jq '.[] | "\(.id) \(.user.login)"'
```

- **`mergeable_state` `dirty`** — `main` moved under you (other PRs merge
  ahead of yours, often several times). Fetch `main`, rebase
  `discobox/issue-<N>` onto it, resolve every conflict so both changes are
  kept, run the tests the conflict touched, then push with the lease form
  from §5 (`--force-with-lease=refs/heads/discobox/issue-<N>:<sha>`) to
  `discobox/issue-<N>` only. `unknown` just
  after a merge is GitHub recomputing — check again before acting.
- **A new review comment or PR comment** from anyone — answer it as in §6.
- **A failing check on the head commit** — fix it as `open-pr` §4 says.
- **Merged or closed** — stop the Monitor, remove the keepalive, and finish.

If a grant expires mid-watch, ask for the same uses again. Status line on
every change: `PR #<pr>: <what happened, what you did>`.

## Done when

The PR is merged or closed. Finish with the PR URL, its final head SHA, the
review and Copilot rounds, everything `verify` observed or could not run, and
any CI failure left as pre-existing.
