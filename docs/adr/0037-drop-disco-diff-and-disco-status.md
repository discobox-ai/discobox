# 0037 — Drop `disco diff` and `disco status`

- **Status**: Accepted
- **Date**: 2026-08-12
- **Supersedes**: [0018](0018-disco-diff-resolves-its-base-inside-the-sandbox.md)

## Context

`disco diff` and `disco status` answered "what has this sandbox changed?" by
running git inside the sandbox over the exec API: base resolution as its own
exec (ADR 0018), a scratch-index commit of untracked work, a payload guard
that hashed untracked files before diffing, a client-side render/stream split,
and a pager. The launcher grew a matching surface: `d` and `i` drew the
commands in panes on local ptys so their output could be read beside a
terminal.

Meanwhile the sandbox-agent now reports git state with the rest of its status
(ADR 0030): branch, head commit, cleanliness, and a `git diff --shortstat`
against the spawn commit. Every listing and the launcher show that diffstat
for free, with no exec and no wake-up.

That left two answers to "what changed" built on different machinery: a pushed
summary that costs nothing, and a heavyweight pull pipeline whose entire
apparatus existed to render a patch the user usually acts on by running
`disco apply` anyway.

## Decision

Remove `disco diff` and `disco status`, their TUI invocations, and the
machinery only they used: the exec-driven base resolution, the untracked
payload guard, the diff renderer, and the pager. The agent-reported git state
is the one answer to "what has this sandbox changed" at a glance;
`disco apply` remains the way to bring the changes to this machine, where
local git tooling can inspect them.

ADR 0018 is superseded as a command decision: there is no pulled-patch diff
left to resolve a base for. Its base rule outlives the command, because the
failure it prevents — pulled upstream commits counting as the sandbox's own
changes — now lands in the diffstat column instead: the agent measures
against the spawn commit, forwarded to the merge base with the source's
upstream tracking ref when that is a strict descendant (0018's resolution
order, computed by the sandbox-agent from the manifest's base commit and
upstream ref rather than by an exec).

## Consequences

- The CLI no longer renders patches. Inspecting a sandbox's changes in detail
  means applying them or opening a shell in the sandbox.
- If a full patch view returns, it should be designed against the
  agent-reported state and the apply path, not by resurrecting the exec-driven
  pull pipeline this ADR removes.
