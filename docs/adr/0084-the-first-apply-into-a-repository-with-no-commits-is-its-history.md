# 0084 — The first apply into a repository with no commits is its history

- **Status**: Accepted
- **Date**: 2026-09-02
- **Supersedes**: [0083](0083-a-repository-with-no-commits-is-uncommitted-work-on-an-empty-base.md)'s
  consequence that `discobox apply` fails back into such a repository; the rest
  of 0083 stands unchanged.

## Context

ADR 0083 made `git init` and then `discobox` work: the working tree is
uncommitted work on an empty base commit, delivered by push. It listed one thing
it did not do — apply back:

> `discobox apply` back into the repository fails while HEAD is still unborn —
> the cherry-pick (ADR 0014) resolves the host's HEAD to find what to apply
> onto, and there is none. The user's own first commit in the host repository
> resolves it; bringing a sandbox's first commit home into a repository with no
> commits is future work.

That is the wrong place to stop. The whole point of `git init` and then
`discobox` is that the project does not exist yet and the discobox is where it
gets written; a round trip that can deliver the work out but not bring it back
leaves the user's repository empty forever, and telling them to invent a first
commit to make apply work is the same non-answer 0083 rejected at create.

Three things about this case are already recorded, and together they are enough
to do it properly: the source directory, the machine it is on (`Origin.HostId`,
which `resolveApplyHostDir` already gates on), and `NoLocalCommits` — the fact
that this discobox was created from a repository that had none.

Two mechanisms in the apply path assume a HEAD that resolves, and both fail for
a different reason.

The base is a merge base: `git merge-base <tip> HEAD`. There is no HEAD, and
there would be nothing to find in it if there were — a repository with no
commits shares no history with anything, so a merge base is not merely
unavailable but meaningless here.

The landing is `git merge --ff-only` onto the checked-out branch, which needs a
branch that exists. Pointing it at the discobox's commits directly does not
work either: the files the discobox was created from are still sitting in the
working tree untracked, and git refuses to overwrite an untracked file on
checkout — including one whose content is byte-for-byte what it is about to
write.

## Decision

### 1. The base is the discobox's own empty base commit

A source that recorded `NoLocalCommits` takes its base from the source itself —
`checkout.commit`, the empty root commit 0083 created — instead of a merge base.
A prior apply still wins, as it does for every source; only the fallback
changes.

This is not conditional on the local repository still having no commits. The
histories are unrelated by construction, whatever has happened here since, so
the merge base would be wrong on any run rather than only the first. What it
means in practice is that a user who has since committed something of their own
gets the discobox's commits cherry-picked on top of it — the ordinary apply,
reached without a common ancestor, because cherry-pick never needed one.

The report says which of the three answers it used (`baseOrigin:
"discobox-base"`), since "everything after the empty base this discobox started
from" is a different claim about what is being applied than either of the
others.

### 2. The empty base is replayed away, not inherited

When the local repository still has no commits, the cherry-pick runs onto an
unborn HEAD of its own — a scratch worktree, detached at the empty base so its
working tree and index start empty, then `git checkout --orphan` to drop the
history and keep them.

So the discobox's first commit becomes the repository's root commit, authored by
whoever wrote it, and 0083's `discobox run empty base` never enters the user's
history. That commit is scaffolding: it exists so a snapshot has something to
hang off and a push has something to carry, and the user's project should not be
rooted at a commit authored `Discobox <discobox@example.invalid>` forever.

Losing it costs nothing that apply was not already spending. Cherry-pick makes
new commits every time; a local commit never has the SHA it had in the discobox,
and the applied-commit record has always been a pairing of two different SHAs.

### 3. The landing is guarded by what the discobox was created from

There is no branch to fast-forward: the branch HEAD already names is created at
the applied tip — that is what being born means — and `git reset --hard` then
fills the index and replaces the working tree.

That reset is the one destructive step in this ADR, so it is gated on the
working tree still being exactly what the discobox was given: the workspace
snapshot's tree, or the empty tree for a discobox created from an empty
repository. The snapshot ref is resolved out of the local repository, where
create wrote it, and fetched from the discobox's origin if it has been pruned
from here — delivery pushed it there, and a missing ref must not read as a
changed working tree.

If the tree differs, the apply is refused with the paths that differ and
nothing changes. The files about to be replaced are untracked and this
repository has no commits, so there is nowhere for anything the user changed
since to survive; this is the only moment anything can be said about it.

The way out is the first next step the refusal prints, and it works because of
§1: commit the local changes, and the discobox's commits cherry-pick on top of
them.

### 4. Unborn HEAD is one shared answer

`HeadIsUnborn` and the working-tree-with-no-HEAD walk are the same two questions
create and apply both ask, so they live in one package (`cli/internal/gitunborn`)
that both use rather than being written twice with two chances to disagree about
what `.gitignore` means.

## Alternatives rejected

**Fast-forward the branch straight to the fetched tip.** No cherry-pick, no
scratch worktree, and the local commits keep the SHAs they had in the discobox —
a property no other apply has, and a genuinely nice one for a repository whose
history this is about to become. Rejected because it makes 0083's empty base the
permanent root of the user's project, which is exactly the commit 0083 went out
of its way not to put in their history at create. It also cannot be done at all
without the same untracked-file problem, so it does not even buy simplicity.

**Refuse unless the working tree is untouched, with no way forward.** Simplest
guard, and safe. Rejected because §1 makes a way forward exist: once the local
changes are committed, the same range applies onto them cleanly, so the refusal
can name a next step that actually finishes the job rather than only saying no.

**Compare the files the apply would overwrite, instead of the whole tree.** Only
guards the paths at risk, and lets an apply through when the user's edits are
somewhere else. Rejected as the wrong test: the discobox has usually edited the
same files, so "differs from what is about to be written" is the normal case
rather than the dangerous one. "Differs from what the discobox was given" is the
question worth asking, and it is one tree comparison.

**Commit the working tree locally on the user's behalf before applying.** Would
make the guard unnecessary: local work would have somewhere to survive, and the
range would cherry-pick onto it. Rejected on 0083's grounds — a commit the user
did not write, made silently, in the repository they were only asking to apply
into.

## Consequences

- A discobox created from a repository with no commits never uses a merge base
  again, on any apply. If that repository later grows a history that genuinely
  does share commits with the discobox's — someone fetched the discobox's own
  branch into it by hand — the recorded base is still the empty one, and the
  range is still everything the discobox committed. Cherry-pick skips what is
  already there, so the result is right; the report just names more commits than
  it lands.
- The first apply is the only one that replays onto no history. Once the branch
  exists, every later apply is the ordinary fast-forward path.
- A refused first apply leaves the repository with no commits, so the discobox's
  work is still only in the discobox. Nothing is lost either way, but the user
  has to act before the round trip completes.
- `git reset --hard` removes nothing untracked, so anything ignored or never
  carried into the discobox stays where it is. A file the discobox deleted also
  stays, as an untracked file: apply brings commits home, and deleting a file
  the local repository never tracked is not one of them.
