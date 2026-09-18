# 0128 — The command that makes a discobox is `new`, and `run` is an alias

- **Status**: Accepted
- **Supersedes**: [0100](0100-the-prompt-is-a-flag-and-the-root-takes-no-words.md) §§3–4's command spelling — the command that keeps a trailing prompt is `discobox new`, and `new` is what the documents and the launcher's preview name, including §4's fallback for the one invocation that carries neither a prompt nor a run flag. What §3 and §4 decide stands: the named command takes trailing words and the bare one takes none, documents lead with `discobox -p '...'`, and no example spells a form that is gone — the rule this change obeyed. [0089](0089-the-bare-command-is-a-run-and-costs-unknown-command.md)'s `discobox run` spelling throughout; its §§1–3 stand, and this ADR is built on them.
- **Date**: 2026-09-17

## Context

`discobox run` was the name from the first CLI. It is accurate about what
happens — a harness runs against a prompt — and wrong about what the person is
doing, which is making a discobox. Every other top-level command names what it
acts on: `ls`, `rm`, `attach`, `apply`, `push`, `cp`. `run` names the verb the
harness performs, and the thing it produces appears nowhere in the name. The
command's own one-line help has always been "Launch prompt in new discobox".

It is also the most-typed word in the product, which is why
[ADR 0089](0089-the-bare-command-is-a-run-and-costs-unknown-command.md) §4 made
the bare `discobox` stand in for it — on the flags alone, since 0100 §2 took
§4's positional half. That cuts both ways: a name typed that often is in
scripts, CI, shell aliases and muscle memory.

[ADR 0119](0119-registered-servers-are-admin-remote.md) is the precedent for
renaming a command here, and it left no alias behind. What it moved was
`discobox servers` — configuration a person does once per machine. This is the
command a person runs many times a day.

## Decision

### 1. The command is `new`, and `run` stays a spelling of it

The command is `new`. `run` is the same command registered a second time and
hidden: same flags through `addRunFlags`, same body through `runPrompt`, same
help text. It is not deprecated and prints no warning. `n` and `r` are the short
forms of the two names; `r` predates this and stays for the same reason `run`
does. Removing either would be its own decision, and nothing here plans one.

Two registrations rather than one command with an alias, because cobra prints a
command's aliases in its help and offers no way to keep one out — see §2.

### 2. Only `new` is taught

Nothing the CLI prints says `run`. Help text, examples, error messages, the
`DESIGN.md` files, the in-box skill's command list, the launcher's options panel
and its live command preview all say `new`; the old spelling is absent from the
command list, from shell completion, and from `new`'s own help, which is what
the hidden second registration in §1 buys. Somebody who types `run` is not
corrected, because nothing they typed failed.

That includes the commit messages a create writes, which become `discobox new
workspace snapshot` and `discobox new empty base`
([ADR 0083](0083-a-repository-with-no-commits-is-uncommitted-work-on-an-empty-base.md)
§1 for the second). They are the one place the old name reached a person who
never typed it — `git log` inside the discobox — and nothing reads them back, so
changing the text costs nothing but the two spellings sitting side by side in
the history of boxes cut before and after this. Commits already written keep
their own text; they are history like any other commit.

### 3. Nothing behind the name changes

The same `App.runPrompt`, the same `addRunFlags` shared with the bare command
(0089 §3), the same trailing-prompt rule (0100 §3), the same `tui.RunRequest`
handed to the window. Go identifiers keep the run vocabulary, as 0119 kept
`serverRegistry` for `admin remote`: a run is still what the command starts,
and only the command is spelled for the person.

## Rejected

- **A hard rename, as 0119 did for `discobox servers`.** The cost is paid at
  once by every script, alias and CI job that says `run`, in exchange for a
  better spelling. Cobra's suggestion would not soften it either:
  `SuggestionsFor` needs a Levenshtein distance of 2 or less, and `run` to
  `new` is 3, so a broken script would fail without a pointer to the name that
  replaced it. 0119 could afford this because it moved a command run once per
  machine, by a person watching it.
- **A deprecation warning on `run`.** There is nothing to warn about — the
  alias is staying — and a run has nowhere to put the line. Everything written
  to stderr while a discobox is created shares the one status line the attach
  takes back, because a run that ends in a full-screen terminal may leave no
  rows standing above it (`cli/DESIGN.md`, ADR 0060).
- **`create`.** The API's own verb, and what `admin box create` uses. But the
  top level is for people, not the transport
  ([ADR 0112](0112-the-top-level-is-for-people-not-a-transport-diagnosis.md)),
  and there `new` is the shorter and commoner word.

## Consequences

- `discobox run` and everything written against it keeps working. `discobox
  new` is what the documents teach, and `discobox -p '...'` is still the form
  they lead with.
- The in-box skill ships with the sandbox-agent image, which a sandbox pins
  from the server's release
  ([ADR 0114](0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md)).
  A client older than this change can therefore be told to type `discobox new`
  and answer `unknown command "new"`, with no suggestion for the distance
  reason above. Accepted rather than fixed: the skill describes the release it
  ships with, and reaching it needs a server newer than its client — a
  registered remote ([ADR 0116](0116-a-discobox-address-names-a-server-and-a-discobox.md)
  §3, respelled by [ADR 0119](0119-registered-servers-are-admin-remote.md)),
  which is live for anyone with a second machine.
- ADRs 0089 and 0100 still say `discobox run`; read it as this command.
