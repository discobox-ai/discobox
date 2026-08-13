# 0018 — `disco diff` resolves its base inside the sandbox

- **Status**: Superseded by [0037](0037-drop-disco-diff-and-disco-status.md)
- **Date**: 2026-07-30

## Context

`disco diff` answers "what has this sandbox changed?" That is a diff, so it
needs a base commit, and the answer is not given: a sandbox's repository holds
several plausible ones, and picking the wrong one makes the command report work
the sandbox never did.

The first implementation used the source's `checkout.commit` — the commit
recorded at create — unconditionally. That is right in the ordinary case and
wrong in one:

**Pulled upstream work.** An agent that fetches and merges newer upstream has
brought in commits it did not write. Against `checkout.commit` those appear as
the sandbox's own changes, and the diff grows by however far upstream has
moved.

`disco apply` (ADR-0014) already faced the base question and answered it with a
merge base — but computed in the *local* repository, between local `HEAD` and
the fetched sandbox tip. Reusing that answer verbatim is the obvious move and is
what this ADR rejects.

## Decision

`disco diff` resolves its base **inside the sandbox**, from the sandbox's own
refs, and never from a repository on the machine running the command. The base
and the reason for it are reported with every diff.

Resolution order, evaluated by one shell command in the sandbox:

1. `--base COMMIT`, when given. `--base snapshot` is a keyword for the
   dirty-workspace snapshot ref, which only the sandbox record knows.
2. The **merge base** of `HEAD` with the remote-tracking ref for the branch the
   source was cloned at (`refs/remotes/origin/<refName>`), when that ref exists
   and the merge base is a strict descendant of `checkout.commit`. The base only
   ever moves forward: an upstream branch that was rewritten leaves a merge base
   *older* than the cloned commit, and taking it would widen the diff with
   commits the sandbox never wrote.
3. `checkout.commit`.

The default is therefore `checkout.commit`, and `disco diff` means what
`git diff <that commit>` means inside the sandbox. The merge base only ever
displaces it when the sandbox actually fetched.

The right-hand side is the sandbox's whole working state — including files git
has never been told about — written into a scratch index as a tree object, so
the comparison is tree against tree.

## Consequences and rejected alternatives

**Rejected: compute the merge base locally, as `apply` does.** `apply` must
work locally: it is about to cherry-pick onto a local branch, so its base is
only meaningful relative to local `HEAD`. That is why `apply` legitimately
refuses to proceed without a local directory and demands `--dir`. A diff only
*describes* the sandbox. Resolving locally would import `apply`'s
preconditions — a local clone, on the machine the sandbox was created from —
into a command that otherwise needs neither, breaking `disco diff` for remote
URL sources and for any sandbox created on another host. It would also make the
output depend on where the command was run, which is worse than being wrong:
two people would see different diffs of the same sandbox and both would be
right.

**Rejected: default to the dirty-workspace snapshot.** ADR-0001 lets a sandbox
be created from a dirty local tree: the client snapshots the working tree into a
commit under `refs/discobox/run/snap_*`, and the sandbox re-applies it as
uncommitted changes on top of `checkout.commit`. Basing the diff on that
snapshot excludes what the user handed the sandbox, which is appealing in the
abstract — "show me what the *sandbox* did" — and was implemented first. It is
wrong in practice: on a real sandbox it turned a 130-file diff into an empty
one. The carried-in work is in the sandbox and is not in the base commit; a
command that answers "nothing has changed" about a workspace visibly full of
changes is answering a question nobody asked. `disco diff` is the sandbox's
`git diff`, and a `git diff` never hides a change on the grounds of who made it.
The view is kept, one flag away, as `--base snapshot`.

**Consequence: push-delivered sandboxes can only ever reach rule 3.** A
push-delivered sandbox's repository is created by `initGitSource` as `git init`
with no remote added at all; the client pushes commits in over the control-plane
proxy. There is no upstream in that repository, so no merge base exists to find.
This is a real limit of resolving in the sandbox, accepted because the
alternative is the local resolution rejected above. `--base snapshot` still
works there: the snapshot ref is pushed in alongside the branch.

**Consequence: the diff writes objects into the sandbox's object store.**
Building the working state as a tree requires `git add` into a scratch index
(`GIT_INDEX_FILE`, seeded from the real index for its stat cache) and
`git write-tree`. The repository's own index is untouched, so the agent working
in the sandbox is unaffected, but blobs and trees are written and left for `gc`.
The rejected alternative — `git diff BASE` against the working tree plus a
per-file `--no-index` pass for untracked files — cannot compare against a base
that already contains those files: git consults the index for what exists, so
untracked files present in a snapshot base are reported as deletions. It also
produced one `--stat` summary per untracked file.

**Consequence: `diff` and `apply` can report different bases for the same
sandbox.** They are answering different questions — "what did this sandbox
change" against "what has not yet landed on this machine" — and `apply`
additionally narrows by its own `AppliedSourceCommit` history, which `diff`
deliberately ignores: work already applied is still work the sandbox did.
