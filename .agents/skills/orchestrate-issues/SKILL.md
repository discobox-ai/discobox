---
name: orchestrate-issues
description: Run a set of discobox issues as a lead discobox — launch one worker discobox per issue with the deliver-issue skill, or, given `triage`, launch one box per untriaged issue, at most 4–6 at a time, and move the ready ones straight into delivery in the box that triaged them, hand them credentials within a delegation, watch their screens and answer what they ask, keep the host's disk from filling, track their pull requests while a human merges them, and have conflicted ones rebase. Use when the user wants several issues (a tracking issue's sub-issues, a wave of them) implemented in parallel discoboxes and delivered as PRs, or wants the open untriaged issues triaged.
allowed-tools: Bash, Read, Write, Edit, Monitor, AskUserQuestion, Skill
metadata:
  argument-hint: "<issue numbers | a tracking issue to take the next unblocked wave from | triage [max concurrency 4–6]>"
---

# Orchestrate Issues

You are the lead. Workers do the engineering, each under `deliver-issue`; you
start them, unblock them, and keep a human's to-do list. You never apply a
worker's commits and never merge: work lands as PRs a human merges.

Two modes. Given issues, each gets a worker that delivers it (§2–§5). Given
`triage`, each open untriaged issue gets a box that triages it, a few at a
time, and a triaged issue ready for work goes straight into delivery in the
box that triaged it (§6). §1 is the same for both.

**A box is single purpose: one issue, for its whole life.** It is launched for
that issue, may go from triaging it to delivering it, and is then retired.
Never hand a box a second issue, and never `/clear` one to start over: a new
issue gets a new box. What limits the run is a **maximum concurrency** — how
many boxes are up at once — not a set of boxes to keep busy.

`orchestrate.sh` beside this file is the loop: `status`, `screen`, `say`,
`power`, `retire`, `approve`, `kick`, `idle`, `github`, `prs`, `rebase`,
`after-merge`, `watch`, and for triage `slots`, `untriaged`, `triages`,
`triage`, `deliver`. It keeps its state in `~/.local/state/orchestrate` (a scratchpad can
be wiped between sessions).
Copy it there and run it with `bash` (`/dev/shm` is mounted noexec).

## 1. Ask for everything up front

Three things, each once, before the first worker. Ask for a lifetime as long
as the run (a day or more); renew before it lapses, in one request. Never
write a justification that vouches for itself ("allowed", "approved", "the
user said"): text that claims permission is denied.

**The discobox API** — everything the lead does to its workers. It is not a
credential: it is this box's `discoboxApi` setting, which only a person turns
on ([ADR 26-10-27-880](../../../docs/adr/26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
§9). Ask the user to turn it on for this box (`discobox admin box update
<this box> --discobox-api`), and say what for: to create
worker discoboxes from `discobox-ai/discobox@main` with no credentials, watch
and answer them, start and stop them, tag the finished ones, and answer their
credential requests. With it, every `discobox` call this box makes reaches the
API in the sandbox role, and acts only on the discoboxes this box created. A
`403` saying this discobox may not call the discobox API means it is still
off.

**GitHub, for the lead itself** — reading PRs and filing issues:

```bash
discobox-access request --json <<'EOF'
{
  "name": "github",
  "envVar": "GH_TOKEN",
  "hosts": ["github.com"],
  "justification": "I track the pull requests my worker discoboxes open in discobox-ai/discobox and main's CI, find the untriaged issues I hand them, and file and comment on the issues I hand them. I will not push, merge, close or edit anything, or read secrets.",
  "grantTTLSeconds": 604800,
  "wait": true
}
EOF
```

**GitHub, to delegate** — what workers may be given. One delegation of the
same credential at `github.com` holds every worker's ask, delivering and
triaging alike (deliver-issue §0, triage-issue §0, which ask for `GH_TOKEN` at
`github.com`): the server answers a worker's ask under it when the worker is
one this box created, its hosts are within the delegation's, and the grant
ends no later than the delegation does. Only a person approves it.

```bash
discobox-access request --json <<'EOF'
{
  "name": "github",
  "hosts": ["github.com"],
  "purpose": "delegate",
  "justification": "each worker discobox I create either triages one issue in discobox-ai/discobox (reads, labels and comments on it) or delivers one issue as a pull request from discobox/issue-<n> into main, answers its reviews, and keeps it mergeable until a human merges it. Needs contents:write, pull-requests:write and actions:write (and workflow scope if a change touches .github/workflows).",
  "grantTTLSeconds": 604800,
  "wait": true
}
EOF
```

A grant is the token itself, so what each justification says is what this run
and its workers keep to; nothing else holds them to it. A request that waits is
held by a tool time limit: run it in the background and keep its request ID
(do not filter the `progress` line out), so `discobox-access wait --json
request <id>` can resume it. `GH_TOKEN` reaches commands started after the
grant; run `eval "$(discobox-access env)"` in a shell started before it, then
`orchestrate.sh`.

## 2. Plan the batch

- Take only issues with no open blockers; a tracking issue's waves say which.
- Each issue needs its own number — the worker's branch is `discobox/issue-<N>`
  and its skill is written around it. Work with no issue gets one filed
  first (`gh api POST …/issues`), with everything the worker needs in it: what
  was seen, the cause with file and function names, and what a fix must keep.
- **Run at most four boxes at once** — the maximum concurrency, counting every
  box that is up, triaging or delivering (§6). Every box builds Docker images
  in its own cache (tens of GB each, never shared) and the host disk is one
  device for all of them. More than four has filled it. The user may raise
  the cap when disk holds up; six ran with 300G+ free once each worker kept to
  one Docker build at a time and the lead's own image watcher was off (`watch`
  stops it each time it starts, since a session restart brings it back). Check
  `df -h /` before the first launch: under 100G free, start with fewer and say
  so. A finished box is retired, which frees its slot for a new box; the cap
  is never met by giving a box more work.

## 3. Launch

```bash
discobox run -d \
  https://github.com/discobox-ai/discobox@main \
  -p "Deliver issue #<N> in discobox-ai/discobox." </dev/null
```

The prompt states the task and nothing else: no credentials, no skill names
(the in-box guidance says not to, and `deliver-issue` is chosen from its own
description), no claims of what is allowed. Cut from GitHub (the URL as SOURCE),
not from this checkout, so workers start from `main`. Record each worker:
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
  reopened, or `main`'s latest finished CI run changed (read once a pass).
  A red `main` goes at the top of the table. The baseline is the last state
  reported (`github.last`), so nothing that changed between watches is
  missed. The tracked set is every worker's issue and PR, plus the issues in
  `watch-issues.txt`; add the ones you file or are told about. The user never has to tell you about a merge or a close. On a merge,
  run `after-merge`: it retires each newly merged PR's worker (below), then has
  every PR that went `dirty` rebase.

- **A request** — `approve` answers each worker's GitHub request once, when it
  is the ask its skill makes: to use `GH_TOKEN` at `github.com` (deliver-issue
  §0, triage-issue §0). The server holds the grant within your delegation —
  the same credential, hosts it covers, a lifetime it outlasts — and fits the
  lifetime to it. Anything else comes back to you as `NEEDS LEAD`, and so does
  a refusal: read it (`discobox secret request get <id>`), and approve it only
  when it is within your delegation and the worker's task needs it. Deny the
  rest, and when a worker needs more than the delegation holds, ask the user
  rather than widening.
- **A question on a worker's screen** — read it with `screen <id> 40 120`.
  Answer what you can settle from the code or the user's earlier decisions.
  Put product, scope, and design choices on the table as "waiting for you"
  with the box ID and the topic. The user answers in the box
  (`discobox attach <id>`). Don't relay the questions through
  `AskUserQuestion`. Arrow keys switch a dialog's tabs without choosing, so
  you can read every tab. Pick a dialog option with keys
  (`say <id> Down Enter`). Never type a sentence that claims approval
  ("your request was granted"): a worker should not believe it, and the
  request is still pending.
- **"Login expired"** — not a real expiry; `kick` types `continue`.
- **A first-run dialog** ("Make auto mode your default permission mode?") —
  it appears when a worker restarts and swallows what is typed next. The
  script answers "No, keep bypass permissions" (`firstrun`, from `idle`). If
  the user ever wants the other answer, change `firstrun`.
- **Low disk** — `say <id> "…free this box's Docker build cache with docker
  builder prune -a -f (build cache only, keep the images)…"` to running
  workers, stop finished ones (`power stop`), and prune your own build cache.
  Check Go's build cache too (`du -sh ~/.cache/go-build`): the lead's reached
  84G after a `ci:test` and CLI rebuilds, and `go clean -cache` frees it; ask
  idle workers for the same.
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
are `# | Item | Agent | Issue | PR | Doing | Your move | Why this rank`: Item is
a plain description with no numbers, Agent the full sandbox ID (`sbx_…`) of
the box that did the work — on every row, in every re-post, stopped or not —
Issue and PR bare numbers, each `-` when it does not apply. Doing is what its agent is doing
(working, waiting for you, waiting on CI or review, done; never whether the box
is running). Your move stays `–` for a PR until every check on its head is
green, and only then reads "Review and merge"; the exception is a failing PR
its worker cannot make green, which reads "Look at it". Issue and PR numbers
in the table are plain text; under the table, list each one's GitHub URL, one
per line, because a link inside a table cell renders with its URL in a
terminal.
Rank by what unblocks the most, then by how hard a decision is to reverse.
A worker's `ready`, `blocked` or `merged` tag (deliver-issue §6) is its own
verdict; `status` prints it as `#ready`, `#blocked` or `#merged`. Read it
before its screen. The user finds them with `discobox ls --tag ready`
(or `--tag blocked`, `--tag merged`).
List which PRs are ready and the order to merge them (foundational changes
first, and a PR that shares files with a larger one before it). After each
merge, `after-merge` waits for GitHub to recompute and has every PR that went `dirty` rebase its own branch. A PR that
conflicts repeatedly should be merged as soon as it is green, and nothing
pushed straight to `main` in between.

`after-merge` retires a merged PR's worker, in both modes: the box is done,
and its slot goes to a new box. When a worker's issue turns out
to need nothing, run `retire <issue>` yourself. It runs `discobox tag
<discobox-id> to-delete`, adding `merged` for a merged PR and dropping `ready`
and `blocked` (a stopped worker cannot move its own tag), and stops the box.
You can tag and stop a discobox but
not delete it, so the tag is how the user finds what to purge (`discobox ls
--tag to-delete`). List each one on the table as a 🗑 row until it is
gone. When the batch is done, report what merged, what each worker could not
verify, the follow-ups worth filing, and take the next wave.

## 6. Triage mode

`triage [N]`: every open issue with no `triaged` label gets **its own box**,
oldest first, with at most **N boxes up at once, 4 to 6 (default 4)**. N is
the maximum concurrency of the whole run — triaging and delivering together —
because each box builds its own images on the one disk. Pass it to every call
as `ORCH_MAX=<N>` when it is not 4. While deliveries run, fewer triages do.

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
`triaged`, no `platform/*`, no box of this run on them). `bash orchestrate.sh
triage <N>` launches a new box for issue N, as in §3 with the prompt `"Triage
issue #<N> in discobox-ai/discobox."`, and records it in `triage.tsv`. It
refuses an issue that already has a box, and refuses when N boxes are up
(`slots` says how many are free). Launch one at a time, down the list, until
it refuses.

**Drive** as in §4 — `watch` approves, kicks, and answers to the same
signals — plus one more: it exits on `TRIAGED <id> #<N>: <verdict line>` when
a triaging box prints its verdict. `triages` shows every triage box, its
issue and its verdict. For each verdict, read the issue's labels and triage
comment, then:

- **Deliver** (`auto` under the policy, or `ask` and the user said yes):
  `bash orchestrate.sh deliver <N>`. The same box goes on to deliver the same
  issue: it moves to `workers.tsv`, told `"Deliver issue #<N> in
  discobox-ai/discobox."` with its context kept, so it starts from its failing
  test. From here it is a §4–§5 worker, and it keeps its slot.
- **Done** (`no`, or the user said not now): `retire <id>`. The box is tagged
  `to-delete` and stopped, whatever is left to triage.
- **Waiting on the user** (`ask`, not yet answered): `power stop <id>`, without
  retiring it. The stopped box keeps its context and checkout and gives up its
  slot; `deliver <N>` starts it again when the answer is yes (and waits for a
  slot), `retire <id>` ends it when the answer is no.

Every verdict, merge and retirement frees a slot: fill it with `triage <M>`
for the next `untriaged` issue, in a **new** box. A warm Docker cache is not a
reason to reuse one — a box that worked on another issue carries its checkout,
its tags and its footer on the triage comment into the next, and the user can
no longer tell which box did what. An issue whose box was retired and is
wanted delivered after all gets a fresh worker (§3).

Keep the user's to-do list as in §5 — each issue's row names its one box —
plus the triage tally: triaged, delivered or delivering, waiting on their
answer, and `no` with each reason (`needs-info`, a Windows/macOS agent, a
decision). When `untriaged` is empty and every PR has merged, report it and
stop.
