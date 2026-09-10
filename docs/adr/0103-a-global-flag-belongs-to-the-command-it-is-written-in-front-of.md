# 0103 — A global flag belongs to the command it is written in front of

- **Status**: Accepted
- **Date**: 2026-09-10
- **Supersedes**: [0100](0100-the-prompt-is-a-flag-and-the-root-takes-no-words.md)
  §1's mechanism — the root command's `Args` is set again, and cobra's
  `legacyArgs` no longer runs. 0100's decision is unchanged and is what this
  ADR keeps working: a word that names no subcommand still reports `unknown
  command` with a suggestion, and `discobox -- <words>` still errors pointing
  at `-p`. §§2–5 stand untouched.

## Context

`discobox cp` and `discobox tools ssh` hand their arguments to `scp` and `ssh`,
so they set `DisableFlagParsing`: after the command name every flag is the
other program's, and `-r`, `-o`, `-p`, `-L` mean what they mean there. Cobra's
default dispatch finds the command first and then hands it *every* flag,
wherever it stood, so a command that parses none of them silently dropped the
root's:

```
$ discobox --server discobox://d1-etqbsvr-… cp fail.log sbx_4t34pnm1v6dgnpj4:
do request: Get "http://discobox.local/…": dial unix /run/user/1002/discobox/server.sock: …
```

The endpoint was ignored and the copy went to the local socket. The same held
for `discobox --server X tools ssh`. `admin provider create` and `update` set
`DisableFlagParsing` too, for a different reason — their flags come from the
provider catalog and are not known until the server answers — and had already
paid for it with `consumeProviderCreateGlobalFlags`, a hand-written scan that
pulls the globals back out of their argv.

## Decision

### 1. The root parses the flags written in front of a command

`TraverseChildren` on the root. Cobra then parses each command's flags as it
walks the argument list, so `--server` in front of `cp` is the root's flag and
`-o` after `cp` is scp's.

The alternative was to do for `cp` and `tools ssh` what
`consumeProviderCreateGlobalFlags` does: scan their own argv for the globals.
It was rejected because it cannot be made correct there. `-o` is this CLI's
`--output` and one of scp's and ssh's options; `-p` is `--prompt` and scp's
preserve. Once the arguments are one flat list, nothing in it says which
program a flag was meant for — only where it stood does, and that is exactly
what the scan has thrown away. The provider commands get away with it because
nothing downstream of them is another program's flag table.

Passing the endpoint only through `DISCOBOX_SERVER` — what `cp`'s help used to
say — was rejected as well: every other command takes the flag, and a copy is
the one place a second server comes up most (`discobox cp box-a:/x box-b:/x`).

### 2. The root reports the word that names no command

`legacyArgs` runs from `Find`, which `TraverseChildren` replaces, so cobra's
own unknown-command check is no longer on the path. `rootArgs` is that check,
in the CLI rather than in cobra: `unknown command "lst"` with the suggestions
`SuggestionsFor` gives, and 0100 §1's `--` case answered with `-p`.

### 3. What is written in front of a command and means nothing there is refused

`refuseRootOnlyArguments`, from the root's `PersistentPreRunE`. Two things
reach it, both silent otherwise:

- **Words a `--` hid.** Cobra's scan for the command name does not stop at a
  `--` the way `stripFlags` did — it reads one as a flag awaiting a value,
  takes the word after it as that value, and keeps looking. So `discobox --
  please run the tests` dispatches to `run` from the middle of a sentence and
  creates a discobox prompted "the tests". The root having parsed a positional
  word while a subcommand was found is what gives it away; nothing else does
  that.
- **Run's own flags.** They are the root's *local* flags, so they are parsed
  wherever they stand, and in front of a subcommand they are parsed into a run
  that never happens: `discobox -p '…' ls` would list with the prompt dropped,
  and `discobox -p '…' run` would create a discobox with an empty prompt, since
  `run` has its own copy of those flags and nothing was written after the name.

Under the default dispatch cobra rejected both as unknown flags for the
subcommand. Neither is a case a user is likely to reach on purpose; both cost a
sandbox when they are reached, which is the failure 0100 exists to prevent.

## Consequences

- `discobox --server X cp …`, `discobox --project P tools ssh …` and every
  other global in front of a command now reach it. `DISCOBOX_SERVER` and
  `DISCOBOX_PROJECT` keep working and `cp`'s help offers both.
- **A subcommand's own flag must be written after it.** `discobox --wait admin
  server shutdown` was accepted before and is now the root's flag to parse,
  which it does not know. This is the price of the decision, and it is the
  order every example, every help text, and every `globalFlags()` child spawn
  already uses.
- `version` carries an empty `PersistentPreRunE` (0100 §5) and so reaches
  neither check in §3. Printing which build you have is not something a
  smuggled word can spoil, and an environment this cannot parse must not be
  what stops somebody hearing it.
- A command whose `Args` refuses the extra words says so in its own words
  before §3's check runs — `discobox -- make ls faster` reports them against
  `ls`. Loud rather than silent either way, which is what 0100 asked for.
- `consumeProviderCreateGlobalFlags` stays. `admin provider create` still
  cannot let cobra parse its flags, and the globals in front of it now work
  through §1 as well.
