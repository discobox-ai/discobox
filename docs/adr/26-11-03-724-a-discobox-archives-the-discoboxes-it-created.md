# 26-11-03-724 — A discobox archives the discoboxes it created

- **Status**: Accepted
- **Date**: 2026-11-03

  Accepted by the owner by merging #463, 2026-11-03.
- **Supersedes**, for the discoboxes the caller created and only for
  archive (`DELETE sandboxes/{id}`):
  - [26-10-02-478](26-10-02-478-a-discobox-starts-and-stops-the-discoboxes-it-creates.md)
    §2's keeping of archive out of the sandbox role. Its keeping of purge and
    port tunnels out stands.
  - [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)
    §4's refusal of archive, and its deferral of archive under "A wider role".
    Its refusal of purge stands.
- **Relates to**:
  [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §9, which gates the role behind `Sandbox.DiscoboxAPI` and holds every route
  that names a discobox to the ones the caller created; this adds one such
  route. [26-10-14-320](26-10-14-320-a-discobox-acts-on-any-discobox-in-its-project-and-the-judge-decides-which.md),
  superseded by 880, deferred archive, purge and unarchive "until a lead must
  clean up workers it finished with and a person's confirmation is the
  bottleneck". That condition is now met.
  [0022](0022-sandbox-deletion-is-archive-then-confirmed-purge.md), whose
  archive and retention this relies on, unchanged.

## Context

A lead discobox (the manager of discobox-ai/manager) creates workers, tags
them, reads and types into their terminals, and starts, stops, repairs and
upgrades them: every route of the sandbox role that names a discobox, held to
the ones the caller created (`Sandbox.CreatedBySandboxID`, ADR 26-10-27-880
§9; `createdDiscobox` and `withinCaller` in
`server/internal/auth/authorizers.go`). It cannot remove one. `discobox rm
<worker>` from a lead answers `403 not in the sandbox role`, because
`DELETE sandboxes/{id}` is not in `sandboxRole`. The manager tags finished
workers `to-delete`, stops them, and lists them for a person to remove by
hand. They pile up, and the person's confirmation is the bottleneck.

`DELETE sandboxes/{id}` archives (ADR 0022 §2, `Service.DeleteSandbox`): the
container and its runtime resources go, the durable tree and the row stay, and
`unarchive` brings the box back, stopped, until the project's archive
retention runs out (`ArchiveRetentionSeconds`, else the server's default,
24 hours by default). Then the reconciler purges it on the person's behalf
(`expireArchive`). `POST sandboxes/{id}/purge` destroys the box and its data
at once.

ADR 478 §2 kept archive and purge together as "a person's decision about
their discobox", since both change what exists rather than whether it runs.
They differ in the way that matters here: archive keeps the data for a window
the person set, and purge does not.

## Decision

### 1. The role archives a discobox the caller created

The sandbox role gains `DELETE sandboxes/{id}`, allowed only when the named
discobox's recorded creator is the calling sandbox, and only when the
target's effective egress policy is covered by the caller's: the
`withinCaller` check its neighbours `start`, `stop`, `repair` and `upgrade`
already use (ADR 26-10-01-032 §4).

- A discobox a person created records no creator, so no discobox archives it.
- A discobox is never its own creator, so none archives itself through the
  role.
- Any other discobox answers `403`, as the other created-only routes do.

`withinCaller` rather than `createdDiscobox`: archive writes to the target
rather than reading it, and 032 §4 holds every write to a discobox to the
caller's reach. It sends nothing into the target, so this costs nothing in
practice except that a worker a person widened after creating it is the
person's to remove.

The route is an instruction recorded as intent, as a person's archive is. The
sandbox service needs no change: it records `archived` and the reconciler
converges it.

### 2. Purge stays refused, for every discobox

`POST sandboxes/{id}/purge` stays out of the role, on a discobox the caller
created or any other. Destroying data now, with nothing to recover it from, is
a person's call.

A lead's archive starts the retention clock and cannot shorten it. When the
window ends, the box is purged by the project's retention policy, which a
person set, exactly as when a person archives it. Within the window, a person
keeps the box by unarchiving it.

### 3. It is in the role, not a separate per-box permission

The issue asked whether archive should always be in the role, as start and
stop are, or be something the owner turns on per box. It is in the role. A
person allows it by giving a box `DiscoboxAPI`, as they allow everything else
the role does.

- The role already holds steps that lose more than archive does. Repair and
  upgrade rebuild the container and discard everything outside its durable
  tree, with no way back. Archive keeps that tree for the retention window.
- A second permission would need what `DiscoboxAPI` already has: a field, a
  rule for whether a sandbox creating a sandbox may pass it on, the API, CLI
  and console to set it, and an upgrade default. It would buy a lead that may
  create, drive and repair workers but must leave them behind, and nobody has
  asked for that.
- The person's control over what an archive costs is the project's retention,
  which already exists.

### 4. Unarchive stays out

`POST sandboxes/{id}/unarchive` is not added. The safety net archive relies on
is a person bringing a box back, and nothing so far needs a lead to undo its
own archive.

## Alternatives rejected

**A per-box flag the owner turns on, at launch or with `DiscoboxAPI`.** §3.
Revisit if a person needs to give a box the API without letting it archive
the boxes it creates.

**Archive and purge together, as the issue's starting point refused them.**
Purge loses data with no window and no record left to check against (ADR 0022
§3). Archive keeps both. Granting archive gives a lead what it needs to clean
up after itself and leaves the irrecoverable step with a person.

**Let a lead purge the workers it created.** Workers would be gone at once
rather than after the retention window, and the disk freed sooner. But the
retention window is the only time a person can see that a lead removed
something it should not have. A person who wants that window short sets the
project's retention.

**`createdDiscobox` instead of `withinCaller`.** It would let a restricted lead
archive a worker a person later widened. Archive reaches nothing through the
target, so this is not an escape, but every other write the role makes to a
discobox is held to the caller's reach, and a route that is the exception has
to be remembered as one.

**Add unarchive too.** §4. Revisit when a lead must recover a worker it
archived by mistake and a person's unarchive is the bottleneck.

**Cascade an archive to the archived box's own children.** A discobox's
children are not a tree it owns (ADR 0140, rejecting a tree with cascading
archive and purge). Each is archived on its own.

## Consequences

- `discobox rm <worker>` from a lead archives a worker it created. The
  manager can remove finished workers itself rather than tagging them
  `to-delete` for a person.
- A lead archiving a worker that is still running ends its processes and loses
  anything outside its durable tree, as a stop followed by a repair would.
  The workspace and its uncommitted changes are kept until retention ends.
- A project with a short archive retention makes a lead's archive close to a
  purge. That is the person's setting, and it already applies to their own
  archives.
- A worker's own children keep their creator. Once that worker is purged,
  nothing but a person can act on them, as today when a creator is purged.
- In `server/internal/auth/authorizers.go`, `sandboxRole` gains
  `{DELETE, "sandboxes/*", withinCaller}`, and its comment stops saying archive
  is not in the role. `sandbox_role_internal_test.go` covers archive of a box
  the caller created (allowed), of one it did not and of one a person created
  (403), and purge of either (403).
- The in-box `discobox` skill lists `discobox rm` among what a box may do to
  the boxes it created, and says purge is a person's.
