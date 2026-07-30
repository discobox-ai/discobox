# 0017 — Orchestration is generation convergence; a resource has state and desired state

- **Status**: Accepted
- **Date**: 2026-07-30
- **Relates to**: [ADR 0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md),
  whose re-pin rule this model would have made correct by construction, and
  whose image-drift check generalizes into §5

## Context

`model.ResourceLifecycle` carries nine fields — `DesiredState`, `Phase`,
`ActiveOperation`, `LastOperationStatus`, `Generation`, `ObservedGeneration`,
`StatusMessage`, `ErrorMessage`, `PhaseChangedAt` — and `Sandbox` adds
`RestartGeneration`/`RestartedGeneration`. The API serves a tenth,
`displayState`, derived from four of them. Three vocabularies reuse the same
words for unrelated facts: `LastOperationStatus == "running"` means an operation
is executing, `Phase == "running"` means the container is up, `DesiredState ==
"running"` means the user wants it up. Reading a resource means constantly
asking "running *what*?"

The model is a holdover from edge-triggered reconciliation, where an operation
was accepted, a durable job was queued for it, and the job carried what to do.
Nothing works that way now. `internal/reconcile` keeps a dirty set keyed
`(resource_type, resource_id)` that coalesces by primary key; `Reconcile(ctx,
id)` receives only an ID and re-reads the row, so no operation payload survives
the trip. `internal/resources/jobs` is a read-only projection of that dirty set
into the legacy `model.Job` shape, whose `MaxAttempts`, `Error`, `Metadata`,
`StartedAt`, and `CompletedAt` are never populated. The engine is already
level-triggered. Only the state model still talks about operations.

The remnants are measurably inert. `ActiveOperation` has exactly one read in the
monorepo — `services/helpers.go`, which copies it into the API response. Nothing
branches on it: not the reconciler's dispatch (which switches on `DesiredState`),
not a concurrency guard, not an idempotency check. `OperationSpec.Operation`
exists solely to set it. `SandboxPhaseProvisioning` is never assigned anywhere.
`starting`, `stopping`, `deleting`, `pending`, and `launching` are written by
`BeginOperation` and never read — `SandboxDisplayState` *synthesizes*
"starting"/"stopping" from `DesiredState` plus `Generation != ObservedGeneration`
rather than reading the phases that exist for exactly that purpose.
`store.ListPoolIDsWithStaleOperations` has no callers.

What is not inert is actively harmful, because `Phase` conflates two kinds of
fact. Most values describe the runtime (`running`, `stopped`). The transient ones
describe an operation in flight. And `failed` describes neither — it records how
the last operation went, overwriting the runtime fact.

That is not hypothetical. Under ADR 0016 §5 a stopped sandbox re-pins to its
harness config's current image on its next start, guarded by `Phase ==
"stopped"`. Sandbox `sbx_h1ssjzhp60emtc2n` failed to start on an image tag that
no longer resolved anywhere; `FailOperation` set `Phase = "failed"`; every
subsequent start therefore skipped the re-pin and retried the same dead
reference. The one mechanism that could rescue it was disabled by the symptom it
existed to rescue.

The codebase already knows this hurts. `FailOperationRetryable` exists solely so
a caller can record a failure *without* the terminal phase — it takes a phase
parameter so pools can land in `offline` and keep reconciling. Pools got the
escape hatch; sandboxes never did. A model that needs an opt-out from its own
failure handling is telling you the failure handling is in the wrong place.

## Decision

### 1. Generation convergence is the only orchestration primitive

`Generation != ObservedGeneration` means the resource needs reconciling. That is
the whole of what the orchestrator knows.

It does not know what changed, what kind of work is pending, or what "converged"
means for any particular resource — only that two numbers disagree. No operation
type, no state value, no per-resource special case reaches it. In particular it
never reads `State` or `DesiredState`: those belong to the resource, not to the
orchestration contract (§2).

Work reaches a reconciler two ways, and only one of them is a mechanism. A dirty
mark is the low-latency path — written in the same transaction as an intent, or
by an observation that wants a look taken now. The generation comparison is the
source of truth: a scan that finds `generation > observed_generation` heals any
mark that was never written or was lost. Today the two scanners disagree about
this — pools list unconditionally and are a true backstop, while sandboxes query
`last_operation_status IN (pending, running)` and therefore never re-drive a
sandbox that drifted but reads `success`. Both become the same generation
comparison, which closes that gap.

The consequence worth stating plainly: once the numbers match, nothing re-drives
the resource on its own. Further work needs new intent, or an observation that
marks it dirty. That is the property §4 depends on.

### 2. Two fields are the orchestration contract; the rest belong to the resource

The split matters more than the field list:

**Orchestration contract** — `Generation`, `ObservedGeneration`. Owned by the
engine's model, read by the engine, identical for every orchestrated resource.

**Resource state** — `State`, `DesiredState`, `ErrorMessage`, `StateChangedAt`.
Owned by the resource, read only by its own reconciler and by the API. The
engine never touches them.

They are named consistently across Sandbox and Pool because consistency helps
readers and lets `displayState` be computed the same way (§7) — not because
anything generic depends on them. Any resource is free to carry whatever state
it needs; the only thing it owes the orchestrator is the two counters. Keeping
them in one embedded struct is a convenience, and the convenience is what
produced the current confusion: fields with a shared home look like a shared
contract.

`Phase` is renamed to `State`, drawn from the same namespace as `DesiredState`.
The rename is the point: "phase" carries the lifecycle-machine baggage this ADR
removes, and the pair should read as one question asked twice — what do you
want, what is true.

`ActiveOperation`, `LastOperationStatus`, `OperationSpec`, `StatusMessage`, and
the `SandboxOperation*`/`PoolOperation*`/`OperationStatus*` constants are
deleted. `BeginOperation` becomes an intent write: set `DesiredState`, bump
`Generation`, clear `ErrorMessage`.

Observed state may hold values desired state cannot — you can observe `pending`
but not request it. Sandbox: `pending`, `awaiting_source`, `running`, `stopped`,
`deleted`, `failed`, against a desired set of `running`, `stopped`, `deleted`.
Pool: `pending`, `registering`, `active`, `offline`, `deleted`, `failed`,
against `active`, `deleted`. Dropped: `provisioning` (never assigned),
`starting`, `stopping`, `deleting`, and `launching` — written, never read, and
already synthesized for display.

`StateChangedAt` survives as the timeout anchor `PhaseChangedAt` is today,
renamed with its field. It is an observed fact — how long this has been true —
not an operation record.

### 3. The reconciler decides when it is done with a generation

There is one reconciler per resource type, and setting `ObservedGeneration =
Generation` is its exclusive right. It means "I have finished acting on this
intent" — which is not the same as "everything worked."

Two outcomes advance it, and one does not:

- **Converged.** The world matches the intent. Record the resulting `State` and
  advance.
- **Finished, and the outcome is failure.** The reconciler has done all it can
  for this intent. Record `State`, the `ErrorMessage`, and advance (§4).
- **Not finished.** Return an error without advancing. The orchestrator retries
  with backoff. This is for transient conditions the reconciler expects to pass.

The distinction between the last two is the reconciler's judgment and nobody
else's. That is the entire reason the orchestrator's contract is two integers:
"can this be retried usefully" is a domain question, and every attempt to answer
it generically is how operation types get reinvented.

The orchestrator's side stays tiny: select where the numbers disagree, invoke,
back off on error, repeat.

### 4. Failure is a settled outcome for one intent, not a stuck resource

A reconciler that has given up on an intent sets `State = failed`, records
`ErrorMessage`, and advances `ObservedGeneration`. The resource is in sync: the
orchestrator asked for this intent to be acted on, and it was — the answer is
"no." Nothing re-drives it, which is correct, because retrying an intent the
reconciler already rejected is how backoff loops turn into hot loops.

`ErrorMessage` is cleared on every accepted intent, so a non-nil error always
describes the generation currently recorded. New intent — start it again, change
its image, upgrade it — bumps `Generation`, and the reconciler gets a fresh
attempt because the numbers now disagree.

This is why `failed` stays in the vocabulary after all the other operation-shaped
values leave. It is a real observation about the resource: this is what became of
it. What it must never be is a *substitute* for the runtime fact, and that
distinction is the actual lesson of the ADR 0016 incident.

**Guards must therefore be written against what they mean, not against a single
state value.** The re-pin guard that wedged `sbx_h1ssjzhp60emtc2n` asked
`State == "stopped"` when it meant "is anything relying on this container right
now." A sandbox in `failed` answers no to that question just as a stopped one
does, and the fix — a `sandboxIsLive` predicate naming the live states — is
correct under this model and stays. The rule generalizes: a guard that names one
state is almost always asking a question that several states answer the same way.

`FailOperation` and `FailOperationRetryable` collapse into one `RecordFailure`.
The retryable variant existed so pools could fail without the terminal phase;
with failure now advancing the generation rather than freezing the resource,
pools and sandboxes need the same single path. `offline` survives as a pool
state — a genuine observation about a host that is expected back, distinct from
a reconciler giving up.

### 5. Convergence is whole-spec, and the runtime compares a spec fingerprint

`State`/`DesiredState` is one dimension of convergence. Image and restart are
others, and under §1 they cannot have their own counters without reintroducing
per-operation bookkeeping through the back door.

So `Generation` versions the **whole** spec — lifecycle intent, image pin,
restart nonce, and anything added later — and the runtime decides drift by
comparing the container against the spec it was built from, not against a coarse
enum. The pool-agent already does this for one field: `containerImageDrifted`
inspects the running container and recreates it when its image no longer matches
the pinned digest. Generalizing that comparison to a fingerprint of the whole
sandbox spec, recorded as a container label alongside the existing
managed/project/pool/sandbox labels, makes image upgrade, restart, and every
future spec change one mechanism instead of three.

**`RestartGeneration` and `RestartedGeneration` are therefore deleted.** Restart
becomes a nonce in the spec: bumping it changes the fingerprint, the fingerprint
no longer matches the container, the container is recreated. This is precisely
what `kubectl rollout restart` does — it writes an annotation whose only job is
to change the pod template hash — with the indirection removed. The imperative
verb that had no declarative expression turns out to have one after all, and it
is the same one that already handles image upgrades.

This keeps the division ADR 0016 established: the server owns policy and states
the desired spec, the runtime owns identity and enforces it.

### 6. Parked is finished, not in-flight

`awaiting_source` is a sandbox that is provisioned and waiting for the client to
push. The reconciler has done everything this intent allows, so it records the
state and advances `ObservedGeneration` — the same "finished" as any other
outcome in §3. It is already written this way today.

What changes is that it stops needing a disguise. Today the sandbox is parked
with its operation left deliberately `running` so the stale-operation scanner
keeps an eye on it, which is why that backstop query is expressed in operation
terms and why it fails to cover drifted-but-idle sandboxes. Under §1 the push
marks the sandbox dirty and the give-up deadline is a timer armed off
`StateChangedAt`, exactly as it is armed off `PhaseChangedAt` today. Neither
needs the resource to pretend it is mid-operation.

### 7. `displayState` is the only user-facing vocabulary

`displayState` stays derived and becomes the single vocabulary the CLI and UI
consume: `starting`, `running`, `stopping`, `stopped`, `deleting`, `deleted`,
`error`. It is computed from `DesiredState`, `State`, whether the generations
agree, and whether an error is set. It already synthesizes the transient values
§2 removes, so its output does not change when the internal fields do.

This is where operation-shaped language legitimately survives — as the
UX-generated value it always effectively was. "Starting" is a fact about the
user's screen, not about the resource. The internal fields stop trying to be
user-facing, and the user-facing field stops being reverse-engineered from five
internal ones.

The CLI wait loop, which reads raw `phase` plus `lastOperationStatus`, moves to
`displayState` plus `errorMessage` — what it was approximating.

### 8. Only Sandbox and Pool are orchestrated

They are the only models embedding `ResourceLifecycle`, and the only ones with a
spec, a status, and convergence between them.

`HarnessConfig` is registered with the reconcile engine but is not an
orchestrated resource: its `Reconcile` is a TTL reaper that deletes a configure
sandbox the client abandoned, then goes quiet. No desired state, no observed
state, no generation, nothing that converges. It is registered to borrow
`MarkDirtyAt` as a timer that survives a server restart.

It stays as-is and does not gain `ResourceLifecycle`; its `Configured bool` plus
self-armed timer is already the smaller, correct model. It is named here so the
registration list is not misread as evidence that harness configs are
orchestrated — a reconciler that is really a cron job makes the state model look
more general than it needs to be, and under §1 it is now visibly outside the
model rather than awkwardly inside it.

## API shape

`SandboxRuntime` and the pool lifecycle block lose `activeOperation`,
`lastOperationStatus`, `statusMessage`, `restartGeneration`, and
`restartedGeneration`; `phase` becomes `state` with the §2 vocabulary and
`phaseChangedAt` becomes `stateChangedAt`. `desiredState`, `generation`,
`observedGeneration`, `errorMessage`, and `displayState` are unchanged.

This is a breaking wire change. `api/openapi/server.yaml` is a single
unversioned spec (`version: 0.1.0`) with one generated client, and the enum
cross-checks in `model/enum_contract_test.go` and `model/enumsync_test.go` fail
loudly until regenerated — the intended safety net, since a phase value missing
from a generated enum has broken `pool ls` before.

Two consumers are easy to miss. Project events marshal the raw `model.Sandbox`,
so the lifecycle JSON tags are a second, undeclared wire format
(`store/resource_events.go`). And the `enum:` struct tag on the shared
`ResourceLifecycle` is a union matching neither resource, which is why
`enumsync_test.go` exempts all four lifecycle fields on both models; with
per-resource vocabularies still divergent, those exemptions stay.

## Alternatives rejected

**Keep `ActiveOperation` as UX metadata.** Honest about what the field is, and
it would preserve the API surface. Rejected because `displayState` already
serves that need from derived data, and a stored field only the UI reads is one
every write path must maintain correctly with no test that fails when someone
forgets — nothing branches on it. Derived beats stored, for the same reason ADR
0016 §2 derives upgrade availability rather than caching it.

**Retry forever; never let a resource settle as failed.** Attractive because it
self-heals: the sandbox that could not pull its image would recover on its own
once the image existed. Rejected because it makes the orchestrator guess at a
domain question. Most failures are not transient, and a system with no way to
say "I am done, and the answer is no" either spins on hopeless work or grows a
generic retry-classification layer — which is an operation type wearing a
different hat. §3 gives the reconciler the judgment instead, and new intent is
the retry.

**Keep `failed` out of the state vocabulary entirely.** An earlier draft of this
ADR did, on the theory that failure should never overwrite the runtime fact.
Rejected: with §4's semantics `failed` *is* the runtime fact — it is what became
of the resource under that intent — and removing it only pushes the same
information into an error string that guards cannot read. The real defect was
guards asking `State == "stopped"` when they meant "is anything relying on this",
and §4 addresses that directly.

**Keep `RestartGeneration`/`RestartedGeneration` as a counter pair.** It already
mirrors `Generation`/`ObservedGeneration` and needs no fingerprint machinery.
Rejected because it is a second, parallel convergence channel that the
orchestrator must not know about (§1) and that every future spec dimension would
want to copy. One fingerprint comparison covers restart, image, and whatever
comes next; two counters cover restart only.

**A `state` enum shared verbatim between desired and observed.** Tempting for
symmetry, and wrong: `pending`, `awaiting_source`, `registering`, and `offline`
are observations no client can request. Forcing one vocabulary would either
leak unrequestable values into the request schema or push real observations into
an error string.

**Nest `spec` and `status` structs in the API.** Closest to Kubernetes, rejected
as churn without benefit at this size: two resources, a flat GORM embedding that
works, and an API that already groups sandbox lifecycle under `runtime`. The
conceptual split was what was missing, not the nesting.

**Do nothing and document the axes.** Cheapest, and genuinely on the table.
Rejected because the model has already produced one production wedge, and
documentation does not stop `State == "stopped"` from being the natural and
wrong thing to write.

## Consequences

- Ten lifecycle-ish fields become six, and only two of them are the orchestration
  contract. Two of the three overlapping vocabularies disappear; `running` means
  one thing per axis.
- The orchestrator's contract shrinks to one comparison and a backoff loop, with
  no resource-specific knowledge. Both scanners collapse to the same query.
- Sandboxes gain a lost-mark backstop they do not currently have: drifted but
  idle sandboxes are re-driven. A behavior change, not a refactor.
- A failed resource settles instead of spinning, and retry is an explicit user
  or system action that bumps the generation. Anything wanting automatic
  recovery — a pool whose host reappears, a sandbox whose image becomes
  pullable — must observe that and mark the resource dirty; it will not happen
  by itself. That is a deliberate trade against hot-looping on hopeless work.
- Guards keep needing care: `failed` remains a state, so a check written as
  `State == "stopped"` is still the wrong question. `sandboxIsLive` stays, and
  the ADR names the rule rather than relying on the field to enforce it.
- Restart stops being special. The CLI's restart path bumps a spec nonce like
  any other spec edit, and the pool-agent recreates on fingerprint drift rather
  than on a dedicated counter.
- Breaking API change, caught by the enum contract tests. Blast radius is
  roughly 95 non-generated Go call sites across ~20 files in `server/` and
  `cli/`, ~230 test references, and 5 Bats assertions — two of which query
  `last_operation_status` in SQL and must move to generations.
- Persisted rows carry `active_operation`, `last_operation_status`,
  `restart_generation`, and `restarted_generation` columns the model no longer
  has, and stored `phase` values including `failed`. The read-time
  interpretation or migration is part of the implementation, not deferred.

## Deferred

- **Conditions.** Kubernetes carries `[]Condition` rather than one
  `ErrorMessage`, expressing overlapping facts like "reachable but not ready"
  that a single observed state cannot. Not adopted: with two resources and one
  failure mode each, conditions are ceremony. Revisit when a resource must
  report two independent health facts at once.
- **A scheduled-work API.** If a second non-orchestration user of `MarkDirtyAt`
  appears, give delayed work its own registration rather than dressing it as a
  reconciler. One caller does not justify it (§8).
- **Splitting the shared struct per resource.** `Pool` and `Sandbox` have
  disjoint observed vocabularies inside one embedded type whose declared `enum:`
  matches neither. Revisit if a third orchestrated resource appears.
- **Automatic recovery from settled failures.** §4 stops retrying once a
  reconciler gives up, so a resource that would succeed now stays failed until
  something bumps its generation. Observations that already exist — pool
  heartbeats, sandbox-removed reports, harness image refresh — could mark such
  resources dirty and give them a fresh attempt. Left out because "which
  observations should un-stick which failures" is exactly the domain judgment
  §3 keeps out of the engine, and it should be designed per resource once there
  is a second example.
