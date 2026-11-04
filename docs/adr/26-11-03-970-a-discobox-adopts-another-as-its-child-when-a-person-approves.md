# 26-11-03-970 — A discobox adopts another as its child when a person approves

- **Status**: Accepted
- **Date**: 2026-11-03
- Accepted by the owner by merging #494, 2026-11-04
- **Supersedes**, in each, only that the authority it gives is read from
  the record of who created a discobox; what each route allows stands:
  - [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md)
    §2 (source delivery);
  - [26-10-01-397](26-10-01-397-a-discobox-reads-and-types-into-the-terminals-of-the-discoboxes-it-creates.md)
    §1 (terminals);
  - [26-10-02-478](26-10-02-478-a-discobox-starts-and-stops-the-discoboxes-it-creates.md)
    §1 (power);
  - [26-10-08-447](26-10-08-447-a-discobox-tags-the-discoboxes-it-creates.md)
    §1 (meta);
  - [26-11-03-724](26-11-03-724-a-discobox-archives-the-discoboxes-it-created.md)
    §1 (archive);
  - [26-09-30-782](26-09-30-782-a-discobox-answers-its-own-discoboxes-requests-within-what-it-may-delegate.md)
    §2 (answering a discobox's secret requests);
  - [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
    §9's "it reaches only the discoboxes the sandbox created";
  - [26-11-01-560](26-11-01-560-discobox-box-watches-other-boxes-through-the-sandbox-event-stream.md)
    §§1–2's `created-by` selecting by `createdBySandboxId`.
- **Relates to**:
  [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  which rejected a tree of discoboxes with cascading authority; this keeps that
  rejection (§7).
  [26-10-31-819](26-10-31-819-an-egress-policy-may-pass-a-hosts-connect-through-unread.md)
  §2, whose passthrough rule keeps reading the creator (§2).
  [26-10-01-032](26-10-01-032-a-sandboxs-egress-is-denied-when-any-of-its-levels-says-so.md)
  §4, whose `withinCaller` check applies to a parent unchanged.

## Context

Everything a discobox may do to another rests on one fact: the other's
recorded creator is the caller (`Sandbox.CreatedBySandboxID`, set at create,
immutable). `SandboxRoleAuthorizer` reads it in `authorizeCreatedDiscobox`,
which `withinCaller` routes call first, and in `authorizeOwnedRequest`; the
store reads it to filter the secret requests a sandbox lists
(`ListSecretRequests`' owner option); the sandbox event carries it
(`createdBySandboxId`) and the `discobox-box` monitor selects `created-by=me`
by it (`server/internal/auth/authorizers.go`, `server/internal/store/secrets.go`,
`sandbox-agent/monitors/discoboxbox`).

It is also read for something that is not authority:
`store.EffectiveSandboxEgress` passes "created by a discobox" to
`egress.Effective`, which gives such a box only its own level's passthrough
(ADR 26-10-31-819 §2).

Because the fact is fixed at create, a discobox a person created, or one
another lead created, can never come under a lead. In discobox-ai/manager this
is why the manager must create every box, including those the owner works in
by hand: only a box the manager created can join the process. A box the owner
started on their own stays outside it.

Issue #481 asks for adoption: a discobox asks to take another as its child, a
person approves, and the parent then acts on the child as on a box it created.
It leaves three questions open: what re-parenting needs, whether a parent may
be in another project, and whether adoption covers source delivery.

## Decision

### 1. A parent is recorded beside the creator, and the creator stays a fact

`Sandbox` gains `ParentSandboxID` (`parentSandboxId`), nullable and indexed.

- **At create** it is the creating sandbox, the same value as
  `CreatedBySandboxID`. A box a person creates has none.
- **Adoption sets it** (§3). **Release clears it** (§5).
- **It has its own setter** in the store, as `DiscoboxAPI` and the egress
  level do. A whole-row save never writes it, and no update body sets it.
- **The upgrade backfills it** from `created_by_sandbox_id` on every existing
  row, in a migration, so no box loses the lead it has today.

`CreatedBySandboxID` keeps its meaning and stays immutable: which box made this
one. It is what an audit reads, and it stops conferring authority.

### 2. Creator-scoped authority reads the parent

Every check that today asks "did the caller create this box?" asks "is the
caller this box's parent?" instead:

| Reader | Today | After |
|---|---|---|
| `authorizeCreatedDiscobox`, and through it every `createdDiscobox` and `withinCaller` route | creator | parent |
| `authorizeOwnedRequest` (782 §2) | the requester's creator | the requester's parent |
| `ListSecretRequests`' owner filter | `created_by_sandbox_id` | `parent_sandbox_id` |
| the sandbox event, and `discobox-box`'s `created-by` (§6) | `createdBySandboxId` | `parentSandboxId` |

The predicate is renamed for what it now checks (`createdDiscobox` becomes a
parent check; its name is the implementation's). Nothing else about the routes
changes: `withinCaller` still also requires that the target's effective egress
policy be covered by the caller's.

Three readers keep the creator, because each is about how a box was made, not
who leads it:

- **Passthrough** (`egress.Effective`, 819 §2). A box a person made carries the
  passthrough of every level a person set; adopting it changes no policy a
  person decided. A box a discobox made has only its own level's, and a release
  does not hand it the rest.
- **The create-time bounds**: `refuseWiderThanCreator`, and that a sandbox may
  set `DiscoboxAPI` on a create only when it has it itself (880 §9). Both are
  checked once, at create, against the creator.
- **The record**: listings and the event show both fields.

### 3. Adoption is a request a person answers

A new inbox kind, the **adoption request** (`AdoptionRequest`), stored beside
`SecretRequest` and `HostTrustRequest`:

- **Filed through the discobox API** by the sandbox role:
  `POST projects/{projectId}/adoption-requests` with the child's ID and a
  justification. The requester is the calling sandbox. It is not part of the
  agent credentials protocol (`discobox-access`): it asks for no credential and
  the pool has nothing to swap. The CLI spells it `discobox adopt <box>
  --reason "…"`, which files the request and waits for its answer as
  `discobox-access request` does.
- **Read back by its requester**: the role gains `GET adoption-requests` and
  `GET adoption-requests/{id}`, both held to requests the caller filed.
- **Answered only by a person.** `approve` and `deny` are not in the sandbox
  role, on any request. A box that could approve adoptions could take any box
  in the project by asking and answering itself, or through a box it leads.
- **Listed in the inbox.** `approval-requests` includes pending adoption
  requests as their own kind, and the console's inbox answers them. The dialog
  names the child, the requester, the child's current parent if it has one
  (§4), the requester's justification, and what the parent will be able to do:
  read its terminals, type into them, start, stop, repair, upgrade and archive
  it, change its meta, deliver its source, reach its ports, and answer its
  credential requests within what the parent may delegate. When the child is
  wider than the requester's egress, the dialog says that every write and send
  into it will be refused (`withinCaller`), so the parent may only read it.
- **Recorded.** The request keeps its requester, child, justification, the
  parent it replaced, its decision, who decided and when, after it is answered,
  as a secret request does. The parent change is also a sandbox event (§6).

One request is pending per requester and child; filing again returns it.

### 4. The limits, checked when filed and again when approved

A request is refused (`400`/`403`/`409` as fits) when:

- **the child is the requester** — no box is its own parent;
- **the child is one of the requester's ancestors** — the requester's
  chain of parents, walked to its root, must not contain the child, so no cycle
  forms;
- **the child is in another project** — the sandbox role already refuses every
  path outside the caller's project; the request names a sandbox of the
  caller's own project, and a person's approval cannot widen that;
- **the requester does not have `DiscoboxAPI`** — the role refuses it before
  the route is read, as every call (880 §9);
- **the child is archived or being deleted**; or
- **the requester is already its parent**.

Approval checks the same again, since any of it may have changed while the
request waited: the requester may have lost `DiscoboxAPI` or been deleted, or
another approval may have moved a parent. The ancestor walk and the write of
the parent are one transaction that no other parent write interleaves with,
so two approvals answered together (A adopts B, B adopts A) cannot both land.
How that is serialized (a project-row lock, or a conditional write on each
parent read) is the implementation's.

A request that fails its checks at approval is answered `409` and stays
pending, for the person to deny.

### 5. Release gives a child back

`POST sandboxes/{id}/release` clears the parent.

- **The parent may release** its child: the route is in the sandbox role,
  held to a box whose parent is the caller, without the egress check. Giving up
  a box sends nothing into it, so a restricted parent may release a wider child
  it could only read.
- **A person may release** any box.
- Releasing records the change as a sandbox event. The creator is untouched.
- A box with no parent answers `409`.

Purging a box clears the parent of each of its children in the same
transaction: a parent that no longer exists leads nothing, and a listing should
not name it. Archiving one does not, since a person may unarchive it.

### 6. Listing shows the parent and filters by it

- **The API** returns `parentSandboxId` on a sandbox and on its event, and
  `list-sandboxes` takes a `parent` query parameter (a sandbox ID).
- **`discobox ls`** shows a `PARENT` column and takes `--parent <box>`; from
  inside a discobox, `--parent me` names the calling box. `createdBySandboxId`
  stays in `-o json`.
- **`discobox-box`'s `created-by`** selects by the event's `parentSandboxId`.
  The option keeps its name: enabled instances persist their options, every
  in-box skill and the manager write `created-by=me`, and from a lead's side
  "the boxes I created" and "the boxes I lead" were the same set until now.
  Its description changes to say it selects the boxes this box leads.
- **A box entering or leaving that selection is news.** A box newly adopted is
  reported as newly watched (not recorded quietly, as a box newly matching a
  tag is), and a box released or adopted away is reported once as gone from
  the selection, so a lead stops waiting on it.

### 7. Re-parenting is a person's approval alone, and the old parent is told

A child that already has a parent may be adopted; approval moves it. The old
parent is not asked. It is told: the parent change is a sandbox event, and the
old parent's `discobox-box` reports the box gone from its selection (§6).

A person can already do anything to any box in the project; a parent's
authority over a child comes from a person, either by approving adoption or by
granting the creator `DiscoboxAPI`. Asking the old parent would let a box veto
a person. The person is shown the current parent and decides with it named.

### 8. Adoption covers source delivery

Source delivery (630 §2) moves with the rest. A parent may push into its
adopted child's origin as into a created child's.

ADR 630 rejected letting any sandbox push to any discobox's origin because the
target's agent fetches its origin as "the user's newest commits". A parent can
already type into its child's terminals, which reaches everything a push into
its origin does, and its parent was set by a person approving it with the
pushes named. Holding delivery to the creator alone would make one route the
exception every reader of the role has to remember.

### 9. What does not move

- **The child's credentials stay its own.** Its bindings, delegations and
  grants are untouched by adoption and by release. The parent gains the routes
  of §2, none of which returns a secret's value; it may give the child
  credentials only within its own delegations, as it may a box it created
  (782 §3).
- **Authority is one level deep.** A parent acts on its children, not on their
  children, as today.
- **Nothing cascades.** Archiving, purging or releasing a parent changes
  nothing about its children but §5's clearing on purge (0140, 724).

## Alternatives rejected

**Make `CreatedBySandboxID` mutable.** One field, no migration. But the record
of which box made which would be lost on the first adoption, and passthrough
(819 §2) and the create-time bounds would start answering for a box's lead
rather than its making: adopting a person's box would strip its passthrough,
and releasing a discobox's box would hand it passthrough a person never
decided for it.

**Require the current parent's consent to re-parent, or refuse adopting a box
that has one until it is released.** §7. A box would hold a veto over a
person's decision, and a parent that is stuck or stopped could not give its
child up. Revisit if leads run work a person should not be able to move
without the lead's knowledge, beyond the event the lead already gets.

**Let a discobox approve adoption, within what it leads.** A lead could then
take any box its children asked for. Who leads a box is a person's decision,
as `DiscoboxAPI` is.

**File adoption through `discobox-access`, beside credential requests.** The
agent credentials protocol is for credentials a pool delivers; adoption
delivers nothing to the box that asks, and only a box with `DiscoboxAPI` may
ask, which the discobox API already checks.

**Exclude source delivery from adoption.** §8.

**Allow a parent in another project.** Every route of the sandbox role is held
to the caller's project, and a box's credentials, pools and egress levels are
its project's. A cross-project parent would need the role to name another
project's boxes. Revisit if leads are ever meant to span projects; nothing asks
for it now.

**Rename `created-by` to `parent`.** §6. It would break every enabled instance
and every skill that writes it, for a name only a lead reads.

**A separate permission to be adopted, set on the child.** The person's
approval of each adoption already is that decision, made with the child named.

## Consequences

- The manager can bring a box the owner started, or one another lead made,
  into its process by asking to adopt it, and a person approving it once.
- A box may change hands while a lead is using it: the old lead's calls start
  answering `403`, and its `discobox-box` reports the box gone.
- A child wider than its parent's egress can be adopted but only read; the
  approval dialog says so, since this is likely to surprise.
- The schema gains `sandboxes.parent_sandbox_id` (backfilled from
  `created_by_sandbox_id`) and an `adoption_requests` table; both migrate
  forward with no rewrite of existing rows' other fields.
- The API gains the adoption-request routes, `release`, the `parent` list
  filter and `parentSandboxId`; the sandbox role gains filing and reading its
  own adoption requests and releasing its own children. The inbox read model,
  the console's inbox and `discobox ls` learn the new kind and column.
- `server/internal/auth/DESIGN.md`, `server/internal/resources/sandboxes/DESIGN.md`,
  `server/internal/resources/secrets/DESIGN.md` (the inbox),
  `sandbox-agent/monitors/discoboxbox/DESIGN.md` and the in-box `discobox` and
  `discobox-monitor` skills describe the parent where they describe the
  creator's authority today.
- Tests cover adoption approved and then each route of the role allowed on the
  child; self, ancestor, cross-project, archived and no-`DiscoboxAPI` requests
  refused when filed and when approved; two crossing approvals of which only
  one lands; release by the parent and by a person; a sandbox refused
  `approve`; the child's bindings unread by the parent; and passthrough
  unchanged by adoption and release.
