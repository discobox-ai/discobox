# 26-10-12-711 — Using a failed discobox with no container repairs it once, and the server decides what a request starts

- **Status**: Accepted (§7's keeping of the repair operation out of the sandbox role superseded by [26-10-14-320](26-10-14-320-a-discobox-acts-on-any-discobox-in-its-project-and-the-judge-decides-which.md))
- **Date**: 2026-10-12
- **Supersedes**: where [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §12 places the *decision* to start a sandbox on use (the pool agent's
  per-route table). Its first two reasons stand, and so the start still *runs*
  in the pool agent.
- **Amends**: [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §9 (an action, and a start or restart instruction, can now lead to recorded
  intent); [0035](0035-repair-is-one-rebuild-intent-plus-a-start-instruction.md)
  (repair gains an automatic author) and
  [0064](0064-repair-rebuilds-on-the-current-image.md) (that author re-pins
  only under an `automatic` upgrade policy);
  [0039](0039-attach-waits-for-readiness-at-every-tier.md) (tier 1 waits for
  every action, not only attach, and tier 2 no longer waits for a container).
  [26-10-11-657](26-10-11-657-a-stopped-discobox-is-read-from-what-it-last-posted.md) §4 (which reads never start a sandbox is marked on the request by
  the control plane, not registered per route in the pool agent; what those
  reads are answered with is unchanged).
- **Issue**: [#154](https://github.com/discobox-ai/discobox/issues/154)

## Context

On 2026-10-10 a harness config change auto-upgraded three stopped discoboxes
(ADR 0082). Each upgrade's replacing create removed the old container, and the
pool agent was itself replaced before it built the new one. The reconciler
recorded the resulting `EOF` as the create's answer: `failed`, generations
equal, no container.

Nothing recovers that today:

- A settled failure is never retried (ADR 0017 §4). The pool reports the
  container missing on every complete sync, and `ensure` returns early for a
  row with a latched error.
- Start is not intent (ADR 0017 §9). It is forwarded to the pool, which has
  nothing to start: `409 … it has no container on its pool`.
- Repair (ADR 0035) is the only thing that records new intent, and a person
  has to know to run it.

A create the pool never answered is being made to retry separately, which
removes that cause. This ADR is the backstop for a failure that still settles.

Where a request starts a stopped sandbox is decided today by the pool agent,
per route (`autoStart`, `requireRunning`). ADR 0017 §12 put it there because
the agent knows the container's true state, serialises a start against a stop,
and served some routes directly. The third no longer holds: every route that
starts a sandbox sits behind the control plane's token, and the one direct path
is a sandbox fetching its own origin, which is running by definition. The pool
agent cannot record intent, so a decision that may end in a repair cannot be
made there.

## Decision

### 1. A request has a use, and the server decides by it

Every route into a sandbox acquires its pool client at one choke point
(`AcquireSandboxHTTPClient`). The acquire takes the caller's **use**:

| Use | Routes | Starts a stopped sandbox | Repairs a failed one with no container |
| --- | --- | --- | --- |
| action | exec attach, create, input, start, delete; service start, stop, restart; HTTP, tcp and udp traffic; ssh; the worktree's git; the judge; harness configure; meta write | yes | yes, once |
| read | exec logs, events, resources; service logs | yes, as before | no |
| inspect | exec list and get; service list and get; tools | no: it waits out a boot something else began | no |
| observe | terminal screen, exec wait, exec-events, harness-hooks | no, and a boot under way is refused too | no |

A sandbox's origin repositories are served by the pool, not the sandbox, so
that route is a read: it repairs nothing and waits for nothing, and the pool
agent starts a stopped sandbox for it as it did before.

The inspects and the screen are the reads ADR 26-10-11-657 answers from the
sandbox's last inventory while it is not running. That ADR had the pool agent
register them, route by route, as never starting. Here they are two more uses
the control plane marks, so the split it drew is kept and the table that
draws it is this one. The reads left are the unbounded ones, which have no
last value to answer with and so still start a sandbox. None of them repairs:
there is no reason to rebuild a sandbox to answer a read.

`POST …/start` and `…/restart` are not acquires, and count as actions for
repair: a start refused for want of a container is the first thing the
incident's user met.

### 2. The pool agent executes, and obeys the mark on the token

- The pool agent keeps the per-sandbox power lock and `EnsureSandboxRunning`,
  and exposes it as `POST …/sandboxes/{id}/ensure-running`. It answers once the
  sandbox agent inside answers. The start instruction is not reused: it returns
  for a container Docker calls running before that agent is up, and it always
  queues on the start gate a cache clear holds.
- For an action the server calls it before handing out the lease. Nothing has
  been forwarded yet, so there is no request body to replay after a repair.
- The server skips the call when the row reads `ready` and `running`: the HTTP
  port proxy acquires per request, and a round trip on each is not worth what
  it finds. A failed sandbox is always asked.
- The signed pool token carries the use as a claim. The agent's one wrapper
  starts the sandbox for `action` and `read`, and requires it running for
  `inspect`, after waiting out a boot under way, and for `observe`, at once.
  So the agent holds no table that could drift from §1. It is also the
  backstop for a stop that lands after the server looked.
- A token with no mark starts nothing. The one such token a current server
  leads to is a sandbox's own, fetching its own origin, and that sandbox is
  running. A server from before the mark has none on any token, so against a
  current agent — a server rolled back under its pools — a stopped sandbox is
  refused until it is started explicitly. A server replaces its pools' agents
  with its own version, which ends that. The alternative, starting on an
  unmarked token, would have this agent starting sandboxes for requests
  nobody classified.
- A pool agent from before the endpoint answers it 404. The server proceeds:
  that agent still starts sandboxes by its own table, and repairs nothing
  until its pool moves onto the current agent.

### 3. No container, on an action, is repaired once

The trigger is the pool answering `ErrNoContainer` to an action's ensure, or
to a start or restart instruction. The pool's `ErrArchived` for a sandbox
whose desired state is `present` is the same answer: a repair's teardown is
the archive's and leaves its marker until the create that follows clears it,
so that is a rebuild between its halves, or one whose create failed.

- **Failed and settled**: record an automatic repair intent, wait for it, and
  ask again. It is ADR 0035's repair — teardown, create, start — and with no
  container the teardown destroys nothing durable.
- **`ready`** (container lost, row not failed): mark it dirty so the ordinary
  rebuild runs now, not at the next complete sync, and wait for it.
- **A generation not yet observed**: a rebuild is already in flight. Wait.
- **Never**: a failed sandbox that kept its container, or one still running.
  Those are not `ErrNoContainer`, and are started and used as before.

A sandbox archived by intent is refused before the pool is asked, and told to
unarchive.

### 4. One, bounded by the generation

The intent records itself on the row (`Sandbox.AutomaticRepair`: its
generation, when, what triggered it, the failure it answers, and when it
landed). An automatic repair is allowed only when the sandbox is `failed`, its
generations agree, and the failure is not that record's own: its generation
is not the record's, or the recorded repair landed and this failure came
after.

- If the automatic repair fails, the sandbox settles on that generation with
  the record not landed, and every later action is refused with both failures
  and the repair command.
- Any explicit intent — a repair, an upgrade — moves the generation on, which
  allows one more.
- Concurrent actions collapse to one: the condition is re-read in the
  transaction that records the intent, and a loser finds a generation not yet
  observed and waits.

There are no timers and no retry counts.

### 5. It re-pins only under an `automatic` upgrade policy

A typed repair re-pins to the harness config's current image (ADR 0064). An
automatic one does the same unless `Project.SandboxUpgradePolicy` is `manual`,
where it rebuilds on the pin the sandbox has: nobody asked for this repair, so
it does not move an image a project chose to hold.

### 6. Every action waits, under the attach wait's stall budget

The server waited only for attach (ADR 0039 tier 1). It now waits for every
action: for a sandbox still being provisioned, and through a rebuild this ADR
set in motion. The bound is the same stall budget — restarted by progress,
with the pool-host ceiling — and the caller's own deadline. A start or restart
instruction waits only once it has found no container.

The pool agent's wait for a container that is being rebuilt (tier 2) is
removed. The server now knows whether a rebuild is coming, and the agent never
could: that wait is what held an attach to a failed sandbox for ninety seconds.
A container the agent retires on start is reported as no container and rebuilt
by the `ready` case above.

Reads, inspects and observes answer at once, as before.

### 7. People and the discoboxes that created it trigger it; reports never do

An action comes from a user, from a discobox acting on one it created
(ADR 26-10-02-478), or from the server acting for a request (the judge). A
report from the pool is an observation, not demand, and acting on "container
gone" every sync is the loop ADR 0017 §4 exists to prevent.

ADR 26-10-02-478 §2 keeps the repair *operation* out of the sandbox role, and
that stands: a discobox cannot ask for one. Its use of a worker may cause the
one automatic repair anybody's use would.

### 8. It is shown while it runs and kept after

- `displayState` gains `repairing`: a repair's generation not yet observed,
  whoever authored it. `discobox ls`, the console, and the attach status line
  show it, with the trigger and the failure when it is automatic.
- `Sandbox.AutomaticRepair` stays on the row after it succeeds, because
  success clears the recorded error. It is replaced by the next one.
- The server logs the repair with who triggered it, as it logs an automatic
  upgrade. The server keeps no lifecycle audit store, and this adds none.
- An ssh session writes a line to its stderr before the shell starts.

## Alternatives rejected

**Let the reconciler retry a settled failure when the pool reports no
container.** It needs no use and no wait. But the report repeats every sync,
so a create that keeps failing is retried forever — ADR 0017 §4's hot loop —
unless a counter or a timer is added, and then the count is the design.

**Make start intent.** `discobox start` would then repair by converging. It
undoes ADR 0017 §9 for every sandbox to serve the failed ones, and stores
"should be running", which the idle stop (ADR 0108) and a host reboot both
contradict.

**Keep the decision in the pool agent and have it ask the server to repair.**
The agent has the request and the true container state. But it would hold the
request, with its body, across a rebuild it does not run, and a pool calling
its control plane to change intent inverts every other call between them.

**Retry counts or backoff.** One attempt per explicit intent needs no state
beyond the generation it is tied to, and tells the user exactly when it will
not try again.

**Reads repair too.** A list or a log read would then rebuild a container
nobody is about to use, and a list is answered from the sandbox's last
inventory without reaching it at all (ADR 26-10-11-657).

**Always re-pin, as a typed repair does.** One code path, and a `manual`
project's sandbox changes image because somebody listed its ports.

**Ensure on every acquire.** Always exact, at one pool round trip per proxied
HTTP request, to find a state the agent's wrapper catches anyway.

## Consequences

- A sandbox whose create failed after its container was removed comes back
  the first time it is used, and says that it did.
- An action on a sandbox still being created waits for it rather than being
  refused, on every route. A client that polled for readiness still works.
- A failed automatic repair leaves the sandbox where a failed typed repair
  would: settled, with the command to run.
- The pool agent no longer decides what starts a sandbox. A new route is
  classified once, in the server.
