# 0150 — Attaching to a discobox still awaiting its source delivers it

- **Status**: Accepted
- **Date**: 2026-09-24
- **Supersedes**: [0095](0095-an-attached-client-pushes-the-commits-made-where-it-runs.md)
  §2's rule that delivering a parked discobox "stays a thing a person asks for
  by name" at `discobox push`. The rest of 0095 stands, including that the
  automatic push's beat never delivers.
- **Relates to**: [ADR 0058](0058-a-push-delivered-source-has-a-pool-side-origin.md),
  the origin a delivery pushes into;
  [ADR 0039](0039-attach-waits-for-readiness-at-every-tier.md), the attach wait a
  parked discobox never leaves.

## Context

A discobox whose source is delivered by push parks in `awaiting_source` once it
is provisioned, and starts when its client pushes every such source and reports
the set complete (`CompleteSandboxSourcePush`). Only the create does this, and
`discobox push`, which performs the create's own delivery against a discobox
that is still parked (`deliverAwaitedSource`).

When the create fails between parking and reporting — the push errors, the
connection drops, the window is closed mid-push — the discobox stays parked.
Attaching to it again does nothing about that:

- The attach wait (`AwaitSandboxHTTPClient`) treats `awaiting_source` as "still
  being provisioned" and waits. Nothing moves the discobox while it waits, so
  it gives up only when its stall budget runs out.
- The status line says "waiting for its source to be pushed", which reads as
  though somebody is pushing it. Nobody is.
- The automatic push excludes a parked discobox by rule (0095 §2), because a
  push to one is its create's delivery — "a state-machine transition and a
  decision about a create that failed; it stays a thing a person asks for by
  name."

So the one fix is a command the window never mentions, and the server fails
the discobox after `sourcePushTimeout` if nobody runs it.

0095's reason for keeping delivery a named action was the beat: a push nobody
asked for at that moment must not start a discobox. That reason does not cover
an attach. Attaching to a discobox *is* asking for it by name, and asking to
use it. What a parked discobox needs for that is exactly one thing, and it is
the same thing `discobox push` would do with no flags, because every flag
`push` takes except `--dir` is refused against a parked discobox.

## Decision

### 1. An attach delivers first, then dials

When a person attaches to a discobox that is parked in `awaiting_source`, and
this machine can deliver it (§2), the client performs the delivery before it
opens the attach: every push-delivered source is pushed at the commit it was
pinned to at create, with the workspace snapshot the create captured, and the
set is reported complete. This is the delivery `discobox push` performs, through
the same code, with no overrides.

It runs **before** the dial rather than beside it. The attach wait stops after a
stall budget that a large first push can outlast, because nothing it watches
moves while the client pushes. Sequenced this way, the attach begins against a
discobox that is already on its way to running, which is a wait it knows how to
narrate.

Both ways of attaching do it:

- **The launcher's workspace** — opening a discobox from the list, `discobox
  attach`, and the window `discobox new` hands over to. The row says whether
  the discobox is parked with a delivery this machine can make, and opening
  the workspace delivers first, narrating it on the busy line.
- **A raw attach**, from the choke point every raw attach shares
  (`attachSandboxTerminal`), narrating on the status line it already uses for
  the wait. The attach that is not somebody working in a discobox (a harness's
  configure flow, `notWorkingHere`) is not asked, as for the automatic push.

### 2. Only a delivery this machine can make, and a clear refusal otherwise

The attach delivers only when the discobox's origin host is this machine. A
discobox created on another machine is left to wait as it does today: that
machine may be delivering it right now, and nothing here holds its commits.

When this machine is the origin but the delivery cannot be made — a recorded
directory has moved, the pinned commit or snapshot is gone, a source had no
repository of its own (ADR 0045) — the attach fails immediately with that
reason and names `discobox push --dir`. Waiting would end the same way, only
later and with less to say.

### 3. A delivery someone else finished is not a failure

The delivery can race another one: the create that parked the discobox may
still be running, or a second window attached at the same moment. Both push the
same pinned commit, so the second push changes nothing. The completion that
loses may be refused because the discobox is no longer parked. An attach whose
delivery fails re-reads the discobox, and if it is no longer awaiting its
source, carries on with the attach instead of reporting the failure.

### 4. The beat still never delivers

0095 §2's exclusion stays for the automatic push's 5s beat: it runs while a
terminal is attached, and by then the attach has delivered. A beat that meets a
parked discobox is one whose delivery is somebody else's (§2) or already failed
and was reported (§2). Either way, repeating it every five seconds adds nothing.

## Alternatives rejected

**Fail the attach fast and tell the person to run `discobox push`.** This is
better than the current wait, but the command it names would do exactly what
the attach could have done, with no decision left to make. Every flag it would
accept for a parked discobox except `--dir` is refused. Making the person type
it is a step, not a choice.

**Deliver from the automatic push's first beat.** The beat runs beside the
dial. The attach wait would be counting down its stall budget while the push
ran, with nothing it watches moving, so a large first push would fail the
attach it was meant to rescue. It would also merge "push new commits into a
running discobox" with "start a parked one", which 0095 kept apart for good
reason.

**Have the server deliver.** It cannot. A push-delivered source is one the
provider cannot read (ADR 0058), and the commits exist only on the client that
created the discobox.

**Deliver another machine's discobox from here too, when the directory
exists.** Rejected. The same path on two machines is not the same repository.
The discobox's origin host is the only machine its recorded directory refers
to, and the delivery can still be made from elsewhere, deliberately, with
`discobox push --dir`.

## Consequences

- Recovering from a create that failed mid-delivery is attaching again.
- An attach to a parked discobox on this machine costs one push of its sources
  before the terminal, narrated as it happens.
- An attach to a parked discobox created elsewhere still waits, and the
  status line still reads as a wait on somebody else's push. That is now
  accurate.
