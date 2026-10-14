# 26-10-14-320 — A discobox acts on any discobox in its project, and the judge decides which

- **Status**: Accepted
- **Date**: 2026-10-14
- **Supersedes in part**: the authority by creator that
  [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md) §2,
  [26-10-01-397](26-10-01-397-a-discobox-reads-and-types-into-the-terminals-of-the-discoboxes-it-creates.md) §1,
  [26-10-02-478](26-10-02-478-a-discobox-starts-and-stops-the-discoboxes-it-creates.md) §1, and
  [26-10-08-447](26-10-08-447-a-discobox-tags-the-discoboxes-it-creates.md) §1
  put on the sandbox role's discobox routes; 26-10-02-478 §2's exclusion of
  repair and upgrade, and
  [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md)
  §7's keeping of the repair operation out of the sandbox role; and
  [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)
  §4's refusal of port tunnels and its deferral of a wider role, for the routes
  §3 adds.
- **Relates to**: [26-09-30-782](26-09-30-782-a-discobox-answers-its-own-discoboxes-requests-within-what-it-may-delegate.md)
  §2, which this keeps; [26-10-14-691](26-10-14-691-the-judge-is-told-who-is-asking-from-the-control-planes-record.md),
  whose deferred facts about a request's target §5 decides; and
  [26-09-25-428](26-09-25-428-the-judge-may-let-its-allow-stand-for-a-route.md),
  whose standing allows §6 narrows on the discobox API.

## Context

ADR 0140 gave a sandbox one fixed role and kept every judgement about what a
use means in the judge. It rejected authorizing by who created a discobox. Each
widening since then brought creator authority back for its own routes: source
delivery, terminals, power, and meta are each allowed only on a discobox the
caller created (`createdSandbox` in `SandboxRoleAuthorizer`).

That rule now fights the judge. A person approves a use of
`ai.discobox.sandbox` in their own words, for example "check on the reviewer
discobox and restart it if it is stuck", and the judge allows the call. The
server then refuses it because the reviewer was not created by the caller. The
person has said yes and the judge has said yes, and a rule neither of them can
see says no. There is no way to approve past it.

Several routes a lead needs to look after its workers are also missing from the
role: repairing or upgrading a broken one, reading its services and logs,
running its services, and reaching what it serves over HTTP.

## Decision

### 1. The role's discobox routes are scoped to the project, not the creator

Every per-discobox route in the sandbox role is allowed on any discobox in the
caller's own project. The project rule `SandboxRoleAuthorizer` already applies
is the boundary, and `default` still means the caller's project. The
`createdSandbox` ownership is removed from:

- source delivery: the `git-receive-pack` refs advertisement and push into
  `git-origins`, `pin`, and `complete-source-push`;
- terminals: `execs`, `screen`, `wait`, and `input`;
- power: `start`, `stop`, and `restart`;
- meta: `PATCH meta`.

Which discobox a call may act on is the judge's question, decided from the
use's words and the facts in §5. A person who wants the old bound writes it
into the use: "only the discoboxes this discobox created".

### 2. Secret requests stay the creator's, hard-coded

`GET`, `approve`, and `deny` on `secret-requests/{id}` keep the `ownedRequest`
ownership, and the request listing stays filtered to owned requests
(ADR 26-09-30-782 §2). A discobox answers only requests filed by a discobox it
created, within its delegation grants as before.

The two grant purposes are kept apart by this rule, not by the judge. A person
who approves a `delegate` grant agrees that the holder may hand a credential
on, not that it may use it. Answering any request in the project would undo
that in two ways:

- A sandbox could approve its own request.
- Two sandboxes holding the same delegation could approve each other's
  requests.

A discobox is never its own creator, and two discoboxes cannot each have
created the other, so the ownership rule rules out both.

### 3. The role gains repair, upgrade, services, exec reads, and HTTP

Each of these is allowed on any discobox in the project, as in §1:

- `POST sandboxes/{id}/repair` and `POST sandboxes/{id}/upgrade`. Both move a
  discobox to the image its harness config resolves to, and the caller chooses
  nothing about which image that is.
- `GET sandboxes/{id}/services`, `GET .../services/{serviceId}`, and
  `GET .../services/{serviceId}/logs`, plus `POST .../services/{serviceId}/start`,
  `stop`, and `restart`. A service runs a command the target discobox itself
  declares.
- `GET sandboxes/{id}/execs/{execId}`, `GET .../execs/{execId}/logs`, and
  `GET .../execs/{execId}/events`.
- `sandboxes/{id}/http/{port}` and `sandboxes/{id}/http/{port}/*`, with any
  method and any port.

`authorizeRequestedScopes` adds `sandbox:http` to the scopes a sandbox may use.
The exec and service routes use the `exec:read` and `exec:write` scopes it
already allows.

A discobox's status and sources need no route. The control plane does not
serve the sandbox agent's `status`, `sources`, or `project-layer` to anyone.
What a client sees of them is on the discobox record, which the role already
reads.

### 4. The judge discobox is never a target

No per-discobox route in the role reaches the project's judge discobox
(`Project.JudgeSandboxID`). `SandboxRoleAuthorizer` refuses it as a target
before the judge is asked.

Its logs and terminals hold every job it judged, including other discoboxes'
request bodies. Restarting, repairing, or upgrading it, or reaching its ports,
would interfere with the thing deciding whether the call is allowed. This is
the one hard-coded exclusion among the project's discoboxes.

### 5. The judge is told about the discobox a call names

When a judged call to the discobox API names a discobox in its path
(`/projects/{projectId}/sandboxes/{sandboxId}/...`), the server reads that
discobox's row and states its core facts, the same ones ADR 26-10-14-691 §3
states for the discobox asking, labelled as the call's target:

- its ID;
- its primary source as created, or that it has none;
- the discobox that created it, or that no discobox did.

This is the fact ADR 26-10-14-691 left undecided. It meets that ADR's §1 test
for a fact: the control plane recorded it at create, and neither discobox can
change it afterwards. With it, "the discoboxes this discobox created" is a use
the judge can hold a call to. Without it, the judge sees only an opaque ID.

A path that names no discobox, or one not in the project, states no target.
`System` says that facts may label a target, and `PromptVersion` moves.

### 6. A standing allow on the discobox API names its discobox

A standing allow answers a call to the discobox API that names a discobox
only when its route names that discobox in a literal segment. A `{name}` in
that segment, or a `{name...}` at or before it, leaves it open, and the call is
put to the judge instead. The server checks this both when it records an allow
on a call that names a discobox and every time a standing allow would answer
one. So a broad route granted on a call that names no discobox, such as the
listing, never covers one that does, and neither does an allow recorded before
this decision.

Otherwise one allow on a route like `POST .../sandboxes/{id}/stop` would cover
every discobox in the project for up to fifteen minutes, without the judge ever
seeing the target facts §5 adds.

## Alternatives rejected

**Keep authority by creator, and widen route by route.** This is what we have
now. Each lead workflow outside the creator's tree, such as a reviewer looking
after a person's discobox, needs another ADR. Meanwhile a person who has
approved the use and a judge that has allowed the call can both be overruled
by a rule neither can see.

**Let a discobox answer any secret request in the project, held only to its
delegations.** The delegation checks bound which credential may leave, but
not who receives it. Self-approval and mutual approval would turn `delegate`
grants into `use` grants (§2).

**Limit `http/{port}` to ports the target declares.** Whether a call is part
of the work is the judge's question. A declared port is a statement about the
target, not about the call. What reaching a port can lead to is listed under
Consequences.

**Hard-protect discoboxes a person created.** That would be a second authority
rule beside the judge, keyed on creator, which is the kind of rule this ADR
removes. The target facts tell the judge whether a person or a discobox created
the target, and the person's use can say which it may touch.

**Add archive.** Archive changes what exists, not whether it runs, and
together with purge it is the one step that loses a discobox's workspace. It
stays a person's call.

## Deferred

- **Archive, purge, and unarchive.** Revisit when a lead must clean up workers
  it finished with and a person's confirmation is the bottleneck.
- **Attach, exec create, and exec start or delete.** ADR 26-10-01-397 §2's
  reasons still hold: attach is an unjudged stream, and a new exec is a second
  way into a discobox beside its harness.
- **Long-lived streams through the gate.** The gate answers one request with
  one response within its timeout. Following logs, resource streams, and
  anything that upgrades the connection do not fit that model and are out of
  scope here.
- **`resources/*`, `tools`, `exec-events`, and `harness-hooks`.** These are
  reads nobody has needed yet.

## Consequences

- What one discobox may do to another in the same project is bounded by the
  use a person approved and the judge's reading of it, not by the server. A
  use worded without a target, like "manage discoboxes", reaches every
  discobox in the project except the judge.
- Source delivery reopens what ADR 26-09-24-630 rejected. A discobox the judge
  allows may push into the origin of a discobox a person created, and that
  discobox's agent fetches its origin as the user's newest commits. The judge
  is told whose discobox the target is (§5), and that fact is the only thing
  between the push and the target's agent.
- `http/{port}` reaches anything listening inside the target: a dev server, a
  debugger port, or a nested discobox server with its own default user. Calls
  the target's software makes in response carry the target's credentials. The
  target's judge still holds those calls to the target's approved uses, but
  the caller now chooses which calls are made.
- Repair and upgrade rebuild the target's container and lose anything
  installed outside the workspace. That includes a discobox a person
  deliberately held on its image under a `manual` upgrade policy.
- Every judged call that names a discobox costs one more sandbox read on the
  server, and answers recorded before §5 are asked again once.
- Tests in `sandbox_role_internal_test.go` that refuse a discobox the caller
  did not create are replaced by tests that allow one in the project, refuse
  one in another project, and refuse the judge discobox.
  `test/judge-evals` gains a use "only the discoboxes this discobox created",
  allowed for a child and refused for another discobox.
