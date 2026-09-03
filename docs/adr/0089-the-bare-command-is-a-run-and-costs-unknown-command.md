# 0089 — The bare command is a run, and costs "unknown command"

- **Status**: Accepted
- **Date**: 2026-09-03

## Context

`discobox run "fix the failing tests"` is the single most common thing this
CLI is asked to do, and it is also the one command whose name is a formality:
nothing else `discobox` is bare for takes a prompt, so typing `run` before the
prompt says nothing a reader could not infer from the prompt itself.

The launcher already treats the bare command this way at one remove — the
opening prompt in `discobox tui` is one field and an Enter, described in
`cli/internal/tui/DESIGN.md` as "the shape of the loop" (see the welcome
screen it opens on). This decision brings the non-interactive path to the same
place: `discobox -H codex -d -p "fix the failing tests"` should work exactly
as `discobox run -H codex -d -p "fix the failing tests"` does, without a
second implementation to keep in step with the first.

Two things stood in the way.

**`-p` is taken.** It is `--project`'s hidden shorthand (`root.go`). A run's
own prompt has no flag form today; it is always the words after the command,
which a shell has already split. Making the bare command take run's flags
means those flags reach a command with no subcommand in front of them to
absorb the prompt, so a flag-only run (`discobox -H codex -d`) needs a way to
say the prompt as a flag rather than as trailing words — the words are what
made `run` findable in the first place, and removing them removes the reason
to type `run`.

**cobra's root-only "unknown command" check.** `cmd.Find` calls `legacyArgs`
when a command's `Args` is unset, and `legacyArgs` reports `unknown command
%q` only for the command with no parent — the root, in this tree — when its
first word does not match a subcommand. (Nested commands never got this for
free: `discobox admin bogus` already fell through to admin's own help, ADR or
no ADR.) It is the only mechanism in the tree that distinguishes "a word that
should have been a subcommand" from "a word that is the start of a prompt",
and it lives on the exact command that is about to gain a prompt.

## Decision

### 1. `-p`/`--prompt` is added to `run`, and `-p` is taken from `--project`

`--project` keeps its long form only; `-p` becomes the run's prompt-as-one-
argument, registered once in `addRunFlags` (shared by `discobox run` and the
bare command — see §3) so the two spellings cannot drift apart. The words
after the command remain the primary way to give a prompt; `-p` exists because
the bare command has no other way to spell one, and it is available on
`discobox run` too for a caller who would rather quote the whole prompt than
let the shell split it.

The two internal callers that build a `-p PROJECT` argv by hand
(`tui/options.go`'s rendered command, `provider_create_dynamic.go`'s arg
parser) move to `--project`.

The alternative — a different shorthand for the prompt, such as `-P`, leaving
`--project` as `-p` — was rejected. `discobox -p '...'` is what someone
reaches for first; making it mean the project, silently, on the one command
where a mistyped flag creates a discobox rather than erroring, was judged the
worse of the two costs. Every script or alias that passes `-p PROJECT` is a
casualty of this either way, since scripts should already prefer
`--project` — the flag is hidden from `--help` (see `cli/DESIGN.md`) — for
exactly the readability reason that favors the long form there.

### 2. `--project`'s hidden hyphen-p is gone, not replaced

`admin` still exists for the multi-project case; nothing about the flag's
long form, its env var, or its default changes. Only the letter goes.

### 3. `addRunFlags` is shared, and `runPrompt` is shared

`run`'s `RunE` body becomes `App.runPrompt(cmd, opts, args)`, and its flag
registration becomes `addRunFlags(cmd, opts) *pflag.FlagSet`, returning the
set it added. The root command constructs its own `runCommandOptions`, calls
`addRunFlags` on itself, and its `RunE` calls `runPrompt` with the same
options and args whenever `runRequested` says to. `runWindowRequest` takes a
`*runCommandOptions` (it was already never copied usefully by value) so both
paths hand the launcher the identical request shape run does.

The alternative — a second, smaller flag set and RunE body for the bare
command, covering only what looked common — was rejected on the same grounds
`cli/DESIGN.md` already gives for run and the launcher sharing one creation
path: two implementations of "make a discobox from a prompt" drift, and the
second one is the one nobody remembers to update.

### 4. `runRequested` decides between a run and the launcher

```go
func runRequested(flags *pflag.FlagSet, args []string) bool {
    if len(args) > 0 {
        return true
    }
    given := false
    flags.VisitAll(func(flag *pflag.Flag) { given = given || flag.Changed })
    return given
}
```

Any positional word, or any run flag actually given, makes the bare command a
run; `discobox` with neither is the launcher exactly as before. `-d` alone
(`discobox -d`) is a run with an empty prompt, matching `discobox run -d`
today — this ADR does not add a rule requiring a non-empty prompt, since `run`
has never required one either.

### 5. The root command's `Args` becomes `cobra.ArbitraryArgs`

This is the cost `legacyArgs` needed disabling for §4 to see positional words
at all: `Args` must be non-nil for `cmd.Find` to skip the root-only
unknown-command check. `discobox lst` no longer reports `unknown command
"lst"` — it creates a discobox prompted "lst". Every other command's `Args`,
and cobra's own dispatch into named subcommands (`discobox admin bogus` still
reaches `admin`'s own help, as it always did) are unaffected; this is a
root-only mechanism cobra never extended to the rest of the tree.

The alternative — keeping the check by only forwarding to `runPrompt` when a
recognized run flag was given, and still erroring on a bare unrecognized word
— was rejected. It would make `discobox fix the failing tests` an error
(`fix` matches no subcommand and no flag was given) while `discobox -p "fix
the failing tests"` works, which defeats the point: the words after the
command are the natural way to give a prompt, not the fallback for the flag.
The whole feature exists so `discobox <what you want>` behaves like `discobox
run <what you want>`; carving out bare words specifically would leave the
shortcut covering the case nobody uses it for.

## Consequences

- `discobox <prompt words>` and `discobox <run flags>` both create a discobox,
  through the same `runPrompt`/`runWindowRequest` path `discobox run` uses.
- `-p`/`--project`'s shorthand is gone; any script or alias relying on
  `-p <project>` must move to `--project <project>`.
- `discobox <misspelled subcommand>` no longer errors; it opens a discobox
  prompted with the misspelling. There is no mechanical way to tell a typo
  from the first word of an intended prompt, so this is accepted rather than
  guarded against.
- `cli/DESIGN.md` describes the resulting dispatch and flag-sharing as current
  state; this ADR keeps the alternatives it was chosen over.
