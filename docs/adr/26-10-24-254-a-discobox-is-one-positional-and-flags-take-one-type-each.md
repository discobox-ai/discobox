# 26-10-24-254 — A discobox is one positional, a command follows `--`, and each kind of flag value has one type

- **Status**: Accepted. Supersedes in part:
  - [0097](0097-a-discobox-address-is-versioned-and-names-no-transport.md)
    §7's hidden `admin iroh` and `admin iroh-id`, kept for
    [0095](0095-an-enrolled-iroh-id-is-a-managed-resource.md) §5's reason:
    they are removed. The rest of both stands.
  - [0100](0100-the-prompt-is-a-flag-and-the-root-takes-no-words.md) §5's
    "hidden": `version` is listed. The command stands.
  - [0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md)
    §5's command lines: `ls`, `hooks` and `execs` take `[DISCOBOX]`, `get`
    takes `DISCOBOX`, and the filters are `--discobox`. What each reads stands.
  - [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md)
    §4's `--json`: read it as `--from-json`.
  - [26-10-09-389](26-10-09-389-the-cli-reads-one-schema-checked-configuration-file.md)
    §2's `new.skills` and `new.userSkills`, and their spelling in
    [26-10-14-894](26-10-14-894-a-project-names-skills-repositories-every-discobox-is-created-with.md)
    and [26-10-15-664](26-10-15-664-a-source-is-named-one-way-everywhere.md):
    read them as `run.skills` and `run.userSkills`.
  - [26-10-23-600](26-10-23-600-the-cli-is-spelled-one-way-run-takes-its-source-and-only-commits-go-in.md)
    §1's `admin audit ls` with no `-q`, its "old spellings stay working as
    aliases" for flags (it stands for command names), and its consequence
    that `client.yaml`'s section stays `new:`.
- **Date**: 2026-10-24

## Context

[26-10-23-600](26-10-23-600-the-cli-is-spelled-one-way-run-takes-its-source-and-only-commits-go-in.md)
made the command tree spell each thing one way, and left the rest of the
review to issue #342: commands that guess whether an argument is a discobox,
`--discobox-id` beside a positional `DISCOBOX`, flag values of one kind parsed
several ways, flags that clash with a convention, and hidden duplicates. Each
item below was decided when it was taken up.

## Decision

### 1. A discobox is one positional, and a command follows `--`

Every command that acts on one discobox names it as `DISCOBOX`: the NAME
`discobox ls` prints, an ID, a short ID, or an address, resolved by one
function (`selectSandbox`) on every server. `--discobox-id` is gone. Only zero
arguments open the picker. An argument given is the discobox, and one that
names nothing is an error; nothing guesses from an argument's shape. A name
that is also a short ID of another discobox is refused, since a box writes
its own listed name. A full ID nothing lists is taken as it stands, on the
primary when several servers are known, so a deleted discobox's audit trail
can still be read.

A command that runs a command takes it after `--`:
`shell [DISCOBOX] [-- CMD...]`, `tools <tool> [DISCOBOX] [-- ARG...]`,
`admin exec create [DISCOBOX] [-- CMD...]`. More than one argument before the
`--` is refused with the line that works. A command with another positional
takes `DISCOBOX` first and requires it (`admin terminal screen DISCOBOX
TERMINAL_ID`). A filter over many discoboxes keeps a flag, `--discobox`.

### 2. Each kind of value has one type

- **A length of time** is `lifetime.Flag`: `--ttl`, `--grant-ttl`,
  `--max-grant-ttl`, `--expires-in` and the trust's `--ttl` take `5m`, `3d`,
  `2w`, `1mo`, and `forever` where it is allowed. `--since` reads the same
  words back from now, or an RFC 3339 time. Timeouts and poll intervals keep
  Go's durations: they are sub-second and nobody types a week into them.
- **A size** takes units, `8g` or `512m`, binary as `docker run --memory`
  reads them: `--memory` and `--storage` replace `--memory-bytes` and
  `--storage-bytes`.
- **Switching a thing off** is `--disabled[=false]`, on create and update.
- **A harness** is a slug, a name or an ID wherever `--harness` is taken, the
  server resolving the same three for `run -H` and an import.
  `admin box create --harness-config` is gone.
- **Emptying a list** an update replaces is `--clear-<flag>`, refused beside
  the flag it clears. `secret update --no-routes` is `--clear-route`, and an
  empty `--egress-allow ""` is refused for `--clear-egress-allow`.

### 3. Clashes resolved, and an old flag goes

A flag renamed or removed here keeps no alias: the old spelling stops working
in the same release, and everything that teaches it changes with it.

- `-s` is `--secret` alone; `tools --source` has no shorthand.
- `admin box export DISCOBOX_ID [FILE]` takes its file as `import FILE` does,
  so `-o` is only the output format.
- `admin terminal wait --idle`, since `-q`/`--quiet` is a bool everywhere else.
- `run --from-json`, since the request read from stdin is not the global
  `-o json`.
- `secret request approve --secret` takes a name or an ID, as every other
  `--secret` does.

### 4. Smaller shapes

- `pool unset-default` and `harness unset-default` take no argument: there is
  one default to clear.
- `admin iroh` and `admin iroh-id` are removed; nothing in the repository
  called them.
- `version` is listed beside `-v`/`--version`.
- `configure` answers to `config` alone.
- `admin audit ls -q` prints the record IDs `audit get` takes.
- `client.yaml`'s section is `run:`. A file that says `new:` is read as if it
  said `run:`, an edit renames the key in place, and a file naming both is
  refused.

## Alternatives rejected

- **Guessing whether the first argument names a discobox**, as `shell` and
  `tools ssh` did. A word that happens to be a discobox's name changed what
  ran, and a typo in a name ran the typo in a picked discobox.
- **An optional `DISCOBOX` before another positional, decided by count.** It
  makes one argument mean the discobox in one command and something else in
  its neighbor, which is what §1 is for ending.
- **`--no-<flag>` to empty a list.** It reads as a negated boolean, and
  `--clear-` says the one thing it does.
- **Dropping `admin exec create -i`.** #342 called it a no-op; it is what sends
  stdin to the command, and `-it` what puts the terminal in raw mode, so its
  help was fixed instead.
- **Renaming `client.yaml`'s section without reading the old one.** It is
  persisted configuration; every file written before would stop loading.
- **Keeping a renamed flag's old spelling as a hidden alias**, as
  26-10-23-600 §1 does for command names. A flag is typed in scripts and skills
  that are updated with the CLI, and a hidden alias is a second spelling that
  lives on untaught; several of these cannot be one anyway (`--discobox-id`
  beside a positional DISCOBOX, `--enable`/`--disable` beside `--disabled`).

## Deferred

- `tools` stays plural, the in-box skills teaching it: renamed when there is a
  reason beyond the convention.
- Whether `admin box` keeps `start`, `stop`, `restart`, `rm` and `ls` beside
  the root's: decided when one of them is to behave differently from its root
  counterpart, which is when keeping both starts to cost something.

## Consequences

- Scripts that wrote `discobox shell box cmd`, `tools git status` or
  `--discobox-id X` must write `shell box -- cmd`, `tools git -- status` and
  `X` as `DISCOBOX`. Each refusal names the form that works.
- `attach`, `apply`, `push`, `proxy` and `tools` resolve a name where they
  took only IDs before.
- Scripts using a renamed flag (`--json`, `--secret-id`, `--quiet`,
  `--no-routes`, `--memory-bytes`, `--harness-config`, `--enable`) fail with
  "unknown flag" until they use the new one.
