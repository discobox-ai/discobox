# 26-11-04-389 — A lead binds secrets within its delegation

- **Status**: Accepted (amends [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §3's approval-or-create-only handoff and its decision not to require route containment at approval; §5's direct-write authorization, extended to bounded sandbox writes; narrows [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md) §5's replacement preview requirement for sandbox approvers only)
- **Date**: 2026-11-04
- Accepted by the owner by merging #645, 2026-11-05
- **Issue**: [#616](https://github.com/discobox-ai/discobox/issues/616)
- **Keeps**: [26-11-03-970](26-11-03-970-a-discobox-adopts-another-as-its-child-when-a-person-approves.md) §§2, 9's immediate-parent authority and private child credentials; [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md) §5's replacement protections and person-facing replacement preview.

## Context

Checked against main at `c5594a11dbb477f118cc77ebe78cac19cc187bda`:

- `server/internal/auth/authorizers.go` already scopes secret requests by the
  requester's `ParentSandboxID`. Adoption is not missing from that predicate.
  The sandbox role has no secret-binding routes.
- `server/internal/resources/secrets/bindings.go` rejects sandbox principals
  for direct binding writes. Both created and adopted children are affected.
- `cli/internal/cli/secret.go` reads standing bindings before approval with
  an explicit secret, then reads granted bindings to print its answer. Those
  forbidden reads can prevent approval or report failure after it succeeded.
- `server/internal/resources/secrets/delegation.go` chooses a live delegation
  of the same secret, rechecks it transactionally, rejects explicitly excessive
  lifetimes and caps default lifetimes. `store.RouteLists` in
  `server/internal/store/sandbox_secrets.go` intersects the child's routes
  with the parent's and secret's at use. A submitted broader list can thus be
  stored without granting broader effective access.
- `server/internal/resources/secrets/service.go` refuses any request containing
  a delegate part when the answerer is a sandbox. This is a deliberate trust
  restriction, not a missing role route.

The issue asks for direct grants on any led child and rejection of grants
that exceed the lead's delegation. Route intersection already prevents
broader effective access; it does not establish that the submitted grant was
within the authority the lead was given.

## Decision

### 1. The same authority applies to every handoff

A sandbox with `DiscoboxAPI` may create a sentinel use binding on an existing
sandbox whose current `ParentSandboxID` is the caller, in the same project.
Created and adopted children are equal. The target must still exist and be
neither archived nor being deleted. It may also grant that use by approving
a child's request or in the transaction that creates a child (`run --bind`).
A grandchild, unrelated sandbox, the caller itself, a harness config and the
project are not targets of this authority.

Every path requires one live delegate binding covering the caller, of the
same secret. A use binding confers no authority to hand anything on. An
attached harness config's use-only coverage confers no delegation. Secret
lookup remains restricted to secrets the caller may delegate; it does not
reveal the project's other secrets.

The child records that delegate binding as `ParentID`. Several delegations
are alternatives: one must cover the whole grant. Their routes or lifetimes
are never combined to manufacture a wider delegation. Parent choice is
deterministic among fitting candidates (latest expiry, then binding ID),
and the chosen ID is recorded.

Direct creation uses `POST /projects/{projectId}/sandboxes/{sandboxId}/secret-bindings`.
The sandbox role authorizer loads the target from the path and checks its
current parent against the authenticated caller before body interpretation,
following `server/internal/auth/DESIGN.md`'s resource-identity rule. The body
carries the grant but no target kind or target ID; the path fixes both. The
service rechecks this authority in the write transaction (§3). The existing
project-level `POST /projects/{projectId}/secret-bindings` remains person-only;
the CLI routes a sandbox's direct grant through the target-addressed endpoint.
General binding list, get, update, delete and event-list routes remain
unavailable to the sandbox role. This proposal
gives no authority to modify a binding a person or another delegation owns.
A direct create that collides with an existing binding returns a conflict;
it does not replace it. Approval retains the existing replacement and
addition rules, including checking every affected binding before writing.
Raw delivery remains person-only on every path.

### 2. Submitted routes must fit; runtime intersection remains mandatory

For a sandbox handoff, let `D` be the requests allowed by the delegation's
effective route lists (the secret and delegate binding), and `G` the requests
allowed by the proposed use list under the secret, before adding the parent.
The server accepts only when it can prove `G` is a subset of `D`. An omitted
use list explicitly inherits the selected delegation's effective limits;
it does not mean unrestricted access.

The proof follows `pkg/secretroute` semantics, including:

- exact, wildcard and suffix hosts;
- methods, including method overrides; exact paths and segment globs;
- GraphQL operation types and root fields, and MCP tool patterns;
- most-specific-route selection and blocked routes, including exceptions;
- unreadable or ambiguous protocol bodies and the matcher's refusal rules.

Checking only open hosts, comparing individual allow routes, or sampling
requests is insufficient. For example, an open `/**` without the delegate's
blocked delete route must be rejected even when runtime intersection would
have stopped that delete. Empty and deny-only lists keep their existing
meanings. A broader submitted grant returns 403 and writes nothing. If a
bounded proof cannot establish containment, it fails closed with an
explanation that the caller must use a provably narrower list or ask a person.
It never falls back to accepting based solely on runtime intersection.

A credential policy is a way of supplying concrete routes. Its file name,
description, digest and arguments are provenance, not authority. The same
resolved list gets the same containment result whether supplied inline or
through any policy file. Changing a label cannot authorize broader routes;
an unused policy argument cannot imply scope it did not put in a route.

Runtime enforcement still intersects the secret, child and parent lists on
every use. A subsequent narrowing of the secret or delegation immediately
narrows children; passing containment once does not freeze the parent's old
permission. Existing bindings are not rewritten or deleted merely because
the new write-time rule would reject their submitted list.

### 3. Lifetimes, races and changes of lead

An explicit lifetime must fit the secret's limit and end no later than the
selected delegation. An explicitly unlimited grant cannot come from a finite
delegation. Omitted lifetimes use the existing defaults capped to both limits;
a parent with less than one second left grants nothing.

The write transaction rechecks the caller's API access, current parent
relationship, target eligibility, live delegation coverage, secret identity,
route containment and expiry against the rows it will commit under. The
create path establishes the child's parent in that same transaction. Locks
or conditional writes must serialize with adoption, release, delegation
changes and deletion, so an authorization read before such a change cannot
commit afterward using the old authority. Rejected creates leave no sandbox;
rejected approvals leave the request pending and all bindings unchanged.
Bindings, audit events and reconcile dirty marks commit together.

Removing or expiring a delegation removes or invalidates every use handed on
under it, as today. Removing its delegate side has the same effect. Adoption
and release do not transfer bindings or change their `ParentID`: existing
grants continue under the original delegation until revoked or expired, as
970 §9 requires. The former lead cannot make another grant after reparenting;
the new lead must use its own delegation and cannot take over the old one's
bindings. Grant authority depends on the current parent; existing credential
lifetime depends on the recorded binding parent.

### 4. Further delegation stays person-only

Only a person grants delegation authority. A sandbox hands on use only,
never delegation, whether by direct binding, at create, or by approval.
A request containing a delegate part is answered entirely by a person,
including denial. This retains 880 §3 and 022 §4; an approved use binding
cannot become a new delegation parent.

The owner chose this boundary for #616 on 2026-11-04. Recursive delegation
is outside this proposal. Revisit only if a concrete workflow needs a child
to grant authority onward without a person giving that child its own
delegation; that decision must specify chain validation, ancestor expiry
and revocation, and the accepted decisions it supersedes.

### 5. Approval succeeds without reading the child's bindings

The sandbox CLI path may read the request it is authorized to answer and its
own delegable-secret view. It sends the approval without listing the child's
bindings and reports the committed decision from the response, without a
follow-up binding GET. Server validation and conflict responses decide what
can be replaced. The CLI must not turn a successful mutation into failure
because an optional display fetch was forbidden.

Approval responses carry a bounded receipt for the grants just written:
request and binding IDs, secret identity, target, variable, effective route
lists, effective expiry, selected delegation ID and answerer identity.
Direct create returns the equivalent receipt for the binding just created.
These are mutation results, not permission to inspect the child's other
bindings. A denied part returns no binding receipt. No receipt contains a
value, raw handle or sentinel. Person clients retain their full replacement
preview and binding inspection; the sandbox path does not fabricate a preview.
The response schema and client rendering are implementation work.

### 6. A person can see who granted access

The console's answered-request view and binding history say “Approved by
<box>” or “Bound by <box>”, and show the stable sandbox ID, time, target,
selected delegation and effective routes and expiry. A mutable box name is
only a display label beside the ID. The binding event and request record
carry the sandbox actor, never merely the human user attached to its
principal. A person can follow the delegation to the person who authorized
it, inspect the resolved routes, and revoke through existing binding controls.

No extra confirmation or pending inbox item is created for each bounded grant.
A direct binding has no invented credential request: its binding event is its
history. Policy labels never replace the effective route summary. Historical
records remain attributable after a box is renamed or deleted.

## Alternatives rejected

- **Only fix adopted-box authorization.** Main already uses the parent for
  requests; created children also fail the binding reads and direct writes.
- **Keep runtime intersection as the only route bound.** It prevents wider
  effective use, but accepts a submitted grant outside the delegation and
  hides mistakes in role files. Keep it as defense after strict write checks.
- **Give leads child binding CRUD or broad read access.** That reveals and
  risks modifying credentials a person or another lead gave the child.
  Mutation receipts solve the CLI problem without changing that boundary.
- **Allow bounded recursive delegation.** Even with route and expiry bounds,
  it lets a sandbox decide who may grant access onward. The owner keeps that
  decision with people; this issue needs leads to grant use to their children.
- **Union several delegations to fit one grant.** There would be no single
  parent whose removal takes back the authority it supplied.
- **Revoke every existing grant when a child changes lead.** Adoption changes
  leadership, not the credential decisions already made. Its existing
  semantics deliberately preserve those decisions.
- **Trust CLI checks or policy names.** Direct API callers choose their own
  body; the service must enforce the concrete routes on every path.

## Consequences and implementation questions

Implementation changes the sandbox role, binding and approval services,
create-time binding preparation, mutation response contracts, CLI output,
console attribution and tests. Live design documents change with that code;
this ADR alone changes no runtime behavior. Any receipt or attribution fields
requiring persistence must be added by forward migrations; existing history
and bindings remain intact.

The implementation must choose and document a bounded containment algorithm
in `pkg/secretroute`, its resource limits and supported proof cases. It must
handle identical normalized policy lists, narrower repository paths and the
blocked-route example above; inability to prove other valid expressions is
an explicit refusal, never implicit acceptance. Lock ordering and the exact
receipt DTO are also implementation choices; the authorization semantics are
not.

Verification must exercise raw API callers as well as the CLI. The server
matrix covers create, direct bind and approve, for created and adopted
children: allowed same-secret grants; missing, expired, revoked or unrelated
delegations; wrong secret; excessive host, method, path, GraphQL and MCP scope;
removed blocked routes; misleading or changed policy labels; excessive and
unlimited durations; additions and replacements; raw or delegate attempts;
unled, cross-project and grandchild targets. Race tests cover reparenting,
expiry and delegation edits between preparation and commit. Tests also prove
that effective intersection and revocation still work after acceptance,
that a successful CLI approval needs no forbidden binding reads, and that a
person sees the sandbox approver and parent delegation in history.
