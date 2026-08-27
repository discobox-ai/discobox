# 0072 — A repository ships skills that only exist inside a sandbox

- **Status**: Proposed
- **Date**: 2026-08-27

## Context

A coding harness reads its skills from the developer's home directory —
`~/.claude/skills` for Claude Code, `~/.agents/skills` for the emerging shared
convention. That location is the problem: the skills a *repository* wants the
agent to have are properties of the repository, but the only place to put them
is a directory the repository does not own and cannot write, on a machine it
does not control. Asking every contributor to copy files into their home
directory is not a contract; it is a README step people skip, and one that
changes their own agent everywhere else they work.

Inside discobox the home directory is ours. A sandbox is created per task, from
one repository, and its home directory exists only for the life of that
sandbox. So the repository can be given the missing half of the contract: it
declares skills, and they exist for the agent working on it here and nowhere
else. A clone on a laptop leaves `~/.claude/skills` untouched.

The repository already has a place to speak to discobox — `.discobox/`, which
holds `project.json` (ADR 0012 §7), `hooks/`, and `services/` (ADR 0070). What
was missing is that all three of those are *read* where they lie; skills are
files that have to be *delivered* to a location the harness independently
decides to look in.

## Decision

### 1. `.discobox/skills` in the primary source is copied into the harness's skill directories

The primary source's `.discobox/skills` tree is copied, recursively, into
`~/.claude/skills` and `~/.agents/skills` in the run user's home, owned by the
run user.

Both directories, unconditionally. The repository declaring the skills does not
know which harness the sandbox runs — the sandbox's harness is chosen at create
time, by the sandbox or the project (ADR 0048), not by the repository — and a
directory the running harness never reads costs a few files.

Only the primary source. A sandbox may carry several sources, but one of them is
what it is working on, and skills that follow a checked-out dependency into home
would be the dependency configuring the agent working on somebody else.

The copy overwrites by name and leaves the rest of the directory alone. The
repository is the more specific declaration and wins over an image-installed
skill of the same name, but it does not own the directory: the image, the
harness config's files, and the harness itself write there too.

Symlinks are skipped rather than copied. A link inside a checkout resolves
against the checkout; the same link resolved from a home directory points
somewhere else or nowhere. Executable bits carry over — a skill's helper script
has to stay runnable — and nothing else about the checkout's permissions does.

### 2. It happens once, on the primary terminal's first launch

The install hangs off the branch of the primary launch that has never run
before (`PrimaryTerminalLaunched`, ADR 0039), not off the installer that runs
before every terminal.

**Why not the per-terminal installer.** `EnsureInstalled` runs on every create
and every revive, and it is idempotent by design because harness *files* are
declared configuration the sandbox owns. Skills are not: once copied, they are
the harness's files. Claude Code prunes, rewrites, and reorganizes its skills
directory as it works; re-copying on the next launch would put back what the
agent deliberately removed, every single time it is resumed. A file the user
cannot delete is worse than a file they never got.

**Why not every source-facing terminal.** A second terminal in the same sandbox
shares one home directory. Installing per terminal would be the same copy done
repeatedly to the same destination, with the same problem.

Ordering falls out of this placement for free: the first primary launch is the
one already sequenced behind source delivery (ADR 0055), and it is the only
launch that is. A copy that ran earlier would read an empty repository.

The consequence is that a skill added to the repository later reaches the next
sandbox, not this one — the same rule `.discobox/project.json` already has
(ADR 0012 §7: read once, at the commit that materialized the source), so the
repository's contribution to a sandbox has one lifetime rather than two.

### 3. A failed install fails the launch

A repository that declares skills and does not get them is misconfigured, and
the failure is in a directory its own author controls. It surfaces where every
other install failure does — the primary terminal's launch — rather than as a
missing capability the agent never mentions.

## Alternatives rejected

**Deliver them as harness files through the project layer.** `ProjectLayer`
already has `FilesAdd`, which appends home-relative files from
`.discobox/project.json`. It is the wrong mechanism twice over: those files are
re-installed before every terminal, which is exactly the reconcile-forever
behavior §2 rules out, and the content would have to be inlined into JSON
rather than living as an ordinary directory of Markdown that a person can read,
review, and diff in the repository.

**Symlink the home directories at `.discobox/skills`.** Cheap, and edits in the
repository would appear live. Rejected because it makes the repository's
working tree the harness's skills directory: the agent's own pruning and
rewriting would land as uncommitted changes in the user's diff, and a harness
that replaces a file it does not recognize would silently modify the checkout.
It also cannot merge — a symlinked directory replaces the image's skills instead
of joining them.

**Do it in `boot`.** Boot is PID 1 and runs before systemd; it also runs before
a push-delivered source exists (ADR 0055). It would be both too early and on the
latency path every start pays.

**Make it a harness-agnostic list in the image.** The two skill directories are
a property of the harnesses, not of discobox, so an image-declared list is the
natural home if a third convention appears. Deferred until one does: two
hardcoded paths in the sandbox agent are honest about how much is actually
known, and inventing an image-owned declaration now would have to be revisited
anyway when the shape of the third is known.
