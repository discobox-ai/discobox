# 0095 — An attached client pushes the commits made where it is running

- **Status**: Accepted
- **Date**: 2026-09-05
- **§1 amended**: 2026-09-09 — the trigger is a terminal attach, not the
  launcher's workspace. Nothing else changes.
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
- **The lease already answers the one real hazard** (0058 §6). A rewind is refused
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

### 1. Attaching to a terminal pushes, and keeps pushing

**An attach is the trigger.** Attaching a terminal to a discobox is the act of
going to work in it, and it is the moment the commits made where the client runs
should already be in its origin. So for as long as a terminal attach lasts, its
client pushes that discobox's push-delivered sources: once at the start, and
again every 5s.

This covers both ways a terminal is attached, without either being a rule of its
own:

- **The launcher's workspace**, which attaches to the discobox's terminals as
  soon as it opens (`openWorkspace`) and holds them until it is detached. Its
  loop is guarded by the workspace generation, so leaving ends it the way it
  ends every other workspace poll.
- **Every raw attach** — `discobox attach --raw`, `discobox run --raw`,
  `discobox admin terminal attach`, and `admin terminal start --attach` — which
  has no window at all. One choke point serves all of them
  (`attachSandboxTerminal`), and the loop lasts as long as the stream does.

That choke point has one caller that is **not** somebody going to work in a
discobox: the throwaway sandbox a harness's configure flow attaches to, so a
person can answer a login prompt. It holds no source of anyone's, so it says so
at the call site (`execAttachOptions.notWorkingHere`) and is not asked. An
attach that is part of a flow rather than a session is the exception the rule
needs, and naming it there is cheaper than teaching this to tell them apart.

Nothing else triggers it. Not the cursor moving down a list, not a row being on
screen, not a one-shot `discobox shell -- <cmd>`, and not a box nobody is
attached to. "Attached" is what tells a discobox somebody is working in from the
rest of the project, and it is a fact both front ends already have.

The earlier draft of this section said the launcher's workspace, and named the
window as the thing that pushes. That was the visible half of the real rule
rather than the rule: `--raw` is the same session with a different renderer, and
a person working in it has exactly the same reason to expect their commits to be
there.

### 2. Only where the push is this machine's to make

A source is pushed only when every one of these holds, and is silently skipped
otherwise:

- The source is push-delivered (`sandboxpush.CheckPushDelivered`). A
  clone-delivered source's origin is live and a remote-URL source's origin is
  the real remote; there is no mirror to write.
- **The source was checked out at a branch.** A source created from a tag or a
  bare commit names no branch, and `discobox push` then falls back to whatever
  `HEAD` is now (`pushRefs`) — which is a rev a person picked the moment for,
  and on a 5s clock is whatever branch the developer has switched to since.
  Sending that into a discobox's origin with nobody present is the one thing
  this must not do, so those sources are left to the command. That is what
  0058 §6's argument keeps the command for.
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

There is no `--dir` here. An automatic push sends what the discobox recorded or
it sends nothing; a source whose directory has moved is a question to answer at
a prompt, with the flag that exists for it.

### 3. It is `discobox push` with no flags, through the same code

Both front ends call `sandboxpush.Push` per source with zero options, through
one function that resolves what a discobox's sources are on this machine and
pushes them. Not a pane running the Cobra command, which is what §8 proposed for
a key: an interaction that takes the screen is exactly wrong for something
nobody asked for at that moment.

Everything 0058 §6 decided applies unchanged and is not re-decided here: the
lease, the related-history check, the no-op on an unmoved tip, and uncommitted
changes left where they are. In particular **an automatic push never forces**. A
lease refusal is a real signal — another machine pushed to this discobox — and
the answer to it is a person deciding, at `discobox push --force`.

### 4. Nothing is spent when nothing changed

The common tick is the one where nothing has been committed since the last, and
it must cost nothing worth measuring. It resolves the local branch tip and
compares it to the lease ref (`refs/discobox/origin/<sandboxID>/<slug>/<branch>`,
0058 §6) — two ref reads in a repository this machine already has open — and stops
there. No network, no git transport, no control-plane request.

`sandboxpush.Push` is itself cheap on an unmoved tip — it resolves the same two
refs and returns. What is not cheap is *getting to it*: a push needs an origin
URL, so the caller resolves one first, and on a unix-socket or named-pipe
endpoint that means standing up a loopback proxy and tearing it down again
(`App.gitServerURL` → `endpoint.StartLoopbackProxy`). Every 5s, forever, for a
beat that sends nothing.

So the order is: **resolve every source, and only open the git route if one of
them moved** (`sandboxpush.Resolve`, then `Push` for the pending ones). Resolve
is the same two ref reads Push would do, exported so the decision can be made
before anything is dialed, and it is the head of Push itself — so what counts as
"new commits" is decided in one place rather than once for the asking and once
for the doing.

What is also added is caching the discobox's sources and their resolved
repository roots, so the beat makes no control-plane request either. Only what
cannot change is held: which sources a discobox has, their delivery, and the
directories they came from are fixed at create, and so is the answer "none of
them is this machine's to push". A discobox that is merely *not yet* pushable —
still awaiting its source, a directory not mounted yet, a repository nobody has
run `git init` in — is asked again on the next beat, because a client that
remembered the first no would keep it for the whole session.

### 5. A push says so where there is somewhere to say it

In the window, a push that moved the mirror reports on the status line, in the
ordinary way — the same line a verb reports on, for the same few seconds.
Silence means nothing needed pushing, which is the state it is in almost always.

A raw attach has nowhere to say it. `--raw` is documented as "the stream and
nothing else, for a pipe, a recording, or a terminal you would rather keep as it
is", and a line written into that stream lands in the middle of whatever the
harness is drawing. So there it says nothing while it runs, and reports what
could **not** be pushed once the stream is over and the terminal is the
client's again. A refusal is the half somebody has to act on; a successful push
is visible in git.

### 6. A transfer that has started finishes, and is reported even if it lands late

Leaving — a detach, a window quitting — ends the **beat**. It does not end a
`git push` that is already going.

Killing one mid-transfer is not a tidy no-op. The receiving end may already have
taken the pack while the lease this client leases against is only written after
the push returns (0058 §6, `pushTo`), so a client that stopped in that window
would leave the discobox's origin ahead of its own lease — and the next push,
automatic or typed, is refused until somebody passes `--force`. That is exactly
the refusal §5 treats as another machine's doing, manufactured out of pressing
Ctrl-A d.

So the halves are split. Working out *whether* to send — reading the discobox,
resolving a branch tip — is cancellable, and an error from there while stopping
is dropped, because it is about the stop. The send itself runs on a context
nothing cancels, and whoever is leaving waits for it: the raw attach in its
stop, the launcher on its way out, because a Bubble Tea program returns on Quit
without waiting for the commands it has in flight. An error from a transfer is
never about the stop — nothing could have interrupted it — so it is reported
even when it arrives after the detach. Commit, detach a second later, and the
push that goes out in between is the one most likely to be refused; silence
there would be the worst version of §5.

**The cost is real and is the point of writing this down**: `Ctrl-A d` no longer
always returns the shell instantly, and neither does quitting the launcher. Two
things bound it, and a third says what it is:

- git is told not to prompt (`GIT_TERMINAL_PROMPT=0`). Its own prompt and every
  credential helper open `/dev/tty` directly, and that terminal is the one just
  handed back — so without this, a push that wants a credential is a hang with
  nothing on screen, output captured.
- git is told to give up on a connection that has **stopped moving**
  (`http.lowSpeedLimit`, `http.lowSpeedTime`), rather than on a clock. A
  deadline is the wrong instrument here: the largest transfer this ever makes is
  the first one after attaching, carrying everything committed since this client
  last pushed — days of work, for a discobox attached to on Thursday and made on
  Tuesday — and that is precisely the transfer a deadline would cut in half,
  producing the stranded lease this section exists to prevent, at the moment it
  is most expensive. Rate is what tells a big push from a wedged one; elapsed
  time is not.
- A wait long enough to notice says so (ADR 0060). Both waits print what they
  are waiting for, because the launcher's has already taken the screen down and
  would otherwise be an unexplained pause at a shell prompt.

Nobody should undo any of it for feeling slow.

**Rejected: kill the push and say nothing.** Simpler, and it makes detach
instant. It is the version that strands the lease, and the failure surfaces
minutes later as a refusal nobody caused.

**Rejected: kill the push and report it.** What was built first. It prints
`signal: killed` for a push nobody asked for, on an ordinary detach, and still
strands the lease.

**Rejected: bound the send with a deadline.** Also built first, at two minutes.
It is the same rejected alternative wearing a bound: a transfer that outlasts it
is killed mid-pack, reported, and then *held* — so the origin is left ahead of
the lease and nothing retries it. The only transfers long enough to reach a
deadline are the honest ones.

A push that failed reports once and is **not retried against the same tip**. The
failures 0058 §6 produces are decisions, not transients: a stale lease, an unrelated
history, a directory that is gone. Retrying one every five seconds would put a
red line on screen forever and send the same rejected pack with it. The next
local commit is a new tip and a new attempt.

At most one push per attach is in flight; a tick that arrives while one is
running is dropped rather than queued.

### 7. There is no push key in the window

§8's `InteractPush` is not built. `discobox push` remains the whole of the
explicit path, and is what covers everything the automatic one deliberately does
not: `--force` past a lease, `--branch` to offer the box a second branch,
`--dir` for a checkout that moved, a discobox nobody is attached to, and the
delivery of a parked one.

A key that does what an attached client is already doing is a key whose only
real use is the flag it cannot pass.

## Alternatives rejected

**Build §8's key as written.** Rejected because it leaves the gap that motivates
the feature: the origin is stale exactly when nobody remembered, and a key is
remembered by the people who already knew. It is also a pane taking the screen
for a git transfer that has no output worth reading in the ordinary case.

**Keep the workspace as the trigger, and leave `--raw` out.** The first draft of
§1. Rejected once it was written down: the window is a renderer, not a reason. A
person attached over `--raw` is working in that discobox exactly as much, and
telling them their commits do not travel because of which front end they chose
is a rule nobody could predict from what it does.

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
about five seconds, with nothing typed, for as long as a terminal is attached to
it — from the launcher or from a raw attach. The
sandbox's reported diff base self-corrects through `UpstreamRef` once it fetches
(0058 §5), so the list's ahead/diff columns follow without further work.

Commits made while nothing is attached do not travel until something is, which
is the deliberate edge of §1. Attaching is what closes it.

The mirror's history grows with every push rather than only at create. It is
reaped with the sandbox (§1) and hardlinked into the sandbox's clone (§4), so
the cost is objects on the pool host for the life of one discobox.

A machine that shares a checkout with another machine pushing to the same
discobox will hit a lease refusal without having asked for anything. It is
reported and not retried (§5), and `discobox push --force` is the answer, as it
already was.
