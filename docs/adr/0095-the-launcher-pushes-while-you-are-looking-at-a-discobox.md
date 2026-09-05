# 0095 — The launcher pushes while you are looking at a discobox

- **Status**: Proposed
- **Date**: 2026-09-05
- **Supersedes**: [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md) §8's
  manual `InteractPush` key. §§1–7 stand unchanged and are what this builds on.

## Context

[ADR 0058](0058-a-push-delivered-source-has-a-pool-side-origin.md) gave a
push-delivered source a pool-side bare repository, bound read-only into the
sandbox as its `origin`, and `discobox push` as the client's way to move commits
into it. §8 proposed the launcher's share of that: a `p` key beside apply,
running the real Cobra command in a pane. It was never built.

What the intervening use showed is that the key was answering the wrong
question. Three facts about that origin, all of them already in 0058, make a
push into it unlike any other write this window performs:

- **Nothing in the sandbox changes.** The mirror is bound read-only (§2) and the
  push is transport only (§5): no phase, no continue call, no ref the sandbox is
  working on. The sandbox gains `origin/<branch>` and rebases when whoever is
  working in it decides to. A push cannot interrupt, cannot overwrite a working
  tree, and cannot lose uncommitted work.
- **Nobody else is writing it.** The mirror belongs to one sandbox, created and
  reaped with it (§1), and the sandbox's own view of it is read-only (§2, and
  the writable-origin alternative 0058 rejected). The only writer is a client
  holding the repository the sandbox was cut from.
- **The lease already answers the one real hazard** (§6). A rewind is refused
  unless this client put the commits there, and an unrelated history is refused
  outright. Those refusals do not become less safe when nobody typed a key
  first.

So the cost of pushing is a lease-checked ref update on a repository nothing
else reads, and the cost of *not* pushing is a discobox whose `origin` is stale
in a way nothing on screen explains: a person commits locally, an agent in the
box reaches for `git fetch origin && git rebase origin/<branch>` — the reflex
0058 §2 exists to make ordinary — and gets what it had five commits ago. The
manual key does not remove that gap; it only tells you whose fault it was.

`discobox apply` is the opposite case and stays the opposite case. It writes the
developer's own working tree, cherry-picking sandbox commits onto it, and it is
offered rather than performed (`apply.go`'s ready band). Push writes a
throwaway mirror. Treating the two symmetrically — a key each, beside each other
— was the mistake §8 made.

## Decision

### 1. An open workspace pushes, and keeps pushing

While the launcher's workspace is open on a discobox, the window pushes that
discobox's push-delivered sources: once when the workspace opens, and again on
its own generation-guarded clock at `refreshEvery` (5s) for as long as it stays
open. Closing the workspace bumps the generation and the loop stops with it, the
way every other workspace poll does.

Nothing else triggers it. Not the cursor moving down the list, not a row being
on screen, not a box in another folder: the workspace is the one place the
window knows which discobox is being worked on, and a window that pushed to
every box it can see would be doing work for boxes nobody is looking at at a
cost per box per tick.

### 2. Only where the push is this machine's to make

A source is pushed only when every one of these holds, and is silently skipped
otherwise:

- The source is push-delivered (`sandboxpush.CheckPushDelivered`). A
  clone-delivered source's origin is live and a remote-URL source's origin is
  the real remote; there is no mirror to write.
- The discobox's `Origin.HostId` is this machine's host id — the same test
  `resolveApplyHostDir` makes. Another machine's checkout is not this one's to
  push.
- The source's recorded `LocalDirectory` is present, and is a git repository.
- The discobox is not archived, and is not parked awaiting its source.

The last one is a rule, not a guard against an unreachable state. `discobox
push` against a parked discobox *delivers* it — pushing every source at the
commit it was created from and reporting the set complete, which starts it
(`deliverAwaitedSource`). That is a state-machine transition and a decision
about a create that failed; it stays a thing a person asks for by name.

There is no `--dir` here. The window pushes what the discobox recorded or it
pushes nothing; a source whose directory has moved is a question to answer at a
prompt, with the flag that exists for it.

### 3. It is `discobox push` with no flags, through the same code

The window calls `sandboxpush.Push` per source with zero options, through one
new `DataSource` seam. Not a pane running the Cobra command, which is what §8
proposed for a key: an interaction that takes the screen is exactly wrong for
something nobody asked for at that moment.

Everything 0058 §6 decided applies unchanged and is not re-decided here: the
lease, the related-history check, the no-op on an unmoved tip, and uncommitted
changes left where they are. In particular **the window never forces**. A lease
refusal is a real signal — another machine pushed to this discobox — and the
answer to it is a person deciding, at `discobox push --force`.

### 4. Nothing is spent when nothing changed

The common tick is the one where nothing has been committed since the last, and
it must cost nothing worth measuring. It resolves the local branch tip and
compares it to the lease ref (`refs/discobox/origin/<sandboxID>/<slug>/<branch>`,
§6) — two ref reads in a repository this machine already has open — and stops
there. No network, no git transport, no control-plane request.

That is what `sandboxpush.Push` already does on an unmoved tip, so the seam is
free to call on every tick rather than needing a cheaper pre-check of its own.
What the window does add is caching the discobox's sources and their resolved
repository roots for the life of the workspace: a source's delivery, slug and
local directory are fixed at create, so re-reading them every tick would be an
API call to learn something that cannot change.

### 5. A push says so; a refusal says so once

A push that moved the mirror reports on the status line, in the window's
ordinary way — the same line a verb reports on, for the same few seconds. Silence
means nothing needed pushing, which is the state it is in almost always.

A push that failed reports once and is **not retried against the same tip**. The
failures §6 produces are decisions, not transients: a stale lease, an unrelated
history, a directory that is gone. Retrying one every five seconds would put a
red line on screen forever and send the same rejected pack with it. The next
local commit is a new tip and a new attempt.

At most one push per workspace is in flight; a tick that arrives while one is
running is dropped rather than queued.

### 6. There is no push key in the window

§8's `InteractPush` is not built. `discobox push` remains the whole of the
explicit path, and is what covers everything the automatic one deliberately does
not: `--force` past a lease, `--branch` to offer the box a second branch,
`--dir` for a checkout that moved, a discobox nobody has open, and the delivery
of a parked one.

A key that does what the window is already doing is a key whose only real use is
the flag it cannot pass.

## Alternatives rejected

**Build §8's key as written.** Rejected because it leaves the gap that motivates
the feature: the origin is stale exactly when nobody remembered, and a key is
remembered by the people who already knew. It is also a pane taking the screen
for a git transfer that has no output worth reading in the ordinary case.

**Push from the list, for every push-delivered box on the tick.** Widest reach,
and it would cover the box you are about to open. Rejected on scope: the tick
would resolve refs for every row in the project — a shell out per box per tick,
including boxes on other folders and other branches — to serve a case the next
workspace open serves anyway. The window's other ambient work (resources,
credential requests) is one request each; this would be N pieces of local work
for the same beat.

**Push on a filesystem watch of the local repository, instead of a clock.**
Sharper: the push would follow the commit within a moment. Rejected for what it
would cost to be correct — a watch on `.git` fires on index writes, fetches,
gc, and every intermediate state of a rebase, so the interesting edge has to be
recovered by comparing refs anyway, which is what the tick already does at a
tenth of the machinery. The 5s beat is well inside human time for the reflex it
serves.

**Ride the workspace's 2s exec poll rather than a 5s clock of its own.** No new
timer, and the generation guard is already there. Rejected: the exec poll is
paced for noticing sessions started elsewhere, and there is no reason for local
git process spawning to inherit that pace. The listing's beat is the one this
belongs on.

**Retry a failed push on the next tick.** Rejected in §5. The failures are
standing conditions, and the sole effect of retrying is a permanent error line
and a repeatedly rejected transfer.

**Force when the lease is stale, since the mirror is disposable.** Rejected.
0058 §6 chose the lease over a blind force precisely for the case that produces
this failure — two machines pushing to one discobox — and an automatic force is
strictly worse than the manual one it was rejected against: it would rewind the
other machine's push with nobody present.

**Ask before the first automatic push, then remember the answer.** Rejected. A
question is worth asking when the answer could reasonably be no, and this one
writes a repository belonging to the discobox in front of you, which that
discobox reads and nothing else does. A confirmation would be teaching a reflex
rather than protecting anything — and 0058 §8's disabled-with-a-reason menu
entry is what already establishes that push refuses rather than asks.

**Also fetch and rebase inside the discobox.** Rejected, and it is the line this
ADR is careful not to cross. 0058 §5 is explicit that the sandbox rebases when
whoever is working in it decides to; the push moves `origin` and stops. Moving
the box's own branch would be interrupting an agent mid-edit, which is the
destructive case the whole design routes around.

## Consequences

A discobox created from this machine sees local commits in its `origin` within
about five seconds of the workspace being open on it, with nothing typed. The
sandbox's reported diff base self-corrects through `UpstreamRef` once it fetches
(0058 §5), so the list's ahead/diff columns follow without further work.

Commits made while no workspace is open do not travel until one is, which is the
deliberate edge of §1. Opening the box is what closes it.

The mirror's history grows with every push rather than only at create. It is
reaped with the sandbox (§1) and hardlinked into the sandbox's clone (§4), so
the cost is objects on the pool host for the life of one discobox.

A machine that shares a checkout with another machine pushing to the same
discobox will hit a lease refusal without having asked for anything. It is
reported and not retried (§5), and `discobox push --force` is the answer, as it
already was.
