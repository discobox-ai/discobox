# 26-10-23-600 — The CLI is spelled one way, `run` takes its source first, and only commits go in by default

- **Status**: Accepted. Supersedes in part:
  - [0133](0133-the-command-that-makes-a-discobox-is-new.md) §§1–2: the
    command is `run` again, and its other spellings are aliases.
  - [26-09-25-027](26-09-25-027-only-new-makes-a-discobox.md)'s spelling: read
    its `new` as `run`. Its decisions stand.
  - [0073](0073-a-directory-with-no-repository-is-copied-only-when-asked.md)
    §2, not copying as the default answer, and §3, `--include-dirty=false` as
    "start empty".
  - [0077](0077-declining-a-directory-copy-creates-a-discobox-with-no-source.md)
    §2, `--include-dirty=false` declining the copy. §1 stands: declining
    still means no source.
  - [0083](0083-a-repository-with-no-commits-is-uncommitted-work-on-an-empty-base.md)
    §3's "`--include-dirty` still answers ahead of time".
  - [26-10-20-596](26-10-20-596-the-console-keeps-one-memory-per-machine-and-the-last-source-leads.md)
    §3's `-C` half: the console takes no source. Its `--server` half stands.
  - Every ADR that spells `-C` (0077, 0096, 0100, 0111): read it as `run`'s
    SOURCE.
- **Date**: 2026-10-23

## Context

A review of the whole command tree (every command, argument and flag on one
page) found the same thing spelled several ways:

- `delete`, `rm`, `remove` and `revoke` for removing.
- `list` beside `ls`, and `ls` without `-q`.
- `harnesses` and `services` beside singular groups.
- `DISCOBOX` and `DISCOBOX_ID` for the same argument, and `HARNESS` and
  `HARNESS_CONFIG_ID` within one group.

Two placeholders also promised more than their resolver did: `trust ls
DISCOBOX` took an ID only, and providers did not resolve by name.

The command that makes a discobox had the most weight on it:

- **Its name was hidden.** It was `new`, with `run` hidden (ADR 0133), so
  the spelling scripts and habits use was never taught.
- **Its source was a global flag.** `-C`, a global flag every command
  carried, though only `run` and the console read it. A prompt typed
  without `-C` cut from wherever the shell happened to be.
- **Uncommitted work went in unless refused.** `auto` asked at a terminal,
  and carried the work everywhere else. So a scripted or agent-driven run
  quietly shipped whatever was half-edited, and an interactive one asked a
  question on every dirty run.

## Decision

### 1. One spelling per meaning

The rules are `cli/DESIGN.md`'s "Command Naming", and
`command_conventions_test.go` enforces what a tree walk can see:

- **Removing is `rm`**, with `delete` and `remove` as aliases, and `revoke`
  where it is the word for it.
- **Listing is `ls`**, with `list` as an alias, and every `ls` takes `-q`. The
  `-q` lines are what the group's other verbs take back: an ID, a remote's
  name, an env name. `admin audit ls` has no `-q` yet: it is a timeline of
  records rather than resources, and what its `-q` should print is #342's.
- **Groups are singular**, with the plural as an alias. `tools` stays plural
  for now, because it is a root command the in-box skills teach; whether it
  moves is #342's.
- **A placeholder says what resolves.** It is `DISCOBOX`, `POOL`, `HARNESS`
  and the like where a name works, and `X_ID` only where just an ID does.
  Resolvers that fell short were fixed, not the placeholders.
- **Short help is a sentence** starting with a capital letter.

Old spellings stay working as aliases and are taught nowhere.

### 2. The command is `run`, and `new`, `create`, `r` and `n` are aliases

It is one registration. 0133's hidden second one is removed, and help lists
every spelling: each is what someone reaching for "make a discobox" types.
Only `run` is taught. The in-box skill ships in the same bundle as the in-box
CLI, so the two always agree. The name alone is understood by every client
back to before 0133.

The credential judge's description of a create names `run`, and says `new`
and `create` are the same command, because purposes approved before this are
worded in the old spelling.

### 3. `run SOURCE [PROMPT...]`, and no `-C`

- **SOURCE is the first argument:** a directory (`.`), a URL or OWNER/REPO,
  with `@REF` and `//PATH`. `-C/--clone` is removed from the root.
- **Only zero arguments opens the source picker.** With nobody to ask,
  SOURCE is required.
- **Which argument is which is decided by position and `--no-source` alone,
  never by what an argument looks like.** `--no-source` takes SOURCE's
  place, and every argument is then the prompt.
- **A SOURCE that names nothing is refused**, with the reminder that SOURCE
  comes first.
- **`--json` requests carry `"source"`.**
- **The console takes no source from the command line.** Its own folder is
  the directory it runs in.

### 4. Only committed history goes in unless `--include-dirty` says so

- **`--include-dirty` is a plain flag, off by default, for `run` and its
  `--json`.** Work left behind is warned about, naming how many changes.
- **The console's create card still asks** (`auto`): it is a person at a
  window choosing per create.
- **A source with no commits is not the flag's to decide**, since there is
  no committed history to start from instead:
  - A directory in no repository is copied. At a terminal the copy question
    is still asked, with its size, and now leads with copying. Its answer is
    its own (`CopyDirectory`), not `IncludeDirty`'s.
  - A repository with no commits yet carries its files and is not asked
    about.
- **The shared create no longer asks** (`ConfirmIncludeDirty` is removed).
  Both frontends settle the answer first.

## Alternatives rejected

- **Keeping 0133's two registrations with `run` visible.** That hides `new`
  and `create`, which are meant to be seen, and keeps a second copy of the
  command alive only to stay out of help.
- **Keeping `-C` on the console only.** That would be two spellings of one
  idea. The console's folders and filters are how it narrows, and its own
  directory is where it runs.
- **A SOURCE value of `none`** in place of `--no-source`. It takes a word
  away from what a source can be called, to save a flag that already reads
  clearly.
- **Guessing whether the first argument is a source or a prompt.** That is
  exactly what makes `shell`'s argument handling surprising. A wrong guess
  cuts a box from somewhere unintended or sends a path as a prompt.
- **Refusing a non-repo directory without `--include-dirty`.** The flag is
  about work on top of a commit; here there is no commit, and an empty box is
  never what running against a directory asks for.
- **Making the console opt-in too.** It asks a person who is looking at the
  question, which is the case the old default was right for.

## Deferred

The rest of the review — flag types and clashes, the remaining commands that
guess at a discobox argument (`shell`, `tools ssh`), duplicated lifecycle
commands, hidden duplicates, and smaller items — is tracked in issue #342, and
each item is decided when it is taken up.

## Consequences

- Scripts that ran `discobox run 'prompt'` from a checkout must add `.`, and
  ones that passed `-C DIR` must move DIR to SOURCE. Both errors name the form
  that works.
- A script that relied on dirty work going in must add `--include-dirty`. The
  warning names what was left behind.
- `client.yaml`'s section is still `new:`, because it is persisted
  configuration.
