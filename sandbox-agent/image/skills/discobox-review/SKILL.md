---
name: discobox-review
description: Have fresh-eyes subagents review the local changes — the default reviewer, plus one in parallel for each reviewer the repository defines in .discobox/review/*.md — and leave their findings in discobox-review under their own names, address what they raise, then send them back in until every comment is closed and every file approved. Use when the user wants the current work reviewed with discobox-review, or wants such a review driven to sign-off.
---

# Review Until Accepted

You are working inside a discobox, and `discobox-review` is installed in it.
It is a review of the local working tree: comments anchored to lines, replies
on them, per-file approval, all kept in `.git/discobox-review.json` beside the
repository rather than on a forge. Nothing here needs a remote, a pull request,
or a network.

Two roles over one working tree. A **reviewer** subagent reads the change cold
and writes its findings into that review with the `discobox-review` command.
You are the **author**: you fix what is worth fixing, push back on what is not,
and send the reviewer back in. Rounds repeat until the reviewer has closed
every comment and approved every file.

There is always the default reviewer. A repository can define more — a
security reviewer, one that knows its migrations — and then every round is all
of them at once, each a subagent of its own, each signing what it writes with
its own name. See [The reviewers](#the-reviewers).

A reviewer never edits code. You never close a reviewer's comments. That
separation is the whole point — if you resolve the threads yourself there was
no review, only a checklist.

## What this needs from the harness

Three capabilities, by whatever name they have here:

- **Delegation** — start a subagent that reasons on its own and can run shell
  commands. It must work in *this* checkout, not an isolated copy: the review
  file and the diff both live here. With several reviewers, start them
  together so they run in parallel; a harness that can only run one subagent
  at a time runs them one after another, and the result is the same.
- **Continuation** — send that same subagent a further message later, so it
  keeps what it already said. If the harness cannot continue a subagent, start a
  fresh one each round and tell it to read the existing review first — the
  review file is the shared state, and it is enough to pick the thread back up.
- **Asking the user** — put a decision to the user and wait for an answer.

If delegation is unavailable, stop and say so. Reviewing your own change in your
own context is not what this skill does, and doing it anyway produces a review
that agrees with you.

## Before the first round

```bash
discobox-review status
```

One fact to a line. Read four of them:

- `files 0` → nothing is changed. Say so and stop.
- `base` / `base-chosen` → what the review measures from. If the base is wrong
  the whole review is wrong: `discobox-review base <ref>` sets it,
  `discobox-review base --auto` re-derives it. A base that looks off is a
  question for the user, not a guess.
- `open` → comments already waiting. A non-zero count means a review is already
  under way; continue it rather than starting over.
- `unapproved` → files still to sign off.

Then read the change yourself before anyone else does: `discobox-review diff
--stat`, then `discobox-review diff`. You cannot triage findings on a change you
have not read.

## The reviewers

The default reviewer is named `reviewer` and is always one of them. The others
are the Markdown files in `.discobox/review/` of the box's primary source:

```bash
primary=$(jq -r '.sources[] | select(.slug == "primary") | .target' /etc/discobox/sandbox.json)
ls "$primary"/.discobox/review/*.md 2>/dev/null
```

Each file is one more reviewer. Its file name without `.md` is its **name** —
`security.md` is the reviewer `security` — and that name is what it passes to
`--by`, so the review says which reviewer raised what. The file's contents are
what that reviewer looks for; front matter, if it has any, is read along with
the rest. Read each file yourself before round 1: you will be triaging what
these reviewers find.

- No such directory, or no `.md` in it: there is one reviewer, and the rest of
  this skill reads exactly as it says.
- `reviewer`, `author` and `git-user` are taken. Skip a file with one of those
  names and tell the user it was skipped.
- The definitions come from the primary source even when the change under
  review is in another of the box's sources.

Three things differ once there is more than one, because of how
`discobox-review` keeps a review:

- **A file has one approval, not one per reviewer** — a second `approve`
  replaces the first. So only `reviewer` approves files. Every other reviewer
  signs off by resolving its own conversations and ending its report with a
  verdict line.
- **`resolve` takes no `--by`.** Each reviewer closes only the conversations it
  started; `discobox-review list` names who started each one.
- **Two commands at once can lose one of their writes** — and reading the
  review can write to it. So every reviewer runs every `discobox-review`
  command under one lock, as the brief below says. You do not need it: you
  only touch the review between rounds, when no reviewer is running.

## Round 1: send the reviewers in

Delegate to **one subagent per reviewer**, all working in this tree, all
started at once. Each gets this brief with its own `<name>` filled in:

> You are reviewing the local changes in this repository. You are not the
> author, you did not write this, and you have not been told it works. Your
> name in this review is `<name>`.
>
> Read the change with `discobox-review diff --stat` then `discobox-review
> diff`. The line numbers it prints are the ones a comment anchors to. Read the
> surrounding code — a diff hides its own context. Read what the repository
> tells agents about itself — `AGENTS.md`, `CLAUDE.md`, and any design or
> review notes from the root down to each directory the change touches; a
> change that contradicts the closest one of those is a finding.
>
> Leave every finding in the review, one conversation per concern:
>
>     discobox-review comment <path>:<line> --by <name> "<what is wrong and why>"
>     discobox-review comment <path>:<line>-<line> --by <name> "<...>"   # a block
>
> Approve each file you are satisfied with:
>
>     discobox-review approve --by <name> <path>
>
> `--by` is required on all three — it is who the remark is signed by, and
> discobox-review has no default for it. `<name>` is yours, so the review says
> who wrote each remark.
>
> Do not edit any file. Do not commit. Do not run the test suite as a substitute
> for reading the code — CI already runs it; your value is what CI cannot see.
>
> Look for: correctness bugs and the failure that reaches them; a claim in a
> comment or doc that the code does not honour; design guidance in the repo's
> notes this change breaks; abstractions that exist only to shrink the diff;
> missing migration or upgrade path for persisted state; a case the change
> plainly forgot. Say what breaks and when, not that something "could be
> clearer".
>
> Approve what deserves it. A review that comments on everything and approves
> nothing is not a careful review, it is an unhelpful one.
>
> Report back: what you approved, what you flagged with each conversation id,
> and anything you could not judge from the code alone.

That is the whole brief when `reviewer` is the only one. With more, change it
for each of them:

- **Every reviewer, `reviewer` included** — add:

  > Other reviewers are working on this same review right now, each under its
  > own name. Run every `discobox-review` command — reading ones too — under
  > the review's lock, or one of you will overwrite what another just wrote:
  >
  >     flock "$(git rev-parse --absolute-git-dir)/discobox-review.lock" discobox-review <command> ...
  >
  > Conversations started by another name are not yours: do not reply to them
  > and do not resolve them. If another reviewer has already raised what you
  > were about to, leave it with them.

- **Every reviewer but `reviewer`** — replace the "Look for:" paragraph with
  where its definition is:

  > What you are here to look for is in `<path to its .md>`. Read it first and
  > review for that; another reviewer is covering general correctness.

  and replace the approval instructions (the `approve` command and the
  "Approve what deserves it" paragraph) with:

  > Do not run `discobox-review approve` — a file has a single approval and it
  > is another reviewer's. You sign off in your report: end it with the line
  > `verdict: satisfied` if nothing you raised is still open and you have
  > nothing more to raise, or `verdict: not satisfied` otherwise. A change with
  > nothing in it for you to flag is `verdict: satisfied` and no comments;
  > say so plainly instead of finding something.

Wait for all of them before touching the code. A fix made while a reviewer is
still reading moves the lines it is about to anchor a comment to.

## Round 1: address what came back

```bash
discobox-review list --open
discobox-review show <id>      # each one, in full
```

`list` names who started each conversation. Two reviewers will sometimes raise
the same thing from two sides; fix it once and reply on both.

Sort every open conversation into one of three:

1. **Reasonable — fix it.** Do these first, and do them all before the next
   round. Fix the cause, not the line the reviewer happened to point at.
2. **Wrong, or right but out of scope.** Say so on the thread, with the reason.
   Pushing back is a legitimate answer; pushing back without a reason is not.
3. **A judgement that is not yours to make.** Take it to the user — see below.

Reply on **every** thread, whichever bucket it fell in, saying what you did:

```bash
discobox-review reply <id> --by author "fixed: <what changed>"
discobox-review reply <id> --by author "not changing: <why>"
```

Then stop. **Do not run `discobox-review resolve`.** Closing a conversation is
the call of the reviewer who started it, in the next round, and a thread you
closed yourself proves nothing.

## When to involve the user

Put it to the user, quoting the reviewer's own words, when:

- the finding is about product behaviour, scope, or a default the user chose —
  not about whether the code is correct;
- the reviewer wants a different design, and both designs are defensible;
- you disagree with a finding and the reviewer has now raised it twice;
- two reviewers want opposite things of the same code;
- the fix would touch code outside what the user asked you to change, or would
  mean deleting, migrating, or rewriting persisted state;
- the reviewer's concern is real but the fix is large enough that the user
  should decide whether to pay for it now.

Do not ask about things you can settle by reading the code. Do not batch a
round's worth of small mechanical choices into a question — fix those and say
what you assumed.

## Later rounds

Send the same subagents back in — every reviewer, every round, again all at
once — so none of them re-litigates settled threads. A reviewer with nothing
open still goes back in: the fixes are new code, and it has not read them. The
brief for round *n*:

> Round <n>. The author has replied to your comments and changed the code.
>
> Read the current state: `discobox-review status`, `discobox-review list
> --open`, and `discobox-review show <id>` for each open conversation. Then
> re-read the change with `discobox-review diff`.
>
> For each open conversation: if the author's change actually addresses it,
> close it with `discobox-review resolve <id>`. If it does not, reply saying
> precisely what is still wrong (`discobox-review reply <id> --by <name>
> "..."`) and leave it open. If the author pushed back and the reason holds,
> resolve it and say you accept the reason — being persuaded is a valid outcome.
>
> Then approve the files that are now right (`discobox-review approve --by
> <name> <path>`), and raise anything the fixes newly broke as a new comment.
> Editing a file nullifies its earlier approval, so every file the author
> touched needs approving again.
>
> Do not edit any file.

With more than one reviewer, the same changes as in round 1 apply: "each open
conversation" becomes "each open conversation you started", the lock is still
on every command, and every reviewer but `reviewer` ends on its verdict line
where this brief has it approving files.

Then triage and reply again, exactly as in round 1.

## Stopping

Done when both are zero:

```bash
discobox-review status | grep -E '^(open|unapproved)'
```

`open 0` and `unapproved 0` is full sign-off. Because an edit nullifies that
file's approval, the last round has to be one where the reviewer approved
everything *after* your final change — if you edited anything after the
approval, run one more round.

With more than one reviewer there is a third condition, which `status` cannot
show: every other reviewer's report from that same last round ended `verdict:
satisfied`. A report with no verdict line is not a sign-off — ask that reviewer
for one.

Before you report done, run whatever check this repository tells agents to run
before handing work back — a build, a lint, a test target, whatever its
`AGENTS.md` or `CLAUDE.md` names — and report what it says. The reviewer read
the code; nobody here ran it.

Cap the loop at **5 rounds**. If it has not converged by then the disagreement
is not going to be settled by another round: stop, summarise both positions, and
put it to the user.

Report at the end: how many rounds it took, which reviewers ran, what each
found and you fixed, what you pushed back on and why, and anything the user
decided.

## Rules

- `--by <the reviewer's name>` and `--by author` on every `comment`, `reply`
  and `approve`. There is no default: without it the command refuses. (A person
  at the window passes `--by git-user`, which is git's own `user.name`; neither
  role here is the person.)
- A reviewer edits nothing. The author resolves nothing.
- Only `reviewer` approves files. A reviewer resolves only what it started.
- Neither role commits. This skill reviews work; committing it is a separate
  step the user asks for.
- One subagent per reviewer, carried across rounds — not a new one per round
  with no memory of what it already accepted.
- The set of reviewers is fixed at round 1. A definition added or edited
  mid-review takes effect in the next review.
- If round 1 ends with everything approved, no comments, and every verdict
  satisfied, say so plainly rather than manufacturing a second round.
