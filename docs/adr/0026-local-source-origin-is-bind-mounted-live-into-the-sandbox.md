# 0026 — A local source's origin is bind-mounted live into the sandbox

- **Status**: Proposed
- **Date**: 2026-08-01

## Context

ADR 0001 established that a clone-delivered local source's bind mount "exists
only so git can reach that path... the bind is the current transport for the
objects, not the mechanism." That transport is transient by construction: it
is scoped to pool-agent's own `git clone` subprocess
(`pool-agent/sandboxruntime/runtime.go`, `materializeGitSource`), which runs
against a pool-agent-process-local path — `hostMountedLocalDirectory` rewrites
`GitSource.LocalDirectory` through pool-agent's own `/host` bind (present only
when pool-agent itself runs containerized and needs to reach real host paths).
`git clone` records that rewritten path as the new repository's `origin`
remote. Once the sandbox container exists, that path means nothing inside it —
it was never mounted there, and even if it were, it is meaningful only in
pool-agent's own mount namespace, not the daemon's or the sandbox's.

The common case this affects is the local dev loop this repository's own
`task dev` exercises: CLI, server, and pool all on one machine, so ADR 0001's
bind-vs-push decision (`sourceNeedsPush`,
`server/internal/resources/sandboxes/source_delivery.go`) resolves to a bind
whenever the provider's `LocalSourceBind` capability and the client's
`Origin.HostID` match the server's own host ID. In exactly this case, the
sandbox's source repository already has everything needed to track the
developer's real, still-changing working directory — except a way to reach
it. Today a sandbox can `git log`, `git diff`, and work with the commit it was
cloned at, but it has no path back to origin: no `git fetch origin`, no `git
rebase origin/<branch>`.

ADR 0007 already established the mechanism for handing Docker a declarative
`[]mount.Mount` list at sandbox-container-create time
(`prepareSandboxVolumes`) for host-backed paths the sandbox needs to see. This
decision is an incremental case of that pattern: one more mount per source,
sourced from an arbitrary external host directory instead of a pool-owned
volume tree.

One wire-contract detail matters for eligibility. `GitSource.Delivery` is
documented as "defaults to clone" (`server/internal/model/model.go`) and the
server does resolve it to an explicit value before persisting the sandbox —
but `poolGitSource` (`server/providers/poolruntime/agent_client.go:428-438`),
the function that turns the persisted `model.GitSource` into the wire
`workerapimodel.GitSource` pool-agent actually receives, only ever sets the
wire field's `Delivery` when the source is push-delivered. The clone case
leaves it unset. Pool-agent therefore never observes a literal `"clone"`
value on the wire; the only signal it has is the absence of `"push"` — which
is exactly what `gitSourceAwaitsPush`, the predicate already used throughout
`materializeGitSource`, tests for. Any eligibility check for this feature has
to use that same predicate, not a literal comparison against `"clone"`.

A related, pre-existing gap, out of scope here: `resolveSourceDelivery` is
only invoked for the primary `Source`
(`server/internal/resources/sandboxes/service.go`), never for
`SourceCodeReferences` entries — their local-directory reachability is never
validated against the server's own host ID the way the primary source's is.
This decision does not widen that gap: it treats every local
`SourceCodeReferences` entry exactly as reachable as the clone pool-agent
already unconditionally performs for it today.

## Decision

### 1. One read-only bind per eligible source, at `/.discobox/origins/<slug>`

For every source (primary or `SourceCodeReferences`) where
`!gitSourceAwaitsPush(source) && source.LocalDirectory != ""`,
`prepareSandboxVolumes` adds one `mount.Mount` binding the real host
directory — not a copy, not a pool-owned volume — onto
`/.discobox/origins/<slug>` in the sandbox container, using the same `<slug>`
already assigned to that source's `/.discobox/sources/<slug>` entry. The bind
is read-only.

Unlike the five primary roots (`data`, `cache`, `config`, `sources`,
`secrets`), this is not one pool-provisioned volume with per-source
subdirectories underneath it — it is N independent binds of N unrelated
external directories, so each needs its own `mount.Mount` entry. There is
nothing to provision, own, or later reap: the directory already exists on the
host, outside Discobox's state root.

The `Source` side of the mount is `r.daemonPath(*source.LocalDirectory)` —
the raw `LocalDirectory`, translated through the same `layout.HostMapping`
every other primary-volume mount already uses. This is a different path space
than `hostMountedLocalDirectory`/`r.hostMountPrefix`: that rewrite exists
solely so *pool-agent's own subprocess* can resolve a path through its own
`/host` bind before invoking `git clone`. The Docker daemon needs the raw host
path instead, and `layout.HostMapping.HostPath` already passes through any
path outside `layout.ContainerRoot` unchanged — which every `LocalDirectory`
always is — so no new translation code is needed.

### 2. `origin` is rewritten to the in-sandbox path after materializing

Once a local source's initial `git clone` (and any dirty-workspace restore)
completes, `materializeGitSource` runs `git remote set-url origin
/.discobox/origins/<slug>` inside the cloned repository. From that point, a
sandbox's own view of `origin` is the live in-sandbox bind, not the
pool-agent-local path `git clone` originally recorded.

### 3. `restoreGitWorkspace`'s fetch no longer depends on the `origin` remote's URL

`materializeGitSource` has a retry-safe branch, entered whenever the target
already has a `.git` directory, that also calls `restoreGitWorkspace` to
reapply a dirty-workspace snapshot. Today that function resolves its fetch
source with `git remote get-url origin`. If a prior materialize already
rewrote `origin` per decision 2, a later retry landing in this branch would
try to fetch from `/.discobox/origins/<slug>` — a path pool-agent's own
process cannot resolve — and fail.

Sequencing the rewrite to run strictly after `restoreGitWorkspace` fixes the
first pass but not a subsequent retry, which re-enters `restoreGitWorkspace`
against an already-rewritten remote. The actual fix is to stop
`restoreGitWorkspace` from depending on `origin`'s configured URL at all: it
now takes an explicitly computed fetch URL — the same value
`gitSourceCloneURL` already produces for the initial `git clone` — and fetches
by that URL directly (`git fetch <url> <refspec>`, which resolves identically
to fetching from a named remote for a fully-qualified refspec). This makes the
rewrite in decision 2 safe to run unconditionally on every successful
materialize, independent of retry timing.

### 4. No manifest or sandbox-agent change

Nothing inside the sandbox needs to discover this mount programmatically —
git's own `.git/config` remote entry is self-describing, and the mount is
simply present in the container's filesystem by the time any process,
including sandbox-agent's PID-1 `boot` flow, runs. This is unlike
`/.discobox/sources`, where `boot`'s `wireSources` must actively rebind each
source from the shared pool-owned volume onto its manifest-declared target;
here, pool-agent already binds each origin directly onto its final path via
Docker at container-create time.

## Alternatives rejected

**Materialize/copy the origin into a pool-owned volume, mirroring
`/.discobox/sources`.** Rejected: a copy is a snapshot frozen at clone time.
The entire point is a *live* connection back to a directory the developer
keeps committing to; a copy would need its own sync mechanism and still lag.

**Read-write mount.** Rejected. `git fetch`/`git rebase` against origin are
read-only operations against that repository; a writable mount would let a
sandbox mutate the developer's actual working directory, which nothing about
"rebase on origin" requires and which is a meaningfully worse failure mode
than a read-only mount ever being insufficient.

**Deriving eligibility from a literal `GitSource.Delivery == "clone"`
comparison.** Rejected once traced through the wire contract: `poolGitSource`
never forwards an explicit `"clone"` value, so this comparison would never be
true and the feature would never activate. The only implementable predicate
pool-agent can use is the same "not explicitly push" test the rest of
`materializeGitSource` already relies on.

**A manifest field plus a sandbox-agent `boot` wiring step, mirroring how
`sources` are wired.** Rejected as unneeded complexity. That two-step
indirection exists for `sources` because many sources share one pool-owned
volume that sandbox-agent must split apart per-slug at boot. Origins have no
shared volume to split — pool-agent already knows each origin's final
in-sandbox path at container-create time and can bind directly onto it, the
same way `data`/`cache`/`config`/`secrets` are bound directly today.

**Naming the concept `Origin` (or a `SourceOrigin` type, etc.) in Go.**
Rejected: `server/internal/model/model.go` already defines an unrelated
`Origin` struct — client-declared provenance (which host and directory a
create request came from), explicitly documented as "never used to
materialize source." Reusing that name for a materialization-facing concept
would conflate two things ADR 0001 went out of its way to separate. New
identifiers here stay filesystem/mount-path oriented:
`sandboxOriginsMount`, `rewriteOriginRemote`.

## Consequences

- A sandbox's `origin` remote no longer literally records where `git clone`
  ran from. Inspected from inside the sandbox — the only place that matters —
  it is the intended, live, in-sandbox path.
- `restoreGitWorkspace` fetches by explicit URL rather than by remote name.
  This is a strictly more robust fix than sequencing the rewrite after it,
  since the retry-safe branch can re-enter `restoreGitWorkspace` after a
  previous materialize has already rewritten `origin`.
- Docker-provider only, and only for the subset of local sources that already
  bind rather than push. VM and cloud providers never receive a
  `LocalDirectory` in a bind-eligible state (`LocalSourceBind`, ADR 0001 §3),
  so this feature is naturally inert there — no origins mount, no rewrite.
- One additional `mount.Mount` per eligible source (typically zero or one, for
  the primary source; occasionally more, for local `SourceCodeReferences`).
  Unlike the primary volumes, nothing here is pool-owned, provisioned, or
  reaped — the mount source is a directory the host already owns.
- Push-delivered local sources — used precisely when the origin is *not*
  reachable from the pool host (a different machine, or a provider without
  `LocalSourceBind`) — get neither the mount nor the rewrite, by the same
  `gitSourceAwaitsPush` guard that already governs the rest of
  `materializeGitSource`.

## References

- `docs/adr/0001-sandbox-origin-and-remote-source-push.md` — the transient-bind
  observation this decision acts on, and the `LocalSourceBind`/host-ID gate
  that determines eligibility upstream, in the server.
- `docs/adr/0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md` —
  the `[]mount.Mount` pattern this decision extends with a sixth mount kind.
