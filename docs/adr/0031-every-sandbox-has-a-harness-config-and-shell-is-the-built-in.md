# 0031 — Every sandbox has a harness config, and `shell` is the built-in one

- **Status**: Proposed
- **Date**: 2026-08-11
- **Supersedes**: parts of
  [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) — see
  "Relationship to 0016"

## Context

ADR 0002 made the harness config the only harness concept, and the image a
sandbox runs comes from it. But a sandbox may have no harness config at all,
and then its image comes from somewhere else entirely: the server's
`DISCOBOX_DEFAULT_SANDBOX_IMAGE`, applied inline in `CreateSandbox`. Inside
such a sandbox the agent falls back to its own `shell` harness — a login shell
— resolved from the image, invisible to the control plane.

So "no harness" never meant "no harness". It meant *this* harness, chosen by a
different mechanism, recorded nowhere, and named only in the sandbox agent.
That second mechanism has cost:

1. **Image resolution lives in two places.** `resolveHarnessConfigID` returns
   nil, and thirty lines later `CreateSandbox` reaches for `s.defaultImage`.
2. **Upgrades did not reach these sandboxes.** `upgradeTarget` read "no harness
   config" as "no image to move to", so a harnessless sandbox was pinned for
   life to whatever agent shipped the day it was created. A months-old sandbox
   could not open an SSH session, because its agent predated the workdir
   handling the SSH ingress needs (ADR 0024), and recreation was the only
   remedy. The fix restored upgrades for these sandboxes by special-casing the
   default image as their target — a second image rule beside the first.
3. **Nothing downstream can rely on a harness.** The pool agent, and anything
   reasoning about what a sandbox runs, must carry a "or else the default"
   branch that only exists because the control plane declined to write the
   answer down.

## Decision

### 1. Every sandbox carries a harness config

`Sandbox.HarnessConfigID` is always set. Create resolves it in one chain and
that chain always terminates:

    explicit --harness / harnessConfigId  →  project default  →  the built-in `shell` config

The inline `s.defaultImage` fallback in `CreateSandbox` is removed: a sandbox's
image comes from its harness config, with no second path. Existing sandboxes
with no harness config are backfilled to their project's `shell` config by
migration, and the column becomes required afterwards.

### 2. `shell` is a seeded built-in whose image is the default sandbox image

`SeedBuiltIns` gains a `shell` config alongside `claude-code`, `codex`, and
`opencode`. It differs from them in two ways, both because it is the one that
must always work:

- Its image is `DISCOBOX_DEFAULT_SANDBOX_IMAGE`, not a harness-specific image.
  That image is what carries the sandbox agent; the others are built on top of
  it. Seeding keeps it current the same way it keeps the others current, which
  is what makes an agent upgrade reach a sandbox that runs no agent product.
- It is born `Configured`. Every other built-in starts unconfigured — visible
  but not selectable until its credentials exist — and `shell` has nothing to
  configure. A fresh project has to be usable before anyone configures
  anything.

Its run command is the login shell the sandbox agent already resolves from the
run user's passwd entry, so this records in the control plane what the agent
was doing anyway rather than introducing new runtime behavior.

### 3. There is no `fallback` flag; the built-in is the end of the precedence chain

A `HarnessConfig.Fallback` flag was considered and rejected. "Fallback" and
"default" are not two concepts: `Project.DefaultHarnessConfigID` already
answers "which harness when the caller didn't name one", and a fallback flag
answers "which harness when the project hasn't named one" — the same question
one level down. Two fields answering it means they can disagree, and something
has to define which wins when a project's default is X and the config flagged
fallback is Y. Nobody would set that deliberately; somebody would set it by
accident.

What the flag would buy — letting a project choose a different fallback — is
what setting the project default already does. So the chain ends at a
well-known seeded config, identified by its reserved slug exactly as the other
three built-ins are.

The distinction the flag was reaching for is worth keeping, and survives
without it: a project with no default is still visibly a project where nobody
has chosen an agent, because `DefaultHarnessConfigID` stays empty rather than
being quietly pointed at `shell`. Resolution falls back; state does not.

### 4. A deleted default resolves to `shell`, not to nothing

`resolveHarnessConfigID` currently returns nil when the project's default was
deleted, with the comment "leave the sandbox agent-less". Under this ADR that
case resolves to `shell` like any other absent default. A sandbox created the
day after someone deleted a harness config should not be a structurally
different kind of sandbox.

## Alternatives rejected

**Keep harnessless sandboxes and special-case them.** This is the status quo
plus the upgrade fix that motivated this ADR. It works, and it costs a second
image rule that every future feature has to remember: the SSH ingress found it
by breaking, and the upgrade path found it by silently excluding a whole
population of sandboxes for months.

**Point the project default at `shell` at seed time, so only "default"
exists.** Simpler by one concept, and it erases the difference between "chose
the shell" and "never chose". That difference is what a UI needs to prompt for
an agent, and once erased it cannot be recovered.

**Give `shell` no image and let the agent keep resolving it.** Keeps today's
runtime behavior exactly, and keeps the upgrade gap: with no image on the
config there is nothing for an upgrade to move to, which is the bug.

## Consequences

- The harnessless branch of `services.SandboxUpgradeTarget` becomes
  unreachable and is removed: every sandbox upgrades to its harness config's
  image, one rule. `DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST` is still read, now
  as the seeded `shell` config's digest rather than as a per-sandbox fallback.
- A sandbox's image is answerable from its harness config alone, so the pool
  agent's contract can require one rather than tolerating its absence.
- `disco box harness configure shell` has nothing to do and should say so.
- Migration must backfill every existing sandbox, including archived ones,
  before the column can be required. A project whose sandboxes predate seeding
  gets its `shell` config created by the same migration.
- Deleting the `shell` config has to be refused, the way deleting the last
  route out of a system is refused. It is the end of the resolution chain, and
  a project without it can create no sandboxes.

## Relationship to 0016

0016 stands except for its treatment of sandboxes with no harness config:
"Sandboxes with no harness config (the default image) pin no digest and never
report an upgrade" no longer describes anything, since there are no such
sandboxes. Its substance is untouched and still governs: upgrades are explicit
and in-place, availability is derived at read time rather than stored, and the
comparison is on digest rather than tag because dev workflows rebuild tags in
place. Accepting this ADR marks 0016 `Partially superseded by 0025`.
