# 0100 — The prompt is a flag, and the bare command takes no words

- **Status**: Accepted (§1's mechanism superseded by [0103](0103-a-global-flag-belongs-to-the-command-it-is-written-in-front-of.md))
- **Date**: 2026-09-07
- **Supersedes**: [0089](0089-the-bare-command-is-a-run-and-costs-unknown-command.md)
  §4's positional half and §5 ("the root command's `Args` becomes
  `cobra.ArbitraryArgs`"). §§1–3 stand and this ADR is built on them: `-p` is
  still the prompt, `--project` still has a long form only, and both spellings
  of a run still share `addRunFlags` and `App.runPrompt`.

## Context

ADR 0089 made `discobox <words>` a run, so `discobox fix the failing tests`
created a discobox prompted with those words. §5 recorded what that cost:
cobra's root-only unknown-command check runs from `legacyArgs`, which
`cmd.Find` only reaches when a command's `Args` is unset, so taking positional
words meant giving that check up. `discobox lst` stopped reporting `unknown
command "lst"` and started creating a discobox prompted "lst".

0089 accepted that trade on the grounds that no rule can tell a misspelled
subcommand from the first word of a prompt. The rule holds; the trade did not.
Every subcommand name is one typo away from a sandbox, and the failure is
silent in the direction that costs something: a misspelling does not fail, it
provisions a machine and runs an agent on a prompt nobody wrote. `discobox
version` had already had to be carved back out of the prompt space for exactly
this reason — `versionRequested` reserved the single word so that asking a CLI
its version did not spend a sandbox — which is the shape of a rule that is
answering the wrong question. There is no end to that list: every word anyone
might reasonably type expecting an answer has to be bought back one at a time.

What 0089 was actually after is in its own §1: `discobox -p 'fix the failing
tests'` working exactly as `discobox run -p 'fix the failing tests'` does,
without a second implementation. That part needed nothing from positional
words. The words were the part that looked natural in a README.

## Decision

### 1. The root command's `Args` goes back to unset

`legacyArgs` runs again, so a word that names no subcommand reports `unknown
command %q` with cobra's own suggestions. `discobox lst` says `unknown command
"lst"` and offers `ls`.

`legacyArgs` is not the whole guard, because cobra's `stripFlags` stops at a
`--` and the check never sees what follows one. `RunE` therefore refuses any
positional word that reaches it, naming `-p`. Without that, `discobox -d -- fix
the failing tests` creates a discobox with an empty prompt and says nothing
about the four words it dropped — the same silent failure this ADR is about,
reached by the spelling `run`'s own help teaches.

### 2. The prompt at the bare command is `-p`, and only `-p`

`runRequested` loses its `args` parameter and asks only whether any of run's
own flags was given; the root's `RunE` passes no positional prompt to
`runPrompt`. `discobox -p '...'` is a run, `discobox -H codex -d -p '...'` is
a run, `discobox` alone is still the launcher.

### 3. `discobox run` keeps its trailing prompt

`run`'s `Args` stays `cobra.ArbitraryArgs`. The hazard was never the words —
it was words with nothing in front of them to say what they are. After `run`,
a word is unambiguously part of a prompt, and `discobox run -- --flag-like
text` keeps working.

The alternative — removing positional prompts from `run` as well, so a prompt
is `-p` everywhere — was rejected. It breaks a form that has never been
ambiguous and never cost an error message, and the reason to prefer `-p` is
uniformity rather than anything that goes wrong. `run` de-emphasizes it in its
own help instead.

### 4. Documents lead with `discobox -p '...'`; no example is a dead spelling

The root's help, `run`'s examples, the README, the harness wrapper notes in
`harness/DESIGN.md` and the two `launch.sh` comments that repeat them, and the
launcher's command preview (`optionSet.command` in
`cli/internal/tui/options.go`) all show a spelling that still exists — the flag
form where the point is a prompt, `discobox run <words>` where the point is
that a shell splits them.

The preview drops `discobox run … -- words` for `discobox -p 'words' …`, which
is also the more faithful rendering of what Enter does: the composer holds one
piece of text and sends it as one argument, which is what `-p` is. It falls
back to naming `run` for the one case that carries neither a prompt nor a run
flag, since `discobox -C dir` on its own is the launcher.

### 5. `discobox version` becomes an ordinary subcommand

`versionRequested`'s carve-out is deleted. With words no longer a prompt,
`version` is a command like any other: hidden, because `--version` is the
spelling the help documents, and carrying an empty `PersistentPreRunE` so the
root's does not run — a leader key the environment spells wrong must not be
what stops somebody hearing which build they have.

## Consequences

- `discobox <misspelled subcommand>` errors again, with a suggestion, instead
  of creating a discobox prompted with the misspelling. `discobox -- <words>`
  errors too, pointing at `-p`.
- `discobox fix the failing tests` no longer works. `discobox -p 'fix the
  failing tests'` is the replacement, and `discobox run fix the failing tests`
  is unchanged.
- Nothing about `-p`, `--project`, or the shared run path changes; 0089 §§1–3
  are load-bearing here rather than reversed.
- No word has to be reserved out of the prompt space ever again, and the one
  that was (`version`) becomes a command instead of a special case.
- `cli/DESIGN.md` describes the resulting dispatch as current state; this ADR
  keeps the alternatives it was chosen over.
