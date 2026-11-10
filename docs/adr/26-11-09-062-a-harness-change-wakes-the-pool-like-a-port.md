# 26-11-09-062 — A harness change wakes the pool like a port

- **Status**: Accepted (narrows [26-11-03-815](26-11-03-815-a-pool-waits-on-a-sandboxs-ports-and-reports-a-change-at-once.md)
  §2, whose version moves only with the port snapshot, and §4, "Only ports
  wake the pool")
- **Date**: 2026-11-09

Accepted by the owner by merging #903, 2026-11-10.
- **Relates to**: [26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md),
  whose console explainer and band offer to save a box's harness change, and
  [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md),
  the capture that finds it.

## Context

A person sets a harness up in the box it runs in: they sign in, or change a
setting, and the console offers to save it for every box on the harness (ADR
26-11-02-152). The offer is how they learn a sign-in can be kept. It reaches
them late. The box waits a second for the files to settle, then runs the
harness's export, and publishes a candidate when the export finishes — which
for a credential can take tens of seconds. The pool then reads the box's
status only every 15 seconds (ADR 0030), so the candidate reaches the control
plane up to 15 seconds after that, by which time the person has moved on, and
the box they signed in to is one they will not sign in to again. This ADR
removes the pool's 15 seconds; the export's time is the harness's own.

ADR 26-11-03-815 gave ports a wait for the same race and kept everything else
on the 15-second check-in, because program and git status change constantly
while a harness works (§4). A harness configuration candidate does not: it
moves when a person signs in or edits a setting, and its state moves when they
save or discard it.

## Decision

### 1. The status wait answers a harness configuration change too

`GET .../status/wait`'s version also moves when the capture's published
preview changes its candidate or its state — a new candidate, one saved,
discarded or superseded, capture becoming unavailable. Re-observing unchanged
files, which the capture does on every 30-second backstop, changes neither and
does not move it — with one exception: after an export fails, capture reports
itself unavailable, and the next successful look mints a fresh candidate for
the same files. Each failed export therefore moves the version twice, which
the pool's two-second wake spacing (815 §3) bounds like any other change.
Everything else 815 §2 says of the wait holds: a call naming the current
version is held until it moves or 25 seconds pass.

The version stays one opaque token, and the response keeps its one field,
`portsVersion`. A pool and the agents in its boxes upgrade apart; a pool reads
an empty or missing version as an agent with nothing to wait on and backs off
for five minutes (`pool-agent/statuswait.go`, as 815 §3 does for a 404). A
renamed field would read as missing to whichever side is older. The field's
name is now narrower than what it versions, and its description says so.

### 2. Nothing else wakes the pool

815 §4's reason stands for the rest of the status. The pool's waiter, its
two-second spacing between wakes and its fallback to the tick are unchanged:
a capture change is one more reason the version moves.

## Alternatives rejected

- **A second wait route for the harness.** A second held connection per box
  and a second waiter in the pool, to learn the same thing — that this box's
  status is worth reading now.
- **Rename the field to `version`.** Correct, and a five-minute regression for
  every box whose agent and pool are not upgraded together, for a name. Revisit
  when the wait next changes shape in a way old pools could not read anyway.
- **Send `version` beside `portsVersion`, the pool reading whichever is
  there.** No regression, but two names for one value in the contract
  indefinitely: the old one can go only once no pool reads it, which nothing
  can tell the agent. It buys a name and nothing a pool does differently.
  Revisit with the rename above.
- **Have the console poll every box's harness preview.** The console already
  reads a box's live preview through the server on each listing, but only for
  a box whose pushed status names a candidate (`refreshHarnessChanges`): each
  preview runs the harness's export in the box. Asking every running box on
  every listing would run an export in each, every few seconds, under a
  timeout a credential export can miss. The pushed status is what says there
  is a candidate to ask about, so the pool is the hop that has to be quicker.

## Consequences

- A harness candidate reaches the control plane within about a second of the
  capture publishing it — subject to 815's spacing — instead of up to 15
  seconds later. The console still finds it on its next listing. The export
  before publication is unchanged.
- An agent older than this answers the wait for ports only; its harness
  changes keep the 15-second check-in.
