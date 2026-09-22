# 0139 — The first apply into a directory with no repository makes one

- **Status**: Accepted
- **Date**: 2026-09-21
- **Supersedes**: [0045](0045-a-directory-with-no-repository-is-delivered-by-push.md)'s
  consequence that `discobox apply` fails back into such a directory; the rest
  of 0045 stands unchanged.

## Context

ADR 0045 lets a discobox be created from a directory in no Git repository — an
empty one being where "build me a new project" starts — and stopped at the way
back:

> `disco apply` back into such a directory fails: it is still not a repository,
> and `--dir` cannot help. […] the natural answer is for the user to `git init`
> when they decide the work is worth keeping.

ADR 0084 then made the case one step in — `git init` and nothing since — round
trip properly. After it, the only thing standing between 0045's directory and
the same round trip is the `git init` the user is told to run by hand: the
source records `noLocalRepository`, the directory, the machine, and the empty
base commit it started from, and the first apply into an unborn repository
already knows how to land that history safely.

0045 refused to `git init` at create because the user had only asked to *run
against* the directory. Apply is different: bringing commits home is a request
for them to exist locally, and a repository is the only place they can.

## Decision

### 1. Apply makes the repository, unborn, on the discobox's branch

When a source that recorded `noLocalRepository` resolves to a local directory
that is still in no repository, apply runs `git init` there, with
`--initial-branch` set to the branch the discobox was created on
(`checkout.refName`). HEAD is left unborn, so what follows is exactly ADR 0084's
first apply: the discobox's empty base is replayed away, its first commit
becomes the root, and the landing is guarded by the working tree still being
what the discobox was given — the empty tree for an empty directory, or the
workspace snapshot, fetched from the discobox's origin since the repository
create built it in is gone.

### 2. The repository is kept only once its branch holds the commits

What decides is the repository itself, not how apply reports ending: while HEAD
is still unborn — up to date, a dirty discobox, a refused landing, an error
before the branch is written — the `.git` apply made is removed, so the
directory is left as it was found. A refusal's next steps start with `git init`
accordingly. Once the branch holds the discobox's commits the repository is
kept, whatever fails after — checking them out, recording the apply on the
discobox — because it holds them. Apply never makes a repository over a `.git`
that is already there, readable or not, so it never removes one it did not
make.

### 3. The discobox's empty base is the base on every apply

As ADR 0084 §1 does for `noLocalCommits`: the histories are unrelated by
construction, so `noLocalRepository` also takes `checkout.commit` as the base
when no prior apply is recorded. A user who ran `git init` and committed
something of their own first gets the discobox's commits cherry-picked on top.

## Alternatives rejected

**Keep telling the user to `git init`.** Safe and already implemented. Rejected
because it is a step apply can do with the same guard ADR 0084 already relies
on, and the instruction arrives only after a failure.

**Check the working tree from a throwaway repository first, and `git init` only
once the landing is known to be safe.** No `.git` would ever appear and vanish.
Rejected as twice the fetching and a second copy of the guard for no
observable difference: removing a `.git` apply itself made seconds earlier
leaves the directory as it was either way.

**Refuse unless the directory is still empty.** Simpler to explain. Rejected
because the snapshot guard already answers the general question — "is this
still what the discobox was given?" — and an empty directory is just the case
where that is the empty tree.

## Consequences

- `discobox push` into such a discobox still refuses: `CheckDeliverable` reads
  `noLocalRepository`, and the repository apply makes holds the applied
  commits, not the objects the discobox was created against.
- The report says a repository was made (`createdRepository`), since that is a
  change to the directory beyond the files the commits carry.
