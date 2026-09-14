# 0121 — A stopped sandbox that failed tracks its harness image too

- **Status**: Accepted
- **Date**: 2026-09-14
- **Supersedes**: [0082](0082-a-stopped-sandbox-tracks-its-harness-image.md)
  §2's `state = ready` and `error_message IS NULL` conditions, and its rejected
  alternative *"Include `failed` sandboxes."* §§1, 3 and the rest of §2 stand.

## Context

ADR 0082 made a harness image's digest moving upgrade that harness's stopped
sandboxes, and limited it to sandboxes converged at `ready` with no latched
error. It considered `failed` sandboxes and rejected them for two reasons:

- **Repair already exists.** Repair re-pins to the current image (ADR 0064) and
  is one keystroke in the TUI list.
- **A settled verdict would become a recurring one.** Sweeping failures would
  retry every unrelated failure each time an image moves.

Neither holds up in use. A failed sandbox is precisely the one most likely to be
failing *because of* its image — a pruned image (`resolveSandboxImage`), a
harness bug fixed in the rebuild — and it is the one left behind: every healthy
stopped sandbox on the harness moves forward, and the broken one stays on the
image that broke it until somebody notices and repairs it. Repair being
available does not make it the right default for a fix that is already on its
way.

The recurrence is also bounded by the mechanism 0082 already built. The
fan-out fires only when a harness config's digest *changes*, and only re-pins a
sandbox whose pin differs from the new digest (`SandboxUpgradeTarget`). A
failed sandbox is retried at most once per image that actually moves, not on a
timer and not on every reconcile — and each retry is an attempt on an image it
has never been tried on.

## Decision

**A sandbox observed `stopped` is upgraded by the fan-out whether its last
reconcile left it `ready` or `failed`, and a `failed` sandbox is upgraded even
when no runtime state has ever been observed for it.**

ADR 0082 §2's table becomes:

| Condition | Why |
| --- | --- |
| `desired_state = present` | Not on its way to archived or deleted. |
| `state = ready` and `runtime_state = 'stopped'`, **or** `state = failed` and `runtime_state IN ('stopped', '')` | Settled on the existence axis — `pending` and `awaiting_source` are using the pin right now — and not running. |
| `generation = observed_generation` | Nothing is mid-flight; never pile intent onto an unsettled row. A failure settles its generation (ADR 0017 §4), so a failed row qualifies. |
| not in config mode | Unchanged. |
| the target digest differs | Unchanged; this is also what bounds retries to one per digest move. |
| the project's policy is not `manual` | Unchanged (ADR 0082 §3). |

The `error_message IS NULL` condition is dropped: in the sandbox reconciler a
latched error always comes with `state = failed`, and it is exactly what the
recorded intent clears (ADR 0017 §4).

The action is still the existing upgrade, unchanged — `imageRepin` through
`recordSandboxIntent`. Recording that intent clears the latched error and bumps
the generation, so the reconciler's "a settled failure needs new intent"
early-return no longer applies and the ordinary ensure rebuilds on the new
image. It is not a repair: there is no `RepairGeneration`, so no teardown, and
`ensure` issues no start for a sandbox that has run before, so the sandbox is
left stopped exactly as a healthy one is.

### Never observed is admitted for a failure, not for `ready`

ADR 0082 §2 excluded an empty `runtime_state` because not observed is not
stopped. That still holds for `ready`: a converged sandbox with no report is in
the brief window before its create's own report lands, and its container may
well be up.

It does not hold for `failed`. A failed sandbox that no pool agent has ever
reported on is a first create that failed before a container was seen — a
pruned or unpullable image being the canonical case. There is no observation
because there is nothing to observe, and it is exactly the sandbox a new image
may fix. Excluding it would leave behind the one sandbox on the harness that
never worked at all, while it reads `error` in the listing like any other.

The retry does not power it on: `ensure` issues a start only for a sandbox
still `pending` or `awaiting_source`, and a failed one is neither. It comes back
`ready` with its container created, and on-demand start covers first use — the
same shape as a recovered sandbox (ADR 0017 §13).

### A failed sandbox still owed a client push is not retried

A sandbox with a push-delivered source whose delivery was never reported
(`awaitingSourcePush`) is skipped, whatever its state. Its retry cannot end
`ready`: the create succeeds and re-parks it at `awaiting_source`, which
re-stamps the park anchor and arms a fresh push deadline. It then reads as
starting for that whole window while no client is pushing, and settles
`failed` again with the push timeout in place of the cause it had. A new image
delivers no source, so there is nothing here for it to fix; repair, run by
someone whose client can push, is still the way back.

The skip is in the fan-out rather than the eligibility query, because whether a
source is push-delivered lives in the sandbox's source document, not in a
column.

### A failure being retried is waited on, not answered

Recording the re-pin clears the error and bumps the generation, but `state`
reads `failed` until the reconciler writes — a full container rebuild, and on a
remote pool an image pull. The attach wait answered any `failed` row at once,
because a settled failure needs new intent (ADR 0017 §4). A failed row whose
generation is unobserved *has* new intent, so the wait treats it like any other
sandbox being provisioned: it holds until the reconcile lands `ready`, or
settles `failed` again and is answered then. Without this, an unattended image
move would turn a stopped failed sandbox that attach could auto-start into an
immediate 409 for the length of the rebuild. The same holds for a typed upgrade
or repair of a failed sandbox, which had the same shape.

## Alternatives rejected

**Upgrade failed sandboxes by repairing them.** Repair tears the runtime down
first, which would make the automatic path for a failed sandbox a different
operation from the one for a healthy sandbox and from a typed upgrade.
Rejected: 0082 §1's whole point is that an automatic upgrade is the same
operation as `POST …/upgrade`, and the re-pin alone already rebuilds the
container, because the fingerprint moves. A sandbox that needs the teardown
still has repair.

**Include failed sandboxes regardless of runtime state.** They read `error` in
the listing whatever their container is doing, so "errored" could be taken to
mean all of them. Rejected for any observed live state (`starting`, `running`,
`stopping`): the sandbox would be restarted into the new image unattended,
which 0082 never does to a running sandbox.

**Admit never observed for `ready` too.** Rejected: see the Decision. For a
`ready` row it is a transient window with a container that may be running, not
evidence of absence.

## Consequences

- A failed, stopped sandbox is retried on its harness's new image when that
  image lands, and either comes back `ready` and stopped or settles `failed`
  again with the new reason. At most one attempt per digest move.
- A failed first create that never produced a container is rebuilt on the new
  image and left created but not started, rather than started as its original
  create would have. Its first use starts it.
- There is a narrow exposure in the never-observed case: a first create whose
  container did come up on the pool but whose report never reached the control
  plane reads as never observed. Re-pinning it replaces a running container,
  and ADR 0021 §3 restarts the replacement into the new image — the same
  outcome 0082 accepted for its own stopped-to-started window.
- A failure unrelated to the image — an unreachable pool, a bad source — is
  re-attempted once per digest move and fails again. In the development loop,
  where images rebuild often, that is a background rebuild per image change for
  each such sandbox; `manual` remains the opt-out.
- The error message a failed sandbox showed is cleared when the re-pin is
  recorded, as it is for any intent. The reason is not lost if the cause
  persists: the retry records it again.
- While the retry is in flight the listing still shows the sandbox as `error`,
  because its state is `failed` until the reconciler writes, and repair is still
  offered on it. A repair pressed then supersedes the retry with its own
  generation and teardown — a user's explicit intent overriding an unattended
  one, which is how intent already composes.
- The project's `sandboxUpgradePolicy` descriptions (API and CLI) name failed
  sandboxes alongside stopped ones, since 0082 §3 requires the cost of an
  unattended rebuild to be stated where the opt-out lives.
