# 26-09-25-027 — Only `new` makes a discobox; the bare command takes none of its flags

- **Status**: Accepted
- **Date**: 2026-09-25
- **Supersedes**: [0089](0089-the-bare-command-is-a-run-and-costs-unknown-command.md)
  §3's sharing of `addRunFlags` with the root command and §4's `runRequested`;
  [0100](0100-the-prompt-is-a-flag-and-the-root-takes-no-words.md) §2 (the
  bare command's prompt is `-p`) and §4's rule that documents lead with
  `discobox -p '...'`, preview included; and
  [0133](0133-the-command-that-makes-a-discobox-is-new.md) §3's `addRunFlags`
  shared with the bare command, and its consequence that `discobox -p '...'`
  is still the form documents lead with. 0089 §§1–2 (`-p` is the prompt,
  `--project` has a long form only), 0100 §§1, 3 and 5, and 0133 §§1–2 and the
  rest of its §3 stand.

## Context

[ADR 0089](0089-the-bare-command-is-a-run-and-costs-unknown-command.md) made
the bare `discobox` stand in for a run whenever any of the run's flags was
given, so `discobox -H codex -d -p '...'` created a discobox without naming a
command. [ADR 0100](0100-the-prompt-is-a-flag-and-the-root-takes-no-words.md)
took the positional half of that back and made `-p` the only spelling of a
prompt there. What was left is two ways to make a discobox — `discobox new
...` and `discobox <new's flags>` — and machinery kept only for the second:

- every one of `new`'s flags on the root's help, beside the global ones;
- `runRequested`, deciding between the launcher and a run from which flags were
  set;
- the second half of `refuseRootOnlyArguments`, because `new`'s flags, being the
  root's local flags, were parsed in front of *any* subcommand under
  `TraverseChildren` (0103) and silently dropped there (`discobox -p '...' ls`);
- a launcher preview that had to fall back to naming `new` for the one request
  that carried neither a prompt nor a run flag.

## Decision

### 1. The bare command is the launcher or its help, never a run

The root registers none of `new`'s flags. `discobox` with nothing is the
launcher at a terminal and help elsewhere, as before; `discobox -p '...'`,
`discobox -d` and the rest are unknown flags. `new` (and its hidden `run`
spelling, 0133 §1) is the only command that makes a discobox.

`new` itself is unchanged: its prompt is the words after it, or `-p` for one
argument.

### 2. A flag of `new`'s at the root is pointed at `new`

Cobra's answer to `discobox -p '...'` is `unknown shorthand flag: 'p'`, which
says nothing about where the flag went. `Execute` adds one line when the
unknown flag is one `new` takes: `-p is new's flag, and only new makes a
discobox: discobox new -p`. A flag nobody takes gets cobra's answer alone.

### 3. Documents and the launcher spell `discobox new`

Help text, examples, the README, the in-box skill and the launcher's command
preview (`optionSet.command`) all name `new`. Examples lead with the trailing
prompt, `discobox new 'fix the failing tests'`; the preview spells the prompt
`-p`, since the composer sends one argument and trailing words would invent a
tokenization nobody typed.

A `--` in front of words at the root is still refused (0100 §1) and now answers
with `discobox new "<the words>"`.

## Rejected

- **Keep the shortcut.** It saves four characters on the most-typed command, at
  the cost of two spellings for one action, a root help that lists every flag
  of one subcommand, and a class of silently dropped flags that needed a check
  of its own. Naming the command is what every other action here already asks.
- **Keep it hidden, with a deprecation warning.** A run has nowhere to put the
  line: stderr belongs to the status line the attach takes back (0133,
  Rejected). §2's hint on the failure is the one place the message is read.

## Consequences

- `discobox -p '...'`, `discobox -d`, `discobox -H codex ...` and every script
  that spells a run without `new` fail, with §2's pointer to the spelling that
  works. `discobox new ...` and `discobox run ...` are unchanged.
- `-p` still cannot be `--project`'s shorthand: `--project` is persistent, so
  it would reach `new`, where `-p` is the prompt.
- `refuseRootOnlyArguments` keeps only the words a `--` hides from cobra's
  command scan; a run flag in front of a subcommand is now an ordinary unknown
  flag.
