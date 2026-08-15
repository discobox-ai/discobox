# 0042 — Git authorship identity is a first-class sandbox property

- **Status**: Proposed
- **Date**: 2026-08-14

## Context

A sandbox clones a repository, a harness works in it, and `disco apply` pulls the
resulting commits back to the host. Every commit in that chain needs an author.

Nothing in the create path supplies one. The pool agent writes a temporary
`GIT_CONFIG_GLOBAL` holding `safe.directory` and nothing else
(`runGitWithSafeDirectories`), `boot` creates the account and seeds `/etc/skel`
without authoring a `~/.gitconfig`, and the sandbox image runs no `git config`.
So a harness that commits inside a sandbox hits git's "Please tell me who you
are" error, or — worse on images where the account has a hostname-derived
fallback — writes commits authored by `sandbox@<container-id>`.

The host-side `gitutil.CommitTree` shows the shape of the problem: it hardcodes
`GIT_AUTHOR_NAME=Discobox` / `discobox@example.invalid` because it snapshots a
dirty worktree and needed *some* author. That is fine for a snapshot commit
nobody attributes, and wrong for work a person did.

The identity is already known at the only place that can know it: the client.
`disco run` and the TUI both run on the developer's machine, in or beside the
repository they are spawning a sandbox from, where `git config user.name` and
`user.email` answer the question directly — including a per-repository override,
which is how work-versus-personal identity is normally separated.

## Decision

### 1. Git identity is its own create property, not part of `SandboxUser`

`SandboxConfig` and `SandboxCreateConfig` gain a `git` object holding
`userName` and `userEmail`. It is a sibling of `user`, not an extension of it.

`SandboxUser` is the *run* identity: which account a process runs as, and which
groups it holds. ADR 0025 §1 makes it one schema with one meaning at two layers —
`sandbox create` defines the sandbox's user, `exec create` overrides it for a
single exec — and holds that "a field is never accepted at one layer and ignored
at the other". Git authorship has no meaning at the exec layer: an exec does not
have its own committer. Adding it to `SandboxUser` would put the first
ignored-at-one-layer field into the schema whose entire value is that it has
none.

The two are also independently absent. A sandbox running as the image's own
account (ADR 0025 §5, no `user` at all) still wants the caller's git identity,
and a sandbox told to run as a specific uid may have no git identity to give.
Nesting one in the other makes each depend on the other being present.

### 2. It is a declared property, not a `files[]` entry

The runtime layer already carries `Files`, and the client could synthesize a
`~/.gitconfig` entry with no schema change at all. Rejected:

- `files[]` is overlaid **by path** (`sandboxconfig.mergeFiles`). A client-written
  `.gitconfig` silently replaces an image's, or is silently replaced by one, with
  no diagnostic and no way for either side to contribute half. Identity and
  whatever else an image puts in that file are not the same decision and must not
  collide as though they were.
- `files[]` is installed by `terminal.FileInstaller` at **harness install**, not
  at boot — and only when a harness is configured. Identity would then be absent
  for a plain `shell` sandbox and for anything that commits before the first
  terminal launches.
- A rendered file is opaque to the control plane. `git.userName` is a field the
  API can return, a listing can show, and a validator can reject; a blob of INI
  in a file array is none of those.

A declared property also joins `SandboxManifest.Fingerprint()`, so a changed
identity is a spec change like any other rather than something that quietly
differs from what the sandbox was built with.

### 3. The client reads the identity from the source repository

`disco run` and the TUI resolve `user.name`/`user.email` with git's own
resolution, run from the local source directory — so a repository-local override
wins over the global one, which is what a developer who set it intended. A remote
source has no local worktree, so the read falls back to the process working
directory, mirroring how `ResolveOrigin` already resolves an origin for a remote
source.

An unset identity is not an error and not a guess. Git itself is the authority
on whether one is configured; if it reports nothing, the property is omitted and
the sandbox is left exactly as it is today. Inventing `$USER@$(hostname)` here
would relocate the fallback that is already wrong, into a layer that has less
information than git does.

### 4. Boot seeds each key only where git has no answer

A new step in `boot.provision`, after `seedHome`, sets `user.name` and
`user.email` in `<home>/.gitconfig` through `git config` itself, and chowns the
result to the sandbox user. Each key is decided independently, and written
**only when git resolves no value for it**.

The unit is the key, not the file. "Does `~/.gitconfig` exist" and "is an
identity configured" are different questions, and the first is a bad proxy for
the second: a file holding aliases and a signing key but no `[user]` section is
precisely the case that most needs seeding, and skipping it because the file
exists would fail the sandbox that needed this most. Per-key also means a person
who set only `user.email` inside the sandbox keeps it and still gets a name.

Git answers the question, rather than boot parsing the file. Config resolution
has `include`/`includeIf`, `[user]` may be spelled several ways, and a value may
come from `/etc/gitconfig` — an image that shipped an identity there has an
answer, and boot is not entitled to a second opinion about it. This is the same
rule the client follows on the way in (§3): git is the authority on whether an
identity exists, at both ends.

What is never done is overwriting. The home directory is a persistent data
volume that survives restart, upgrade, and container replacement, and a person
working in a sandbox edits that file. Re-asserting an identity that has not
changed since create is not worth overriding a deliberate local change for.

**Deferred:** changing your local git identity does not propagate into sandboxes
that already exist — they keep whatever identity they resolved first. Revisit if
that drifts in practice (a corrected email, a changed name), at which point the
mechanism is already the right one: the same per-key `git config` write, with
the "only if unset" condition relaxed for keys boot itself previously set.
Distinguishing those from a user's own edit is the part that needs designing,
and is why this is deferred rather than done now.

## Alternatives rejected

**Deliver it as `GIT_AUTHOR_*`/`GIT_COMMITTER_*` env vars.** Zero new schema, and
they reach every exec through `execs.EnvWithRuntimeDefaults`. Rejected on three
counts: `git config --get user.email` still reports nothing, so any tool that
asks rather than committing sees an unconfigured repository; `sudo git commit`
loses them, since `boot`'s sudoers `env_keep` carries proxy variables only and
would have to grow a second unrelated purpose; and `agentstatus`'s git shelling
sets no env of its own, so the two would disagree about the same sandbox.

**Extend `SandboxUser` with `gitUserName`/`gitUserEmail`.** Rejected in §1. One
schema shared with `exec create`, where the fields have no meaning.

**Synthesize a `files[]` entry client-side.** Rejected in §2. Silent by-path
collision, installed at harness time rather than boot, and opaque to the API.

**Have the pool agent extend the `GIT_CONFIG_GLOBAL` it already writes for
`safe.directory`.** That file is a host-side temporary, built per git invocation
for the pool agent's *own* clone commands and deleted immediately after. It never
enters the container, and nothing the sandbox runs reads it.

**Default to the OS user when git has no identity configured.** `$USER` and
`$(hostname)` produce a plausible, wrong, unverifiable author — the same failure
as the `sandbox@<container-id>` case this ADR exists to fix, moved one layer out.
Absent stays absent; git's own error message already tells a user exactly what to
set.

**Write `~/.gitconfig` on every boot.** Rejected in §4: the home directory is
persistent and edited, so this overrides a deliberate local change to re-assert
an unchanged value.

**Write the file only when it does not exist (`O_EXCL`).** The first form of
§4, and wrong for the reason §4 now gives: file existence is not identity
configuration. A `.gitconfig` carrying only aliases would permanently block
seeding, in exactly the sandbox that needed it. It is also weaker than it looks
— it cannot fill one key while leaving the other alone, since the file is the
smallest thing it can reason about.

**Parse and merge `~/.gitconfig` in boot.** Avoids shelling out, and is how the
`O_EXCL` version would have to grow to reach per-key behavior. Rejected: it
means reimplementing git's config resolution — `include`/`includeIf`, casing,
multi-valued keys, `/etc/gitconfig` precedence — to answer a question the `git`
binary sitting in the image answers exactly. A sandbox with no git needs no
identity anyway, so the dependency costs nothing real.

## Consequences

`SandboxManifest` gains `git_user_name` and `git_user_email` columns, picked up
by `AutoMigrate`. Both join `Fingerprint()`, so changing either through an update
is a spec change that rebuilds the container — consistent with every other
manifest field, and the reason identity cannot silently differ from what the
sandbox was built with.

`sandbox.json` gains a runtime-owned `git` object. It is single-writer: no image
or project layer contributes to it, so `Effective` copies it straight through.

A sandbox created by `disco run` or the TUI on a machine with a configured git
identity commits as that identity. One created without it behaves exactly as
before. `disco box sandbox create` takes the identity as explicit flags, since
that command is the flag-driven path and does not infer anything from the
environment.

Sandboxes created before this change keep an empty manifest identity; nothing
backfills them, so their next boot has nothing to seed from.

Boot shells out to `git` on the sandbox startup path, twice per unset key. An
image without `git` installed skips the step rather than failing to boot, the
same rule `ensureAdditionalGroups` applies to a group the image never created.
