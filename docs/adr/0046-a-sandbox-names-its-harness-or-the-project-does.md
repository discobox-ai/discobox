# 0046 — A sandbox names its harness, or the project does

- **Status**: Accepted
- **Date**: 2026-08-18
- **Supersedes**: [0032](0032-every-sandbox-has-a-harness-config-and-shell-is-the-built-in.md) §1's
  final step. §2 (`shell` is a seeded built-in) and §3 (no `fallback` flag)
  stand.

## Context

0032 made every sandbox carry a harness config, resolved in one chain that
"always terminates":

    explicit --harness / harnessConfigId  →  project default  →  the built-in `shell`

That last step is the one being removed. It was there so a fresh project was
usable before anyone had configured anything, and it made `disco run` in an
unconfigured project produce *something* rather than an error.

What it actually produces is a sandbox with no coding harness in it, silently,
in the one case where the user is most likely to have wanted one. A project with
Codex configured and no default set answers `disco run` with a shell — not
because anybody chose a shell, but because nobody chose anything and the chain
had to end somewhere. The result is indistinguishable from a working setup until
you are sitting in the sandbox wondering where the harness went.

It also gave the window nothing honest to show. The run options must name the
harness a run will use; with the fallback there is no answer that is both short
and true, because "whatever the server ends up at" depends on state the window
would have to re-derive and could still get wrong.

## Decision

The chain ends one step earlier and fails when it runs out:

    explicit --harness / harnessConfigId  →  project default  →  409

Create refuses with a message naming both ways forward: pass `--harness`, or set
a project default. A harness that resolves but is not configured is refused as
before, unchanged.

`shell` remains an ordinary seeded built-in (0032 §2, 0043) — selectable by name
and eligible to be the project default like any other. What it stops being is
the answer to a question nobody asked.

`harnessdefs.ShellSlug` and `Service.fallbackHarnessConfig` stay, because create
is not their only caller: a sandbox made before every sandbox carried a harness
config (`HarnessConfigID == nil`) adopts the `shell` config when it is upgraded,
and the API reports that as its available upgrade. That is a migration for rows
that predate the contract, not a default for new ones, and it ends when the last
of them is converged.

`resolveHarnessConfigID` now returns a plain `string`: every path either
resolves an id or errors, so the pointer that meant "no harness config" has
nothing left to mean.

## Alternatives rejected

**Fall back to the only configured harness when there is exactly one.** Reads
helpfully and behaves unpredictably: configuring a second harness silently
changes what `disco run` does, and the rule cannot be stated on the run options
without describing the whole project's state.

**Keep the fallback but warn.** A warning on the create path is read once and
then not at all, and the sandbox it warns about still exists and still has no
harness. The error is the same information delivered when it can still be acted
on.

**Make the fallback a project setting.** This is 0032 §3's rejected `Fallback`
flag wearing a different hat, and it is still the question
`Project.DefaultHarnessConfigID` already answers.

## Consequences

- `disco run` in a project with no default now fails until a harness is named or
  a default is set. That is the intended cost: it is one error at create in
  exchange for never silently getting a shell.
- Existing projects are unaffected in behaviour — a default that is set keeps
  resolving — but any script relying on the shell fallback needs `--harness
  shell` spelled out, which is what it always meant.
- A project whose seeded `shell` row is present but unconfigured no longer
  reaches the "not configured" refusal by way of the fallback; it reaches the
  new one, which names something the user can actually do.
