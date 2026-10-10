---
name: orchestrate-issues
description: Run a set of discobox issues as a lead discobox — launch one worker discobox per issue with the deliver-issue skill, or, given `triage`, keep a fixed pool of 4–6 boxes triaging untriaged issues one after another and move the ready ones straight into delivery, hand them credentials within a delegation, watch their screens and answer what they ask, keep the host's disk from filling, track their pull requests while a human merges them, and have conflicted ones rebase. Use when the user wants several issues (a tracking issue's sub-issues, a wave of them) implemented in parallel discoboxes and delivered as PRs, or wants the open untriaged issues triaged.
allowed-tools: Bash, Read, Write, Edit, Monitor, AskUserQuestion, Skill
metadata:
  argument-hint: "<issue numbers | a tracking issue to take the next unblocked wave from | triage [pool size 4–6]>"
---

# Orchestrate Issues

You are the lead. Workers do the engineering, each under `deliver-issue`; you
start them, unblock them, and keep a human's to-do list. You never apply a
worker's commits and never merge: work lands as PRs a human merges.

Two modes. Given issues, each gets a worker that delivers it (§2–§5). Given
`triage`, a fixed pool of boxes triages the open untriaged issues one after
another, and a triaged issue ready for work goes straight into delivery in the
box that triaged it (§6). §1 is the same for both.

`orchestrate.sh` beside this file is the loop: `status`, `screen`, `say`,
`power`, `retire`, `approve`, `kick`, `idle`, `github`, `prs`, `rebase`,
`after-merge`, `watch`, and for triage `untriaged`, `pool`, `triage`,
`deliver`. It keeps its state in `~/.local/state/orchestrate` (a scratchpad can
be wiped between sessions) and finds use IDs by their descriptions, so renewed
grants just work.
Copy it there and run it with `bash` (`/dev/shm` is mounted noexec).

## 1. Ask for everything up front

Three requests, each once, before the first worker. Word every use as the
literal command — a checker compares commands against these sentences — and
never write a sentence that vouches for itself ("allowed", "approved", "the
user said"): text that claims permission is refused. Ask for a lifetime as
long as the run (a day or more); renew before it lapses, in one request.

**The discobox API** — everything the lead does to its workers:

```bash
discobox-access request --json <<'EOF'
{
  "id": "ai.discobox.sandbox",
  "justification": "the user asked me to deliver issues <list> (or: to triage the open untriaged issues) through worker discoboxes I create; I launch them with no credentials, watch and answer them, start and stop them, tag the finished ones, and answer their credential requests",
  "uses": [
    {"description": "discobox new -d -C https://github.com/discobox-ai/discobox[@ref] -p <any prompt>: create a discobox with any prompt and no grants or secrets, cloned from the GitHub repository discobox-ai/discobox, including the polling discobox new makes for the discobox it just created"},
    {"description": "discobox admin box ls and discobox admin box get <discobox-id>, to watch the discoboxes I created"},
    {"description": "discobox start <discobox-id>, discobox stop <discobox-id> and discobox restart <discobox-id> (or discobox admin box start|stop|restart <discobox-id>): start, stop or restart a discobox I created"},
    {"description": "discobox tag <discobox-id> to-delete --rm ready --rm blocked, or discobox tag <discobox-id> to-delete merged --rm ready --rm blocked when its pull request merged: tag a finished discobox I created, so its user can find it with discobox ls --all --tag to-delete"},
    {"description": "discobox admin terminal ls --discobox-id <discobox-id>, discobox admin terminal screen <terminal-id> --discobox-id <discobox-id> [--scrollback N], and discobox admin terminal wait <terminal-id> --discobox-id <discobox-id> [flags]: read what a discobox I created shows in its terminals"},
    {"description": "discobox admin terminal input <terminal-id> --discobox-id <discobox-id> [--literal] <keys or text>: type keys and messages into the terminal of a discobox I created, to answer its questions, give it its next step, tell it to continue, or ask it to free its own Docker build cache"},
    {"description": "discobox secret request ls, to see what the discoboxes I created are asking for"},
    {"description": "discobox secret ls, to see the secrets I was delegated"},
    {"description": "discobox secret request approve <request-id> [--secret-id <secret-id>] [--use <use>]: approve a pending credential request (the server lets me answer only my own discoboxes' requests, within the delegation grants I hold)"},
    {"description": "discobox secret request deny <request-id>: deny a pending credential request"}
  ],
  "grantTTLSeconds": 172800,
  "wait": true
}
EOF
```

**GitHub, for the lead itself** — reading PRs and filing issues:

```bash
discobox-access request --json <<'EOF'
{
  "id": "com.github.api",
  "justification": "I track the pull requests my worker discoboxes open in discobox-ai/discobox and main's CI, find the untriaged issues I hand them, and file the issues I hand them",
  "uses": [
    {"description": "gh api GET repos/discobox-ai/discobox/pulls (with query parameters), repos/discobox-ai/discobox/pulls/<number>, repos/discobox-ai/discobox/pulls/<number>/files, repos/discobox-ai/discobox/branches (with query parameters): list and read pull requests and branches"},
    {"description": "gh api GET repos/discobox-ai/discobox/pulls/<number>/reviews and repos/discobox-ai/discobox/pulls/<number>/comments: read the reviews and review comments on pull requests"},
    {"description": "gh api GET repos/discobox-ai/discobox/issues/<number> and repos/discobox-ai/discobox/issues/<number>/comments: read issues"},
    {"description": "gh api GET repos/discobox-ai/discobox/issues (with query parameters): list open issues, to find the untriaged ones"},
    {"description": "gh api POST repos/discobox-ai/discobox/issues with a title and body, and POST repos/discobox-ai/discobox/issues/<number>/comments with a body: file an issue for work I hand to a discobox, and comment on an issue"},
    {"description": "gh api GET repos/discobox-ai/discobox/actions/runs (with query parameters such as branch=main), actions/runs/<id>, actions/runs/<id>/jobs and actions/jobs/<id>/logs: read CI runs, their jobs and job logs"},
    {"description": "gh api GET repos/discobox-ai/discobox/commits/<sha>/check-runs and commits/<sha>/status (with query parameters): read the checks on a commit"}
  ],
  "grantTTLSeconds": 604800,
  "wait": true
}
EOF
```

**GitHub, to delegate** — what workers may be given. Ask for **one**
delegation that holds every worker use, not one per need: an approval is
judged against a delegation you hold, and splitting the uses across several
has had approvals judged against the wrong one. Its lines are the workers'
requests (deliver-issue §0, then triage-issue §0) with "for the discoboxes I
create" in front, so a worker's uses fall inside it word for word. Ask for the
triage lines in both modes: one delegation serves whichever you run next.

```bash
discobox-access request --json <<'EOF'
{
  "id": "com.github.api",
  "purpose": "delegate",
  "justification": "each worker discobox I create triages issues in discobox-ai/discobox one at a time and labels and comments on them, or delivers one issue as a pull request into discobox-ai/discobox, gets a GitHub Copilot review and answers it, and keeps the PR mergeable until a human merges it. Needs contents:write, pull-requests:write and actions:write (and workflow scope if a change touches .github/workflows).",
  "uses": [
    {"description": "for the discoboxes I create, each on its own issue n: fetch main and discobox/issue-<n> from https://github.com/discobox-ai/discobox, git ls-remote it, and push HEAD or a commit to refs/heads/discobox/issue-<n> with git over https, as a fast-forward or, after a rebase onto main, with git push --force-with-lease=refs/heads/discobox/issue-<n>:<sha> where <sha> is the branch's current head on GitHub; never main and never another branch"},
    {"description": "for the discoboxes I create: gh pr create --repo discobox-ai/discobox --base main --head discobox/issue-<n> --draft --title <title> --body \"$(cat <file>)\", and gh pr view, gh pr list, gh pr edit, gh pr ready and gh pr comment on that pull request; never merge it or enable auto-merge"},
    {"description": "for the discoboxes I create: request a GitHub Copilot review of their own pull request with gh pr edit <pr> --repo discobox-ai/discobox --add-reviewer @copilot or gh api POST repos/discobox-ai/discobox/pulls/<pr>/requested_reviewers"},
    {"description": "for the discoboxes I create: read CI for their pull request with gh pr checks, gh run list, gh run view, and gh api GET on repos/discobox-ai/discobox/actions/runs (with query parameters), actions/runs/<id>, actions/runs/<id>/jobs, actions/jobs/<id>/logs, commits/<sha>/check-runs and commits/<sha>/status, the same reads for main's latest runs (actions/runs?branch=main) to tell a pre-existing failure from theirs, and re-run their pull request's failed jobs with gh run rerun <id> --failed"},
    {"description": "for the discoboxes I create: read their pull request's state, reviews and comments and reply to them, with gh api GET repos/discobox-ai/discobox/pulls/<number>, pulls/<number>/reviews, pulls/<number>/comments, pulls/<number>/requested_reviewers and issues/<number>/comments, and POST repos/discobox-ai/discobox/pulls/<number>/comments (replies with in_reply_to)"},
    {"description": "for the discoboxes I create: gh api GET repos/discobox-ai/discobox/issues/<number> and repos/discobox-ai/discobox/issues/<number>/comments, to read the issue they implement and the issues it names"},
    {"description": "for the discoboxes I create: gh api GET repos/discobox-ai/discobox/issues (with query parameters), repos/discobox-ai/discobox/issues/<number>, repos/discobox-ai/discobox/issues/<number>/comments, repos/discobox-ai/discobox/labels (with query parameters), search/issues (with query parameters), repos/discobox-ai/discobox/git/ref/tags/<tag> and repos/discobox-ai/discobox/git/tags/<sha>: read issues, their comments and labels, search for duplicates, and resolve a release tag to its commit"},
    {"description": "for the discoboxes I create: gh api POST repos/discobox-ai/discobox/issues/<number>/labels -f labels[]=<label> (one or more) and gh api -X DELETE repos/discobox-ai/discobox/issues/<number>/labels/<label>: add and remove the triage-issue skill's labels on an issue they are triaging or delivering; and gh api POST repos/discobox-ai/discobox/labels -f name=<label> -f color=<hex> -f description=<text>: recreate a missing label from that skill's table"},
    {"description": "for the discoboxes I create: gh api POST repos/discobox-ai/discobox/issues/<number>/comments -f body=\"$(cat <file>)\" and gh api -X PATCH repos/discobox-ai/discobox/issues/comments/<id> -f body=\"$(cat <file>)\": post and correct the triage comment and short follow-ups on an issue they are triaging or delivering"},
    {"description": "for the discoboxes I create: git fetch https://github.com/discobox-ai/discobox main and git ls-remote https://github.com/discobox-ai/discobox, with git over https, to start each triage from GitHub's main; never push"}
  ],
  "grantTTLSeconds": 604800,
  "wait": true
}
EOF
```

A request that waits is held by a tool time limit: run it in the background
and keep its request ID (do not filter the `progress` line out), so
`discobox-access wait --json request <id>` can resume it.

## 2. Plan the batch

- Take only issues with no open blockers; a tracking issue's waves say which.
- Each issue needs its own number — the worker's branch is `discobox/issue-<N>`
  and the delegation is written around it. Work with no issue gets one filed
  first (`gh api POST …/issues`), with everything the worker needs in it: what
  was seen, the cause with file and function names, and what a fix must keep.
- **Run at most four workers at once** (in triage mode, the pool size counts
  every box, triaging or delivering — §6). Every worker builds Docker images
  in its own cache (tens of GB each, never shared) and the host disk is one
  device for the whole pool. More than four has filled it. The user may raise
  the cap when disk holds up; six ran with 300G+ free once each worker kept to
  one Docker build at a time and the lead's own image watcher was off.

## 3. Launch

```bash
discobox-access run --use <create-use> -- discobox new -d \
  -C https://github.com/discobox-ai/discobox@main \
  -p "Deliver issue #<N> in discobox-ai/discobox." </dev/null
```

The prompt states the task and nothing else: no credentials, no skill names
(the in-box guidance says not to, and `deliver-issue` is chosen from its own
description), no claims of what is allowed. Clone from GitHub (`-C`), not
from this checkout, so workers start from `main`. Record each worker:
`printf '<N>\t<id>\t-\n' >> ~/.local/state/orchestrate/workers.tsv`, and fill
in the PR number when it opens.

## 4. Drive

Run `bash orchestrate.sh watch 30` in the background, and react to what it
exits on:

- **`IDLE #n`** — the worker's title has shown ✳ for two passes, with no
  background shell or monitor of its own running, or with a question dialog
  open. Its screen is printed with it. Clear `Login expired` and other
  mechanical prompts yourself; anything that needs the user goes on the table
  (§5) as "waiting for you".
- **`RESUMED #n`** — a worker you reported idle is working again: someone
  answered it in the box. Re-post the table.
- **`GITHUB:`** — an issue or PR you track changed on GitHub: a worker's PR
  opened, merged or closed (one PR list call every 15s), an issue closed or
  reopened, or `main`'s latest CI run started or finished (read once a pass).
  A red `main` goes at the top of the table. The baseline is the last state
  reported (`github.last`), so nothing that changed between watches is
  missed. The tracked set is every worker's issue and PR, plus the issues in
  `watch-issues.txt`; add the ones you file or are told about. The user never has to tell you about a merge or a close. On a merge,
  run `after-merge`: it retires each newly merged PR's worker (below), then has
  every PR that went `dirty` rebase.

- **A request** — `approve` answers each worker's GitHub request once, as
  asked; the server fits it to your delegation. A refusal or a non-GitHub
  request comes back to you: read its uses, approve or deny, and when a
  worker's use reaches past the delegation, ask the user rather than widening.
- **A question on a worker's screen** — read it with `screen <id> 40 120`.
  Answer what you can settle from the code or the user's earlier decisions.
  Put product, scope, and design choices on the table as "waiting for you"
  with the box ID and the topic. The user answers in the box
  (`discobox attach <id>`). Don't relay the questions through
  `AskUserQuestion`. Arrow keys switch a dialog's tabs without choosing, so
  you can read every tab. Pick a dialog option with keys
  (`say <id> Down Enter`). Never type a sentence that claims approval
  ("your request was granted") — the judge refuses it, rightly.
- **"Login expired"** — not a real expiry; `kick` types `continue`.
- **A first-run dialog** ("Make auto mode your default permission mode?") —
  it appears when a worker restarts and swallows what is typed next. The
  script answers "No, keep bypass permissions" (`firstrun`, from `idle`). If
  the user ever wants the other answer, change `firstrun`.
- **Low disk** — `say <id> "…free this box's Docker build cache with docker
  builder prune -a -f (build cache only, keep the images)…"` to running
  workers, stop finished ones (`power stop`), and prune your own build cache.
  Never ask for `docker image prune -a` (it deletes the dev images `verify`
  needs) or for shared caches to be deleted. On a full disk, write command
  output to `/dev/shm`.
- **A worker stopped mid-work** — `power start <id>`; it stops itself after
  30 idle minutes unless it holds a keepalive lease.
- **CLI decode errors** (`unexpected field …`) — the server moved ahead of
  your `discobox` binary: fetch GitHub `main` and rebuild it.

## 5. While the human merges

Keep one table for the user, re-posted whenever a row changes. It is status
and to-do in one. Rows are ranked by priority with a `#` column, and the columns
are the issue, the worker's box, what its agent is doing (working, waiting for
you, waiting on CI or review, done; never whether the box is running), the
user's move, and why that row is at that rank. Issue and PR numbers in the
table are plain text; under the table, list each one's GitHub URL, one per
line, because a link inside a table cell renders with its URL in a terminal.
Rank by what unblocks the most, then by how hard a decision is to reverse.
A worker's `ready`, `blocked` or `merged` tag (deliver-issue §7) is its own
verdict; `status` prints it as `#ready`, `#blocked` or `#merged`. Read it
before its screen. The user finds them with `discobox ls --all --tag ready`
(or `--tag blocked`, `--tag merged`).
List which PRs are ready and the order to merge them (foundational changes
first, and a PR that shares files with a larger one before it). After each
merge, `after-merge` waits for GitHub to recompute and has every PR that went `dirty` rebase its own branch. A PR that
conflicts repeatedly should be merged as soon as it is green, and nothing
pushed straight to `main` in between.

`after-merge` retires a merged PR's worker (in triage mode it names the box
instead, for §6 to move back into the pool). When a worker's issue turns out
to need nothing, run `retire <issue>` yourself. It runs `discobox tag
<discobox-id> to-delete`, adding `merged` for a merged PR and dropping `ready`
and `blocked` (a stopped worker cannot move its own tag), and stops the box.
You can tag and stop a discobox but
not delete it, so the tag is how the user finds what to purge (`discobox ls
--all --tag to-delete`). List each one on the table as a 🗑 row until it is
gone. When the batch is done, report what merged, what each worker could not
verify, the follow-ups worth filing, and take the next wave.

## 6. Triage mode

`triage [N]`: a pool of **N boxes, 4 to 6 (default 4)**, works through the
open issues with no `triaged` label, oldest first, one issue per box at a
time. N caps **every** box this run has running — triaging and delivering
together — because each one builds its own images on the one disk. While
deliveries run, the triage pool is smaller.

**Ask up front**, with §1's requests, for the deliver policy — the user's
standing answer to "deliver it now?":

- **Auto-deliver** a verdict of `deliver=auto`: small, reproduced or seen in
  code, the fix obvious and one-shaped (triage-issue §5). Confirm or narrow it
  (e.g. not `priority/low`, not `area/secrets`).
- **Ask** about `deliver=ask`: medium or large work, a design choice, a
  security issue. Ask in batches of up to four with `AskUserQuestion`, each
  option naming the issue, its size, and the choice the verdict names.
- **Never** deliver `deliver=no`; it is reported, not asked.

**Start.** `bash orchestrate.sh untriaged` lists the candidates (open, no
`triaged`, no `platform/*`, not held by a box). Launch one box per issue, up to
N, as in §3 with the prompt `"Triage issue #<N> in discobox-ai/discobox."`,
and record it in the pool, not the workers:
`printf '<id>\t<N>\ttriaging\n' >> ~/.local/state/orchestrate/pool.tsv`.

**Drive** as in §4 — `watch` approves, kicks, and answers to the same
signals — plus one more: it exits on `TRIAGED <id> #<N>: <verdict line>` when
a pool box prints its verdict. `pool` shows every pool box and its last
verdict. For each verdict, read the issue's labels and triage comment, then:

- **Deliver** (`auto` under the policy, or `ask` and the user said yes):
  `bash orchestrate.sh deliver <N>`. The box leaves the pool and becomes a
  worker (`workers.tsv`), told `"Deliver issue #<N> in discobox-ai/discobox."`
  with its context kept — no `/clear` — so it starts from its failing test.
  From here it is a §4–§5 worker.
- **Next** (`no`, or the user said not now): `bash orchestrate.sh triage <id>
  <M>` with the next `untriaged` issue. It types `/clear` first, so the box
  does not carry one issue's context into the next; triage-issue §0 clears the
  checkout. An `ask` waiting on the user holds its box until answered; give
  the others their next issue meanwhile.
- **Out of issues**: `retire <id>` the idle box (it is tagged `to-delete` and
  stopped).

When a delivering box's PR merges, it rejoins the pool if `untriaged` still
lists work: `triage <id> <M>` moves it back (its Docker cache is warm),
instead of tagging it `to-delete`. Launch a new box only while fewer than N
are running.

Keep the user's to-do list as in §5, plus the triage tally: triaged, delivered
or delivering, waiting on their answer, and `no` with each reason
(`needs-info`, a Windows/macOS agent, a decision). When `untriaged` is empty
and every PR has merged, report it and stop.
