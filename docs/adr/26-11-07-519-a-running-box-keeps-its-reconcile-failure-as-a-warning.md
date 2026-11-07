# 26-11-07-519 — A running box keeps its reconcile failure as a warning

- **Status**: Proposed (supersedes 0034 §5's unconditional error-first precedence for a present, running sandbox; amends 26-10-29-316 §1 and 26-11-01-560 §2's event fields with `errorMessage`)
- **Date**: 2026-11-07
- **Issue**: [#837](https://github.com/discobox-ai/discobox/issues/837)
- **Relates to**: [0034](0034-sandbox-state-and-runtime-state-are-separate-fields.md)
  §§1–2 and 7, separate writers for existence, power and recorded failure;
  [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md)
  §§3 and 8, using an existing container and showing an unfinished repair;
  [26-11-06-628](26-11-06-628-attach-waits-for-the-outcome-of-blocking-provisioning.md),
  blocking provisioning readiness. Those decisions remain.

## Context

A reconcile can fail after a container exists, or fail to ensure one that is
already running. Its recorded failure and the pool's observation of power are
independent facts. Today `Sandbox.DisplayState()` in
`server/internal/model/model.go` returns `error` for a failed existence state
or any non-nil `ErrorMessage`, before consulting power. A working box therefore
looks unavailable until the reconciler clears its failure. Issue #836 describes
one cause; correcting that cause alone does not correct this presentation rule.

Clients correctly trust the server's composition. In `cli/internal/cli/tui.go`,
`toTUIState` maps `error` to `StateError`, disabling actions guarded by
`StateRunning`, hiding the program status and excluding the box from running
counts. `cli/internal/tui/data.go` already carries `HasRuntime` for operations
that need to distinguish a failed create with no runtime from an existing one;
it is not proof that the runtime is running.

The listing already carries `runtime.errorMessage`, and `sandboxMessage` in
`cli/internal/cli/output.go` shows it independently of display state. Events do
not: `SandboxEventData` in `server/internal/model/sandbox_event.go` carries the
axes and `displayState`, but no recorded error. The store projection in
`server/internal/store/sandbox_events.go` reads `error_message` only to compute
display state. Once a running box can keep its display state through an error,
a consumer needs that independent signal to see the warning appear or clear.

## Decision

### 1. Running can qualify a recorded reconcile failure

`Sandbox.DisplayState()` remains the only rule composing existence and power.
After the existing `Repairing()` check, a recorded reconcile failure
(`State == failed` or `ErrorMessage != nil`) is presented as `running` when
`DesiredState == present` and `RuntimeState == running`, subject to blocking
provisioning readiness below. The stored failure is retained.

Only running earns this exception. A recorded failure with stopped, suspended,
starting, stopping or unreported power remains `error`. A healthy pending or
awaiting-source create still displays `starting` even if its container runs;
this decision does not make every runtime report outrank existence.

The archive and delete paths keep their current precedence. The running
exception applies only to desired `present`, so a failure with desired
`archived` or `deleted` remains `error`, even with a last report of `running`.
Without a failure, an outstanding archive or delete remains `archiving` or
`deleting`, and settled archived/deleted rows keep their existing presentation.
A last power observation must not make those rows look usable.

Blocking provisioning still qualifies running: a valid report with
`provision.ready == false` displays `starting`, or `error` when its blocking
preparation has an error. Apply this qualification to the new running exception
as well as the existing ready-and-running case. A ready report, or no usable
provision report as with older agents, permits `running`. Async work after
readiness does not hold it back. This follows the existing projection in
`Sandbox.DisplayState()` and does not alter request readiness gates.

The important combinations are:

| Condition, in precedence order | Display state |
| --- | --- |
| Repair generation still in flight | `repairing` |
| Recorded failure, desired present, runtime running, blocking provision incomplete | `starting`, or `error` for a blocking provisioning failure |
| Recorded failure, desired present, runtime running, provision ready or unreported | `running` |
| Any other recorded reconcile failure | `error` |
| No recorded failure | Existing archive, delete, creation and runtime projection |

This changes presentation, not convergence. `State`, `ObservedGeneration`,
`ErrorMessage` and `ErrorReason` keep their existing writers and clearing rules.
A power report clears none of them, and a running warning triggers no automatic
repair or retry. A non-empty error message is shown verbatim as today. A failed
state without a message still receives the running exception; clients must not
invent a diagnostic text for it.

### 2. The console draws a warning without changing the state

A `StateRunning` row with a non-empty `Message` keeps its running state and gains
a visible warning indication beside that state. Under the cursor, its message
replaces the name, as it already does for error and repairing rows. The exact
marker and colour are left to implementation; its meaning must remain clear
without colour alone.

Running actions, program status and counts continue using `StateRunning`.
Clients do not recombine `state` and `runtimeState` to undo the server's answer.
`HasRuntime` retains its distinct meaning, presence of an observed runtime,
including one that is stopped. The CLI listing's ERROR column already reads
`sandboxMessage` independently and requires no new state or warning enum.

### 3. Events carry the error message as part of the snapshot

Amend [26-10-29-316](26-10-29-316-sandbox-state-changes-are-a-resumable-event-stream.md)
§1 and [26-11-01-560](26-11-01-560-discobox-box-watches-other-boxes-through-the-sandbox-event-stream.md)
§2: `SandboxEventData` gains optional `errorMessage`, the same recorded
reconciler message served in `Sandbox.runtime.errorMessage`. Omit it when there
is no message. Each event is a whole snapshot: absence clears a consumer's
previous warning; it is not a patch that leaves the old value in place.

Message appearance, replacement and clearing produce `sandbox.updated` even
when both axes and `displayState` stay unchanged. The store compares this field
with the rest of the projection and records it in the transaction that changes
the row. Unchanged reports still produce no event. A boolean is insufficient:
it would miss message replacement and require a GET to explain the warning.

The contract is declared in `api/openapi/server.yaml` and generated with the
existing API machinery. The stream adds no new event type, broker or table.
Existing persisted JSON events may lack the field. Keep them intact; on upgrade,
the existing startup backfill appends updates where the current projection
differs, so replay reaches a current snapshot without deleting history or
resetting sequence numbers. Before that update, an older event's absence says
only what that event contained, not that the current row has no error.
The stream keeps its existing project and sandbox-principal access rules;
the error is already visible in the listing those principals may read.

## Alternatives rejected

**Keep error-first and have clients also inspect runtime power.** Every client
would own a second composition rule, contrary to 0034 §5 and 26-11-01-560 §2.
Fixing only console guards would leave listings and other clients misleading.

**Let runtime reports clear the recorded failure.** That introduces a second
writer of the reconciler's verdict (0034 §§1–2 and 7). Observing a running
container proves neither that ensure succeeded nor that its failure is obsolete.

**Let every power report override every error or lifecycle transition.** A
stopped container does not prove it can start, and stale running observations
must not conceal repair, archive, delete or blocking provisioning work.

**Add a `running_with_error` state.** That expands the shared vocabulary and
forces every client to special-case another running value, despite an existing
independent error field. Running plus a warning expresses both facts directly.

**Keep errors out of events or carry only a boolean.** A stream consumer would
need to re-list to explain or replace the warning. Whole snapshots should carry
the small diagnostic field needed to render the state they report.

## Consequences

A usable box with a recorded reconcile failure remains usable in the console,
and its failure stays visible for diagnosis. This does not claim the failure
has healed or change what an action can actually do; request handlers and the
pool still enforce runtime and provisioning readiness.

The implementing change updates the model, resources and console design docs
alongside the code, including the old error-first explanation on `HasRuntime`.
It must cover failed+running (with and without a message), failed+stopped and
unreported, repair-in-flight, failed+running with archive/delete intent, and
blocking provisioning readiness. Console coverage must show the warning and
cursor message while preserving running actions/status/counts. Event coverage
must show error appearance, replacement and clearing with otherwise identical
snapshots, unchanged-report suppression, and upgrade backfill from old JSON.
No implementation or current-state design change belongs in this proposal.
