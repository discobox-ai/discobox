# 0141 — A host tool with no working tree opens the working root

- **Status**: Accepted
- **Date**: 2026-09-22
- **Supersedes**: [0125](0125-tools-are-declared-in-files-the-way-services-are.md)
  §4's `/` for `{workdir}` when no working tree is known, and its consequence
  that such a VS Code window opens `/`; the rest of 0125 stands unchanged.

## Context

ADR 0125 §4 hands a host tool the discobox as placeholders, and gave
`{workdir}` a fallback for a box whose working tree is not known: `/`, "a
connected window whose tree is the box". A box with no source — `--no-source`,
or a directory whose copy was declined — always takes that fallback, so VS Code
and Zed opened such a box on the filesystem root.

Nothing about that box lives at `/`. Boot seeds the sandbox working root
(`/workspace`) and gives it to the sandbox user whatever the sources are, every
exec and shell starts there, and it is where work in a box with no source ends
up. The editor was the one way in that disagreed.

## Decision

With no working tree, `{workdir}`, `{workdir.urlpath}`, `{ssh.url}`, and
`DISCOBOX_WORKDIR` name the sandbox working root, `sandboxconfig.DefaultWorkingRoot`
— the directory boot creates and the exec default resolves to. `{git.url}`
and `DISCOBOX_GIT_URL` are unchanged: an error and empty respectively, since the
working root of a box with no source is not a repository.

The CLI names the constant rather than asking the box. The working root is one
constant that pool-agent, sandbox-agent, boot and the CLI already agree on
(`sandboxconfig`), and it reaches the CLI with no round trip into a box that
may be stopped.

## Alternatives rejected

- **Keep `/`.** It is a connected window, but on a tree nobody works in; the
  first thing done in it is navigating to `/workspace`.
- **Open with no folder at all.** VS Code's `--remote` without a path says
  "connected, nothing open", but Zed's URL has no such form, and 0125 already
  chose one rule for both.
- **Give `{git.url}` the working root too.** It would name a directory that is
  not a repository, so a clone would fail later and less clearly than the
  error does now.

## Consequences

- A tool opened on a box with no source lands where a shell in it does.
- A box whose manifest names a different working root would still be opened
  on the default. None does today: pool-agent writes the default into every
  manifest, and a configurable root would have to reach the CLI through the
  sandbox record first.
