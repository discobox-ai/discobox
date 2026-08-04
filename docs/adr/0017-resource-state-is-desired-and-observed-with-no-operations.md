# 0017 — Orchestration is generation convergence; a resource has state and desired state

- **Status**: Accepted 2026-07-30. **Amendment Accepted 2026-08-04** — §§2, 5, 7
  revised; §§9–13 added.
- **Date**: 2026-07-30
- **Relates to**: [ADR 0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md),
  whose re-pin rule this model would have made correct by construction, and
  whose image-drift check generalizes into §5

## Amendment (2026-08-04)

Nothing has shipped against this ADR — `ActiveOperation`,
`LastOperationStatus`, `PhaseChangedAt`, `RestartGeneration`, and
`RestartedGeneration` are all still on the models — so this is an amendment
rather than a superseding ADR.

The original decision treated power state as desired state: a sandbox's
`DesiredState` could be `running` or `stopped`, and the reconciler converged
toward it. That is wrong, for a reason the original context nearly reached but
did not name. It is being corrected before implementation.

The incident that prompted it: a host restart stopped every sandbox container
on a pool. Nothing observed it, because the reconciler short-circuits on
`Phase == running && ObservedGeneration == Generation && LastOperationStatus ==
success` and never asks the runtime anything, and because the only liveness
signal the pool agent emits is keyed on container *destroy* rather than *death*.
`disco ls` reported nineteen sandboxes as `running` for 41 hours. The provider
interface has a `Get` that would have answered correctly; it has no callers.

The original model would not have fixed this. It removes the operation
vocabulary and adds a lost-mark backstop, but a sandbox whose generations agree
and whose stored state reads `running` is converged under §1 no matter what the
container is doing. The defect is not the operation fields. It is that the
server holds an opinion about power that it has no means to verify.

§§9–13 move power state out of the orchestration model entirely; §§2, 5, and 7
are revised where they assumed it. §§1, 3, 4, 6, and 8 are unchanged and
continue to govern existence and spec convergence.

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

### 2. Two fields are the orchestration contract; the rest belong to the resource *(amended 2026-08-04)*

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
but not request it. Under §9, desired state answers existence and nothing else.

- **Sandbox.** Observed: `pending`, `awaiting_source`, `starting`, `running`,
  `stopping`, `stopped`, `deleted`, `failed`. Desired: `present`, `deleted`.
- **Pool.** Observed: `pending`, `registering`, `active`, `offline`, `deleted`,
  `failed`. Desired: `present`, `deleted`.

Dropped: `provisioning` (never assigned), `deleting`, and `launching` — written,
never read, and already synthesized for display.

`starting` and `stopping` were dropped in the original decision for that same
reason, and they return here on a different justification. They were fake when
the server wrote them from its own dispatch: nothing had been observed, so the
value described the server's place in a code path. They are real when the pool
agent posts them (§10), because a runtime that has begun a start and not
finished it genuinely is in `starting`, and that is a fact only the runtime
holds. A state value is legitimate when someone can observe it, not when
someone finds it convenient to display.

Pool's desired `active` is renamed `present` so both resources say existence the
same way. It is the same concept under two words, which is the confusion this
section exists to remove; observed `active` is untouched, since it is a distinct
and real observation about a registered host.

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

### 5. Convergence is whole-spec, and the runtime compares a spec fingerprint *(amended 2026-08-04)*

Existence is one dimension of convergence. The image pin is another, and under
§1 it cannot have its own counter without reintroducing per-operation
bookkeeping through the back door.

So `Generation` versions the **whole** spec — existence intent, image pin,
resources, sources, and anything added later (§11) — and the runtime decides
drift by comparing the container against the spec it was built from, not against
a coarse enum. The pool-agent already does this for one field:
`containerImageDrifted` inspects the running container and recreates it when its
image no longer matches the pinned digest. Generalizing that comparison to a
fingerprint of the whole sandbox spec, recorded as a container label alongside
the existing managed/project/pool/sandbox labels, makes image upgrade and every
future spec change one mechanism instead of many.

**`RestartGeneration` and `RestartedGeneration` are deleted**, as the original
decision had them, but not for the reason it gave. That draft made restart a
spec nonce — the `kubectl rollout restart` trick, where an annotation exists only
to change a template hash — because the model had no imperative channel to put
restart on and a declarative one had to be invented for it.

§9 supplies that channel, so the trick is unnecessary and, once available, wrong:
a restart changes nothing about what the sandbox *is*, and encoding it as a spec
edit means the spec carries a field whose only content is "somebody pressed a
button." Restart is an operation. The fingerprint covers spec drift only.

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

### 7. `displayState` is the only user-facing vocabulary *(amended 2026-08-04)*

`displayState` stays derived and stays the single vocabulary the CLI and UI
consume: `starting`, `running`, `stopping`, `stopped`, `deleting`, `deleted`,
`error`. Its output is unchanged. What changes is where it gets its answer.

The original derivation synthesized `starting` and `stopping` out of
`DesiredState` plus `Generation != ObservedGeneration` — reverse-engineering a
runtime fact from orchestration bookkeeping, because the runtime fact was not
recorded anywhere. Under §10 it is recorded, by the component that observes it.
So the derivation collapses to close to the identity function on `State`:

- `ErrorMessage` set → `error`
- desired `deleted` and not yet observed → `deleting`
- otherwise → `State`, with `pending` and `awaiting_source` displayed as
  `starting`

Only the existence axis still consults the generations, because existence is
the only thing left that the server converges.

This is where operation-shaped language legitimately survives — but less of it
is synthesized than before. "Starting" turns out to be a fact about the sandbox
after all, once something is willing to observe it; what was never a fact was
the server's inference that a sandbox must be starting because it had recently
been told to start.

The CLI wait loop, which reads raw `phase` plus `lastOperationStatus`, moves to
`displayState` plus `errorMessage` — what it was approximating. Since start,
stop, and restart no longer return state (§9), that loop is now the only way a
client learns an operation's outcome, and it watches project events rather than
polling.

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

### 9. Power state is not orchestrated *(added 2026-08-04)*

**No component holds an opinion that a sandbox should be running.** Not the
server, not the pool agent, not the container runtime's restart policy.

`start`, `stop`, and `restart` are operations. They instruct, they are attempted
once, and whatever the world looks like afterwards is the state. A start that
fails leaves a stopped sandbox and an error, and nothing retries it. There is no
"this sandbox should be running" to converge toward, so there is nothing to
converge. We reconcile later, when something asks us to.

`DesiredState` therefore answers existence only: `present` or `deleted`.
Everything §§1–8 say about generation convergence continues to govern existence
and the spec (§11); none of it applies to power.

Two reasons, and the second is the load-bearing one.

**An opinion the server cannot verify is a lie in waiting.** That is the
incident above, and any design where the server stores a power state and hopes
it stays true will reproduce it in some form.

**Power state is about to become dynamic.** Sandboxes will start and stop on
criteria — idleness, demand, capacity, cost — that no stored intent can
anticipate. A field that says "should be running" is wrong the instant a policy
decides otherwise, and a reconciler reading it would spend its time fighting the
policy that just acted. The two mechanisms would each be certain, and they would
disagree. Deleting the field is what makes those policies expressible at all:
they become callers of `start` and `stop` like any other caller, with no
privileged channel and nothing to overrule them.

This is deliberately *not* the Kubernetes node model, and the difference is
worth being precise about, since §10 borrows the kubelet's reporting shape. A
kubelet is fire-and-forget about instructions because it re-reads a durable pod
spec that says the pod should be running; the desired state is real, it just
lives in one place. Here there is no equivalent. The closer analogy is a
socket-activated service: it runs because something wants it right now, and the
absence of demand is a sufficient reason for it not to be running.

The consequence to state plainly: **a host restart leaves every sandbox
stopped, and nothing brings them back.** They report `stopped`, which is true,
and §12 starts the ones somebody actually uses. That is the intended behavior,
not a gap — see the rejected alternative on durable power intent.

### 10. The pool agent reports state on its own channel *(added 2026-08-04)*

Operations never return state. The response to `start` says the instruction was
accepted and nothing more. State arrives separately, from the pool agent, which
is the only component that can observe it.

- **On transition.** Driven by the Docker event stream, which must cover
  `start`, `die`, `stop`, and `destroy` — not `destroy` alone, as today. A
  start posts `starting` and then `running`; a stop posts `stopping` and then
  `stopped`.
- **On start and on an interval, a complete report.** The full set of
  `{sandbox → state}` the agent hosts, flagged `complete`. A sandbox the server
  believes is on this pool and that a complete report omits is gone. This is the
  level-triggered half and it is what makes the channel correct: a dropped delta
  self-heals at the next sync, where a delta-only channel drifts permanently the
  first time a post fails.
- **Ordering.** Reports carry the agent's boot ID and a per-agent monotonic
  sequence. The server ignores a report older than what it has already recorded,
  so a delayed delta cannot overwrite a newer complete sync.
- **Liveness.** The interval's arrival time is enough to serve as the pool
  heartbeat if we want one; no separate mechanism is being added now.

The existing `/api/pools/{pool_id}/sandbox-removed` route is subsumed — removal
is one observation among several rather than its own endpoint and its own
server-side dance (§13).

Reports are observations, so under §1 they may mark the sandbox dirty. That is
the hook §13 uses to re-drive existence and re-pin, and it is the general answer
to "what wakes a resource up now that power no longer does."

### 11. The spec is an embedded `Manifest` *(added 2026-08-04)*

§5's fingerprint needs a precise answer to "what is the spec." Hand-maintaining
that list is a mechanism that rots silently: someone adds a field, forgets the
list, and containers stop being rebuilt for a change that should rebuild them —
invisible until a sandbox is quietly several images behind.

So each orchestrated resource gets a `Manifest`: an anonymously embedded struct
holding the spec, with observed state, generations, timestamps, and runtime
bookkeeping outside it. The fingerprint is the hash of its canonical JSON, so a
field added to the `Manifest` is in the fingerprint by construction.

The cost is smaller than the "every orchestrated resource" framing suggests.
There are two of them (§8). Anonymous embedding promotes fields in
`encoding/json`, so project events — which marshal the raw model and are a
second, undeclared wire format — stay flat. And the REST shape is a separate
generated type reached through a conversion, so clients see nothing. This is the
same arrangement used in `obot`, where the flat external shape is the point:
the split is a Go-side statement about ownership, not an API redesign.

Membership has one test: **does changing this field require rebuilding the
container?** Yes, it is spec. No, it is status, provenance, or annotation.
`Prompt`, `Model`, `ModelServiceTier`, and `ModelReasoningLevel` are the fields
to examine against that test during implementation — they are user intent, which
makes them feel like spec, but a prompt edit rebuilding a running container is
probably not what anyone wants.

### 12. Auto-start latches in the pool agent *(added 2026-08-04)*

Sandbox-directed traffic starts a stopped sandbox on demand. The latch is in the
pool agent, not the server.

The pool agent is the right place for three reasons: it is the only component
that knows the true container state, so it needs no round trip and cannot race a
cache; it is a single process per pool, so the latch is a mutex rather than a
distributed lock, and an explicit `stop` serializes against a concurrent
auto-start on that same mutex; and it already serves sandbox-directed routes
directly — `git-repositories/*` and `http/{port}/*` — which a server-side gate
would never see.

Which routes latch is a property of the route table: sandbox-directed proxy
routes auto-start, control operations (`pool-get-sandbox`,
`pool-list-sandboxes`, `pool-stop-sandbox`, `pool-delete-sandbox`) never do. A
started sandbox posts `starting` then `running` on the §10 channel like any
other start; there is no separate path for implicit ones.

The pool agent cannot distinguish a user's `disco exec` from a background
poller, and it should not have to. Nothing server-side may poll a sandbox
through the proxy routes; if something ever must, the server marks the request
rather than the agent guessing.

This is the recovery path §9 leaves open. It brings back exactly the sandboxes
someone actually wants, at the moment they want them, with no stored intent
anywhere.

### 13. What moves *(added 2026-08-04)*

Three existing mechanisms are attached to the start reconciler and need new
homes.

**ADR 0016 §5 re-pin.** A non-live sandbox re-pins to its harness config's
current image on its next start, guarded by `sandboxIsLive`. Start is leaving
the reconciler, so the trigger becomes the §10 report that observes a sandbox
leaving a live state: the report marks it dirty, and the reconciler re-pins.
The guard is unchanged and is finally reading an observation instead of a
recollection.

**Worker-observed runtime loss.** The current removal report atomically records
stopped intent, then stop-reconciles by recreating the sandbox from persisted
intent and stopping the recreated runtime — a contortion that exists only to
make a lost container converge against a desired power state. With no desired
power state, a removed container is an observation. If the sandbox is still
`present`, existence reconciliation recreates it; it stays stopped until
something uses it.

**The pool-local API.** `pool-start-sandbox` and `pool-stop-sandbox` return
acceptance rather than a sandbox; `pool-restart-sandbox` is added. The server's
own `POST /sandboxes/{id}/start|stop|restart` do the same, and write no state.

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

*Amended 2026-08-04.* `desiredState` narrows to `present`/`deleted` on both
resources, and `state` gains `starting`/`stopping` on Sandbox (§2). Start, stop,
and restart return acceptance rather than a sandbox, on both the server API and
the pool-local API, and `pool-restart-sandbox` is added (§13); clients learn
outcomes from project events. The pool agent gains an authenticated
control-plane route for sandbox state reports carrying transitions and complete
syncs, and `/api/pools/{pool_id}/sandbox-removed` is removed (§10). The
`Manifest` embedding changes no wire format (§11).

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

### Rejected in the 2026-08-04 amendment

**Durable power intent in the pool agent, via a container restart policy.**
Docker's `unless-stopped` is exactly "this should be running," it survives a
daemon or host restart for free, and setting it would have made the incident
above self-healing rather than merely visible — the containers would have come
back on their own. Today's policy is `no`, which is why they did not. Rejected
because it is the same unverifiable opinion relocated to a cheaper store: the
moment a policy stops a sandbox for idleness, the restart policy is a stale
instruction sitting in the runtime waiting for a reboot to act on. §12 recovers
the same sandboxes on demand, and recovers only the ones somebody wants, which
after a two-day outage is a small fraction of them.

**Keep desired power state, and add observation alongside it.** The smallest
change that fixes the reported bug: leave `DesiredState` as it is, have the pool
agent report reality into a separate observed field, and show the truth. Rejected
because two writers with overlapping authority need a conflict rule, and every
such rule is wrong in one direction — either the reconciler restarts what a
policy just stopped, or the observation is decorative and the server keeps
acting on an opinion nobody honors. Deleting one of the two writers is what
makes the question disappear.

**Let the operation's response carry the new state.** Rejected on the user's
point, and it is the sharper argument: a response cannot express `starting`, so
it either lies about a completed start or blocks until one finishes. And the
transitions that caused the incident — a container dying, a host rebooting, an
OOM kill — have no request to respond to, so the separate channel is required
regardless. Having both means two paths that can disagree, and the redundant one
is the one that only works when someone happens to be asking.

**Delta-only reporting, without periodic complete syncs.** Cheaper, and correct
whenever every post is delivered. Rejected because the failure mode is silent
permanent drift, which is precisely the bug being fixed here, arrived at by a
narrower road.

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

### From the 2026-08-04 amendment

- A sandbox's recorded state is only ever as good as the last report from its
  pool. The staleness that produced the incident is not eliminated, it is
  bounded by the sync interval and made attributable — a pool that stops
  reporting is a visible fact about the pool, not a silent fact about nineteen
  sandboxes. Rendering that staleness to users is left to the pool liveness work
  §10 defers.
- A host restart leaves every sandbox stopped until used (§9). For a pool that
  was hosting long-running agent sessions, that is a real loss of running work
  that a restart policy would have prevented. Accepted deliberately: the
  alternative is a stored intent that the coming start/stop policies would
  immediately contradict.
- The server can no longer answer "did my start succeed" synchronously. Every
  client learns outcomes from project events, which the CLI wait loop already
  does and other consumers may not.
- Auto-start makes any sandbox-directed request potentially expensive — a
  request that used to fail fast against a stopped sandbox now blocks on a
  container start. Callers that cannot tolerate that need a timeout, and the
  latch must not serialize unrelated sandboxes.
- Existence reconciliation and power operations can interleave: a create that
  is still converging can receive a `start`. The pool agent's per-sandbox mutex
  is the serialization point for that, which means the pool agent — not the
  server — owns the ordering guarantee.

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

### Deferred by the 2026-08-04 amendment

- **The criteria that start and stop sandboxes.** §9 exists to make idle-stop,
  demand-start, and capacity policies possible; none of them are decided here.
  They will be callers of the operations, and the test of this ADR is that they
  need no new state to be written.
- **Pool liveness and stale-state rendering.** §10's interval is sufficient to
  detect a silent pool, but what a user sees when a pool has stopped reporting —
  `unknown`, a staleness marker, the last known state with an age — is left
  open. Until it is decided, a silent pool's sandboxes show their last reported
  state, which is a smaller version of the incident and should not stay open
  long.
- **Whether boot ID plus sequence is enough ordering.** §10 assumes a single
  reporting agent per pool. If reports ever originate from more than one place,
  or a sandbox migrates between pools, per-sandbox versioning will need
  revisiting.
