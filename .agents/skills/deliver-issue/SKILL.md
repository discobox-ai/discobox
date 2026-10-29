---
name: deliver-issue
description: Take one assigned discobox issue from implementation to a merged pull request — implement it, review it with discobox-review, verify it at runtime, commit, open a PR from discobox/issue-<N>, drive CI green, then keep the PR mergeable and answer comments until a human merges it. Use when a discobox is launched to deliver an issue, or the user wants an issue taken all the way to a PR and kept there.
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
and re-run its failed CI jobs. It is **not** authorization to push `main` or
any other branch, merge, enable auto-merge, close anything, or force-push
without a lease.

These widen `open-pr` (and `test-fix`, `commit`) while this skill runs: where
`open-pr` says invoking it does not authorize force-pushing, or says a
conflict with `main` needs a rebase and to "ask first",
this skill has already asked — rebase and force-push with a lease to
`discobox/issue-<N>` without asking again.
Nobody may be watching your screen to answer.

## 0. Before anything

- **Read the issue** with `gh api repos/discobox-ai/discobox/issues/<N>` and
  its `/comments`, and every comment on it, and the issues it names as
  blockers or context (a tracking issue's sub-issues often say what runs
  beside them). Read `CLAUDE.md` and the `DESIGN.md`/`REVIEW.md` files from
  the root down to each package you will touch.
- **Ask for GitHub now, in one request**, so nobody is asked again mid-run,
  unless `discobox-access list` already shows a credential in `GH_TOKEN`
  sent to `github.com` — a triage ask (triage-issue §0) reaches only
  `api.github.com`, so a box that triaged this issue asks again, and the
  approval replaces that binding's routes. Fill in `<N>` before sending, and
  change nothing else: its routes are the ones the lead's delegation holds
  (orchestrate-issues §1), and the lead approves only this exact ask, so a
  lead approves it as asked, and a person can approve it just the same when
  there is no lead:

  ```bash
  discobox-access request --json <<'EOF'
  {
    "name": "github",
    "envVar": "GH_TOKEN",
    "routes": [
      "github.com/discobox-ai/discobox.git/**,/discobox-ai/discobox/**",
      "GET api.github.com/repos/discobox-ai/discobox/**",
      "api.github.com/graphql graphql=query",
      "api.github.com/graphql graphql=mutation:markPullRequestReadyForReview",
      "POST api.github.com/repos/discobox-ai/discobox/pulls",
      "PATCH api.github.com/repos/discobox-ai/discobox/pulls/*",
      "POST api.github.com/repos/discobox-ai/discobox/pulls/*/comments/*/replies",
      "POST api.github.com/repos/discobox-ai/discobox/issues/*/comments",
      "POST api.github.com/repos/discobox-ai/discobox/actions/runs/*/rerun-failed-jobs"
    ],
    "justification": "I am delivering issue #<N> in discobox-ai/discobox as a pull request from discobox/issue-<N>: push that branch, open the PR into main, get CI green, answer review comments, and keep it mergeable until a human merges it. I will not push any other branch, force-push without a lease, merge, close anything, or touch labels, issues, workflows or secrets.",
    "grantTTLSeconds": 259200,
    "wait": true
  }
  EOF
  ```

  What is approved is these routes, and the egress holds every request
  carrying the token to them. They stop merging (there is no merge route,
  REST or GraphQL), deleting a branch or ref through the API, dispatching a
  workflow, any host but `github.com` and `api.github.com`, and any REST or git
  write to another repository. They do not stop, and the approver should read
  it so: the GraphQL queries read every repository the token can see;
  `markPullRequestReadyForReview` works on any pull request the token can
  write, in any repository; the REST `PATCH` of a pull request can change its
  title, base or state — close it — as well as its body, since a route cannot
  read a REST body; the comment and reply routes reach every issue and pull
  request of this repository; and a git push writes whichever branch it names,
  a force-push or a delete included. Those are this skill's to keep — push
  `discobox/issue-<N>` and nothing else, edit only this PR's body — and `main`'s branch
  protection covers a push to `main`. A step refused with a `route:` reason
  (`discobox-access audit --blocked`) is outside what was approved: stop and
  say so rather than working around it. Keep
  to what the justification says. Run it
  in the background and keep its request ID, so a tool time limit does not
  lose it (`discobox-access wait --json request <id>` resumes). Ask for
  nothing outside it. The justification may not claim permission ("this is
  allowed", "the user approved", "pre-cleared"): text that vouches for itself
  is denied. Once granted, `GH_TOKEN` holds the credential's sentinel in
  every command started after it was granted; a shell you already had takes it up
  with `eval "$(discobox-access env)"`. `gh` reads it as it is, and git takes
  it through the credential helper `open-pr` §0 shows.
- **Tag this box** `issue=<N>`, merged into the tags already there, so the
  user's `discobox ls --tag issue=<N>` finds it (`open-pr` adds `pr=` in §5):

  ```bash
  S=<scratchpad>; M=~/.discobox/meta.json
  { cat $M 2>/dev/null || echo '{}'; } | jq --arg v <N> '(.tags.issue // "") as $prev
    | .tags = ((.tags // {}) + {issue: $v} | del(.ready, .blocked, .merged)) | if $prev != $v then del(.tags.pr) else . end' > $S/meta.json &&
    jq -e --arg v <N> '.tags.issue == $v and (.tags | length) <= 64' $S/meta.json >/dev/null && cp $S/meta.json $M
  ```

  A new issue drops the `pr` tag the last one left, and a fresh start drops a
`ready`, `blocked` or `merged` tag (§6) the last run left. The `jq -e` check proves
  the merge produced the tag and at most 64 tags (more makes the whole file
  invalid); when it fails nothing is written: with more than 64 tags merged,
  tell the user the box's tags are full and drop none of theirs to make
  room; otherwise (the file empty or not valid JSON) say the merge failed
  and leave the file alone. A file with any field but `description` and
  `tags` is ignored too.
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
  `env -u FORCE_COLOR -u CLICOLOR_FORCE -u COLORTERM go test ./pkg/installer/ -run DrawsTheMark`.

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

- Push under the §0 credential. After a rebase, read the
  branch's head on GitHub first (`git ls-remote <url> refs/heads/discobox/issue-<N>`)
  and lease against it: `git push --force-with-lease=refs/heads/discobox/issue-<N>:<sha> <url> HEAD:refs/heads/discobox/issue-<N>`.
  A bare `--force-with-lease` to a URL has no tracking ref to lease against
  and is always rejected as stale.
- The body's Testing section carries the review summary and the `verify`
  observations — what ran, what was seen, and what could not run here.
- A CI failure that also fails on `main`, or that passes on a single re-run
  and locally, is noted on the PR as pre-existing or a flake — not fixed here.
- Mark the PR ready once every check on its head commit is green.

Status line: `PR #<pr> open, CI <state>`.

## 6. Keep it mergeable until merged

Arm a Monitor that checks every 5 minutes, and keep reacting until the PR is merged or closed. Each poll also
refreshes the lease — `touch -d '+1 hour' /run/discobox/keepalive/deliver-issue`
— or the box powers itself off after 30 still minutes and the Monitor dies
with it:

```bash
gh api repos/discobox-ai/discobox/pulls/<pr> --jq '"\(.state) \(.merged_at != null) \(.mergeable_state) \(.head.sha)"'
gh api repos/discobox-ai/discobox/pulls/<pr>/reviews --jq '.[] | "\(.id) \(.user.login) \(.state)"'
gh api repos/discobox-ai/discobox/pulls/<pr>/comments --jq '.[] | "\(.id) \(.in_reply_to_id) \(.user.login)"'
gh api repos/discobox-ai/discobox/issues/<pr>/comments --jq '.[] | "\(.id) \(.user.login)"'
gh pr checks <pr> --repo discobox-ai/discobox --json name,bucket
```

- **`mergeable_state` `dirty`** — `main` moved under you (other PRs merge
  ahead of yours, often several times). Fetch `main`, rebase
  `discobox/issue-<N>` onto it, resolve every conflict so both changes are
  kept, run the tests the conflict touched, then push with the lease form
  from §5 (`--force-with-lease=refs/heads/discobox/issue-<N>:<sha>`) to
  `discobox/issue-<N>` only. `unknown` just
  after a merge is GitHub recomputing — check again before acting.
- **A new review, review comment or PR comment** from anyone — read a
  review's body as well as its threads, and answer every finding, each thread
  on that thread (`gh api -X POST
  repos/discobox-ai/discobox/pulls/<pr>/comments/<comment-id>/replies -f
  body=...`). A review's body can hold findings with no thread of their
  own (a `CHANGES_REQUESTED` with no inline comment); answer those together in
  one PR comment (`gh api -X POST
  repos/discobox-ai/discobox/issues/<pr>/comments -F body=@<file>`):
  - right → fix it, and reply with what changed and the commit;
  - wrong or out of scope → reply with why, briefly;
  - a finding already answered, raised again → reply with a link to that
    answer;
  - a judgement for the user → ask with `AskUserQuestion`, then reply.

  A fix is new code: `discobox-review` round, re-run `verify` if it changes
  runtime behavior, commit on top, push (fast-forward), CI green again.
- **A failing check on the head commit** — fix it as `open-pr` §4 says.
- **Merged or closed** — tag the box `merged` or drop its tag (below), stop
  the Monitor, remove the keepalive, and finish.

If the credential's binding lapses mid-watch, ask again with the same request. Status line on
every change: `PR #<pr>: <what happened, what you did>`.

### Ready, blocked, merged

Three plain tags on the box tell the user where its PR stands: `ready`,
`blocked` and `merged`, each with an empty value. The console shows them as
`#ready`, `#blocked` and `#merged`, and `discobox ls --tag ready` lists
them. Set at most one of them, and move it each time the state
changes, from §0 until the end:

- **`ready`**: every check on the head commit is green, every
  review, thread and comment has a reply, `mergeable_state` is neither `dirty`
  nor `unknown` (still recomputing: check again first), and
  the PR is marked ready. All it needs is a human merge.
- **`blocked`**: you cannot get there without someone else. That covers a
  question waiting on the user, a credential request refused or still waiting for an
  answer when the next step needs it, five failed CI rounds (`open-pr` §4), or a conflict you cannot
  resolve while keeping both changes. Say why in the status line. GitHub's
  own `mergeable_state` `blocked` (a required review not yet given) is not
  this tag: that PR is waiting for its human, so it can still be `ready`.
- **neither**: you are working. When a `ready` PR goes `dirty`, gets a new
  review or comment, or has a check on its head commit leave `pass` or `skipping`
  (re-run, failed or cancelled), drop `ready` before you start on it.
  Drop `blocked` as soon as the answer comes.
- **`merged`**: the PR merged. The box's work is done.
- **closed without merging**: drop all three.

Set `<s>` to `ready`, `blocked` or `merged`, or leave it empty to drop all three:

```bash
S=<scratchpad>; M=~/.discobox/meta.json
{ cat $M 2>/dev/null || echo '{}'; } | jq --arg s '<s>' '.tags = ((.tags // {}) | del(.ready, .blocked, .merged))
  | if $s != "" then .tags[$s] = "" else . end' > $S/meta.json &&
  jq -e --arg s '<s>' '($s == "" or .tags[$s] == "") and (.tags | length) <= 64' $S/meta.json >/dev/null && cp $S/meta.json $M
```

If the check fails, nothing is written, and the rules for §0's tag merge apply.

## Done when

The PR is merged or closed. Finish with the PR URL, its final head SHA, the
review rounds, everything `verify` observed or could not run, and
any CI failure left as pre-existing.
