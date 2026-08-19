# 0056 — A repository declares the sources it is worked on with

- **Status**: Proposed
- **Date**: 2026-08-19

## Context

A sandbox can hold more than one source (`SourceCodeReferences`), and
`disco run -i ../foo` puts one there. That is per-invocation knowledge: the
caller has to know which sibling repositories this one is worked on with, and
type them every time. The repository already knows — a service and the client
library it is developed against, a CLI and the server it talks to — and nothing
records it.

Two constraints shape where that knowledge can live.

The resolution is client-side. Whether `../foo` exists on this machine, whether
its working tree is dirty, and whether it can be bound or must be pushed are
facts only the CLI can establish, and it must establish them before the create
request is built. `.discobox/project.json` is read by pool-agent out of the
materialized clone (ADR 0012 §7), long after every source has been decided, so
the project layer cannot carry this.

The in-sandbox path matters. A local source keeps its own host path inside the
sandbox, so `../foo` relative to the primary source resolves there exactly as it
does here. A declared source that resolved to a clone instead would break that
relationship if it landed somewhere else, and a script in the repository that
worked for the author would fail for everyone without the checkout — the
opposite of what declaring a dependency is for.

## Decision

### 1. `.discobox/sources.json`, read by the client

A repository names its companion sources at its root, as a JSON object of name
to Git URL, optionally with an `@REF` suffix:

```json
{"foo": "https://github.com/acme/foo", "bar": "git@github.com:acme/bar@main"}
```

The CLI reads it from the primary source's working tree at create, in
`sandboxcreate` — the same place `--include` is resolved — and files the result
as ordinary source code references. Nothing about the server, the pool, or the
sandbox changes: they receive the sources they would have received had the
caller typed `-i` for each.

It is a file of its own rather than a field of `.discobox/project.json` because
the two have different readers, at different times, in different trust domains:
this one is read on the caller's machine before the sandbox exists, that one on
the pool host after it does. `.discobox/` is already a client-read convention
directory (`hooks/`), so this sits beside an established one.

A value is a Git URL and nothing else. Where a source is looked for locally is
not the file's to say — decision 2 finds the checkout by name — so a path is
refused rather than resolved: relative to the caller's working directory, which
is the only thing it could be relative to at that point, it would quietly bring
in some other repository.

A remote primary source declares nothing. The file lives in a checkout, and
reading it would mean cloning the repository on the client first, which is the
sandbox's job.

### 2. A local checkout wins, whatever its origin says

`foo` resolves to the sibling of the primary source named `foo` — `../foo` —
when that is a directory, and to the declared URL when it is not. The local
checkout is used even when its `origin` disagrees with the declared URL: a fork
checked out next door is the ordinary reason for a mismatch, and is what the
caller has and almost always what they meant.

The disagreement is reported instead of resolved. A directory that merely
shares a name is indistinguishable from a deliberate fork at this level, so the
resolution of every declared source is announced —
`PromptOptions.ReportDeclaredSource`, which `disco run` prints on stderr —
naming the checkout's own origin when it differs. Sources nobody asked for on
the command line are exactly the ones a caller needs told about.

### 3. A cloned fallback lands where the checkout would have

A declared source that resolves to a clone is placed at the sibling path the
local checkout would have occupied, not under `/workspace`. Both resolutions
therefore produce the same sandbox layout, and `../foo` from the primary source
means one thing regardless of which machine the sandbox was created from.

This is the difference between a declared source and `-i`, which leaves the
placement to the source (a remote `-i` goes under `/workspace`, since nothing
says where else it belongs).

### 4. Explicit beats declared, and there is no recursion

`--include` is resolved first and outranks a declaration of the same source; a
declaration that resolves to something already brought in — or to the primary
source itself — is skipped rather than refused. Only the primary source's file
is read: a declared source's own declarations do nothing, so a cycle is not
expressible and a sandbox's contents are decided by one file the caller can
read.

`--declared-sources=false` opts out, for a caller who wants only what they named
or does not want a large clone on every run.

A malformed file fails the run. It is a statement about what the sandbox must
contain, and a sandbox quietly missing the sources it names is worse than no
sandbox.

## Alternatives rejected

**A field in `.discobox/project.json`.** One project file rather than two. It
cannot work: that file is read by pool-agent from the materialized clone, after
the sources it would name have already been resolved and delivered, and pool-agent
cannot see the caller's disk to prefer a local checkout. Adding a second reader
of the same file at a different time is precisely the split-precedence problem
ADR 0012 removed.

**Place cloned fallbacks under `/workspace/<name>`.** Consistent with `-i`'s
remote handling and simpler to explain. Rejected because it makes the sandbox's
layout depend on which checkouts the caller happens to have, which defeats the
purpose: the repository declares the source so that everyone's sandbox has it in
the same place.

**Verify the checkout's origin and fall back to the URL on a mismatch.** Safer
against picking up an unrelated directory. Rejected because it silently ignores
a fork the caller deliberately checked out next door, which is the common case,
and because ssh-versus-https, host aliases and mirrors make the comparison
unreliable enough that acting on it would misfire more often than it saved
anyone. The comparison is still made — it is just reported rather than obeyed.

**Resolve declarations transitively.** A declared source could name its own.
Rejected for now: it introduces cycles, makes the contents of a sandbox
unreadable from any single file, and no case needs it yet. Revisit if one does.
