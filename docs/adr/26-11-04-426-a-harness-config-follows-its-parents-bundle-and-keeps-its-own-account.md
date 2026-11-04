# 26-11-04-426 — A harness config follows its parent's bundle and keeps its own account

- **Status**: Proposed (would amend [0002](0002-harness-config-is-the-only-harness-concept.md)'s Decision, “Built-ins clobber their image”, and its rejection of a thin pointer resolved live; and [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md) §7's harness bundle named by its config, to allow a config to follow another config's bundle)
- **Date**: 2026-11-04
- **Issue**: [#625](https://github.com/discobox-ai/discobox/issues/625), following [#71](https://github.com/discobox-ai/discobox/issues/71)

## Context

A second account for the same harness needs its own harness config, but a
separately registered copy stays on the bundle it first registered.
`SeedBuiltIns` in `server/internal/resources/harnessconfigs/service.go`
re-pins built-ins; it does not know that a custom config was meant to follow
one. A custom config can already move without deletion through
`UpdateHarnessConfig` with `bundle`, or re-inspect its reference through
`RefreshHarnessConfigImage`. Neither expresses a continuing relationship.

The bundle reference alone is not the harness. `snapshotImageMetadata`
records its composed manifest digest, run/relaunch commands, files, secret
declarations, environment, volumes, groups, save-back definition and declared
egress hosts. Following a bundle while leaving those fields behind would
repeat the incoherence ADR 0002 rejected. Conversely, copying a parent's
saved account files and bindings would defeat the reason for a second config.

The owner chose one inheritance level, refusal to delete a parent with
children, conversion of existing copies in place, inheritance of bundle
metadata alone, and the existing project upgrade policy. In particular,
#625's reference to explicit upgrades does not withdraw
[0082](0082-a-stopped-sandbox-tracks-its-harness-image.md) §§1–3 or
[0121](0121-a-stopped-sandbox-that-failed-tracks-its-harness-image-too.md):
eligible stopped boxes already follow re-pins unless the project chooses
manual upgrades. Running boxes are not selected by that fan-out.

## Decision

### 1. A config may extend one standalone config in its project

A non-built-in config may name another harness config as its parent, instead
of supplying a bundle. The parent may be built-in or user-registered, must
have a resolved bundle, and must belong to the same project. Persist its
stable ID as `ExtendsID`; CLI selection may use its slug or ID. A rename does
not change the relationship.

There is one level: a child cannot be a parent. Self-reference, a parent
that already extends another, and converting a config with children into a
child are refused. Built-ins remain roots tracking the server's release.
A child is an ordinary selectable harness config with its own ID, slug and
name; it is not another built-in and introduces no definition resource.

The contract-first API adds `extends` as a parent config ID. Create requires
exactly one of `bundle` or `extends`; a request naming both is refused.
The CLI supports:

```text
discobox admin harnesses create --extends codex --slug codex-work --name "Codex (work)"
```

List and show expose the parent relationship and the effective bundle and
digests. CLI and console selectors show a child as a variant of its parent,
while retaining its own selectable identity and account status. A parent
with no credentials does not make its child signed out if that child has
its own credentials, or the reverse.

### 2. Follow the whole bundle snapshot, and no personal state

The child stores a materialized copy of the parent's bundle-derived
snapshot. Its effective bundle reference, bundle digest and composed
manifest digest are the parent's. This is a cache of resolved metadata,
not a second independently writable bundle pin.

| Follows the parent | Belongs to the child |
| --- | --- |
| Bundle reference and digest; composed manifest digest | ID, project, slug, name, revision; project default selection |
| Run and relaunch commands, including the parent's resolved shell convention | Saved account files (`ConfiguredFiles`) and saved-secret ownership (`ConfiguredSecretIDs`) |
| Bundle files (`Files`), environment, volumes and additional groups | Personal files (`FileOverrides`) |
| Secret declarations and save-back definition | Use and delegate bindings, secret values, and account/sign-in state |
| Bundle-declared egress hosts (`ImageEgressHosts`) | Person-set egress policy (`Egress`) |

Copy the parent's resolved commands, rather than resolving them against the
child's slug: a variant of `shell` must still open the login shell.
Neither a parent's personal file edits nor its sign-in, save, reset, bindings
or personal egress edits propagate. Saving or resetting a child's account
operates on that child alone. Existing file precedence remains
`FileOverrides` over `ConfiguredFiles` over `Files`
([26-11-04-028](26-11-04-028-a-persons-harness-files-are-their-own-layer-and-a-bundle-refresh-never-writes-it.md),
[26-11-04-711](26-11-04-711-a-reset-keeps-a-persons-harness-files-and-a-save-never-writes-them.md)).

There is no general settings merge, feature override or command override
through this relationship. Features come from the selected bundle and the
box's existing provisioning policy; a child adds no feature-selection layer.
To change bundle declarations independently, register a standalone config.
A new child starts with no inherited bindings or saved account files; any
personal overrides supplied at creation are its own.

### 3. A root re-pin publishes its children's snapshots with it

Every writer of a root's resolved snapshot — seeding, bundle replacement,
and refresh, including a platform-layer change that moves only the composed
manifest digest — updates its children too. Inspection stays at the root's
write boundary. List, get, sandbox creation and upgrade consume the stored
resolved snapshot without registry access or recursive resolution.

Publish the root and all changed child snapshots in one store transaction.
Serialize relationship changes and deletion against that publication, so a
concurrent child create or conversion cannot commit a stale parent snapshot,
and a concurrent delete cannot leave a dangling parent ID. A child's
personal edits use its revision as today; a changed inherited snapshot bumps
that revision too, invalidating stale edits and save-back baselines rather
than overwriting newer state. An unchanged snapshot does not bump it.

Apply the existing re-snapshot effects to each changed config under its own
identity: synchronize its owned credential routes from the new declarations,
and mark affected proxy policies in the same transaction. This does not
create or copy bindings from the parent. Personal egress remains untouched.
A failure rolls back publication for the root and children together.

After publication, run the existing `applyResolvedPins` path for each config
whose bundle or manifest digest moved. Child configs use the same project
upgrade policy and eligibility predicate as roots. New boxes use the new
snapshot; eligible stopped boxes may re-pin automatically; other existing
boxes keep their pins until the ordinary explicit upgrade or repair path
moves them. The existing best-effort sandbox fan-out and its reported
upgrade availability remain; this proposal adds no new upgrade policy or
runtime convergence path. The race between an observed stop and a start
recorded in ADR 0082 remains unchanged.

A child's refresh request is refused with a message naming the parent to
refresh. It must not secretly refresh a shared root and move its siblings,
or re-inspect the child's cached reference independently.

### 4. An existing config can opt in without losing its account

Update supports `extends` under the config revision the caller read. The
CLI exposes `discobox admin harnesses update HARNESS --extends PARENT`.
It changes only the relationship and the bundle-derived snapshot, preserving
the config's ID, slug, name, personal files, saved account files, secret
ownership and bindings, sandbox references and project default reference.
Existing copies are never inferred from matching bundle names or digests.

Conversion and reparenting are explicit snapshot changes and use §3's
publication and upgrade rules. The selected parent's current snapshot takes
effect even when its bundle differs from the copy's old one; the caller is
choosing a new bundle source, not proving the accounts interchangeable.
The CLI states that account data is retained and which parent will supply
the bundle. Reparenting accepts only another valid root.

A `bundle` update on a child explicitly detaches it and registers that bundle
as standalone, inspecting it before committing. It retains the same personal
state. A request with both `bundle` and `extends`, or either source change
mixed with unrelated edits, is refused. Built-ins cannot convert or detach.
There is no implicit detach or silent fallback to the cached bundle.

Before conversion rewrites a legacy config's unsplit `Files`, preserve its
personal files through the existing split rule against its old bundle
baseline. If that baseline cannot be resolved, refuse conversion with an
actionable error; do not guess using the new parent's paths. The existing
migration limitation for edits indistinguishable from old bundle files is
unchanged. A modern config's personal overrides need no reconstruction.

### 5. A parent with children cannot be deleted

Refuse deletion with a conflict identifying the dependent configs, before
clearing project defaults, deleting bindings or making any other mutation.
The check and delete are atomic with relationship writes. This adds to the
existing built-in and sandbox-reference deletion restrictions in
`server/internal/store/harness_configs.go`; it does not weaken them.

A person first deletes the children, reparents them, or explicitly detaches
them by registering bundles. Never cascade-delete accounts or silently
freeze children on a formerly inherited bundle. Deleting a child uses the
existing config deletion rules and affects no parent or sibling account.

### 6. Existing databases keep every config

Add the nullable parent-ID column and an index in place. Existing rows have
no parent and retain their current behavior and data. Do not match old
copies automatically, delete configs, or recreate the database. The model,
store, service, OpenAPI contracts and generated clients change together when
this ADR is implemented; this proposal changes none of them.

## Alternatives rejected

- **Independent copies and a manual refresh each release.** They already
  exist and are the stale-account-copy problem this relationship addresses.
- **Arbitrary inheritance chains.** They add transitive propagation,
  cycle handling and less obvious bundle ownership without helping the
  requested second account. Revisit when a concrete use case requires a
  variant to follow another variant rather than their common root.
- **Resolve only the bundle live from the parent.** It can disagree with
  snapshotted commands, files and secret declarations. Resolving the entire
  snapshot live avoids that mismatch but would move parent resolution into
  every consumer and leave child revision/save-back baselines unchanged when
  inherited metadata changes. Atomic materialization keeps those boundaries
  explicit and preserves inspection at write time.
- **Inherit the parent's account or merge arbitrary settings.** That makes
  a second account depend on the first account's personal edits and risks
  credential sharing by accident. Inheritance here means a bundle source;
  personal overrides remain local and use their existing precedence.
- **Cascade or detach on parent deletion.** Either destroys independent
  account state or silently stops following updates. Refusal makes the
  person choose what the surviving configs should follow.

## Consequences and implementation boundaries

One built-in re-pin moves every variant's target without copying anybody's
login. The cost is a transaction proportional to the number of children and
a sandbox fan-out per changed config. Account preservation does not promise
that an old login remains valid with a newly selected harness; reset and
sign-in remain the account recovery mechanisms.

Implementation must demonstrate a built-in re-pin reaching a child while
retaining its independent files and bindings, including a manifest-only
change, shell variants, conversion and detach, concurrent relationship/delete
safety, stale-revision rejection, and both automatic and manual project
upgrade policies. Running-box pins must remain unchanged by the fan-out.

The exact SQL locking strategy, migration mechanics, error wording and
selector layout remain implementation choices, subject to the atomicity and
visibility rules above. Cross-project inheritance and a general settings or
feature inheritance system are outside this decision. No implementation or
live design-document update is included in this Proposed ADR.
