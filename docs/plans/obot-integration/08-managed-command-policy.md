# WI-08 — Managed command policy and event correlation metadata

**Goal:** define and enforce what ordinary user lifecycle commands do against a
managed resource, and expose enough management metadata in reads and project
events for an upstream controller to correlate them.

Read `00-CONTEXT.md` first. **Blocked on WI-03's model landing.** The policy
design can be worked out with the engineer in parallel with WI-03.

## Why

A managed sandbox has two writers: the upstream controller, which pushes a
complete desired state on every reconcile, and any user who can reach the
ordinary sandbox API. Left undefined, a user's `stop` is silently reverted by
the next upstream `PUT`, or worse, fights it indefinitely. The managed `PUT`'s
`desiredState` is authoritative; what a user command *means* in its presence has
to be an explicit decision rather than an emergent race.

Separately, upstream accelerates reconciliation by subscribing to project
events instead of polling. That only works if a sandbox event carries enough
information to map back to an upstream resource. Polling remains the correctness
backstop, so this is a latency feature, not a trust boundary — events are
enqueue hints, and upstream re-reads before believing anything.

## Current state

- Ordinary lifecycle commands, all unaware of management:
  `start` (`api/openapi/server.yaml:4458`), `stop` (`:4499`),
  `restart` (`:4341`), `upgrade` (`:4382`), `reconcile` (`:4423`),
  `PATCH` (`:4176`), `DELETE` (`:4112`). Pools have `default` (`:3932`),
  `reconcile` (`:4001`), `PATCH` (`:3892`), `DELETE` (`:3828`).
- Events: `server/internal/events` (project event broker) and
  `server/internal/store/resource_events.go`. There is no client-facing
  transport on top of them any more: the websocket and SSE streams promised a
  resumable list-then-watch they could not deliver and were removed
  ([ADR 0061](../../adr/0061-the-client-facing-project-event-stream-is-removed.md)),
  which also took the `ResourceChangedEvent`/`ResourceListedEvent` wire shapes
  and the list-start/finish envelope with them.
- So this plan builds the stream it wants rather than inheriting one. What it
  describes below — enqueue hints, with polling as the correctness mechanism and
  a re-read before believing anything — is exactly what the broker already
  provides and materially less than what was removed. Managed sandboxes would
  appear as ordinary sandbox resources on it; there is no separate managed
  stream.

## Scope

1. **Policy.** Decide and implement, per WI-01, one of:
   - reject direct lifecycle commands on managed resources with a clear error
     naming the managing owner; or
   - accept them as explicitly temporary actions that the next managed `PUT`
     overwrites.
   Whichever is chosen, it must be uniform across the command set and
   documented in `server/internal/resources/sandboxes/DESIGN.md` and the pools
   equivalent. Half-and-half is the one outcome to avoid.
2. **Management metadata in reads.** `Sandbox` and `Pool` responses carry
   management correlation data, conceptually:
   ```json
   { "management": { "owner": "obot",
                     "externalId": "...",
                     "externalRevision": "sha256:..." } }
   ```
   The owner value is deployment/configuration identity — not a hard-coded
   assumption that only Obot manages Discobox.
3. **Management metadata in events.** The same block rides in the resource
   payloads carried by `resource.changed` and snapshot events, so a subscriber
   can map a Discobox sandbox ID to its upstream resource without a second call.
4. **Do not put utilization in events.** WI-07 is deliberately a separate live
   read path; resource events carry lifecycle only.

## Out of scope

- The managed resources themselves — WI-03.
- Any new stream, resume-sequence, or event-delivery guarantee. Events stay
  best-effort hints; duplicates, delays, and reordering must remain harmless.
- Upstream's subscription logic. It maintains one process-level subscription per
  project and re-requests the snapshot on reconnect. Nothing here should assume
  one connection per sandbox.

## Design questions for the engineer

- **Reject, or accept-as-temporary?** Rejection is predictable and easy to
  explain; accept-as-temporary is friendlier for an administrator debugging a
  stuck agent and matches the upstream note that an administrator may stop a
  sandbox in a suspended pool and have it stay stopped. Note the interaction
  with WI-06: that suspension behavior implies at least *some* user commands
  must survive against a managed sandbox.
- **What error shape?** A `409` naming the owner is more actionable than a
  `403`. Check `server/internal/apperrors` for the existing vocabulary.
- **Is management metadata visible to every project member,** or only to the
  managing service principal and administrators? It contains an upstream
  resource ID and revision — correlation data, not secrets, but still upstream
  internals.
- **Does `reconcile` count as a lifecycle command?** It requests convergence to
  already-declared state rather than changing intent, so it is plausibly always
  allowed.

## Done when

- Every ordinary lifecycle command has defined, tested behavior against a
  managed pool and a managed sandbox.
- Management metadata appears in resource reads and in project event payloads.
- A subscriber can correlate a `resource.changed` event to an upstream resource
  without an extra request.
- Duplicate, delayed, and out-of-order events remain harmless.
- The relevant `DESIGN.md` files record the policy.
- `go tool task check-hooks` passes.
