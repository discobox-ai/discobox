# 0073 — A directory with no repository is copied only when asked

- **Status**: Accepted
- **Date**: 2026-08-24
- **Supersedes**: [0045](0045-a-directory-with-no-repository-is-delivered-by-push.md) §2's
  "Nobody is asked about this"; the rest of 0045 stands unchanged.

## Context

ADR 0045 made `discobox run` work in a directory that is in no Git repository:
a repository is built over the directory from outside it, and the directory's
whole content is snapshotted onto an empty root commit as uncommitted work. It
decided deliberately that nobody is asked first, on the grounds that the
dirty-workspace question has a real alternative to offer — the last commit — and
this one does not.

Its own consequences section named the cost: "The whole directory is hashed into
the temporary repository on every run, with no ceiling … running in `$HOME` will
be slow."

That cost turns out to be the common case rather than an edge. Home directories,
`~/Downloads`, and mounted volumes are all directories with no repository, and
they are exactly the directories somebody types `discobox run` in by accident —
the mistake is invisible until the client has spent minutes indexing tens of
gigabytes and pushed them into a discobox. The directories 0045 was written for
— a scratch directory, an unpacked archive, a folder of notes — are small, and
the ones that hurt are large; nothing distinguishes them but their size, which
is precisely what the user can see and the client cannot guess.

The alternative 0045 said did not exist does exist, and 0045 named it in the
same paragraph: an empty directory is not an error, and starts the discobox on
the empty commit. "Run against nothing" is a real answer.

## Decision

### 1. The question is asked, before the repository is built

For `--include-dirty=auto` — the default — a directory in no repository is asked
about through `sandboxcreate.ConfirmCopyDirectoryFunc`, the same shape the
dirty-workspace question already takes, resolved by each frontend in its own UI.

It is asked before `gitutil.InitOverWorkTree` and before anything is indexed.
Indexing a home directory takes long enough that asking afterwards would be
asking after the damage.

An empty directory is still not asked about: copying nothing and copying its
nothing are the same discobox. With nobody to ask — no terminal — the directory
is copied, which is what 0045 established and what a source directory has always
meant here.

### 2. Not copying is the default answer, and it is an answer

The question leads with "do not copy", so Enter carries nothing in. Declining
does not cancel the run: the discobox is created on the empty base commit, with
the directory's path and none of its content, exactly as an empty directory
already produced.

This is the one asymmetry with the dirty-workspace question, whose default is
also its cheap answer. There the cheap answer keeps a repository's committed
history; here it keeps nothing. That is the point: `discobox run` in `$HOME`
should produce an empty discobox, not a copy of `$HOME`.

### 3. `--include-dirty=false` means "start empty" instead of failing

0045 rejected `--include-dirty=false` for such a directory because it "would
leave nothing to run". Nothing to run is now a thing the flag can mean, and it
is the same answer the question's default gives, so the flag answers this
question ahead of time the way it answers the dirty one. `--include-dirty=true`
is unchanged, and an explicit `@REF` is still rejected: there is still no history
to name.

### 4. The size is counted behind the question, not before it

`sandboxcreate.MeasureDirectory` walks the directory in a goroutine and exposes
a running total; the frontend polls it and rewrites the question as the number
climbs, showing "calculating…" until the walk is done. The user is asked
immediately and decides on as much of the number as has arrived.

It is a filesystem estimate, deliberately: file sizes as they sit on disk, with
symlinks counted as links and unreadable subtrees skipped. What git ends up
storing is smaller — it compresses, and a `.gitignore` in the directory is
honored — but the question is only ever "is this directory small enough to want
copied", and overstating is the safe direction for it.

## Alternatives rejected

**Cancel the run when the answer is no.** Reads as the safer refusal, and the
error could say what to do instead. Rejected because a discobox on an empty
workspace is a thing people ask for on purpose — 0045 already produces exactly
that for an empty directory — and because the answer to "don't copy my home
directory" is not "and now do nothing at all". Cancelling also makes the default
answer destructive of the user's intent to create something, which is what makes
a default hard to leave alone.

**Ask only above a size threshold.** Cheap-looking: small directories keep 0045's
behavior and only the expensive ones stop. Rejected because the threshold cannot
be known before the walk, and the walk is the expensive part — the question would
arrive after the cost it exists to avoid. A fixed number would also be wrong
per-machine and per-project, and unexplainable when it fired.

**Respect `.gitignore` in the count.** Would match what is actually carried, since
`InitOverWorkTree` honors the working tree's own rules. Rejected as the wrong
tradeoff for this question: it needs the repository built first, which is the
work being avoided, and a directory in no repository rarely has a `.gitignore`
at all. An estimate that is never lower than the truth answers the question.

## Consequences

- A non-interactive `discobox run` against such a directory is unchanged: it
  copies, because there is nobody to ask. Scripts that relied on that keep
  working; scripts that want the other answer pass `--include-dirty=false`.
- 0045's "running in `$HOME` will be slow" consequence stands for the user who
  answers yes. What changes is that they answered.
- Declining produces a discobox whose source is an empty commit at the
  directory's own path. Work started in it lands where the directory would have
  been, and `discobox push` delivers it the same way; nothing else about the
  source changes.
