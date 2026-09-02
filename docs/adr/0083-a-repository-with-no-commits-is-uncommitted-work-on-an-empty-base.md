# 0083 — A repository with no commits is uncommitted work on an empty base

- **Status**: Accepted
- **Date**: 2026-09-01
- **Extends**: [0045](0045-a-directory-with-no-repository-is-delivered-by-push.md) to
  the repository `git init` leaves behind; 0045 itself stands unchanged.

## Context

`git init` and then `discobox` is how somebody starts a project that does not
exist yet. It fails before it does anything:

```
$ git init .
$ discobox
resolve git ref "HEAD": exit status 128: fatal: Needed a single revision
```

`git init` leaves HEAD pointing at a branch that has no commits — git's *unborn*
HEAD. `gitutil.Root` succeeds, because the directory plainly is a repository, so
`resolveLocalRunSource` takes the repository path and immediately resolves
`HEAD` to a base commit. There is none, and git's message names neither the
repository nor what the user should do about it.

ADR 0045 already answered the neighbouring case — a directory in *no*
repository — and answered it well: an empty root commit as the base, the whole
working tree snapshotted on top of it as uncommitted work, delivered by push.
An unborn repository is the same situation with a `.git` in it. Nothing has ever
been committed, so nothing can be checked out, and the whole working tree is
uncommitted work.

Two things stop 0045's answer from being reached by simply routing this case
into it.

The repository is real. Its path is where `discobox ls` files the discobox,
where `discobox push` comes back to, and where the objects a delivery needs can
simply live — none of which is true of the throwaway repository 0045 builds and
deletes.

And a clone cannot deliver it whatever the provider can reach. The pool agent
clones, then checks out, then fetches the workspace snapshot
(`materializeGitSource`); an unborn repository clones to nothing, so the
checkout is looking for a base commit that has not arrived yet. The base commit
of a repository with no files is worse: it is reachable from no ref at all, so
no ordering of those three steps would find it. Only the client holds this
source, and only a push can deliver it — which is 0045's conclusion, reached
again for a different reason.

The signal that reaches that conclusion today is `GitSource.NoLocalRepository`,
and it is not this fact. `sourceNeedsPush` reads it to decide delivery, which is
right here; `CheckDeliverable` reads it to refuse a later `discobox push`,
because 0045's throwaway repository is gone and nothing on the machine holds
those commits. That second reading is false for an unborn repository, whose
commits are in the user's own `.git` and stay there.

## Decision

### 1. An unborn repository resolves to an empty base and a snapshot

`resolveLocalRunSource` tests whether HEAD resolves before it demands a commit.
An unborn HEAD resolves to the shape 0045 established: the base is a root commit
of the empty tree, the working tree is snapshotted on top of it under the same
`refs/discobox/run/` ref an ordinary dirty workspace uses, and the checkout
names the unborn branch — `main`, or whatever `init.defaultBranch` gave it — at
that base.

The sandbox comes up with the files as uncommitted changes on that branch, which
is what they are. An empty repository is not an error: it snapshots nothing and
the sandbox starts on the empty base commit, at the repository's own path.

### 2. The objects live in the user's repository, and no branch moves

The empty base commit, the snapshot commit, and the snapshot ref are written
into the repository `git init` created. Writing objects and a
`refs/discobox/run/` ref there is exactly what the ordinary dirty-workspace path
already does to a repository it was asked to run against.

`refs/heads/<branch>` is not touched, and HEAD stays unborn. The repository the
user gets back is the one they made: `git log` still says there are no commits,
and their first commit is still theirs to write.

Because the objects stay, a later `discobox push` can deliver the source again
out of the same repository, and `CheckDeliverable` finds the commit and the
snapshot ref exactly as it does for any other local source.

### 3. Nobody is asked, and `--include-dirty` still answers ahead of time

The question ADR 0073 added is not asked here. It exists because a directory in
no repository is as likely to be a home directory somebody ran in by accident as
it is to be a project, and `git init` is the user saying which of the two this
is. The dirty-workspace question is not asked either: its alternative is the
last commit, and there is none — the choices are the user's files and an empty
repository, which is the pair 0045 declined to ask about.

`--include-dirty=false` still answers ahead of time, and its answer is 0073 §3's
rather than 0077 §1's: the discobox starts on the empty base commit at the
repository's own path, with none of its content. Declining a *directory* copy
sends no source at all because the directory may be `$HOME` and a checkout of
nothing over `$HOME` is nobody's intent. A repository is a project whose path is
a fact the user established, so it keeps its path — the same reasoning ADR 0077
§3 kept for an empty directory.

An explicit `@REF` is refused with a message that says the repository has no
commits, rather than git's.

### 4. `NoLocalCommits` is a second fact, not a widening of the first

`GitSource.NoLocalCommits` says the repository at `localDirectory` has no
commits, so a clone of it yields nothing and the base the source was resolved
against exists only as objects the client holds. Like `NoLocalRepository` it is
a fact about the client's filesystem that the server cannot observe and the
client cannot get wrong, and delivery stays the server's decision:
`sourceNeedsPush` returns `push` on either.

They are separate because only one of them means the commits are unreachable
forever. `CheckDeliverable` keeps reading `NoLocalRepository` alone, and a
source resolved from an unborn repository stays deliverable.

## Alternatives rejected

**Commit the empty base onto the user's branch.** `git init` then one root
commit, and every existing path works unchanged: HEAD resolves, the clone
delivers, `discobox apply` has somewhere to land. Rejected for ADR 0045's own
reason, which applies with more force to a repository the user made: `discobox`
would put a commit in their history that they did not write, and turn a
deliberate `git init` into a repository whose first commit is ours. It is also
invisible — the user asked to run against a repository, not to change its state.

**Route it into 0045's throwaway repository.** `gitutil.InitOverWorkTree` over
the repository root, then `directoryRunSource` unchanged; the smallest possible
diff, and every downstream path is already proven. Rejected because it reports
`NoLocalRepository` for a directory that visibly holds one — a fact the field
exists to state truthfully — and because it throws away the one thing this case
has that 0045's does not: a repository that can keep the objects. A source
delivered out of a deleted repository cannot be pushed again, and there is no
reason for this one to be undeliverable.

**Widen `NoLocalRepository` to mean "nothing to clone".** One field, no schema
change, and the delivery decision comes out right. Rejected because the field is
read twice and only one reading generalizes: `CheckDeliverable` would tell a user
whose commits are sitting in their own `.git` that nothing on this machine holds
them, and refuse a `discobox push` that would have worked.

**Fetch the snapshot before checking out, in the pool agent.** Would make clone
delivery work for an unborn repository that has files, since the base commit
arrives as the snapshot commit's parent. Rejected because it does not cover an
unborn repository with no files — that base commit is reachable from nothing, so
no clone finds it — so push is needed anyway, and reordering materialization to
serve a case that still needs the other path is a change with no payer.

**Fail with a better message.** "This repository has no commits; make one and
run again." Honest, and a two-line change. Rejected because it is the wrong
answer to the request: an agent that is about to write a project from nothing
does not need the user to invent a first commit for it, and 0045 already decided
that a project with no history is a thing this tool starts rather than refuses.

## Consequences

- The discobox's history starts at a root commit of nothing, so the
  agent-reported diffstat (ADR 0030) counts the whole working tree as added.
  Accurate — nothing has a prior version — and the same noise 0045 accepted.
- Two runs from the same unborn repository share no history: the base commit is
  created per run and carries that run's timestamp.
- `discobox apply` back into the repository fails while HEAD is still unborn —
  the cherry-pick (ADR 0014) resolves the host's HEAD to find what to apply
  onto, and there is none. The user's own first commit in the host repository
  resolves it; bringing a sandbox's first commit home into a repository with no
  commits is future work.
- The base commit and snapshot are the only discobox objects in the repository,
  reachable from `refs/discobox/run/` exactly as an ordinary dirty run's are,
  and pruned the same way.
