# 26-11-04-270 — An addition may be granted without expiry

- **Status**: Accepted (amends 26-11-03-329 §1's mandatory expiry and §2's descriptions of additions as expiring bindings, and extends §5's lifetime preview to no expiry)
- **Date**: 2026-11-04

  Accepted by the owner by merging #649, 2026-11-04.
- **Issue**: [#630](https://github.com/discobox-ai/discobox/issues/630)
- **Amends**: [26-11-03-329](26-11-03-329-a-box-may-be-granted-routes-for-a-time-beside-its-binding-without-replacing-it.md) §1: an addition need not have an expiry; §2: an addition without a plain binding may be permanent, and a shadowed addition may apply again without an expiry deadline; §5: the addition preview includes a grant without expiry.
- **Leaves standing**: 329's other binding-shape requirements, resolution and per-grant enforcement (§§1–3), addition/replacement choice and replacement clearing additions (§4), and grant listing (§6); [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §3's parent delegation bounds and §4's route enforcement; [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md) §4's separate decisions for each requested part. Omitted-lifetime defaults remain as implemented today.

## Context

Approving an addition with “forever” currently returns 400, while a
replacement accepts it. The owner chose permanent additions in the
[issue comment](https://github.com/discobox-ai/discobox/issues/630#issuecomment-6644688075):
“We should be able to do forever for additions.” Replacing instead removes
standing sandbox use bindings and additions, so it is not an equivalent way
to preserve existing access.

Checked against main at `aeaf3352e38c56a16f6d1421d6bf35952d852432`:

- `server/internal/resources/secrets/bindings.go`, `checkBindingShape`,
  refuses an addition whose `ExpiresAt` is nil, for both binding CRUD and
  request approval. This implements 329 §1's mandatory expiry.
- `server/internal/resources/secrets/service.go`, `approvalWrite.binding`,
  preserves explicit `ttlSeconds: 0` as nil expiry, checks the secret's
  maximum lifetime and any parent, then checks the binding shape.
  `defaultApprovalTTL` supplies a finite default when the lifetime is omitted.
- `server/internal/resources/secrets/delegation.go`, `fitToParent`, refuses
  explicit forever beneath an expiring delegation; `heldParent` rechecks
  the delegation while committing approval.
- `cli/internal/tui/credentials.go`, `askLifetime`, already offers forever
  when the secret has no maximum lifetime. Its later mode selection permits
  addition. Console and TUI share this approval flow.
- `cli/internal/cli/secret.go`, `approvalBody`, sends an explicitly selected
  forever as zero, independently of `--use-as add|replace`.

## Decision

### 1. Addition and lifetime are independent choices

An addition may have a finite expiry or no expiry, just like a plain use
binding. Remove only the mandatory-expiry condition from 329 §1. It remains
a use-only, sandbox-targeted, sentinel-delivered binding in a named variable.
This applies to creation, update and request approval, rather than making an
approval-only exception to binding validity.

Use the existing representation: explicit `ttlSeconds: 0` means no expiry
and is persisted as `ExpiresAt == nil`. Omitting the approval lifetime keeps
the existing requested-or-default finite lifetime, within applicable bounds;
it does not silently choose forever. No new flag, binding kind or schema is
needed. Existing finite additions keep their stored deadlines.

### 2. Forever remains subject to the secret and parent

An unlimited addition is allowed only when the secret permits an unlimited
binding and, when it has a parent delegation, that parent has no expiry.
An explicit forever choice under a finite secret maximum or expiring parent
is refused, just as for a replacement; it is not silently capped. Existing
fitting of an omitted lifetime to a parent's remaining time stays unchanged.

The addition retains its parent link, live route ceilings and revocation
semantics. Deleting the addition, its secret or target, or its parent
revokes it. Editing secret or parent routes still bounds every request.
Issue [#616](https://github.com/discobox-ai/discobox/issues/616)'s approval
bounds remain required; this proposal grants no exception to them.

### 3. Stacking and display preserve the chosen lifetime

A permanent addition stacks with the same secret's plain binding and other
additions, or delivers alone when no plain binding applies. It can outlive
an unrelated base binding. A plain binding of a different secret shadows it
without deleting it; it can apply again when the variable returns to its
secret, without an expiry deadline. Replacement still clears additions.

Each addition remains a separate grant. Nil expiry means no time deadline
in resolution, sentinel selection, pool enforcement and credential listings;
a finite grant still stops at its own effective expiry. A permanent grant
does not make another grant's routes permanent or remove its blocked routes.

The CLI, console and TUI offer and submit the same lifetime semantics.
The addition preview says “these routes without expiry; the rest unchanged”
when forever is selected, and shows the deadline for finite additions.
Bindings and grant listings show “no expiry” consistently. A known finite
secret limit must prevent choosing forever before submission; the server
remains authoritative for secret and parent bounds, including concurrent
changes. This does not redesign the approval forms or widen route scope
(#635 and #629).

## Alternatives rejected

- **Keep additions finite and validate the clients.** This would remove the
  late 400 but reject the owner's chosen behavior. Stacking preserves
  existing grants independently of whether the added grant is temporary.
- **Require replacement for permanent access.** Replacement removes other
  sandbox use bindings and additions, and can conflict with a base under
  another parent. It does not express adding access beside an existing grant.
- **Use a very long finite lifetime for forever.** That eventually expires
  and misrepresents consent; nullable expiry already represents no deadline.

## Consequences

Permanent additions require explicit revocation instead of expiry. They
remain separate, auditable bindings managed through existing CRUD. No
migration or rewrite of existing bindings is required.

Implementation must update the service invariant and its negative test,
API descriptions, protocol and affected design docs, and prove forever
approval through CLI, console and TUI. Coverage must also preserve finite
expiry, omitted-lifetime defaults, finite secret and parent refusals,
revocation, and resolution of mixed finite and permanent additions. This
proposal changes no product code and leaves the original accepted ADR as
historical text; these amendments govern only after acceptance.

## Open for implementation

Exact wording and layout beyond the lifetime distinction above, and the
placement of shared and entry-point tests. Neither is permission to omit
coverage of any of the three approval paths.
