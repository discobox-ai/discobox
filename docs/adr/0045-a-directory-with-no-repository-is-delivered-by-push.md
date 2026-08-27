# 0045 — A directory with no repository is delivered by push

- **Status**: Accepted (§2's "Nobody is asked about this" superseded by
  [0073](0073-a-directory-with-no-repository-is-copied-only-when-asked.md);
  everything else stands)
- **Date**: 2026-08-08

## Context

`disco run` in a directory that is not in any Git repository fails before it
does anything:

```
$ disco run
resolve git root: exit status 128: fatal: not a git repository (or any of the parent directories): .git
```

Everything downstream of `resolveLocalRunSource` assumes a repository: the
checkout commit is `HEAD`, a dirty workspace is a snapshot commit on top of it,
and delivery is either the sandbox cloning `LocalDirectory` or the client
pushing the commits it resolved. None of that has an input here.

The case is not exotic. A directory with files but no repository is where work
often starts — a scratch directory, an unpacked archive, a folder of notes — and
an empty directory is where "build me a new project" starts. Both are reasonable
things to hand a sandbox, and both are currently a hard stop that asks the user
to go and run `git init` first.

Two things make the naive fix wrong. `git init` in the user's directory is a
filesystem change they did not ask for and cannot easily notice; and the source
still has to *reach* the sandbox, which for a local directory means the pool
agent cloning that path through its `/host` bind mount — a clone that fails, no
matter how reachable the path is, when there is no repository at it.

## Decision

### 1. The repository is built over the directory, outside it

`gitutil.InitOverWorkTree` creates a repository in a temporary directory and
points its `core.worktree` at the user's directory. Every git operation —
status, add, write-tree, commit-tree, push — then acts on the user's files while
writing only into the temporary repository. No `.git` appears in the user's
directory, and deleting the repository leaves the directory byte-for-byte as it
was found.

`HEAD` is left where `git init` put it, so `init.defaultBranch` is honored.

### 2. The directory is uncommitted work on an empty root commit

The base commit is a root commit of the empty tree; the directory's whole
content is snapshotted on top of it as a dirty workspace, under the same
`refs/discobox/run/` ref an ordinary dirty workspace uses.

The sandbox therefore comes up with the files as *uncommitted changes*, which is
what they are: nothing here has ever been committed, and presenting them as a
first commit would attribute to the user a commit they did not make. An empty
directory is not an error — it snapshots nothing and the sandbox starts on the
empty commit.

Nobody is asked about this. The `--include-dirty` question offers the last
commit as its alternative and there is none, so `--include-dirty=false` is
rejected (it would leave nothing to run) as is an explicit `@REF` (there is no
history to name).

### 3. `NoLocalRepository` is a fact the client reports; the server still decides

`GitSource.NoLocalRepository` says the directory at `LocalDirectory` holds no
repository. `sourceNeedsPush` returns `push` on it, ahead of the bind and host
checks.

This does not break the rule that a client may not request `push` (ADR 0001 §3,
enforced in `resolveSourceDelivery`). The client is not asserting anything about
what the server can reach — it is reporting what its own filesystem holds, which
the server cannot observe and the client cannot get wrong. The delivery decision
is still made server-side, from that input.

`LocalDirectory` keeps naming the user's directory, not the temporary
repository. It is what `disco apply` comes back to, what `GitSource.Root()`
groups sandboxes by, and what the sandbox mirrors as its destination path. The
temporary repository is an implementation detail of one create and must never
be recorded as where the source lives.

### 4. The repository is carried from create to delivery, then deleted

`CreatePromptSandbox` returns a `LocalSource` alongside the sandbox, and
`DeliverSource` pushes out of it instead of re-resolving the root from the
source argument. A throwaway repository cannot be found again, and it holds the
only copy of the base commit and the snapshot the sandbox was configured
against.

`LocalSource.Close` deletes it, and callers close as soon as the source has been
delivered.

## Alternatives rejected

**`git init` in the user's directory.** The obvious reading of "make this a
repo", and it has the side benefit that `disco apply` would later have somewhere
to land. Rejected because `disco run` would leave a repository behind in a
directory the user only asked to run against — a change they did not request, on
files they own, that is easy to miss and annoying to undo. Committing their
content on top of that would also put a commit in their history that they did
not write.

**A throwaway repository under the user's home, delivered by clone.** Would make
the existing clone path work unchanged, since the pool agent's `/host` mount
covers `/home` by default. Rejected: it depends on the server's host-mount
configuration, which the client cannot see; it puts the repository somewhere the
user has to notice; and the clone happens during provisioning, so the client
would have to keep it alive past its own exit for `disco run -d`.

**Drop `LocalDirectory` and infer `push` from a source with nothing to clone.**
No schema change at all. Rejected on both ends: the pool agent deliberately
refuses to infer delivery from missing fields, because a source with nothing to
clone from is normally a malformed request rather than a request to wait; and
dropping the directory loses the provenance `disco apply`, the launcher's
folder column, and source grouping all read.

**Ask before doing any of this.** Consistent with how a dirty workspace is
handled. Rejected because the dirty-workspace question has a real alternative to
offer and this one does not: the choices are "run against your files" and "run
against an empty directory".

## Consequences

- The whole directory is hashed into the temporary repository on every run, with
  no ceiling. `.gitignore` is honored — the working tree's rules are read the
  same way a repository living inside it would read them — but a directory with
  no `.gitignore` and a large build output pays for all of it. There is no
  ceiling to raise or lower, so running in `$HOME` will be slow.
- `disco apply` back into such a directory fails: it is still not a repository,
  and `--dir` cannot help. The commits are reachable in the sandbox and through
  the git proxy; bringing them home is future work, and the natural answer is
  for the user to `git init` when they decide the work is worth keeping.
- The sandbox's history starts at a root commit of nothing, so the
  agent-reported diffstat (ADR 0030) counts the entire directory as added. That
  is accurate — nothing has a prior version — but it is noisier than a diffstat
  against a real base.
- Two sandboxes created from the same non-repository directory share no history:
  each gets its own root commit. Nothing is expected to relate them, and the
  origin key still groups them for `disco ls`.
