# 0093 — A local source's origin is its git directory, not its working tree

- **Status**: Proposed
- **Date**: 2026-09-03

## Context

[ADR 0026](0026-local-source-origin-is-bind-mounted-live-into-the-sandbox.md) §1
binds "the real host directory" of a clone-delivered local source, read-only,
onto `/.discobox/origins/<slug>` in the sandbox container
(`originMounts`, `pool-agent/sandboxruntime/runtime.go`). Its purpose is stated
in the same ADR: a sandbox that can `git fetch origin` and
`git rebase origin/<branch>` against a directory the developer keeps committing
to. That purpose is object access. The bind is the whole working tree.

A working tree is not a superset of a repository's objects in the harmless
direction. It is the objects *plus everything git was told to ignore* — which is
the class of file a developer keeps out of git precisely because it should not
travel. `.env`, `.envrc`, `*.pem`, a `.aws` directory someone kept in the tree.
In this repository right now, `.gitignore:4` is `.env`, and `.env` exists and
carries the development server's configuration. Any discobox created from this
checkout can read it at `/.discobox/origins/<slug>/.env`.

The asymmetry is what makes this worth a decision rather than a patch. A
sandbox's own workspace, `/workspace/source`, is a clone: committed content, and
nothing else. The origin bind is the only place a sandbox sees uncommitted host
files at all, so it exposes strictly more than the sandbox already has — and the
difference is exactly the ignored files. Untrusted harness code runs on this
side of that boundary; nothing else about the sandbox grants it a view of the
developer's machine.

[ADR 0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md)'s
macOS backend made this easier to notice, because reaching a developer's
checkout from a VM meant sharing `/Users` into the guest. It did not create it:
the Docker provider has bound the repository root since ADR 0026, on every
platform, and the share and the bind are separate boundaries with different
audiences.

Every consumer of the host path wants objects and refs, and nothing else:

- the pool agent's initial `git clone` (`gitSourceCloneURL`);
- `restoreGitWorkspace`'s fetch of the dirty-workspace snapshot — the client
  writes that snapshot as a commit into the developer's own repository under
  `refs/discobox/run/`, so it is an object in `.git`, not a file in the tree;
- the sandbox's own `origin`, which is ADR 0026's entire purpose.

`git clone <repo>/.git` and `git fetch <repo>/.git <refspec>` both resolve
exactly as they do against the repository root. Nothing needs the tree.

## Decision

### 1. The origin is the git directory

`<LocalDirectory>/.git` is what is bound read-only at `/.discobox/origins/<slug>`,
and what the pool agent's clone URL names. Both sides move together: the clone
source (`gitSourceCloneURL`/`hostMountedLocalDirectory`) and the sandbox mount
(`sourceOriginHostPath`/`originMounts`) address the same directory, as they do
today.

This buys an invariant worth stating on its own, because it is what makes the
bind reviewable rather than merely narrower: **the origin exposes nothing the
sandbox does not already have in its own clone.** More history and more
branches, all of it committed. A file that was never committed is not reachable
from a sandbox by any path.

### 2. A repository whose `.git` is not a directory is delivered by push

A linked worktree and a submodule checkout put a *file* at `.git` holding
`gitdir: <path>`. Binding that file hands the sandbox a pointer into a
filesystem it does not have, and the sandbox's `origin` becomes a path that
resolves to nothing.

Such a source is delivered by push instead. This is not a fallback that
degrades: push delivery is the established answer for a local directory the pool
cannot reach ([ADR 0001](0001-sandbox-origin-and-remote-source-push.md),
[ADR 0045](0045-a-directory-with-no-repository-is-delivered-by-push.md)), and a
git directory that lives somewhere else is a directory the pool cannot reach by
the path the source names.

### 3. The client reports it, alongside the two facts it already reports

The client resolving the source detects that the repository's git directory is
not in place, and reports it on the wire beside `NoLocalRepository` and
`NoLocalCommits` — three facts of the same kind, discovered the same way, and
each one on its own sufficient for `sourceNeedsPush` to choose push.

As with its two siblings, the client is reporting what its filesystem holds, not
asking for a delivery mode. The server draws the conclusion.

## Alternatives rejected

**Keep binding the working tree, and expect developers not to keep secrets in
the repository.** Rejected. `.gitignore` is already how a developer says "this
is not part of the project", and a local `.env` is the canonical thing it is
used for. A boundary that holds only when the person behind it has already done
the right thing is not a boundary, and this one fails in the direction where the
cost is a leaked credential.

**Bind the working tree and exclude the ignored files.** No mechanism exists: a
bind mount carries a subtree, not a filter, and the ignore rules are themselves
content in the tree being filtered.

**Resolve a linked worktree's real git directory and bind that.** Rejected, and
this is the non-obvious one. The resolved path is outside the directory the
developer named, and for a worktree it is the *main* checkout's git directory —
every branch and every object of a repository they did not point at. Silently
widening a source from "this worktree" to "the repository it was cut from" is a
larger surprise than the transfer push delivery costs, and it reintroduces the
same problem this ADR exists to remove: a path the sandbox can reach that the
person creating it did not choose.

**Detect the non-directory `.git` on the server rather than the client.** The
server could: clone delivery already requires the client and the server to share
a machine (`sourceNeedsPush` compares `Origin.HostID` against the server's own),
so the path is stattable from there. Rejected for coherence — the other two
facts of this kind are client-reported, the client has the repository open at
the moment it resolves the source, and splitting one of three across the wire
would leave the server reading a client's filesystem in the single case where it
happens to be able to.

**Drop the bind and deliver every local source by push.** Rejected here, not
dismissed. It is the strictly safer answer and it remains available; what it
costs is ADR 0026's live origin, which is the reason the bind exists at all.
Bounding the bind's exposure to committed content is the smaller change and
leaves that decision open rather than pre-empting it.

## Consequences

A sandbox can no longer read a developer's uncommitted files through its origin.
That is the point, and it is a behavior change for anyone relying on it —
knowingly or not.

Linked worktrees and submodule checkouts become push-delivered: slower on first
create, and without a live origin to fetch from afterwards.

**A client that does not report the new fact will have a linked worktree
clone-delivered**, and the pool agent's clone of a `.git` file fails. The
failure is loud and names the path, which is the right shape for a version skew,
but it is a failure rather than a fallback — a server that wanted to close it
could stat the path itself, for the reason the rejected alternative above
records.

**This ADR does not settle the `/Users` share.** ADR 0062's virtiofs share
grants the guest VM and the pool-agent container read access to every home
directory on the Mac; sandboxes see only their own origin, so this decision does
not narrow it and does not need to. Narrowing it is a separate decision with a
constraint of its own: Virtualization.framework fixes a VM's directory-sharing
devices at configuration time, so no share can be added for a source discovered
after boot, and ADR 0026's live origin rules out staging a copy into a share
root. The realistic options there are a narrower shared root, or no share and
push delivery on that backend.
