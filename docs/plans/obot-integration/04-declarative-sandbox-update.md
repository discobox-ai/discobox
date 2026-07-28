# WI-04 — Declarative sandbox update, in place or by replacement

**Goal:** let a caller declare the full desired runtime configuration of an
existing sandbox, and have Discobox decide which changes apply in place and
which require replacing the concrete sandbox.

Read `00-CONTEXT.md` first. **Independent — can start now.** It is valuable on
its own; today there is no way to change a sandbox's configuration at all.

## Why

An upstream controller pushes a complete desired configuration on every
reconcile and expects Discobox to converge. Discobox currently exposes a sandbox
update that changes *only the name*, so there is no convergence path: a changed
model, prompt, environment, or image cannot be applied to an existing sandbox.

## Current state

- `api/openapi/server.yaml:2645` — `SandboxUpdateConfig` has exactly one field,
  `name`. Reached via `PATCH /projects/{projectId}/sandboxes/{sandboxId}`
  (`server.yaml:4176`).
- `api/openapi/server.yaml:1641` — `SandboxConfig`, the full desired shape:
  harness config, harness mode, model/reasoning/tier, cpu/memory/storage,
  description, env, image, image digest, name, prompt, source, source code
  references, user.
- **Pool-agent already accepts more than the server sends.** `pool-agent/api/openapi/pool.yaml`
  `SandboxUpdateConfig` (~line 255) takes `cpuVcpus`, `env`, `image`,
  `memoryBytes`, `storageBytes`, `workingDirectory`, and
  `PoolSandboxUpdateRequest` (~line 393) additionally carries replacement
  `sentinels` and `secretEnv` for live secret rebinding. The server simply never
  drives it. Check what the pool-agent implementation actually honors before
  assuming the contract is fully implemented.
- Existing adjacent operations that already model "change and converge":
  `restart` (`server.yaml:4341`) and `upgrade` (`server.yaml:4382`), backed by
  `restart_generation`/`restarted_generation` on `model.Sandbox`
  (`model.go:565`), and `SandboxUpgrade` (`server.yaml:1857`).
- `docs/adr/0016-sandbox-image-upgrades-are-explicit-and-in-place.md` governs
  image changes: `image_digest` is written at create and by an upgrade, never by
  a restart, and the pool host refuses an image that does not match it. Any
  declarative image change must respect this, not route around it.
- Reconciliation lives in `server/internal/resources/sandboxes/reconciler.go`
  and `intents.go`.

## Scope

1. Extend the server's `SandboxUpdateConfig` to the runtime fields that a
   controller owns. Start from `SandboxConfig` and subtract what genuinely
   cannot change.
2. Classify every field: **mutable in place** (no restart), **mutable with
   restart**, **immutable** (requires replacing the concrete sandbox), or
   **not updatable at all**. Write this classification down in
   `server/internal/resources/sandboxes/DESIGN.md` — it is the durable output of
   this item, more than the code is.
3. Implement in-place and restart-requiring changes through the existing
   generation reconciler and the pool-agent update call.
4. Implement replacement: when an immutable field changes, the concrete sandbox
   is replaced. Replacement must work for an ordinary sandbox too, not only a
   managed one, but the *identity that survives* is WI-03's concern — keep the
   two seams clean and coordinate on the interface between them.
5. Preserve the existing single-purpose operations (`restart`, `upgrade`,
   `start`, `stop`). Do not fold them into the declarative path; they express
   user intent that the declarative path does not.

## Out of scope

- Managed identity and external revisions — WI-03.
- Per-sandbox files — WI-05 adds the field; this item should classify it once it
  exists (expect: mutable with restart).
- Secret rotation. Already works: rebinding sentinels for a running sandbox goes
  through `PoolSandboxUpdateRequest.sentinels`/`secretEnv` without a restart.
  Confirm it survives your changes; do not redesign it.

## Design questions for the engineer

- **Which fields are truly immutable?** Candidates: `source` and
  `sourceCodeReferences` (the sandbox has already materialized them), `user`,
  and possibly storage. `image` is constrained by ADR-0016 rather than immutable.
  Bring a proposed classification table rather than an open question.
- **Is replacement in scope for ordinary sandboxes now, or gated to managed
  ones?** Replacing a user's sandbox in response to a `PATCH` is surprising;
  replacing a controller-owned one is expected. A reasonable answer is that the
  declarative API rejects an immutable change and only the managed path
  replaces.
- **PATCH or PUT?** Full-replacement declarative semantics fit `PUT` better than
  `PATCH`, but the existing route is `PATCH`. Sparse `PATCH` cannot express
  "remove this env var", which a converging controller needs.

## Done when

- A caller can change the runtime configuration of an existing sandbox and see
  it converge.
- The field classification is documented and enforced.
- An immutable change produces the agreed outcome (rejection or replacement),
  under test.
- Restart and upgrade semantics, and ADR-0016's image-digest pinning, are
  unchanged.
- `go tool task check-hooks` passes.
</content>
</invoke>
<parameter name="description">Write WI-04 declarative update brief