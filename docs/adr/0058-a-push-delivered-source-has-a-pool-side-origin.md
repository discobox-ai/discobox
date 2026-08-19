# 0058 — A push-delivered source has a pool-side origin the client re-pushes into

- **Status**: Accepted
- **Date**: 2026-08-19

## Context

Push delivery (ADR 0001 §4, ADR 0045) is a one-shot. The client pushes the
commit its source names into the sandbox's repository, calls the continue
endpoint, and the sandbox starts. Nothing supports a second push, and the
sandbox has nothing to rebase onto.

Three facts make that a hard stop rather than a missing convenience:

- **There is no `origin`.** `initGitSource` (`pool-agent/sandboxruntime/runtime.go`)
  `git init`s an empty repository and the client's push lands directly on
  `refs/heads/<branch>`, with the working tree updated by
  `receive.denyCurrentBranch=updateInstead`. `git clone` never runs, so no remote
  is ever configured, and `rewriteOriginRemote` — the function that would point
  one at `/.discobox/origins/<slug>` — returns early on `gitSourceAwaitsPush`.
- **The existing transport cannot be reused for a second push.** Re-pushing
  `refs/heads/<branch>` is refused outright by `updateInstead` whenever the
  sandbox's working tree is dirty, and when it is accepted it moves the branch
  the sandbox is working on. `gitSourceMaterialized` then makes every later
  create a deliberate no-op, so nothing re-checks-out either.
- **ADR 0026 gave the live origin only to clone-delivered sources**, and for a
  good reason: it binds the client's real directory, which on the push path is
  by definition not on this host. Push delivery is chosen precisely when that
  bind is impossible.

What is already in place is the contract for an origin that *moves*.
`sandboxconfig.Source.UpstreamRef` is set for every source to
`refs/remotes/origin/<branch>` (`sourceUpstreamRef`), and `resolveDiffBase`
(`sandbox-agent/agentstatus/git.go`) forwards the reported diff base to the
merge base with that ref "once the sandbox has fetched" — with a comment noting
it never resolves for a push-delivered source, because there is no remote at
all. The git transport is also already bidirectional and not create-scoped: the
`git-repositories` proxy serves `upload-pack` under `ScopeSandboxRead` and
`receive-pack` under `ScopeSandboxWrite` for any running sandbox, and
`disco apply` (ADR 0014) already reuses that same URL and bearer-token
construction in the read direction.

So the missing piece is not a transport and not a contract. It is a *place* on
the pool host that is both the target of a client push and the sandbox's
`origin`.

## Decision

### 1. One bare repository per push-delivered source, in the sandbox's tree

`layout.SandboxOrigins(projectID, poolID, sandboxID)` — a fifth per-sandbox
subtree alongside `data`, `config`, `secrets`, and `sources` — holds one bare
repository per push-delivered source at `<slug>.git`. It is created in
`prepareSandboxVolumes`, before the container is created, so the mount below
always resolves.

Living under `layout.Sandbox(...)` means the archive marker, purge, and the
volume reaper already cover it: it is created and reaped with the sandbox and
needs no lifecycle of its own.

It is deliberately **not** under `sources/`. That tree is the sandbox's own
writable mount; a bare repository there would be writable from inside the
sandbox and would read as another source.

It is created `--bare` with two settings that the client cannot override,
because they are the receiving end of the push:

- `receive.denyDeletes=true`. Nothing legitimate deletes a ref here, and a
  delete is the one update whose effect the sandbox cannot see coming.
- `receive.denyNonFastForwards=false`. Non-fast-forward is the *normal* case
  (see §6), so it is allowed deliberately rather than by default.

Its `HEAD` is set to the branch the client will push — `gitSourceInitialBranch`,
or `discobox-source` for a source checked out at a bare commit or tag. `git
init --bare` otherwise points `HEAD` at an `init.defaultBranch` that never
exists, so the clone in §4 would leave `refs/remotes/origin/HEAD` unset — and
that is exactly the ref `sourceUpstreamRef` names for a non-branch checkout.

### 2. It is bound read-only at `/.discobox/origins/<slug>`

The same path, and the same read-only rule, ADR 0026 established for a
clone-delivered source's live bind. `originMounts` stops skipping
push-delivered sources; `rewriteOriginRemote` stops excluding them. After
materializing, `origin` inside the sandbox is `/.discobox/origins/<slug>` in
both delivery modes, and `git fetch origin` / `git rebase origin/<branch>` are
ordinary git.

What differs between the two modes is only who keeps that repository current: a
live bind of the developer's own directory, or client pushes into a pool-side
mirror. `origin` itself means one thing.

### 3. The mirror is addressed by its own route, not a repository id

`/projects/{p}/sandboxes/{s}/git-origins/{slug}.git/*` on the server, mirrored
on pool-agent, with the same scope derivation the worktree route uses
(`receive-pack` → `ScopeSandboxWrite`, everything else → `ScopeSandboxRead`).

A synthesized repository id such as `<slug>-origin` is ambiguous: slugs are
client-suppliable (`services.DefaultGitSourceSlug`) and validated against the
same `[a-z0-9-]` charset, so a source may legitimately be named
`primary-origin`. The path lookup differs too — `GitRepositoryPath` probes for
`<path>/.git`, which is not what a bare repository looks like — so origin
resolution is its own lookup returning the bare path and its owner, not a
special case bolted onto that one.

### 4. Delivery becomes push-to-origin, then a local clone

`initGitSource` creates the bare mirror instead of an empty worktree
repository. The create-time push targets the mirror; `CompleteSandboxSourcePush`
resumes as it does today; and `materializeGitSource` then clones the mirror into
`sources/<slug>` exactly as it clones a local directory, before
`rewriteOriginRemote` repoints `origin` at the in-sandbox path.

Two things collapse rather than grow:

- `gitSourceCloneURL` now resolves for a push-delivered source — it is the
  mirror's pool path — so `restoreGitWorkspace` loses its push-specific branch
  and fetches the dirty-workspace snapshot ref by explicit URL like any other
  source.
- `updateInstead` stops being load-bearing for delivery. The working tree is
  produced by checkout, as on the clone path, not as a side effect of
  `receive-pack`.

A local clone hardlinks its objects (git's default for a local path on the same
filesystem), so the mirror costs inodes rather than a second copy of history.
Not `--shared`/alternates: an alternates chain breaks if either repository is
ever repacked or pruned, while hardlinked loose objects and packs are immutable.

### 5. Re-push is transport-only

`disco push` pushes into the mirror through the route in §3 and touches no state
machine: no phase, no continue call, `SourceDeliveredAt` unchanged. The sandbox
rebases when whoever is working in it decides to, and the reported diff stat
self-corrects through `UpstreamRef` the moment the sandbox fetches (ADR 0030's
status report, ADR 0018's base rule).

`disco push` is the write direction of the pair `disco apply` completes.

Only push-delivered sources have a mirror. A clone-delivered source's origin is
already live (ADR 0026) and a remote-URL source's origin is the real remote;
against either, `disco push` says so instead of inventing a second origin.

### 6. The ref update is a lease, not a force and not fast-forward-only

Neither extreme is right. Fast-forward-only refuses the case the feature exists
for: a local rebase or amend is how a client's branch normally moves, and
refusing it leaves the sandbox with an origin it can never catch up to. A blind
`--force` is wrong too — the mirror is not written by one actor. Two machines
with the same repository can both push to one sandbox, and the older one would
silently rewind the sandbox's `origin` under a rebase already in flight.

So the update is `--force-with-lease`, leased against a client-side record of
what *this* client last pushed: `refs/discobox/origin/<sandboxID>/<slug>` in the
client's own repository, written by the create-time delivery push and updated
after every successful `disco push`. This is the existing ref convention —
`refs/discobox/run/<id>` for snapshots (ADR 0001), `refs/discobox/apply/...`
for fetched sandbox commits (ADR 0014).

The lease is a real one, not a re-read: leasing against a value just read back
with `ls-remote` only narrows the race, while leasing against "where I left it"
answers the question that matters — *has anyone else moved this since I last
pushed?* A missing local ref means this client has never pushed to this sandbox,
so it cannot lease; that refuses and requires an explicit `--force`.

Before the push, and refusable the same way:

- **Related history is required.** `git merge-base <source's base commit> <new
  tip>` must resolve. This catches a push from the wrong repository, and it is
  what makes ADR 0045's non-repository source refusable by a general rule rather
  than a special case: each of its runs mints a fresh root commit, so a second
  push shares no history with what the sandbox holds and nothing can rebase onto
  it. The check is deliberately *not* "the base commit is an ancestor of the new
  tip" — a local rebase rewrites that commit away routinely, so that rule would
  fail in the common case. A client that no longer has the base commit at all
  skips the check: unknown is not the same as unrelated.
- **A push that changes nothing is a no-op**, reported as one, on the local ref
  above.
- **Uncommitted local changes are warned about, never pushed.** `disco push`
  delivers commits. Re-delivering a dirty working tree would mean writing over
  the sandbox's own tree, which is a different feature and a destructive one.

Nothing is validated server-side. The server stays the byte proxy it already is
— it cannot inspect a pack cheaply, and `CompleteSandboxSourcePush`'s
commit-matching check remains the only place it asserts anything about content.
Every rule above is therefore the client's own discipline, which is acceptable
because the blast radius is one sandbox belonging to the client pushing, whose
own view of the mirror is read-only.

### 7. The push is per source, selected by slug, defaulting to the primary

Everything above is keyed by slug — the mirror path, the route, the mount — so
`disco push [--source <slug>]` selects a source and, with no selector, pushes
every push-delivered source the sandbox has.

That set is genuinely per source. `resolveSourceDelivery` decides every source
the same way — the primary one and each `SourceCodeReferences` entry alike — so a
reference can need a push while the primary source is bound, and the other way
round. Each push-delivered source gets its own mirror, its own mount, and its own
lease, and none of that needed anticipating here: keying everything on the slug
is what makes it fall out.

`disco push --source <slug>` against a source the sandbox reaches on its own says
so rather than inventing a mirror for it.

Beyond the source's own branch, a client may push another local branch into the
mirror under its own name. The mirror is a real repository, so the sandbox
simply gains `origin/<branch>` to rebase onto or cherry-pick from, and nothing
about the source's tracked ref changes.

### 8. The launcher offers push as an interaction, keyed `p`, next to apply

`disco push` is reachable from the TUI as `InteractPush` — an `Interaction`,
not a `Verb`. Like apply, it suspends the window and executes the real Cobra
command through `apiDataSource.Interact`, so the launcher runs `disco push`
with its own flag defaults and its own refusals rather than a second
implementation that drifts from it. Both things §6 produces — git's transfer
progress, and a lease refusal that tells the user to pass `--force` — are
terminal output that has to be read.

It is not `paneable`, for apply's reason exactly: the list can act on several
boxes at once and a pane shows one. It takes several targets for the same
reason apply does — one local branch fanned into N mirrors is coherent, and
`Interact` already separates targets with a `── <id>` header.

`p` is free (`P` is purge). The menu entry is the inverse of apply's: where
apply reads "bring the changes back to `<directory>`", push reads "send your
commits from `<directory>` to the box".

Disabled actions stay on the menu with their reason, so push names what is in
the way:

- **The source is not push-delivered**, so there is no mirror to push into. The
  reason says what `origin` already is instead: a clone-delivered local source
  reads the directory live (ADR 0026), and a remote-URL source's origin is the
  real remote.
- **The box is archived**, so the git route has nothing to auto-start.
- **Nothing is new**, by the §6 lease ref against the local branch tip.

This adds exactly one fact to the row model: whether the source is
push-delivered, read from the `Source.Delivery` the listing already carries and
`sandboxcreate.SourceNeedsPush` already interprets. The behind/up-to-date test
is local and costs one `git for-each-ref refs/discobox/origin/` for the whole
listing plus the current branch's tip — the launcher's data source already runs
local git for the session's branch and dirty status, and unlike the fetch-per-row
this repository deliberately removed, it wakes nothing.

## Alternatives rejected

**Push into `refs/remotes/origin/*` in the sandbox's own repository.** No
mirror, no route, no delivery change, and repeat pushes are already incremental
— by far the cheapest option, and enough for `git rebase origin/<branch>` and
for `git status` to show ahead/behind. Rejected because no URL backs that
remote: `git fetch origin` inside the sandbox fails, so origin moves only when
the client pushes, and an agent working in the sandbox that reaches for
fetch-then-rebase — the ordinary reflex — hits an error it cannot act on. It
also leaves `origin` meaning two different things across the two delivery
modes, which is the seam ADR 0026 was closing.

**Re-push `refs/heads/<branch>` through the existing route.** No new surface at
all. Rejected: `updateInstead` refuses it whenever the sandbox's tree is dirty,
and when it does not refuse, it moves the branch out from under the sandbox's
work. That is data loss presented as a delivery.

**A shared bare repository on the control-plane server** — ADR 0001's deferred
per-repo cache. Rejected here because it answers a different question: it
removes the *first* push's full-history cost across sandboxes, not the absence
of a re-pushable origin, and making it serve in-sandbox `git fetch` needs an
authenticated path from inside the sandbox back to the server plus server-side
storage and a GC story. ADR 0001's revisit condition stands unchanged, and a
per-sandbox mirror does not foreclose it: the mirror would become its client.

**A writable origin mount.** Would let the sandbox `git push origin`. Rejected
on ADR 0026's reasoning plus one more: a sandbox writing refs the client never
wrote makes the next `disco push` a non-fast-forward against its own mirror.
`disco apply` reads the worktree repository, so nothing needs this.

**Fast-forward-only pushes, or blind `--force`.** Covered in §6: the first
refuses the ordinary local rebase, the second lets a stale client silently
rewind a sandbox's origin. The lease is what distinguishes "my branch moved"
from "someone else's push is about to be lost".

**Validating the push server-side.** Rejected: the pack would have to be
inspected in the proxy path, which is exactly what keeps that proxy simple and
transparent today, and it buys nothing — the only actor it would protect is the
client pushing into its own sandbox.

**Push as a TUI `Verb` rather than an interaction.** Verbs are control-plane
state changes the reconciler converges, reported once on the status line when
the batch finishes. Rejected: push is a client-side git transfer, and both its
progress on a large history and §6's refusals are things the user reads and
acts on, which is what the real terminal is for.

**Auto-rebase the sandbox on push.** Rejected: a rebase can conflict, and
resolving conflicts inside a sandbox nobody is attached to is worse than
leaving the choice to whoever is working in it. The push refreshes `origin` and
stops.

## Consequences

- One extra repository per push-delivered source on the pool host, hardlinked
  at clone time.
- The repository that exists while a sandbox is parked in `awaiting_source` is
  now the bare mirror; `sources/<slug>` is empty until the resume clones into
  it. `GitRepositoryPath` therefore reports not-found for the worktree route
  during that window, so `disco apply` against a sandbox that has not received
  its source fails as not-found rather than fetching nothing.
- A source created from a directory with no repository (ADR 0045) still cannot
  be re-pushed usefully: its base is a root commit minted in a throwaway
  repository that is deleted after create, so a second push carries unrelated
  history and nothing can rebase onto it. `disco push` refuses that source
  rather than pushing an unrelatable branch. A deterministic empty-tree root
  commit, or a persisted throwaway repository, would fix it. **Revisit if**
  people actually hit it.
- The mirror is pool-side, so a push into it does not need the sandbox
  container running — but the pool route is `autoStart`-wrapped and the server
  acquires a sandbox HTTP client, so a push starts a stopped sandbox today.
  Relaxing that for the origins route is deferred.
- The first push still sends full history (ADR 0001's consequence stands).
  Every later push negotiates against the mirror's objects and is incremental.
- Force-pushing over a commit the sandbox has not yet fetched makes that commit
  unreachable in the mirror, so the sandbox can never obtain it. That is what
  force means; the lease in §6 is what keeps it from happening by accident from
  a stale machine.
- The lease lives in the client's repository, so it is per-clone. A second
  machine, or a fresh clone, has no lease and must pass `--force` for its first
  push — deliberately, since that push is the one that would rewind the mirror.
- The lease ref is per-clone, so on a machine that has never pushed to a
  sandbox the launcher offers push even when the mirror is already current.
  This is the same conservative direction apply already takes with an
  unmeasured diffstat: offered, and the command reports the truth.
- The row gains no column. "Your branch is ahead of the box's origin" is a
  client-side fact about one clone, while every other column on the row is
  something the sandbox itself reported; putting it on the row is deferred
  rather than declined.
- `disco push` is new CLI surface, and the sandbox's `origin` is now meaningful
  in a mode where it never was — so the root, `cli`, `server`, and `pool-agent`
  design docs describing source materialization all move when this lands.

## References

- `docs/adr/0001-sandbox-origin-and-remote-source-push.md` — push delivery, the
  `awaiting_source` phase, and the deferred server-side cache repository.
- `docs/adr/0026-local-source-origin-is-bind-mounted-live-into-the-sandbox.md` —
  `/.discobox/origins/<slug>`, the read-only rule, and `rewriteOriginRemote`.
- `docs/adr/0014-disco-apply-pulls-sandbox-commits-via-cherry-pick.md` — the
  read direction of the same transport, which `disco push` mirrors.
- `docs/adr/0045-a-directory-with-no-repository-is-delivered-by-push.md` — the
  throwaway repository that bounds re-push for a non-repository directory.
