# 0077 — Declining a directory copy creates a discobox with no source

- **Status**: Accepted
- **Date**: 2026-08-28
- **Supersedes**: [0073](0073-a-directory-with-no-repository-is-copied-only-when-asked.md) §2
  and §3's outcome; the rest of 0073 stands unchanged.

## Context

ADR 0073 made `discobox run` ask before copying a directory that is in no Git
repository, and decided what "no" produces: a discobox on the empty base commit,
at the directory's own path, with none of its content. It reached for that shape
because it already existed — an empty directory produced it — and because
cancelling the run is the wrong answer to "do not copy my home directory".

"No source at all" was not a shape the client could ask for then. It is now:
`discobox run --no-source` sends no `config.source`, which the control plane
already understood, and `-C` keeps naming the origin the discobox is filed under
and the Git authorship it commits as. That is a better answer to the same
question than a repository of nothing, and it is what the user actually said.

The empty-commit-at-the-path shape also turned out to carry a cost 0073 could not
have seen. That source is push-delivered, so its directory is created and bound
onto its in-sandbox target before the push that fills it lands. When the target
is the sandbox's own `$HOME` — `discobox run` in a home directory, the exact case
the question exists for — the agent writes into it while it parks, and the clone
that finishes delivery fails on a directory that is no longer empty. The failure
is permanent, because nothing is marked materialized until it succeeds: the
discobox never starts again. pool-agent now materializes into such a directory
rather than failing, which it must whatever this ADR decides, but a checkout of
nothing mounted over `$HOME` was never what declining asked for.

## Decision

### 1. Declining sends no source

Answering "do not copy" resolves to no primary source at all: the same request
`--no-source` builds, reached by answering the question rather than by passing
the flag. No repository is built over the directory — the question is asked
before `gitutil.InitOverWorkTree`, so declining now costs nothing at all.

`-C` keeps its meaning exactly as it does for `--no-source`. The discobox is
still filed under the directory the run came from, still listed there, and still
commits under the Git authorship read from this machine. Only what would have
been checked out is left out.

### 2. `--include-dirty=false` means the same thing ahead of time

0073 §3 made the flag answer this question; it keeps answering it, and the answer
it gives is now §1's.

### 3. An empty directory is unchanged

It is still not asked about, and it still gets its source: the empty base commit
at the directory's own path. Nothing was declined there. Standing in an empty
directory and running is how a project that does not exist yet gets started, and
a discobox whose workspace is that path is what makes `discobox push` carry the
work back to it.

So the two cases 0073 deliberately made identical now differ, and the thing that
separates them is the user's answer rather than the directory's size.

### 4. An extra source declines the same way

`-i` resolves each source exactly as the primary one is, so a declined `--include`
directory is left out of the discobox entirely rather than brought in empty.

## Alternatives rejected

**Keep 0073 §2's empty commit at the directory's path.** It has a genuine use:
work started in the discobox lands where the directory would have been, and
`discobox push` delivers it there. Rejected because that workspace is a path the
user has just said they do not want carried in, and because being at that path is
what puts a parked, empty repository over whatever lives there inside the sandbox
— `$HOME` in the case the question exists to catch. The use it serves is served
by answering yes, by `-i`, or by running in the directory once it is a repository.

**Cancel the run when the answer is no.** Rejected again, for 0073's reason: the
answer to "don't copy my home directory" is not "and now do nothing at all", and
a default that destroys the user's intent to create something is a default they
cannot leave alone.

**Make an empty directory sourceless too, for symmetry.** Rejected: nothing is
declined there, and it would take away the one shape that makes a new project
directory work — a discobox at that path with somewhere to push back to. Symmetry
between an answered question and an unasked one is not worth that.

## Consequences

- A declined run produces a discobox with no working tree. The harness starts in
  the sandbox's home, and `discobox push` has no primary source to carry
  anything back to. That is what leaving the directory out means; `-i` and
  answering yes are how to have a workspace.
- Declared sources fall away with the checkout that would have declared them,
  exactly as they do for `--no-source`: the file that declares them lives in a
  checkout there is none of.
- Non-interactive runs are unchanged. With nobody to ask the directory is
  copied, which is what 0045 established and 0073 kept.
- pool-agent's materialization of a non-empty source directory is not made
  redundant by this. A push-delivered source's directory is bound into the
  sandbox before its push lands whatever the source is, so any source whose
  target overlaps what the sandbox writes — a repository whose root is `$HOME`,
  say — reaches the same clone with the same directory under it.
