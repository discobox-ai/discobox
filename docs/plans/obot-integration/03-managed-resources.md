# WI-03 — Managed pool and managed sandbox resources

**Goal:** add persisted managed-pool and managed-sandbox resources keyed by an
upstream-owned external ID and opaque revision, with idempotent
`PUT`/`GET`/`DELETE` operations over the existing concrete `Pool` and `Sandbox`.

Read `00-CONTEXT.md` first. **Blocked on WI-01 being accepted.** This is the
largest item; it is the one that makes the integration exist.

## Why

The upstream controller retries. A create whose response was lost must not
produce a second runtime. A concrete sandbox that has to be replaced (because an
immutable field changed) must keep the same upstream-facing identity. Neither is
expressible today: sandbox creation always mints a fresh Discobox ID, and
nothing in the model records who manages a resource or which upstream revision
it materializes.

## Current state

- No external identity, manager identity, or revision anywhere in
  `server/internal/model/model.go` or `api/openapi/server.yaml`. Confirmed by
  search; there is nothing to extend.
- Concrete resources: `model.Pool` (`model.go:487`) and `model.Sandbox`
  (`model.go:555`), both embedding `ResourceLifecycle` with
  `generation`/`observed_generation` and desired-state/phase fields.
- Sandbox IDs are generated in `Sandbox.BeforeCreate` (`model.go:608`).
- Existing REST shape to mirror: `POST /projects/{projectId}/sandboxes` returns
  `202` with a sandbox that keeps reconciling; `POST .../pools` likewise.
- `server/internal/resources/{pools,sandboxes}/` own service, manager, executor,
  and reconciliation code per resource area. Read
  `server/internal/resources/DESIGN.md` for the layering rules.
- `server/internal/store/` is split by resource; `transactions.go` exists for
  the "persist intent + project event + reconcile job in one transaction"
  pattern described in the root `DESIGN.md`.
- `docs/adr/0010-deletes-are-hard-deletes.md` governs deletion semantics; WI-01
  decides how managed identity interacts with it.

## Scope

1. **Model + migration.** Managed-pool and managed-sandbox rows storing at
   minimum: manager/owner identity, external ID, external revision, project,
   the mapped concrete resource ID, and lifecycle state. Unique index on
   (project, manager, external ID, kind). Safe upgrade path — no recreated
   database.
2. **OpenAPI.** Conceptually:
   ```
   PUT    /projects/{projectId}/managed-pools/{externalId}
   GET    /projects/{projectId}/managed-pools/{externalId}
   DELETE /projects/{projectId}/managed-pools/{externalId}
   PUT    /projects/{projectId}/managed-sandboxes/{externalId}
   GET    /projects/{projectId}/managed-sandboxes/{externalId}
   DELETE /projects/{projectId}/managed-sandboxes/{externalId}
   ```
   The managed sandbox `PUT` body carries the revision, desired state, pool ID,
   harness selector, and a sandbox configuration that reuses the existing
   `SandboxCreateConfig` shape. The managed pool `PUT` body carries revision,
   name, provider instance ID, capacity, and suspension. Responses return the
   external ID and revision alongside the existing `Sandbox` / `Pool`
   representation, so upstream reads observed phase, generation, and observed
   generation without a second call. The exact generated Go names are not
   fixed by the upstream ADR.
3. **Idempotent PUT semantics.** Required behavior:

   | Condition | Result |
   | --- | --- |
   | external ID does not exist | create and return `202` |
   | same external ID, same revision | accept the payload and converge |
   | same external ID, changed revision | accept the payload and converge |
   | response lost, `PUT` retried | return the same managed resource, never a duplicate |
   | concrete resource disappeared out of band | retain managed identity, recreate/converge |
   | `DELETE` repeated | continue the same deletion, never start a new generation |

   Note that "same revision, different payload" is **accepted**, not rejected.
   The revision is correlation metadata, not an integrity constraint.
4. **Reuse the existing reconcilers.** Managed desired state feeds the existing
   pool and sandbox generation reconcilers. Do not build a parallel
   reconciliation path.
5. **Deletion.** `DELETE` returns `202`, the managed resource enters a deleting
   state, and it is removed only after the concrete resource and its runtime are
   gone — at which point `GET` returns not found, which upstream reads as
   complete. Managed-pool deletion must be refused while managed sandboxes
   remain assigned to it.
6. **Provider binding.** Pool provider binding is immutable
   (`model.go:491`). A managed `PUT` that would change it returns conflict
   rather than silently replacing a non-empty pool.
7. **Source restriction.** Reject `push`-delivered Git sources on managed
   sandboxes; only remotely cloneable sources are supported. See
   `docs/adr/0001-sandbox-origin-and-remote-source-push.md`.
8. Update the `DESIGN.md` files for the packages you touch.

## Out of scope

- Service authentication — WI-02. Develop against existing auth and let the
  service principal drop in.
- Which sandbox config changes apply in place vs. force replacement — WI-04
  owns that. This item defines the identity that survives a replacement; WI-04
  defines when one happens.
- Per-sandbox files in the config — WI-05.
- Pool suspension enforcement and overcommit placement — WI-06.
- Behavior of ordinary user commands against a managed resource, and management
  metadata in event payloads — WI-08.
- Live utilization — WI-07.

## Design questions for the engineer

- **Separate rows, or columns on the concrete resources?** WI-01 should settle
  this; if it has not, settle it before writing the migration. Replacement of
  the concrete sandbox under a stable managed identity is the deciding
  constraint.
- **What is the "manager" value?** The upstream ADR uses `"owner": "obot"` and
  is explicit that it is deployment/configuration identity, not a hard-coded
  assumption that only Obot can manage Discobox. Where is it configured?
- **`created_by_user_id` is `not null` on sandboxes.** What user does a managed
  sandbox belong to? Coordinate with WI-02.
- **How does upstream address the concrete pool for a managed sandbox?** The
  managed sandbox `PUT` carries a `poolId`. Should it accept the *managed* pool
  external ID instead, or as well?

## Done when

- The six routes exist, are generated from the contract, and behave per the
  table above.
- A managed sandbox reconciles to running through the existing engine, and its
  managed identity survives a concrete-sandbox replacement.
- Repeated `PUT` and repeated `DELETE` are provably idempotent under test.
- Migrations upgrade an existing database in place.
- `go tool task check-hooks` passes.
</content>
</invoke>
<parameter name="description">Write WI-03 managed resources brief