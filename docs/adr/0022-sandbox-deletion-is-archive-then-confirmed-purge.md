# 0022 — Sandbox deletion is archive, then confirmed purge

- **Status**: Accepted
- **Date**: 2026-08-06
- **Amended**: 2026-08-06, §6, during implementation and before anything shipped
  against it. The draft gave `DELETE` a `purge` flag. With `archive` as its own
  operation there is no caller that deletes and wants the data kept, so the flag
  would have had exactly one valid value; delete means delete. The same
  reasoning removes `RemoveOption`/`RemoveVolumes()` from the provider contract
  rather than making it finally work: it had one caller and every provider
  ignored it, and `Archive` is what that option was reaching for.
- **Extends**: [ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §9, whose existence vocabulary becomes three-valued. §§1–8 and §§10–13 stand.
- **Relates to**: [ADR 0010](0010-deletes-are-hard-deletes.md), unchanged — the
  row delete is still a hard delete; this ADR only moves when it happens.

## Context

Deleting a sandbox reaches the pool agent and removes the container, its
anonymous Docker volumes, its proxy sentinels, and its proxy material. It never
removes the sandbox's durable tree —
`/var/lib/discobox/projects/<project>/pools/<pool>/sandboxes/<sandbox>/{data,config,secrets,sources}`.
`DockerSandboxRuntime.DeleteSandbox` simply has no call that touches it.

That is not an oversight that can be fixed with one `os.RemoveAll`, because the
intent never arrives. `SandboxReconciler.deleteSandbox` calls
`provider.Remove(ctx, ref, state, sandbox.RemoveVolumes())`, but
`poolruntime`'s agent client takes `_ ...sandbox.RemoveOption` and drops it: the
pool API's `DELETE /sandboxes/{sandboxId}` has no field for it. The control
plane has been asking for volume removal, over a wire that cannot carry the
question, to a handler that would not have read it.

What actually reclaims the tree is `reapDeadSandboxVolumes`. It walks the pool's
sandbox directories, and for any directory with no live container it writes a
`.discobox-orphaned-at` tombstone and `os.RemoveAll`s the tree once that
tombstone is 24 hours old. The reaper exists for a real and different problem:
a container removed out of band, or lost while the pool was down, whose data
should survive a same-day recreate. It runs on a one-minute backstop ticker from
the Docker event watcher.

So an explicit delete is indistinguishable from an accident, and inherits the
accident's retention window. Three consequences follow, in increasing order of
seriousness.

A deleted sandbox's `secrets/secrets.json` — mode 0600, the resolved sentinel
map — and its source checkouts sit on disk for a day after the user was told the
sandbox was deleted. Nothing surfaces them; nothing can, because the DB row is
already gone.

Reclamation depends entirely on a live pool agent's ticker. If the agent never
comes back, or the pool is deleted before the window elapses, nothing on the
server side ever collects the tree. There is no server-side record that it
exists, because the row was hard-deleted the moment `Remove` returned.

And the system has no way to express the case that is actually wanted most of
the time. "Tear down the runtime, keep the work" is a normal thing to want from
a sandbox, and today the only shapes available are `stopped` — which keeps the
container, its image pin, and its slot on a specific pool host — and `deleted`,
which is supposed to destroy everything. The 24-hour window has been quietly
serving as a bad version of the missing state: undiscoverable, unconfigurable,
and not honoured by anything except a directory scan.

## Decision

### 1. Existence is three-valued: present, archived, deleted

ADR 0017 §9 reduced desired state to existence: `present` or `deleted`. That was
right to exclude power, and it is where `archived` belongs — not as a third
power state alongside running and stopped, but as a third answer to "in what
form should this sandbox exist".

- `present` — exists as a runtime. A container, reachable, startable.
- `archived` — exists as data. No container, no runtime resources, no reachable
  endpoint; the durable tree is retained so the sandbox can be reinstantiated
  into an equivalent runtime.
- `deleted` — does not exist. No container, no data, no row.

`model.DesiredStates` is today a single slice shared by pools and sandboxes,
described as "identical for all orchestrated resources". It stops being
identical: it splits into `SandboxDesiredStates` and `PoolDesiredStates`, and
pools do not gain `archived`. A pool's data is many sandboxes' data; archiving
one is a different decision from archiving a runtime, and pretending otherwise
to keep one slice would put a value in the pool's API enum that nothing can
converge.

`archived` is also an observed `State`. The two are not redundant — desired
`archived` is "tear it down", observed `archived` is "it is torn down and the
data is held" — and the gap between them is what the reconciler converges.

### 2. Delete archives; purge is explicit

`DELETE /projects/{p}/sandboxes/{s}` records `archived`. Getting a sandbox out
of the way is the common request and the recoverable one, so it is what the
unqualified verb does.

Destroying the data is a separate, explicitly-requested operation. It is
reachable only by asking for it (`purge`), or by the retention timer in §4
asking on the user's behalf under a policy they set.

`unarchive` records `present` again. It recreates the container from the
sandbox's spec against the retained tree, and leaves it **stopped** — a
rebuilt container that has run before stays stopped until something uses it
(ADR 0017 §13), and the pool agent's on-demand start latch (§12) brings it up on
first real use. Unarchiving is not itself use.

### 3. Purge is a confirmed convergence, not a fire-and-forget one

Every other existence change in this system is asynchronous: record intent,
return 202, let the reconciler converge. Purge is not, and the reason is
specific to purge. Its whole content is a destructive side effect on a machine
the control plane does not own. A 202 for "the data is gone" is a promise the
server cannot keep and cannot later verify, because the row it would have
verified against is the thing being deleted.

So the purge request records `deleted` intent transactionally, then drives that
sandbox's reconcile **inline, in the request**, and returns its result. A 204
means the pool agent confirmed the tree is gone. An error means it is not, and
says so.

This is a composition of the existing model rather than an exception to it. The
intent write is the ordinary `recordSandboxIntent` — generation bump, desired
state, dirty mark, one transaction. Driving the reconcile in the request is an
optimization on latency and honesty, not a separate code path: if the request
dies mid-flight, if the agent is unreachable, or if the removal fails, the dirty
row is already durable and the engine converges it in the background exactly as
it would have anyway. Nothing is lost by the synchronous attempt failing; the
caller is simply told.

The row is hard-deleted only after the pool agent confirms. Today
`SandboxReconciler.delete` deletes the row on the strength of a `Remove` that
does not remove the data, which is how a sandbox can be simultaneously absent
from the API and present on disk. Ordering the row delete after the confirmation
makes the DB row the reliable record that a tree may still exist — which is what
makes §4 possible at all.

### 4. Retention is server policy, enforced by the reconciler

An archived sandbox is purged automatically once it has been archived longer
than its project's retention, default 24 hours, held in `Project.ArchiveRetention`.

The deadline is derived from `StateChangedAt`, never stored. This is the
`sourceAwaitDeadline` pattern already used for the source-push timeout, and it
has the same property that motivated it there: a derived deadline cannot drift
out of agreement with the state it belongs to, and re-entering the state
recomputes it rather than extending it. `SetState` only stamps `StateChangedAt`
on an actual change, so a repeated reconcile of an already-archived sandbox does
not push its expiry out.

Expiry is armed with the engine's existing future-dated mark,
`MarkDirtyAt(deadline)`, whose upsert pulls a deadline forward but never pushes
one back. `ScanDirty` additionally returns archived sandboxes whose deadline has
passed, so a mark lost to a crash is still collected — the same
level-triggered-backstop discipline ADR 0017 §1 requires of everything else.

#### Rejected: let the pool agent's volume reaper enforce retention

It already deletes dead sandbox trees on a 24-hour delay. Making the archive
state simply not interfere with it would have been nearly free, and it is the
wrong answer for three reasons.

The reaper cannot see the policy. Retention is a project setting; the pool agent
is not project-scoped in that sense and has no reason to learn a control-plane
concept in order to run a directory scan.

The reaper cannot distinguish intent from accident. That is precisely the defect
this ADR exists to correct, and reusing it for retention would re-encode the
defect as the design: an archived sandbox and a sandbox whose container crashed
would again be the same thing to the only component that acts on either.

And reclamation would remain conditional on a live agent's ticker, with no
server-side record that anything is owed. Because §3 keeps the row until the
data is confirmed gone, the server knows what exists and can drive its removal;
that is strictly better than a scan that only runs where the data happens to be.

The reaper stays, unchanged in purpose, and is explicitly scoped back to it: it
skips archived sandboxes, and is the recovery path for out-of-band container
loss only.

### 5. Archived sandboxes are inert, and say so

ADR 0017 §12 put an auto-start latch in the pool agent: a request on a
sandbox-directed route — exec, attach, the HTTP port proxy, harness hooks, git —
is the demand that justifies bringing the sandbox up. An archived sandbox must
be exempt. Its container is gone by intent, and quietly recreating one on the
first exec would undo the archive and defeat the retention policy in the same
motion.

An interactive request against an archived sandbox fails with `409 Conflict`
naming unarchive. It does not start it, and it does not fall through.

Today the latch swallows every error and proxies anyway, on the reasoning that
the downstream handler's own error names what the caller was trying to do. That
reasoning holds for a sandbox that is mid-create or genuinely unable to start,
and it does not hold here: "no inspectable IP address" from the proxy is a true
statement about a fact the caller cannot act on. Archived is a state the caller
can act on, so it is reported as one. The latch is the right place for the
check because it is the single point every interactive route already passes
through, and because the pool agent is the only process that knows the
container's true state.

The control plane rejects `start`/`stop`/`restart` on an archived sandbox with
the same 409, alongside the existing rejection for a sandbox already being
deleted.

### 6. The pool agent is told which state to hold, and confirms it

One addition to the pool API carries the intent that today has no wire
representation, and one existing operation stops under-delivering:

- `POST /sandboxes/{sandboxId}/archive` — remove the container and everything
  disposable, keep the durable tree, mark it archived.
- `DELETE /sandboxes/{sandboxId}` removes the durable tree too, and returns only
  once it is gone. It is not gaining a flag: keeping the data is what `archive`
  is for, so there is no caller that deletes and wants it kept.

`sandbox.RemoveOption` and `RemoveVolumes()` are removed with the same
reasoning. The option exists today with one caller and no implementation that
reads it — it was reaching for a distinction the contract could not express, and
`Archive` is that distinction. Making the option finally work would leave two
ways to say the same thing, one of which every provider had already learned to
ignore.

`ArchiveSandbox` goes on the `Runtime` interface, not an optional one, and both
implementations get it. This is a capability every sandbox runtime must have,
not one that may or may not exist at runtime.

What "disposable" means is decided by the existing storage split in `layout`,
which already separates the durable tree from the pool cache and the proxy
material for exactly this kind of reason. Archive keeps `data`, `config`,
`secrets`, and `sources`; it drops the container, the proxy sentinels, and the
proxy material. The pool cache is untouched because it is shared by the whole
pool and was never the sandbox's to release.

Archive writes a marker in the sandbox's tree. The marker is what makes the
retained data legible to the pool agent as retained rather than orphaned: it is
what the reaper skips (§4) and what `EnsureSandboxRunning` refuses on (§5).
`CreateSandbox` clears it, which is the whole of what unarchive needs beyond
recording `present` — the reuse-the-existing-tree path already does the rest.

## Consequences

- A user who deletes a sandbox can get it back within the retention window. This
  is new capability, and it is the main thing this ADR buys.
- A user who purges gets a synchronous, truthful answer, and a failure they can
  see and retry rather than a 202 that was never verified.
- A sandbox with no container is no longer self-evidently garbage to the pool
  agent. Two states share that shape now, and the marker file is what tells them
  apart on disk. Anything that reasons about sandbox directories must read it.
- The pool agent's complete-sync report says "no container" for an archived
  sandbox, which is the same observation it makes for a sandbox whose container
  was lost — and that observation currently marks the sandbox dirty for a
  rebuild. Archived sandboxes must be exempt from that, or the control plane
  will resurrect exactly what it just asked to be torn down. This is the sharpest
  edge the change introduces.
- An archived sandbox keeps its row, so a project or pool holding one is not
  empty. [ADR 0023](0023-projects-are-created-by-copy-and-deleted-only-when-empty.md)
  §3 refuses to delete a non-empty project, and pools already refuse while they
  hold sandboxes — so archived sandboxes are purged explicitly before their
  container is deleted, rather than being cascaded or stranded. Keeping the row
  until the data is confirmed gone (§3) is what makes that refusal correct.
