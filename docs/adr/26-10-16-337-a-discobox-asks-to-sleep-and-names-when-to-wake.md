# 26-10-16-337 — A discobox asks to sleep and names when to wake, and the control plane powers it

- **Status**: Proposed
- **Date**: 2026-10-16
- **Supersedes**: in [0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md),
  §1's power-off by the sandbox itself, and its rejection of the server taking
  part. The sandbox still decides when it is idle; it no longer carries the
  decision out.
- **Amends**: [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §9 (as amended by [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md)):
  the server may hold one scheduled start per sandbox, sent once (§6).
- **Relates to**: [ADR 26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md),
  under which the server decides what starts a sandbox;
  [ADR 0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  the status channel, whose last report the server keeps
  (`runtime.agentStatus`);
  [ADR 26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md),
  the runtime-config intake;
  [ADR 0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md),
  revive in place.

## Context

An agent that has finished for now, but has to look again later — at CI, at a
review, at a long job elsewhere — has two options today. It can hold a
keepalive lease and burn compute until then, or it can let the sandbox go idle
and stop, and nothing will start it again until a person or a lead does. It
cannot say "stop now" and it cannot say "start me at 14:00".

Stopping now is not the idle policy run early. An agent going to sleep wants
the box off even while a person is attached: an attached console left open
overnight is the common case, and it holds the box up for nothing.

Waking at a time cannot be done from inside the sandbox, because at that time
nothing inside it is running. Only the control plane is up to do it.

What a woken agent does is the harness's business. Claude Code, Codex and
opencode each resume their own way, and only the harness's wrapper knows how
to turn "you were woken" into a turn. Discobox can start the box and tell the
harness it was woken; it cannot say anything to the agent that means the same
thing in every harness.

Disco VM ([26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md))
will be able to freeze a machine rather than shut it down. A frozen sandbox
keeps its memory and its tmpfs: `/run` survives, the harness process is still
the one that went to sleep, and nothing boots. Whether a sleep becomes a
shutdown or a freeze is a property of the runtime, which only the control
plane knows. Today the sandbox powers itself off (ADR 0108 §1), which can only
ever be a shutdown — and on a boxd machine (`discobox-ai/vm`) cannot
happen at all, because powering the guest off from inside does not stop the
machine, so a boxd sandbox never idles out.

Idle and sleep are not the same claim. Idle is the sandbox's own inference that
nothing is going on: no terminal changing, nobody connected, no lease. Nothing
in it is worth keeping in memory. Sleep is an agent saying it will be back:
what it holds in memory — a harness session, a running build, a warm cache —
is worth keeping if the runtime can keep it.

## Decision

**A process in the sandbox asks to sleep and names when to wake, with files as
ADR 0108's leases are. The sandbox reports sleep and idleness on its status,
and the control plane carries them out: an idle sandbox is always stopped; a
sleeping one is put to sleep, which the runtime may do by freezing it or by
stopping it. Either way the runtime first asks the sandbox to confirm, fresh,
and the confirmation hands over the sandbox's next wake, at which the server
sends one start.**

### 1. A sleep request is a file in `/run/discobox/sleep/`

```sh
touch /run/discobox/sleep/agent        # sleep now
```

Every regular file in `/run/discobox/sleep/` (mode 1777, created by the
sandbox agent) whose name does not begin with `.` is a request to power the
sandbox down now. Its contents are ignored.

- **Sleep overrides everything.** A connected client, a keepalive lease, and a
  changing terminal do not hold a sleeping sandbox. An attached client is
  disconnected by the power-down, as by any stop. Sleep is what an agent says
  when it knows better than the activity signals, and the case it exists for —
  a console left attached — is exactly the one they get wrong.
- **A request is consumed when it is carried out.** The sandbox agent removes
  the request files when it confirms a sleep (§5), before the runtime powers
  anything down. A frozen sandbox therefore resumes with no sleep pending,
  though its `/run` survived, and a new `touch` is a new request.
- **`/run`, as leases are.** A shut-down sandbox boots with no sleep pending
  even if one was never consumed.

### 2. A wake is a file in `~/.discobox/wake/`, its mtime the time

```sh
discobox-power wake ci --in 45m --note 'check PR 412 CI'
# or by hand — set the time before the file is in the directory:
echo 'check PR 412 CI' > ~/.discobox/wake/.ci
touch -d '+45 min' ~/.discobox/wake/.ci && mv ~/.discobox/wake/.ci ~/.discobox/wake/ci
rm ~/.discobox/wake/ci                           # cancel
```

Every regular file in `~/.discobox/wake/` in the run user's home whose name
does not begin with `.` is a wake. Its mtime is the time to wake, and the
earliest one in the future is the sandbox's next wake.

- **The home, not `/run`.** A wake has to survive the stop it outlives. It
  also has to survive an unscheduled start: a person who attaches before the
  wake time boots the sandbox, and the wake must still be there when it is
  powered down again.
- **A past mtime is due.** A due wake is handed to the harness at the next
  boot and then moved aside (§7); while it waits for one, the next power-down
  is followed at once by a start (§§5–6). A file is put in place with its time already
  set, which is why a name beginning with `.` is ignored: a wake written and
  then dated would otherwise be due the moment it was written.
- **Contents belong to the harness.** Discobox does not read them. They are
  there so an agent can leave itself a note that its wrapper may hand back.
- **No limit.** A wake may be any time in the future.

### 3. `discobox-power` writes these files, and `touch` still works

The sandbox image ships `discobox-power` (`sandbox-agent/cmd/discobox-power`),
which every harness is told about through the image's `discobox` skill:

```sh
discobox-power sleep [--wake-in 45m | --wake-at 14:00] [--note TEXT]
discobox-power wake  NAME (--in DUR | --at TIME) [--note TEXT]
discobox-power wake  ls | rm NAME
discobox-power keepalive NAME (--for DUR | --until TIME) | rm NAME
discobox-power status
```

Its only effect is writing the files of §§1–2 and ADR 0108 §4, each put in
place with its time already set; `status` reads them back with the idle
policy's view from the sandbox agent. The files remain the interface.
`discobox-power` is a convenience over them, so a shell with `touch` and `mv`
can do everything it can, and nothing reads state that only it writes.
`sleep --wake-in` writes the wake before the sleep.

### 4. The sandbox reports sleep, idleness, and wake on its status

`SandboxAgentAutostopStatus` gains:

- `sleep`: present while a sleep request is pending, with `requestedAt` (the
  latest request's mtime) and `request`, its file name.
- `idle`: true when the idle policy has found the sandbox idle for its
  timeout — the point at which ADR 0108 powered off.
- `wakeAt`: the earliest wake in the future, with `wake`, its file name.

The idle policy keeps every rule of ADR 0108 §2 and ADR 0124 and changes only
its last step: where it powered off, it now reports `idle`. It is re-evaluated
on every read, so a client connecting withdraws it.

The file names in `sleep.request` and `wakeAt.wake` are text the sandbox
chose, as every field of this object is: a client prints them under
`cli/REVIEW.md`'s rule that text a discobox wrote is escaped before it is
printed, and nothing branches on them.

### 5. The runtime powers the sandbox down only on a fresh confirmation

When a status reaches the server carrying `sleep` or `idle: true`, the server
marks the sandbox dirty, and its reconciler sends the runtime **power-down**,
a new instruction with a reason (ADR 0017 §9):

- reason **sleep**, for a `sleep`. A runtime that can freeze freezes; every
  other runtime stops. The runtime decides, because only it knows whether it
  can.
- reason **idle**, for `idle`: a stop, on every runtime. Nothing is going on
  in an idle sandbox, so nothing in its memory is worth the host's memory to
  keep.

`sleep` wins when both are reported.

Power-down is its own instruction, not a mode of stop. Stop is a person's or
a lead's and is never refused; power-down is the sandbox's own request and is
refused unless the sandbox confirms it. It is a required method of the
provider contract, implemented by every provider and by the pool agent's
power routes, beside start, stop and restart.

The stored status is up to a poll and a push old, and stays stored after the
sandbox stops, so it is never acted on as it stands. Under its per-sandbox
power lock, the runtime first calls the sandbox agent's new confirm route,
naming the reason:

- **idle** is confirmed only if the policy, evaluated now, finds the sandbox
  idle. A client who attached since the report gets a refusal, and the
  sandbox stays up.
- **sleep** is confirmed if any sleep request is pending now. Confirming
  consumes every pending request (§1).

The confirmation answers with the sandbox's earliest pending wake, read at
that moment — one in the future, or one already due that no boot has yet
handed to the harness (§7) — and power-down returns it.
A refusal, or a sandbox that is not running, ends the instruction with no
power change; a stored status that is stale, including one left from before
the sandbox last stopped, is refused this way rather than tracked. A sleep
whose power-down then fails leaves the sandbox up with its request consumed,
and the idle policy stops it in time like any other.

### 6. The server holds one scheduled start, sent once

The wake a power-down returns is the sandbox's scheduled start. The server
records it on the sandbox's row as `wakeAt`, a new nullable column (null, as
every existing row has, is no wake; no backfill).

- **The sandbox reconciler sets it**, in the reconcile that sent the
  power-down, and returns `Result.RequeueAt(wakeAt)`. That is not a mark: the
  reconciler does not mark the sandbox it is reconciling
  (`server/internal/reconcile` refuses a self-mark), and requeueing is how it
  asks to be called again. A wake already past is requeued for now. It sets
  it only if no instruction from outside has landed since that reconcile
  read the sandbox, so a person's stop that lands mid-power-down is not
  undone by the wake it raced. The guard is `wakeEpoch`, a second new column
  (zero on existing rows) that only the power path moves: every instruction
  from outside increments it, in the transaction that clears `wakeAt`,
  whether or not a wake was set — the race this closes is usually one where
  the wake does not exist yet. The reconciler's write is conditional on the
  value it read before sending the power-down. Not
  `Generation`, which is desired-state intent that a power instruction never
  writes (ADR 0017 §9), and which an unrelated spec edit would move, dropping
  a wake the sandbox did ask for. This is the guard
  `UpdateSecretValueIfUnchanged` already uses. Both columns have more than
  one writer, so both are omitted from `Store.UpdateSandbox`'s whole-row
  write and written only by their own guarded store methods
  (`server/internal/store/DESIGN.md`, field ownership on whole-row writes).
- **Every sandbox reconcile keeps the timer.** A reconcile that returns plain
  success drops the sandbox's dirty row, and one that requeues replaces its
  time, so any reconcile of a sandbox with `wakeAt` set — a secret rebind, an
  upgrade, a spec edit — requeues for the earlier of `wakeAt` and its own
  deadline. The stored column is the wake; the requeue is only how the
  reconciler is called at it.
- **The sandbox reconciler acts on it** when requeued at that time: if the
  sandbox is still powered down, it first claims the wake — clears `wakeAt`
  on the condition that `wakeEpoch` is still the value it read — and only if
  that write succeeds sends the start a person's `discobox start` sends. A
  person's stop that lands between the read and the start moves the epoch,
  so the claim fails and no start is sent. A sandbox already running at its
  wake time has `wakeAt` cleared the same way and is not started: the timer
  is spent, and a wake still due inside it reaches the server again through
  the next power-down (§§5, 7). The start is sent once: a start that fails is recorded as any failed start is,
  and nothing retries it ([26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md)
  rejected a retrying start for the loop it makes of a start that keeps
  failing).
- **The power path clears it** (`power.go`), with `MarkDirtyTx` in the same
  transaction, on every instruction from outside — a person's or a lead's
  `start`, `stop` or `restart`, an archive. A wake is the sandbox's own
request to come back from a power-down it asked for or earned, and a stop
from outside overrides it: the agent does not get the run time back that a
person took away. A sandbox started early by use keeps its wake file, and it
is scheduled again from the next confirmation.

The scheduled start is the control plane's own, as an auto-start is under
26-10-12-711; it acts on nobody's authority but the power-down it follows.

### 7. A wake that falls due is handed to the harness

At boot, before it ensures the primary harness terminal
(`terminal.Service.EnsurePrimary`), the sandbox agent collects the wakes that
are due — mtime past — moves each to `~/.discobox/wake/.done/`, and launches
the primary with `DISCOBOX_WAKE` set to the moved files' paths, oldest first,
separated by newlines. What a wrapper does with it is its own: one may resume
its session with the notes as the prompt, another may ignore it. An
environment variable, not an argument, because the relaunch command is the
image's own (`harness.Image.RelaunchCommand`) and an argument it never agreed
to take can break it, where an unknown variable is ignored by every program.

A boot is the only place a wake is handed over. A wake that falls due while
the sandbox is running — it was started early by use, and the harness was
launched before the wake was due — stays in place, due and undelivered. The
next power-down's confirmation returns it (§5), so the server starts the
sandbox again as soon as it is down, and that boot hands it over. The agent
that asked to wake at 14:00 is woken, late, rather than never.

### 8. The change reaches a pool before the control plane relies on it

Both documents involved refuse fields they do not know: the status object
(`additionalProperties: false`), and the runtime config's `agent` section,
whose delivery is refused whole — secrets, sources and all — over one unknown
field. So each side sends the new thing only to a peer that has said it
knows it:

- The sandbox agent reports `autostop.controlPlanePower: true`, meaning it
  can hand its power to the control plane. Like every field added to the
  status object, it relies on servers accepting it before agents send it.
- The server sets `agent.controlPlanePower` in the runtime config
  (`sandboxconfig.RuntimeConfig`, where `agent.idleTimeout` travels, ADR
  26-10-08-127) only for a sandbox whose last status advertised it. An older
  agent is never sent a field it would refuse the delivery for.
- The sandbox agent powers itself off as before until it receives the flag,
  and reports `sleep`, `idle` and `wakeAt` only after. A new agent behind a
  server that does not act on them still idles out by itself.

## Alternatives rejected

**The sandbox keeps powering itself off, and only wake moves to the control
plane.** It is the smaller change and keeps ADR 0108's property that idle stop
needs nothing outside the sandbox. Rejected because a sandbox can only power
itself off one way, and on boxd not at all. A freeze is a runtime operation
the sandbox cannot perform on itself, and a sleep that always shuts down would
throw away the frozen harness that is the reason to freeze.

**Idle and sleep are one power-down, and the runtime may freeze either.** It
would make an idle sandbox resume instantly. Rejected because a frozen
sandbox still holds its memory on the host, and an idle one has, by the
policy's own evidence, nothing in it worth holding. Freezing is worth its cost
only when someone has said they are coming back, and sleep is that statement.

**The server acts on the stored status as it stands.** ADR 0108 rejected
server reaping for reaping on stale data, and the objection holds: a person
who attached inside the poll window would be cut off. The confirmation
(§5) makes the decision on data read under the lock that serializes it with
every start.

**The pool agent decides, from its own poll.** It reads the status first-hand
and holds the power lock. Rejected because it cannot hold a wake that outlasts
it — it keeps no durable state — and a disco-vm machine is run by the server
(26-10-09-106), not by a pool agent. With the confirmation, nothing is gained
by deciding there: the runtime under its lock has the last word either way.

**The server records which sleep it acted on.** Needed if the request
survives a freeze. Consuming the request on confirmation is first-hand and
needs no second record that could disagree with the sandbox's files.

**Wake retried until it succeeds.** Rejected by 26-10-12-711's reasoning: a
start that keeps failing becomes a loop that holds intent nobody can see.

**`discobox-power` calls the discobox API instead of writing files.** It would
act at once rather than at the next status poll, and report errors. Rejected
because it puts a judged API call (ADR 0140) between an agent and its own
sleep, needs the API reachable to schedule anything, and gives the files of
ADR 0108 a second, different interface beside them.

**Discobox delivers the wake to the agent** — typing the note into the primary
terminal, or a hook event. Rejected: what turns a note into a turn is
different for every harness, and typing into a harness that is mid-prompt or
mid-turn is not safe in general.

**`--wake` on the relaunch command, like `--resume`.** Rejected: `--resume`
is the server's choice under the `discobox-harness-run` convention, and an
image that declares its own relaunch command, or the shell harness, takes no
such argument.

**Sleep yields to an attached client.** Rejected: a console left open is the
commonest thing keeping a finished sandbox up, and an agent asking to sleep is
a stronger signal than an idle connection.

**Wakes in `/run`.** Rejected: a person's start before the wake would clear it,
and the agent's wake would silently vanish.

**A ceiling on how far ahead a wake may be.** Rejected: a wake costs nothing
while the sandbox is down, and a person's stop cancels it (§6).

## Consequences

- Idle stop now depends on the control plane being reachable, which ADR 0108
  avoided. A pool that loses its server keeps its sandboxes running until it
  reconnects. That is the cost of the control plane choosing between stop
  and freeze.
- A boxd sandbox idles out, which it cannot today.
- A sleep or idle stop happens up to one status poll (15 s), a push, and a
  reconcile after it is asked for.
- An attached person is disconnected when an agent sleeps. Their next attach
  starts the sandbox again, and that start does not cancel the wake file.
- A person's stop cancels the scheduled wake; the wake file stays in the
  sandbox and is scheduled again after its next power-down.
- A woken sandbox nobody uses idles out on the pool's timeout like any other.
  An agent that wakes, looks, and has more to wait for writes a new wake and
  sleeps again.
- Each harness wrapper may read `DISCOBOX_WAKE`. One that ignores it relaunches
  as today.
- A wake that falls due while a person is using the sandbox is delivered
  after the next power-down, by an immediate start.
- The API contract gains `sleep`, `idle`, `wakeAt` and `controlPlanePower` on
  `SandboxAgentAutostopStatus` and a confirm route on the sandbox agent; the
  runtime config gains `agent.controlPlanePower`; the sandbox row gains
  `wakeAt` and `wakeEpoch`, by migration; the provider contract and the pool agent's power
  routes gain power-down, with its reason, beside start, stop and restart.

## Deferred

- **Freeze.** Every runtime implements power-down for sleep as stop. Disco
  VM's freeze plugs in behind it. A frozen sandbox resumes without a boot, so
  §7's handover never runs there; how a resumed harness learns it was woken
  is decided with the Disco VM runtime.
- **Waking a running harness.** A wake due while the sandbox runs is not
  delivered until after the next power-down. Revisit when a harness wants a
  signal rather than checking `~/.discobox/wake/` itself.
- **Listing and cancelling a stopped sandbox's wake from outside.** A stop
  cancels it; nothing shows it but the status. Revisit when someone needs to
  see one before it fires.
- **A lead scheduling a worker's wake.** A lead can start and stop its
  workers (26-10-02-478) but not set their wake. Revisit when an orchestrator
  needs it.
