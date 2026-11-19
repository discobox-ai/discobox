# 26-11-19-927 — A person may rebind a box's variable to another secret, and the sentinel stays

- **Status**: Proposed
- **Date**: 2026-11-19
- **Issue**: [#832](https://github.com/discobox-ai/discobox/issues/832)
- **Would amend**:
  - [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md)
    §5's "a variable that already holds a *different* secret is still
    refused instead of taken over". A person approving with an explicit
    `replaces` may take it over (§2). Every other refusal in §5 stands.
  - [26-11-03-329](26-11-03-329-a-box-may-be-granted-routes-for-a-time-beside-its-binding-without-replacing-it.md)
    §2's "additions of another secret … are not deleted" for one case: a
    rebind deletes the old secret's additions in that variable (§4).
  - [26-11-11-539](26-11-11-539-a-replaced-secret-value-reaches-the-proxy-cache-and-a-rejection-report-names-the-value-it-refused.md)
    §1's generation. It names the secret as well as the write stamp (§5).
- **Leaves standing**: [26-10-27-840](26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md)
  §2 (only a grant of the value again renews a raw binding through an
  approval). [26-11-04-389](26-11-04-389-a-lead-binds-secrets-within-its-delegation.md)
  §1 (a lead's direct create that collides is a conflict, never a
  replacement). [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §2 (the box's own binding shadows its harness config's and project's).

## Context

Checked against `main` at `20d5aa5a`.

A box's variable (`GH_TOKEN`) is delivered by one plain use binding, plus
any additions of the same secret (329 §2). Once one of the box's own
bindings holds the variable, nothing binds a **different** secret there:

- **Approval as an addition:** `addBinding`
  (`server/internal/resources/secrets/service.go`) resolves the variable's
  effective binding (`ResolveSandboxBindings` → `UseFor`). If it holds
  another secret, it refuses with 409.
- **Approval as a replacement:** `replaceBindings` refuses with 409 when
  one of the box's own use bindings of another secret has the variable.
  This happens before `bindingplan` plans anything, so `checkReplaced`
  never sees that binding.
- **A person acting directly:** `CreateSecretBinding`
  (`.../secrets/bindings.go`) hits the per-target variable index
  (`idx_secret_binding_use`), and `bindingConflict` returns 409.
  `UpdateSecretBinding` cannot change a binding's secret.

So the way through is to delete the binding and then create another. That
takes everything the old binding carried with it. A binding that both uses
and delegates loses its delegation, and every binding handed on under that
delegation is deleted. Between the two writes the variable delivers
nothing.

Three things already work and shape the answer:

- **The sentinel survives a change of secret.** `materializeSandboxSecrets`
  (`.../sandboxes/secrets.go`) keeps one `sandbox_secrets` row per
  variable. When the binding behind it moves to another secret, the row is
  repointed and keeps its sentinel. It mints a new sentinel only when the
  new secret's sentinel format differs. A process holding the variable
  keeps working, with no restart.
- **An inherited variable can already change secret.** `replaceBindings`
  checks only the box's own bindings. So when a harness config or the
  project delivers `GH_TOKEN` with secret A, approving secret B in
  `GH_TOKEN` writes a box-level binding that shadows A (880 §2). A person
  or a lead answering under its delegation can do this. The preview says
  only `shadowed` (`pkg/bindingplan.Replacing`), because `Held` does not
  carry the secret. The approver is not told that the secret changes.
- **The proxy drops a cached value whose generation changed** (539 §1).
  The generation is the secret's value-write stamp
  (`model.Secret.ValueGeneration`, microseconds in base 36). It does not
  name the secret. Two secrets whose values were written in the same
  microsecond have equal generations. A rebind between them would then
  leave the old secret's value in the proxy cache under the repointed
  sentinel.

The refusal exists for good reasons, and those reasons constrain any
loosening. A binding held under another parent is not taken over by a
request (022 §5). A raw binding is renewed only by granting the value
again (840 §2). Delegations handed on under a binding stay (022 §5's
split). A loosening that keeps those three rules is safe. A swap the
approver cannot see is the hole to avoid: a narrow key silently replaced
by a broad one.

## Decision

**A person may rebind one of a box's variables from secret A to secret B,
either directly or by approving a request. Each path names the binding it
replaces. The variable keeps its sentinel. The old binding's delegation is
kept by splitting it. The proxy drops A's cached value. One event records
both secrets. A box never rebinds a variable, including a lead answering
for its child.**

### 1. Who may rebind

| Actor | Path | Rebind the box's own binding to another secret |
| --- | --- | --- |
| A person | Directly (`discobox admin binding create --replaces`, the console) | Yes (§3) |
| A person | Approving a request, with `replaces` | Yes, narrower (§3) |
| A lead answering a child's request under its delegation (389) | Approval | No: 409 as today |
| A lead creating a binding directly on a child (389 §1) | Direct create | No: a collision is a conflict, as today |
| The box itself | `discobox-access request` | It can ask; it cannot rebind. A person decides |

A box asks the same way it asks today. It names a credential and a
variable, and it cannot name what to replace. The request carries no new
field. Changing which secret delivers a variable is a choice between two
credentials the box did not choose. Only someone who can see both makes
it. A lead approves without a full preview (389 §5), so it never makes
that choice.

### 2. A rebind names the binding it replaces

Both paths gain one field, `replaces`, holding the ID of the box's binding
that delivers the variable now:

- **Direct:** `CreateSecretBindingBody.replaces` and
  `discobox admin binding create … --replaces <binding>`. The new binding
  must be a sandbox-level use binding in the same variable and on the same
  box as the binding it replaces.
- **Approval:** the use part's decision (`SecretRequestPartDecision`)
  gains `replaces`, and `discobox secret request approve` gains
  `--replaces <binding>`. It is valid only with `as: replace`, never with
  `add`.

The server re-reads the variable inside the write transaction. It refuses
with 409 when any of these hold:

- the variable is held by another secret and `replaces` is absent;
- `replaces` names a binding that no longer delivers the variable;
- `replaces` names a binding of the secret being granted, where the plain
  replacement already applies (022 §5).

The refusal names the binding that delivers the variable now. So the
person approves the swap they saw, and a write that lands between preview
and approval is never swapped over unseen. A refusal says which condition
in §3 failed, never just "already holds another credential".

Every caller that reads a preview sends `replaces`: the CLI and the
console. They never fill it in on their own. The person confirms a screen
that shows both secrets (§6).

### 3. What may be replaced on each path

The binding named by `replaces` is checked like every binding a
replacement touches (`checkReplaced`, 022 §5), with these per-path rules:

| The replaced binding is… | Direct (a person) | Approval (a person) |
| --- | --- | --- |
| a plain use binding, parentless (a person's) | yes | yes |
| held under a delegation (`ParentID` set) | yes; the new binding is the person's, with no parent | no: 409, as 022 §5 ("not taken over") |
| use and delegate (combined) | yes, split (§4) | yes, split (§4), as 022 §5 splits today |
| raw | yes (§4) | no: 409; change it directly |
| combined and raw | yes, split (§4) | no: 409, as today |

The direct path is wider for a reason. A person can already delete any
binding of the box and create another. The atomic rebind is never wider
than those two steps, and it loses less. The approval path keeps 022 §5's
protections because the request is the box's, and the answer applies the
same plan the preview showed (`bindingplan`).

Secret B is checked as any new binding's secret is: the lifetime limit
(`guardBindingTTL`), the body-place check (`checkSecretBodyPlaces`), and
`AllowRaw` when the new binding is raw.

### 4. What happens to the old binding

- **Its delegation stays.** A combined binding is split, as 022 §5 splits
  one. It keeps its ID as A's `Delegate` binding. Everything handed on
  under it keeps its parent, its routes and its lifetime. Only the use side
  goes. A delegate binding has no variable, so the box can keep delegating
  A while it uses B in the variable. Nothing is revoked from a child.
- **A use-only binding is deleted**, in the same transaction that creates
  B's binding. The variable is never empty in between.
- **A's additions in the variable are deleted.** A rebind states the
  variable's whole use. A dormant addition of A would apply again if the
  variable ever came back to A, and nobody looking at B's binding would
  expect that. A's bindings in other variables are untouched: a rebind is
  about one variable. On the approval path, 022 §5 still replaces B's own
  other use bindings of the box, because the request states the box's
  whole use of B.
- **The sentinel stays.** The variable's `sandbox_secrets` row is
  repointed to B's binding by `materializeSandboxSecrets`, as it is today.
  A process holding the variable sends the same sentinel. From the next
  delivery the proxy resolves it to B, with no restart. Only when B's
  sentinel format differs from A's (`SentinelFormat`) is a new sentinel
  minted. A running process still holding the old one is then refused,
  and the preview says so beforehand (§6). A sentinel is never re-minted
  just because the secret changed: 539 rejected that for the same reason.
- **Delivery and the gap.** The rebind marks the box's holders
  (`markBindingHolders`). Until the pool takes the delivery, it still holds
  A's grants and may hold A's cached value, so a request in that window
  can still carry A on A's routes. That is the same window a lapse or a
  route change has today (329 Context). A resolve after the commit hands
  out B only.
- **A raw binding** (direct path only). A's value has already been handed
  over and cannot be taken back. Once the delivery lands, the sandbox
  agent removes `/run/discobox/raw-secrets/<VAR>`. A process started with
  `env` delivery keeps A's value. The proxy stops watching for A's value,
  because no binding of the box holds it. Deleting the raw binding does
  the same today. The preview says it (§6). If B's binding is raw, it is
  B's value, granted by the person as 840 §2 requires.

### 5. The proxy drops the old secret's value

The generation delivered for a sentinel, reported by the proxy and
compared by the control plane (539 §§1–2), names its secret: the secret's
ID and its value-write stamp. A rebind therefore always delivers a
generation that differs from the one the proxy cached. The proxy drops
A's value and the previous value it displaced (539 §1) whatever A's and
B's stamps are. A report about A's value that arrives after the rebind
names A's generation. B's generation is different, so the report is
dropped and spends nothing of B's (539 §2).

The generation stays opaque outside the control plane. At upgrade, every
sentinel's delivered generation changes once. `markSentinelsIfStale`
re-delivers each box once, and each proxy re-resolves each
`(box, sentinel, host)` once, which costs what one replacement of every
secret would.

### 6. What the person sees before approving

The CLI (`secret request get` and `approve`, `admin binding create
--replaces`) and the console show, before the person confirms:

- **Both secrets**, A and B, by name and ID, with their own route
  summaries side by side. These are each secret's own routes and the
  binding's routes, as 022 §3 summarizes a role: hosts, methods, path
  patterns with wildcards marked, GraphQL operations and MCP tools.
- **What A's binding loses:** its routes and lifetime in the variable, its
  additions there, and whether it was raw ("A's value stays with processes
  already running, and is no longer watched for").
- **What stays:** A's delegation and how many bindings are handed on under
  it, or "none".
- **The sentinel:** "kept; running processes switch to B", or "a new
  sentinel; processes holding the variable now must restart".

`pkg/bindingplan.Held` gains the secret holding each grant, and `Replacing`
reports a grant of another secret as its own fate, `rebound` for the box's
own binding. A harness config's or project's binding of another secret
stays `shadowed` (§7), but it names both secrets. The CLI and the console
keep reading one rule.

The console's request screen stops locking the use card to the secret
that holds the variable (#722's fix). A person may pick another secret.
The card then shows this preview and sends `replaces`. A lead's answer has
no such choice.

### 7. Inherited variables are unchanged, and now named

A variable inherited from the box's harness config or the project can be
shadowed by the box's own binding of another secret, as today and as 880
§2 intends. This includes a lead answering its child under its
delegation. The inherited binding is never touched. It applies again when
the box's binding goes. That is not a rebind in this ADR's sense, so it
needs no `replaces`. What changes is the preview and the lead's receipt
(389 §5): both name the shadowed secret beside the new one, so the change
of secret is visible wherever the shadow is.

Later, rebinding the box's own shadowing binding to a third secret is a
§1 rebind of the box's own binding.

### 8. Audit

A rebind writes one binding event, `rebound`, in the rebind's
transaction. It carries:

- the new binding, after the change;
- the replaced binding, before the change, with its secret;
- the actor (the person, never a box);
- the cause: the request ID on the approval path.

So one record names both secrets and who did it. The split of a combined
binding writes its `updated` event, as 022 §5 does. No separate
`created` or `deleted` event is written for the swap itself. The event
appears in both bindings' histories (`discobox admin binding events
--binding`, the console's binding history).

## Alternatives rejected

- **Change `SecretID` on the binding in place** (`UpdateSecretBinding`
  gains `secret`). The binding's ID would outlive the secret it was
  written for. Its `ParentID` names a delegation of A, which cannot
  parent a binding of B. Its events, receipt and request would describe
  a binding that no longer exists. A new binding with one `rebound` event
  keeps each record about one secret.
- **Let a box's request ask to replace** (`--replaces` in
  `discobox-access request`). The box would choose which credential to
  remove. A request is how a box asks for what it lacks. The person
  decides what it displaces, as they decide raw (840 §2).
- **Let a lead rebind under its delegation.** A lead approves without a
  full preview (389 §5), and the binding it would replace is usually a
  person's. A lead's own earlier grant could be rebound safely in
  principle. That would need a containment proof across two secrets'
  routes that 389 §2 does not give. A lead that wants a child on another
  secret asks in another variable, or asks a person.
- **Require no delegations below the binding** (the issue's candidate
  rule). The delegation has no variable and is independent of the use
  side. Splitting keeps it, as 022 §5 already does on every approval.
  Refusing would send the person back to delete-and-create, which takes
  the delegations with it: the outcome the rule meant to prevent.
- **Mint a new sentinel on every rebind.** That is what delete-and-create
  does. It breaks every running process for no gain: the proxy resolves
  by row, and the row moves. 539 rejected it for the same reason.
- **Infer `replaces` from the variable.** Simpler to call, but the
  approval would swap whatever holds the variable when it commits, not
  what the person saw. Naming the binding makes the confirmation exact.
- **Allow a raw rebind through an approval.** It would end the proxy's
  watch on a value the box still holds, as a side effect of an answer to
  the box's own ask. That is a decision about A's value, and 840 §2 keeps
  it a direct one.
- **Keep A's additions dormant** (329 §2's shadow rule). They would
  silently come back if the variable returned to A, outside any preview
  of that later change.
- **Require `replaces` to shadow an inherited variable, or forbid a lead
  from shadowing one.** The owner chose to keep 880 §2's shadow as it is
  (2026-11-19, on #832): it is correct as it is. It never touches the
  inherited binding. Naming both secrets is the fix.

## Consequences

- A person can move a box from one credential to another in one step,
  keeping its delegations and its sentinel. Delete-and-create is no longer
  the only way.
- These change: the binding create body and `discobox admin binding
  create`; the approve body's use decision and `discobox secret request
  approve`; `bindingplan` (`Held` names its secret, a `rebound` fate); the
  console's request and binding screens; the secret binding event kinds
  (`rebound`, with the replaced binding); `Secret.ValueGeneration`. Docs
  that change with them: `secrets/DESIGN.md`, `bindingplan/DESIGN.md`,
  `cli/internal/tui/DESIGN.md` and `docs/agent-credentials-protocol.md`.
- No schema change to bindings. The event's replaced binding is a new
  serialized field.
- Every pool re-delivers and re-resolves once at upgrade (§5).
- Tests cover each row of §§1 and 3, allowed and refused, and the
  `replaces` mismatch. They also cover: a split keeping its children; the
  sentinel kept, and re-minted on a format change; the proxy dropping A's
  value when A's and B's stamps are equal; a late report about A dropped;
  and the inherited shadow naming both secrets.

## Open for implementation

- How the `rebound` event appears in the replaced binding's history: a
  second row keyed to it, or an index on the replaced ID.
- The console's layout of the two-secret preview and its controls.
- The exact refusal wording for each §3 condition.
