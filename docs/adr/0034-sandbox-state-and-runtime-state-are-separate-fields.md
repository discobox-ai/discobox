# 0034 — Sandbox `state` and `runtime_state` are separate fields

- **Status**: Accepted
- **Date**: 2026-08-12
- **Supersedes**: [ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §7's derivation of `displayState` from a single `State` field. §§9–13 stand
  unchanged — this ADR does not revisit whether power is orchestrated (it is
  not), only where the two answers are stored.

## Context

ADR 0017 §9 removed desired power state and §10 gave the pool agent its own
reporting channel, so that a sandbox's existence and its power are decided by
different components. That split was carried through the *operations*: `start`,
`stop`, and `restart` write no lifecycle state, `DesiredState` answers existence
only, and the reconciler creates a container without starting it.

It was never carried through the *storage*. Both answers land in one column,
`ResourceLifecycle.State`, whose sandbox vocabulary is the union of two
disjoint sets owned by two different writers:

| Value | Owner |
| --- | --- |
| `pending`, `awaiting_source`, `archived`, `deleted`, `failed` | reconciler (existence) |
| `starting`, `running`, `stopping`, `stopped` | pool agent state reports (power) |

With one column there is no field to attach an ownership rule to, so the rule
recorded for pools in `resources/pools/DESIGN.md` — *each status field has
exactly one writer* — cannot even be stated for sandboxes.

The consequence was a measured, reproducible lost update. `SandboxReconciler`
loads a sandbox, calls `provider.Create` (~5s: source clone, container start,
waiting on sandbox-agent health), then saves the object it loaded *before* that
call through `Store.UpdateSandbox`, which writes every column. Meanwhile the
agent's Docker-event delta has already recorded `running`. The reconciler's save
puts back the `pending` that was true five seconds earlier — not because it has
an opinion about power, but because power rides in a column it has to write for
its own bookkeeping.

Nothing then corrects the row until the agent's next complete sync, up to
`sandboxStateSyncInterval` (60s) later. Measured on a warm pool with a cached
image, `disco run` against a local repository:

```
+0.11s  pending           row written
+1.23s  starting          agent, before container start
+1.50s  running           agent, Docker start event
~+4.5s                    sandbox-agent healthy — the sandbox is usable here
+5.08s  pending           reconciler's save clobbers the observation
+53.68s running           agent's complete sync heals it
```

`disco run` blocks on `displayState == "running"`, so a sandbox that was ready
in five seconds reported itself as starting for another forty-nine.

Narrowing `UpdateSandbox`'s column list would stop that particular write. It
would not make the ownership boundary visible: the next writer of the row faces
the same trap, `SandboxIsLive` still has to answer an existence question and a
power question from one string, and `displayState` still cannot say "converged,
and not yet observed" because the two facts overwrite each other.

## Decision

**A sandbox has two state fields, each with exactly one writer.**

### 1. `state` is existence, written only by the reconciler

`pending` → `awaiting_source` → `ready`, plus the terminal `failed`,
`archived`, and `deleted`. `ready` is the new value: it means the reconciler
has converged the sandbox's container against its spec. It replaces the
previous arrangement where a converged sandbox's `state` held whatever power
value the agent last reported, which is why convergence had nowhere of its own
to be recorded.

Nothing but `SandboxReconciler` writes it.

### 2. `runtime_state` is power, written only by state reports

`starting`, `running`, `stopping`, `stopped`, and empty for a sandbox no agent
has reported on yet. `Store.ApplySandboxStateReports` is the only writer, and
`Store.UpdateSandbox` explicitly omits the column so no other path can carry a
stale value back.

Empty is a real value and means *not observed*, which is distinct from
`stopped`. It is what a sandbox reads between being recorded and its agent
first reporting on it.

### 3. `awaiting_source` stays on `state`

It reads like a runtime condition and is not one. The reconciler sets it, parks
on it, resumes from it, and derives its give-up deadline from the
`state_changed_at` it stamps. The pool agent has no view of it: from the
runtime's side the container simply exists and is not running, which is exactly
what `runtime_state` will say. It is a phase of creation, so it belongs to the
field creation owns.

### 4. The pool agent reports what it observes at the end of every create

`CreateSandbox` publishes the container's observed state before returning. A
create that starts the sandbox already produced a Docker `start` event; the
one that matters is the create that deliberately does *not* start — an
unarchive, or a rebuild after the container was lost — which previously
produced no observation at all and left the sandbox unreported until the next
complete sync.

This is not the control plane forming an opinion about power. It is the one
component that can see the container saying what it sees, on the channel that
already exists for exactly that.

### 5. `displayState` composes the two

It remains the only user-facing vocabulary and its output is unchanged:
`starting`, `running`, `stopping`, `stopped`, `archiving`, `archived`,
`deleting`, `deleted`, `error`. It is no longer close to the identity function
on `state`; it reads the existence axis first and falls through to the runtime
axis:

- error, deleting, archiving, archived, deleted — from `state` and the
  generations, as before
- `pending` / `awaiting_source` → `starting`
- `ready` → `runtime_state`, or `starting` when nothing has been observed yet

The window ADR 0017 §7 could not express — converged but unobserved, and
observed but not yet converged — now has an honest answer on each axis, and
`displayState` collapses both to the phase a caller understands.

### 6. The provider blob is renamed

`Sandbox.RuntimeState` already existed as an opaque provider-owned JSON blob on
column `runtime_state`. It becomes `ProviderState` on `provider_state`, which
is what it always was: state belonging to the provider, not the runtime's power
state. Existing rows are migrated.

### 7. A reported error is not recorded

`PoolSandboxState.error` is removed. `ErrorMessage` is documented as the error
from the generation in `ObservedGeneration` — the reconciler's verdict — and a
second writer of it is the same defect this ADR exists to remove. The field was
never populated by any agent, and it described "why the sandbox reached a
failed state" for a schema whose state enum deliberately has no failed value
(§10: a container that exited looks the same whether it was stopped or died).

## Alternatives rejected

**Keep one column, narrow the writes.** Give `UpdateSandbox` an explicit column
list and forbid the reconciler from writing `state` outside the transitions it
owns — the fix applied to pools in `resources/pools/DESIGN.md`. It stops the
measured bug and is a fraction of the diff.

Rejected because it buys correctness without buying clarity, and the clarity is
the durable part. The rule would live in a `Select` list and a test rather than
in the schema; `SandboxIsLive` would still fuse two axes into one predicate;
and the union enum would still force every reader to know which half of the
vocabulary it is looking at. Pools took this route because a pool's `State` is
genuinely one axis with one owner — the reconciler — and its agent-reported
facts already had their own fields (`Ready`, `Schedulable`, capacity). A
sandbox's two axes have two owners, and the fix that matches its shape is two
fields.

**Split the field on `ResourceLifecycle`, for pools too.** `ResourceLifecycle`
is embedded in both resources, so adding `RuntimeState` there would give pools
one as well. Rejected: a pool has no power axis. Its states are the
reconciler's verdict end to end, and an always-empty field on it would be an
invitation to find a use for it. The runtime axis is declared on `Sandbox`,
where it is true.

**Keep `stopped` as the unobserved value** instead of introducing empty.
Rejected: it makes a sandbox nobody has looked at indistinguishable from one
observed to be down, which is the same conflation this ADR removes one level
up. The distinction has a caller — `displayState` reports a freshly created,
not-yet-observed sandbox as `starting`, and a reported-stopped one as
`stopped`.

**Derive `state` from `runtime_state` on read** rather than storing a `ready`
value, on the theory that "converged" is already implied by
`observedGeneration == generation`. Rejected: those two counters say the
reconciler finished acting on a generation, which is also true of a sandbox
that settled into `failed` or `archived`. Convergence-to-present is a distinct
outcome and gets a value that says so.

## Consequences

- `Sandbox.State` and `Sandbox.RuntimeState` are separately indexed columns,
  with `runtime_state_changed_at` anchoring the runtime axis the way
  `state_changed_at` anchors existence. Retention and source-push deadlines
  keep deriving from `state_changed_at`; nothing derives a deadline from the
  runtime anchor today.
- `model.SandboxIsLive` takes the sandbox rather than a state string, because
  the question spans both fields.
- Existing databases are migrated in place: the provider blob moves to
  `provider_state`, a power value in `state` moves to `runtime_state` and
  leaves `ready` behind.
- API consumers gain `runtime.runtimeState` and `runtime.runtimeStateChangedAt`.
  `runtime.state`'s enum loses the four power values and gains `ready`.
  `displayState` is unchanged, and remains what clients should read.
- The 60-second window in which the control plane misreported a running sandbox
  is closed: convergence and observation no longer overwrite each other, so a
  sandbox reports `running` as soon as both are true.
