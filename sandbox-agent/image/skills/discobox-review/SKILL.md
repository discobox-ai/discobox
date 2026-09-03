---
name: discobox-review
description: Have a fresh-eyes subagent review the local changes and leave its findings in discobox-review, address what it raises, then send it back in until every comment is closed and every file approved. Use when the user wants the current work reviewed with discobox-review, or wants such a review driven to sign-off.
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

The reviewer never edits code. You never close your own comments. That
separation is the whole point — if you resolve your own threads there was no
review, only a checklist.

## What this needs from the harness

Three capabilities, by whatever name they have here:

- **Delegation** — start a subagent that reasons on its own and can run shell
  commands. It must work in *this* checkout, not an isolated copy: the review
  file and the diff both live here.
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

## Round 1: send the reviewer in

Delegate to **one** subagent, working in this tree, with this brief:

> You are reviewing the local changes in this repository. You are not the
> author, you did not write this, and you have not been told it works.
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
>     discobox-review comment <path>:<line> --by reviewer "<what is wrong and why>"
>     discobox-review comment <path>:<line>-<line> --by reviewer "<...>"   # a block
>
> Approve each file you are satisfied with:
>
>     discobox-review approve --by reviewer <path>
>
> Always pass `--by reviewer`. Without it the remark is attributed to the
> repository owner's git identity, and the review stops being legible.
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

## Round 1: address what came back

```bash
discobox-review list --open
discobox-review show <id>      # each one, in full
```

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
the reviewer's call in the next round, and a thread you closed yourself proves
nothing.

## When to involve the user

Put it to the user, quoting the reviewer's own words, when:

- the finding is about product behaviour, scope, or a default the user chose —
  not about whether the code is correct;
- the reviewer wants a different design, and both designs are defensible;
- you disagree with a finding and the reviewer has now raised it twice;
- the fix would touch code outside what the user asked you to change, or would
  mean deleting, migrating, or rewriting persisted state;
- the reviewer's concern is real but the fix is large enough that the user
  should decide whether to pay for it now.

Do not ask about things you can settle by reading the code. Do not batch a
round's worth of small mechanical choices into a question — fix those and say
what you assumed.

## Later rounds

Send the same subagent back in, so it does not re-litigate settled threads. Its
brief for round *n*:

> Round <n>. The author has replied to your comments and changed the code.
>
> Read the current state: `discobox-review status`, `discobox-review list
> --open`, and `discobox-review show <id>` for each open conversation. Then
> re-read the change with `discobox-review diff`.
>
> For each open conversation: if the author's change actually addresses it,
> close it with `discobox-review resolve <id>`. If it does not, reply saying
> precisely what is still wrong (`discobox-review reply <id> --by reviewer
> "..."`) and leave it open. If the author pushed back and the reason holds,
> resolve it and say you accept the reason — being persuaded is a valid outcome.
>
> Then approve the files that are now right (`discobox-review approve --by
> reviewer <path>`), and raise anything the fixes newly broke as a new comment.
> Editing a file nullifies its earlier approval, so every file the author
> touched needs approving again.
>
> Do not edit any file.

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

Before you report done, run whatever check this repository tells agents to run
before handing work back — a build, a lint, a test target, whatever its
`AGENTS.md` or `CLAUDE.md` names — and report what it says. The reviewer read
the code; nobody here ran it.

Cap the loop at **5 rounds**. If it has not converged by then the disagreement
is not going to be settled by another round: stop, summarise both positions, and
put it to the user.

Report at the end: how many rounds it took, what the reviewer found and you
fixed, what you pushed back on and why, and anything the user decided.

## Rules

- `--by reviewer` and `--by author` on every `comment`, `reply` and `approve`.
  The default attributes the remark to the repository owner.
- The reviewer edits nothing. The author resolves nothing.
- Neither role commits. This skill reviews work; committing it is a separate
  step the user asks for.
- One reviewer, carried across rounds — not a new one per round with no memory
  of what it already accepted.
- If the reviewer approves everything in round 1 with no comments, say so
  plainly rather than manufacturing a second round.
