# 26-11-04-616 — A credential approval restricts its grant to requested routes, requested hosts, or anywhere

- **Status**: Accepted (amends 26-11-02-022 §§3–4 for wider approval scope and its display, and §6 for the decision record)
- **Date**: 2026-11-04
- Accepted by the owner by merging #638, 2026-11-04
- **Issue**: [#629](https://github.com/discobox-ai/discobox/issues/629)
- **Amends**: [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md) §§3–4: an approved part may explicitly restrict its grant to requested routes, requested hosts, or anywhere; §6: retain that consent and the scope actually granted.
- **Leaves standing**: [26-11-03-329](26-11-03-329-a-box-may-be-granted-routes-for-a-time-beside-its-binding-without-replacing-it.md) §§1–6: scope and addition/replacement are independent; [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §§3–4 and 6: parent bounds, route-list enforcement and binding history; [26-11-03-970](26-11-03-970-a-discobox-adopts-another-as-its-child-when-a-person-approves.md) §2: authority follows the current parent.

## Context

A person who approves a GitHub request for an hour can face another request
minutes later: the hour covers the routes approved, not every route of the
credential. The issue asks for a short, explicit way to grant all of the
credential's allowed routes for the chosen duration. Requested scope remains
the default.

Checked against main at `c7e3cca1c8e0ad4581ae039ff7daf9362423ef14`:

- `server/internal/resources/secrets/service.go`, `partDecision`, uses the
  requested routes unless the answer supplies edited routes. `commitApproval`
  reloads the secret and writes the decision and bindings in one transaction.
- `server/internal/store/sandbox_secrets.go`, `RouteLists`, keeps the secret,
  binding and parent lists separate. Every list must allow a request; a
  binding elsewhere cannot widen its secret.
- `server/internal/resources/secrets/delegation.go`, `heldParent` and
  `fitToParent`, recheck the selected delegation and bound the lifetime.
- `cli/internal/cli/secret.go` exposes edited routes and addition/replacement;
  `cli/internal/cli/tui_credentials.go`, `ApproveCredentialRequest`, sends the
  shared approval dialog's per-part decisions. Neither offers whole scope.
- `server/internal/model/model.go`, `SecretRequestPart`, retains the ask,
  outcome, binding ID and addition/replacement mode, but no approval-scope choice.

## Decision

### 1. Scope restricts the grant, never changes the credential

An approval part gains `scope: routes | hosts | anywhere`, defaulting to
`routes` when omitted. The console and TUI label these “Requested routes”,
“Requested hosts” and “Anywhere”. They describe the grant's own restriction;
the secret's routes and the upstream credential's permissions remain ceilings.

| Choice | Restriction written on the approved binding |
| --- | --- |
| Requested routes | The part's requested routes, including their method, path and protocol constraints; explicit route edits retain today's behavior. |
| Requested hosts | The host patterns from the part's open requested routes, allowing any method, path and protocol on those hosts. |
| Anywhere | No additional route restriction on this binding. |

Requested hosts derives only host patterns the server parsed from the original
request, including resolved policies. Preserve exact names and wildcard host
patterns; never broaden an exact name to a suffix wildcard. Blocked-only
routes introduce no hosts. This choice deliberately drops the request's path,
method, GraphQL, MCP and blocked-route restrictions; the screen says so.
A part with no open requested host cannot be approved with hosts scope.

Explicit route overrides are allowed only with routes scope. Hosts or anywhere
plus route overrides is invalid, as are unknown scopes and scope on a denied
part. Every requested part still requires its own approve/deny answer; a scope
never creates or approves another part by omission.

Use and delegation each have their own choice. The CLI exposes
`--use-scope routes|hosts|anywhere` and
`--delegate-scope routes|hosts|anywhere`. A person can approve use anywhere
while denying delegation or leaving its scope at requested routes. A request
cannot select a wider approval scope on the approver's behalf.

Scope is independent of `--use-as add|replace`, duration and raw delivery.
Anywhere never implies raw delivery or a longer lifetime; existing refusals
for raw bindings and additions stand.

### 2. Existing ceilings remain live

All three choices use the existing route-list enforcement. Every operation
must pass the secret's routes and, for a delegated grant, the live parent's
routes, as well as any restriction on the new binding. Anywhere uses the
existing empty binding route list, which adds no restriction; it does not
write a wildcard over the secret's or parent's lists. Requested hosts writes
host-only open routes. Neither copies the secret's routes nor snapshots its
ceiling into a new authorization layer.

The grant therefore follows later edits to its secret and parent, within its
own selected restriction and lifetime, just as bindings do today. Narrowing,
blocked routes, expiry and revocation remain effective. Wider bindings on
other boxes, harness configs or projects contribute nothing to this grant.
The upstream service still enforces the credential's own permissions.

A sandbox approver must hold one live delegation of the selected secret and
may answer only a use-only request from a box it currently leads. The new
binding remains a child of that delegation, with no greater effective access
or lifetime. Do not union different delegations or flatten lists, which can
remove blocked-route carve-outs. Requests containing a delegate part remain
a person's to answer in their entirety.

Issue [#616](https://github.com/discobox-ai/discobox/issues/616) owns direct
binding, create-time grants, adopted-child route access and any change to
redelegation. This proposal introduces no exception to those bounds and
requires all three scopes to obey its resulting server-side checks.

### 3. Duration and existing grants keep their meaning

The scope choice changes the grant's route restriction only. An approved use addition lasts for its own
expiry and leaves other grants alone. A replacement retains the existing
preview and replacement rules, including removal of additions and protection
of bindings owned by another parent. Delegation still replaces, never adds.
Expiry must stop the grant's extra routes at the pool even before the
sweep runs. Scope does not waive any lifetime limit.

### 4. The screen is short and says what will be granted

Keep the selected secret, each part's answer and scope, duration, and
addition/replacement visible together. Default to “Requested routes” on every
new approval. Do not remember a wider choice for the box: the binding already
covers later commands until expiry. Standing grants across boxes belong to
#162.

For requested hosts, show the host patterns and “Any method and path on these
hosts”. For anywhere, show “No grant restriction”. Both say “Still limited by
credential scope”, and a delegated grant also names its parent restriction.
Full authoritative route lists are one expansion away. Policy labels supplied
by the box describe the ask, never a wider approval as if it were that role.
Requested-scope route edits remain marked edited.

The replacement preview still shows access being removed and lifetime being
shortened. Shorter text must not hide those consequences. Changing the
selected secret refreshes the preview. The scope is defined relative to live
ceilings, so this proposal needs no revision-pinned scope preview protocol.

### 5. Record the wider consent, not just a mutable binding ID

In the approval transaction, keep the original ask intact and record per
approved part: the chosen scope, selected secret ID and revision, granted
route lists with their source IDs, parent binding ID when present, absolute
expiry, delivery and addition/replacement mode. The existing actor, decision
time and binding ID remain. Store enough to reconstruct what was approved
after the binding changes or expires; never store a credential value here.

Binding events retain their actor and request cause. Request get and approval
history show the selected scope explicitly, with the original ask and the
granted scope available separately. A sandbox approval names its actor and
parent delegation rather than suggesting a person approved it.

Upgrade existing rows in place. Pending requests retain the requested default;
old decisions are marked legacy scope unknown, since route edits were already
possible and a binding's current routes cannot reconstruct past consent.
Never backfill old approvals as whole or rewrite their bindings.

## Alternatives rejected

- **Anywhere by default.** Existing clients that omit a field must retain
  requested routes; broader grants require an explicit choice.
- **Snapshot the credential's routes into the binding.** The owner's choice
  is how much the grant itself restricts access. The existing live secret and
  parent ceilings already bound it; a snapshot adds another permission layer
  and changes how future secret edits affect a grant.
- **Gather routes from bindings elsewhere.** Their targets and parents
  represent other authorizations, not the selected credential's scope.
- **One scope switch silently applies to both parts.** Wider use and wider
  delegation are different decisions; each must be explicit.
- **Merge secret and parent routes into one list.** Most-specific matching
  within one list differs from requiring each list to allow an operation.
- **Remember a wider default for the box.** A later request could receive
  broader access without a fresh explicit choice. The approved binding
  already avoids repeat requests for operations it covers.

## Consequences and acceptance evidence

Implementation updates the canonical OpenAPI approval and request schemas,
generated contracts, secrets service/model/store migrations, CLI, shared
console/TUI dialog and request history. It retains existing binding route
lists and their authorization path. Current-state design docs change with
code, not with this proposal. This PR contains no implementation.

Acceptance must request two routes, approve anywhere for an hour, and exercise
a third route allowed by the secret without another request. Requested routes
must still refuse that third route. Requested hosts must allow another path
on a requested host and refuse an unrequested host even when the secret allows
it. Anywhere must allow that other host only when the secret and parent do.

Cover secret and parent blocked routes, method/GraphQL/MCP ceilings, exact and
wildcard requested hosts, blocked-only host extraction, later secret and parent
edits, expiry, add and replace, both parts independently, denied parts,
conflicting route overrides and historical consent after binding deletion.
A fresh approval must default to requested routes after a prior anywhere grant.

## Open for implementation

Exact compact summary layout, migration column names and serialization of the
historical route-list snapshots are implementation choices. They must retain
the chosen scope, original ask, granted restrictions and independent ceilings.
