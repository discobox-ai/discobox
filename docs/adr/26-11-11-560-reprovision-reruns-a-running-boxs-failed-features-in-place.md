# 26-11-11-560 — Reprovision runs a running box's failed features again, in place

- **Status**: Proposed
- **Date**: 2026-11-11
- **Narrows**: [26-11-09-062](26-11-09-062-a-harness-change-wakes-the-pool-like-a-port.md)
  §2, "Nothing else wakes the pool": a feature's provisioning state moving
  wakes it too (§6).
- **Relates to**: [0035](0035-repair-is-one-rebuild-intent-plus-a-start-instruction.md)
  and [0064](0064-repair-rebuilds-on-the-current-image.md), repair, which this
  is not; [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §§5–6 and [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
  §1, the run and the list it applies;
  [0060](0060-provisioning-progress-is-a-recorded-phase-the-client-polls.md),
  how a waiting client reads progress;
  [26-11-06-628](26-11-06-628-attach-waits-for-the-outcome-of-blocking-provisioning.md),
  attach joining blocking provisioning.

## Context

A box is provisioned from its ordered list of bundles (953 §1). The sandbox
agent runs that once per start of its process
(`sandbox-agent/cmd/discobox-sandbox-agent/bundles.go`, `provisionBox`: fetch
the pins' bundles, then `provision.Apply` with `Background`). It reports each
feature in `/run/discobox/provision.json` (`pkg/provisionspec/status.go`).
The agent's status report carries that file to the control plane as
`agentStatus.provision`, where `discobox ls` and the console read it
(`cli/internal/cli/provisioning.go`).

What a failure leaves behind:

- **A failed step is not recorded.** It fails its feature and every feature
  that requires it, and nothing else (`sandbox-agent/provision/DESIGN.md`,
  Failure). Each step is keyed by a digest and skipped when its record
  matches, so a later run applies exactly what did not finish. Steps are
  idempotent by contract (953, Consequences): a download moves into place
  only once its sha256 matches, an unpack swaps its directory in whole, and
  apt work first finishes an interrupted `dpkg --configure -a`.
- **The document is final for the agent's process.** `Tracker` decides
  readiness once (`readied` is closed once, `sandbox-agent/provision/tracker.go`),
  and nothing starts a second run. A blocking feature that failed keeps every
  terminal launch and command creation refused (`AwaitReady`) until the agent
  starts again. An `async` feature that failed leaves readiness alone, and
  its commands' placeholders report it failed (`discobox-provision run`).
- **The only ways out are heavy.** `restart` restarts the whole box
  (`pool-agent/sandboxruntime/power.go`, `RestartSandbox`): the agent starts
  again and its new run retries the failed steps, but every process in the
  box dies. `repair` is a rebuild intent (0035, 0064): a new container on the
  current image. Neither is needed when a download hit a network blip and
  the box is otherwise fine.
- **The control plane learns slowly.** The pool reads a box's status every
  15 seconds (ADR 0030). Only ports and a harness configuration change wake
  it sooner (26-11-03-815, 26-11-09-062).

## Decision

### 1. Reprovision is a second run of the box's own plan, in its agent

A **reprovision** tells a running box's sandbox agent to provision again. It
starts a new run on the agent's existing `Tracker`, in the same process. The
run uses the same trees, the same feature settings (`sandbox.json`'s
`features`), the same step records and the same `provision.Apply` the boot
run used. Nothing about the box changes: no container or VM, no image, no
pins, no workspace, no running process. The agent, the box's terminals and
its services keep running throughout.

It is not a repair. Repair is lifecycle intent the reconciler converges, and
it tears the runtime down. Reprovision is an instruction to the agent. It
records no intent, bumps no generation, and clears no settled failure in the
control plane's sandbox record (a box whose reconcile failed is repair's).
Repair's behaviour is unchanged.

### 2. It runs the features that failed, and what they take with them

The run's scope is **every feature whose state is `failed`**. That includes
the features that failed only because a feature they require failed: the
runner fails those too, so "failed" already names the whole set.

- A `ready` feature is not run again: no step, no `check`, no restart of the
  units it laid. Its line in the document stays as it is. The plan still
  reads it in requires order, as satisfied.
- An `off` feature stays off. Off is the project's choice, never a
  consequence of a failure, and a reprovision does not override it.
- Nothing is left `pending` once a run is done (`State.Done`). So no third
  set of features "waiting on a failed one" needs a rule.
- **A run that failed before it had a plan** has no feature lines: it could
  not fetch the box's bundles, or could not read or order the plan
  (`Tracker.Fail`). A reprovision then runs the boot path whole: it fetches
  the pins' bundles and provisions every feature, with records skipping what
  is in place.

Within the scope, the run has the boot's shape. A failed feature that is not
`async`, or that one requires, blocks readiness again. An `async` one runs in
the background. A failed `async` feature's placeholders are still in place,
since only a ready feature's are removed. While it runs they wait for it, so
a command a person types waits instead of reporting the old failure.

### 3. It is idempotent and safe to repeat

- **No failed features and no run in progress**: nothing runs. The request
  succeeds and answers the document as it stands.
- **A run in progress** (the boot's, or another reprovision's): the request
  joins it rather than starting a second one, and answers the document as it
  stands. Two runs never share one runner.
- **A half-run feature** is rerunnable by the step contract it already has
  (Context): its applied steps are recorded and skipped, and the first
  unrecorded step runs again from its start. This ADR adds no new obligation
  on a step. A `run` step that is not idempotent was already a bug, since the
  next start of the box reruns it the same way.

### 4. Readiness is decided again when a blocking feature runs again

When the run includes a blocking feature, the tracker reopens readiness: its
`Ready` and `Error` clear, the document's state is `provisioning`, and
readiness is decided again by this run, either way. When the run includes
only `async` features, readiness stays as it was, which was ready.

A terminal launch or command creation that arrives while readiness is
reopened joins the run, exactly as 26-11-06-628 joins the boot's. It waits
for the outcome or for caller cancellation, and has no local deadline. One
that was already refused stays refused: its error was the answer. A
terminal already running is untouched.

### 5. The API, the command, and who may call them

- **API.** `POST /api/projects/{projectId}/sandboxes/{sandboxId}/reprovision`,
  with no body. The control plane proxies it to the box's agent, as it proxies
  a service restart (`server/internal/server/sandbox_agent_terminals_proxy.go`),
  as an action use. The agent answers `202` with the document as it stands
  once the run has started or been joined, and the reprovision continues
  whether or not the caller stays. A box that is not running is a `409`:
  there is no agent to run in, and its next start provisions anyway. A
  reprovision never starts a box. A box with no provisioning (not Linux) is a
  `409` too.
- **CLI.** `discobox admin box reprovision DISCOBOX_ID`, beside `restart`
  and `repair` under `discobox admin box`: it is an advanced command, not
  part of the everyday top level (owner, on #1026). It sends the request,
  then follows the sandbox's recorded `agentStatus.provision` (0060's
  discipline: each read is the current truth) and narrates the features as
  they move. It exits 0 when every feature it ran is `ready`. When one is
  not, it exits non-zero and names each feature still `failed`, with its
  error and log path. `--no-wait` returns after the `202`.
- **No console surface.** The console (TUI) neither offers reprovision nor
  binds a key to it, and keeps showing a failed feature as it does today
  (owner, on #1026).
- **In the box.** `discobox-provision retry` is the same request, made from
  inside the box. A box's processes run as its user and the steps need root,
  so the personality asks the agent through a request file in a sticky
  directory under `/run/discobox`, as `discobox-power` asks for sleep. It then
  waits on `provision.json` as `discobox-provision wait` does.
- **Who.** A person who may act on the box (a project member), and a box over
  the boxes it leads: the route joins the sandbox role's `withinCaller` set
  beside `restart` and `repair` (`server/internal/auth/authorizers.go`). The box
  itself may reprovision itself in the box. That grants nothing new: the run
  applies only bundles the box is already pinned to, with the feature
  settings it was created with, and a box can already rerun the same steps by
  getting its agent restarted.

### 6. Progress is the document, and a change in it wakes the pool

What a reprovision is doing is `provision.json`, as for the boot's run: each
feature it runs moves `failed` → `provisioning` (with its `step`) → `ready`, or
`failed` again with the new error. No new phase or field is added.

So that a person watching sees the move within seconds rather than at the
next 15-second check-in, `GET .../status/wait`'s version also moves when the
document's state, its readiness, or any feature's state changes. A step
moving within a feature does not move it. That narrows 26-11-09-062 §2. Its
reason for keeping program and git status off the wait, that they change
constantly, does not apply: a box's provisioning changes state a handful of
times per run. The pool's two-second wake spacing (815 §3) bounds it like any
other change. The boot's own run benefits the same way.

### 7. Container and machine boxes alike; pools are not boxes this runs on

The run lives in the sandbox agent, which every sandbox runs, whether its pool
is Docker or a machine pool (953 §2: one maker contract, one agent). So
reprovision behaves the same on both. Nothing in a pool agent or provider
changes beyond proxying the route.

A pool box is out of scope. Its provisioning is the one-shot
`discobox-provision apply`, re-run by its provider's reconcile when its list
moves (953 §5), and it runs no sandbox agent to receive the request.

## Alternatives rejected

- **Restart the box, and call that reprovision.** It works today, because the
  next start reruns the unrecorded steps. But it kills every process in the
  box, including the terminal a person or a harness is working in. The ask is
  exactly to fix one feature without that.
- **Restart only the agent's process.** A new process makes a new tracker and
  a new run, and running terminals survive it (26-11-06-628 lets them bypass
  the gate). But it drops every attach, tunnel and files API connection the
  agent serves, and it re-plans and re-checks every ready feature, so a `check`
  that drifted reinstalls something nobody asked about. It also gives the
  caller no request to answer: the process that took it is gone.
- **Route it through repair, or a lighter repair intent.** Repair is
  lifecycle intent converged by the reconciler (0035), and the box's
  provisioning is the agent's own, per start, with no record in the control
  plane. A lifecycle intent whose meaning is "tell the agent to run again"
  would be the operation-queue-as-state 0035 rejected for `repairing`.
- **Every feature, ready ones included.** That is a drift repair, and the
  next start of the box already does it. Rerunning a ready feature restarts
  the units its steps lay (`try-restart`), such as dockerd, under a box
  that is working. The owner asked for the failed ones.
- **A named feature, as well as the failed ones.** For a failed feature it
  adds nothing: all of them should run. For a ready one it is the drift
  repair above, one feature at a time, and it needs a rule for the features
  that require it. Deferred (below).
- **Refuse a reprovision while a run is in progress.** It makes the call
  racy for a client that cannot know whether the boot's async run has
  finished. Joining gives the same outcome with no error to handle.
- **A new progress channel, or a frame on the request.** 0060 rejected both
  for the boot's run, and the document already says everything a
  reprovision does. Waking the pool on its changes (§6) removes the only real
  cost, the 15-second lag.

## Deferred

- **Reprovisioning one named feature, ready or not**, or every feature. That
  would also cover a feature that drifted while ready. Revisit when someone
  needs to redo a ready feature without restarting the box.
- **Reprovisioning automatically**, by the agent retrying a failed feature
  whose error looks transient. The Go installer already retries inside its
  step. Revisit if failed features turn out to be mostly transient network
  errors that a person then has to reprovision by hand.
- **Reprovision in the console**, as repair is offered on `R` (0064 §4).
  The owner kept it out for now, because it is an advanced feature. Revisit
  if people reach for it often enough that the console should offer it
  wherever it shows a failed feature.

## Open, for implementation

- How the in-box request file is laid out and claimed (§5). The precedent is
  `/run/discobox/sleep`; whether `retry` reuses the agent's existing watch
  loop or adds its own is an implementation choice.
