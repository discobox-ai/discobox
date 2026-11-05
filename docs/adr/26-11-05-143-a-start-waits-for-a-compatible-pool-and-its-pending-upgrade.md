# 26-11-05-143 — A start waits for a compatible pool and its pending upgrade

- **Status**: Proposed (would narrow [26-11-04-167](26-11-04-167-an-explicit-start-waits-for-its-pool-to-return.md) §1's missing-runtime exclusion and its statement that restored connectivity does not authorize further waits, and extend §2's ceiling to compatibility waits; would further narrow [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md) §6's instruction wait and [26-11-03-618](26-11-03-618-a-discobox-owed-its-delivery-is-never-started-and-attach-repairs-it-to-deliver.md) §2's no-wait statement only for that upgrade case; source-delivery refusals remain)
- **Date**: 2026-11-05
- **Issue**: [#684](https://github.com/discobox-ai/discobox/issues/684)
- **Preserves**: 26-11-04-167 §§2–4's request budgets, fencing and visible progress except for the additional ceiling trigger below; 0017 §9's power instructions; 0021 §§3–4's replacement power rules; 26-11-03-618's owed-source boundary; scheduled wakes and restart remain single-send outside their existing repair policy.

## Context

The first start after a v0.18.1 upgrade can cross two different transitions.
The server first sends a create containing `pullSources` to the old pool
agent, which rejects the unknown field. Once the new agent answers, start
can find that the sandbox's pending upgrade has not yet created its runtime.
The request returns a missing-runtime 409 although reconciliation later
recovers. Retrying an incompatible create avoids latching a failure but does
not prevent the incompatible request or make the original start succeed.

Checked against `main` at `9750837cb5122c0cf6390f68150a23fb4d62da79`:

- `server/internal/service/service.go` calls `UpgradeReleaseSandboxes`;
  `server/internal/resources/sandboxes/service.go` records stopped-sandbox
  re-pins through `recordSandboxIntent`. The generation comparison alone
  does not say whether pending work is upgrade, first create or repair.
- `server/providers/poolruntime/provider.go`'s `awaitPoolAgentClient`
  accepts a successful client acquisition immediately. Endpoint reachability
  does not establish which create contract that agent understands.
- `server/providers/poolruntime/agent_client.go` builds the new create
  shape, including `pullSources`. The sandbox reconciler preserves an
  unobserved generation on `ErrPoolAgentUnsupported` and requeues it.
- `server/internal/resources/pools/reconciler.go` closes placement through
  `begin` before replacement. This does not guard a create racing before
  `begin`, nor prove the protocol of an already reachable old agent.
- `server/internal/resources/sandboxes/attach_wait.go`'s explicit-start
  policy waits for `ErrPoolNotReachable`, but, unless rebuilding, returns
  other refusals after connectivity recovers. `ErrNotFound` does not mean
  `ErrNoContainer` and must not become permission to repair every 404.

The triage's controlled-provider regression demonstrates the second gap.
The issue reports the real upgrade; this proposal claims no new runtime test.
The directory repair in #677 and bootstrap delays in #679 are separate.

## Decision

### 1. Prove create compatibility before sending the create body

Introduce a small, authenticated pool-local protocol discovery operation in
`pool-agent/api/openapi/pool.yaml`. It reports an explicit create protocol
revision. The server supports an explicit set of revisions; initially only
its own. A revision covers decoding and required create semantics, not the
release version. Any create contract change that an existing decoder or
runtime cannot safely accept advances that revision, including adding a field
that a strict older decoder rejects. Keep the discovery envelope stable so
future revisions remain readable. Unknown revisions never imply compatibility.

Every create, for existing sandboxes as well as initial placement, must use a
lease onto an agent proven compatible with the body it will receive. The
shared pool runtime owns this gate, before `createOnPool` sends anything.
A missing discovery route on a legacy agent, a mismatched revision, or an
agent not yet ready closes the gate. Schedule ordinary pool reconciliation
and retry under the existing bounded create wait; do not send a speculative
create, strip fields, or disable strict unknown-field validation. Initial
placement uses the same gate even if the pool's stored ready/schedulable
flags remain true.

Compatibility is a fact about the serving agent instance. Probe and create
must reach the same instance: bind the lease to that instance's transport,
invalidate it when replacement or reconnect changes the target, and never
transparently retry its create on another target without a fresh probe.
A pool ID, its persistent identity key, a health response, a bundle placement
record or an old heartbeat is insufficient. A cache, if used, belongs only
to that instance and transport lifetime. Test replacement between discovery
and dispatch. If a transport cannot provide this binding, it must establish
one before this gate can be considered implemented.

A discovery 404 means legacy protocol only at this operation; it does not
reclassify arbitrary resource 404s. Authentication and authorization failures
remain errors. Malformed discovery answers are reported as protocol errors,
not retried as ordinary missing runtimes. A reachable but incompatible agent
cannot become compatible through polling alone: reconciliation must apply the
target pool bundle through the existing provider bootstrap path, which must
remain usable without protocol discovery. This avoids a bootstrap cycle.
If replacement fails or compatibility never arrives, the bounded wait ends
with the pool/protocol reason, not a fabricated sandbox failure or success.

This gate controls create traffic. It does not globally declare an old pool
unusable, stop its running sandboxes, or require every read to wait. Runtime
availability and compatibility with the current create contract are separate
facts. Existing error handling for an unexpected unsupported create remains
a defensive retry, not the normal discovery mechanism.

### 2. Identify pending upgrade work explicitly

Record an internal `UpgradeGeneration` on a sandbox when a typed or automatic
upgrade records its re-pin. It equals the generation recorded in the same
transaction, with the dirty mark. It describes existing spec work, never a
power request. A source completion, repair, unarchive, harness change or
initial create must not acquire this marker merely because it increments a
generation. Preserve store field ownership across whole-row writes.

An existing sandbox qualifies for the new start wait only while all hold:

- It is present, assigned to its current pool, and a successful prior create
  is established by durable lifecycle evidence, rather than an assumed
  runtime state or generation number.
- `UpgradeGeneration == Generation > ObservedGeneration`.
- It is not owed client source, not awaiting initial delivery, and has no
  conflicting repair or harness-change intent.
- Its failure is not settled for that generation, and authorization and the
  request's original power fence still permit starting it.

Use existing durable prior-create evidence if sufficient; otherwise add an
explicit last-successful-create marker updated by the successful reconciler.
Do not infer prior creation from `Generation > 1`, `state = failed`, or a
nonempty provider placement alone. Persist new metadata with additive defaults
and safe migrations. Legacy rows default to no upgrade-wait entitlement;
normal upgrade admission in the target server establishes it. Backfill prior
creation only from durable evidence that actually proves it. In particular,
a pre-upgrade row that is `ready`, converged, error-free and not owed source
is evidence of a successful create: capture that evidence under the upgrade
intent transaction before it clears errors or advances the generation. This
lets the seeded stopped v0.18.1 box qualify on its first target-server re-pin.
A failed row with no retained success evidence does not qualify just because
automatic upgrade selects it. No database or runtime reset, and no inference
from an old error string.

### 3. Keep the original start alive through that generation

An explicit start checks this eligibility on each attempt, including its first
attempt. It need not have observed a pool outage first: the new agent can be
reachable before the request arrives. While eligible, a missing-runtime
refusal or the pending upgrade's provisioning gate is waited on instead of
returned. A pool compatibility transition is likewise a request wait, with a
typed internal cause distinct from a missing sandbox. It never sends a start
to an agent unable to enforce the existing explicit-start fence.

Let the existing sandbox reconciler finish the admitted upgrade. This wait
records no repair, performs no teardown, creates no extra generation, and
never sends `Start=true` to make an upgrade into desired running state.
Once that generation converges successfully, retry the ordinary fenced start.
The pool still owns the actual power observation and replacement power rules.
A settled failure is returned with its real reason. If a converged runtime is
still missing, this new entitlement ends; use the existing refusal/repair
policy for the precise runtime error, rather than continuing to retry 404.

Capture the qualifying upgrade generation for the request. Re-read eligibility
from primary state each time. If unrelated newer intent replaces it, stop this
upgrade wait with a conflict; do not silently attach it to arbitrary future
work. If a newer power instruction, repair, archive or delete supersedes its
power token, retain the existing supersession answer. Initial creation,
ordinary missing runtime without this evidence, and owed source retain their
current immediate refusals. Existing no-container repair behavior is unchanged.

### 4. Retain budgets, ordering and narration across both phases

Use the existing two-minute progress-sensitive stall budget, caller context
and five-minute absolute pool-transition ceiling. Start that ceiling at the
first pool-unreachable or protocol-incompatible refusal, or at entry into the
qualifying pending-upgrade wait if neither was seen. Carry it through pool
recovery, upgrade convergence and any existing repair phase. Changing phases,
heartbeats, repeated discovery, reconciliation scheduling and retries cannot
restart it. Every nested RPC, probe and provider wait must fit the remaining
request budget. Release leases and admission serialization between attempts.

Admit start and cancel its scheduled wake once. Retain the same `WakeEpoch`
and execution-boundary authorization across every phase; no unfenced fallback
against an old agent. Keep the existing races against a later stop, restart,
successful suspend, repair, archive and delete closed. A refused suspend still
changes nothing. Cancellation ends retries promptly; a server restart does
not resume the request. Deadline expiry preserves the last relevant refusal
and its 409 classification, with pool context and the fact waiting ended.

Use the existing request-progress transport and CLI/console status handling:
“waiting for the pool to come online” during compatibility replacement, then
“waiting for the sandbox upgrade” while the eligible generation converges.
Clear or replace progress on every exit or phase change. This is request
progress, not observed power or a heartbeat renewing the stall budget. Keep
JSON results parseable and noninteractive stderr bounded. Restart, stop,
suspend and scheduled wakes gain no retry from this decision.

## Alternatives rejected

- **Only continue waiting after pool connectivity returns.** It addresses
  one ordering of events, but misses a request reaching the new agent first
  and leaves the old agent receiving invalid create bodies.
- **Use ready/schedulable or pool generation as protocol proof.** Existing
  heartbeats can describe the old agent and drift replacement need not bump
  a generation. Pool readiness alone cannot prevent the startup race.
- **Compare release versions or exact bundle digests.** A release label is
  not a wire contract, especially in source builds. Exact bundle identity
  unnecessarily couples unrelated feature changes to protocol compatibility.
- **Omit `pullSources` when false, or accept unknown fields everywhere.**
  Neither establishes the required semantics when true or handles the next
  incompatible field. Ignoring a field can silently skip requested behavior.
- **Retry all missing-runtime responses with unobserved generations.**
  Initial creates, repairs, delivery and unrelated intent also have that
  shape. The prior-create evidence and exact upgrade marker are necessary.
- **Repair from the waiting start or store desired running state.** The
  reconciler already owes the create. Another rebuild can race it, and a
  background start can outlive its caller or undo a later stop.

## Consequences and acceptance evidence

This adds a protocol contract and small durable upgrade provenance rather than
an expanding collection of error-string exceptions. Legacy agents fail the
probe closed and upgrade through the existing provisioning path. A server
rollback to an incompatible agent revision may also require pool convergence;
there is no promise of mixed-version create support. Future create changes
must maintain the explicit revision contract.

Implementation must demonstrate that a legacy decoder receives zero creates,
even before pool `begin` runs and with stale ready reports; a matching agent
accepts create; an incompatible, absent, unauthorized or malformed discovery
answer takes its specified path; and replacing/reconnecting the serving agent
invalidates protocol proof. Exercise every pool transport implementation.

Cover pool-unreachable → compatible-but-runtime-missing → upgrade-complete →
start success, and runtime-missing as the first answer. Assert no extra repair
or generation, one wake cancellation, correct narration, caller/stall/ceiling
limits across phase changes, terminal upgrade failures, and immediate refusal
for initial create, owed delivery, arbitrary 404 and unrelated pending intent.
Repeat execution-boundary supersession races and verify restart and scheduled
wakes gain no replay. Test upgrade metadata persistence and safe old-row
migration, including a settled failure and a never-created failure that must
not qualify, and a previously created failed box with a proven pending upgrade
that can qualify.

The runtime acceptance is the issue's real v0.18.1 → target upgrade with one
running and one stopped Docker sandbox. Make start the first target command;
it must succeed without retry, server and pool must converge, both seeded
markers must remain readable, and no unsupported-create 400 or transitional
missing-runtime 409 may end that command. Retain command and pool logs to
prove the incompatible request was never sent. A controlled-provider test
alone does not satisfy this acceptance.

Exact protocol revision encoding, transport instance-binding primitives and
migration layout remain implementation choices within these rules. No product
or trust decision is deferred. Update the affected live design documents with
implementation; this proposal changes no runtime behavior.
