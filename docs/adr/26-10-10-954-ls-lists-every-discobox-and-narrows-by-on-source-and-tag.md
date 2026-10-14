# 26-10-10-954 — No command is scoped to a directory, and `ls` narrows by what its columns print

- **Status**: Accepted
- **Date**: 2026-10-10
- **Supersedes**: [0111](0111-the-origin-is-the-client-and-its-key-names-where-the-source-came-from.md)
  §3's `discobox ls` and `selectSandbox` candidates sending the two origin
  keys for `-C`, and `ls --all`; and [26-09-30-188](26-09-30-188-the-consoles-filters-open-on-everything-and-are-kept-per-directory.md)'s
  "`ls` is unchanged"; and [26-10-14-585](26-10-14-585-a-console-folder-is-a-github-repository-and-servers-are-not-sections.md)'s "`discobox ls` …
  stand" and "`ls` and `apply` … keep the key", for `ls`. The origin key,
  `apply`, and what the console's folder is stand.

## Context

`discobox ls` listed this machine's discoboxes for the current directory, and
`-a` listed the project. Since 26-09-30-188 the console opens on everything
and narrows by a server, a folder and tags, so the two listings of one project
disagreed by default, and the command line could narrow by tag alone.

## Decision

1. **`discobox ls` lists every discobox in the project**, on every server,
   whichever directory and machine created it, with the SOURCE column `--all`
   added. `-a`/`--all` is removed, not kept as a no-op.
2. **Its filters are its own columns**, and a discobox is listed when it
   passes each one given:
   - `--on NAME` — one server, by the name the SERVER column gives it. Only
     that server is asked.
   - `--source TEXT` — the SOURCE column's text, matched exactly: a directory,
     a repository URL, or `-` for the discoboxes with no source. Matched in the
     CLI, against what it prints.
   - `--tag KEY[=VALUE]`, repeated — unchanged.
3. **No command is narrowed to the directory it is run in.** Every command
   that takes a discobox by name, or picks one when none is named, reads the
   project's listing with no filter — the commands that act on a running
   discobox (`attach`, `shell`, `apply`, `push`, `proxy`, `cp`, `tools`) less
   the archived, which have no runtime; `rm`, `start`/`stop`/`restart`, `tag`
   and `rename` with them, since archiving needs no runtime and a name the
   listing shows must not come back as no such discobox. Which servers each
   asks is unchanged from 0116 §4: the pickers, `shell`, `cp` and `tools`
   every server; `rm`, `start`/`stop`/`restart`, `tag` and `rename` the
   primary, so a discobox on a registered server is reached with
   `--server <name>` in front of them. A name is matched across what was listed, and one that several
   discoboxes answer to is refused with their IDs. The picker's `a`, which
   widened a directory's list to the project's, is removed with the scope it
   widened, and every row says where its discobox came from.

## Alternatives rejected

- **`--folder DIR|URL`, the console's folder: the two origin keys 0111 §3 sent
  for `-C`.** Exact parity with the header's card, but a folder is a machine
  and a source, so it cannot name another machine's discoboxes, silently adds
  this machine's sourceless ones, and takes a spelling (`.`, a subdirectory,
  `owner/repo`) the listing never prints. A filter that is the column's own
  text can be copied off the screen and means the same on every machine.
- **Narrow on an explicitly typed `-C` and `--server`, as the console's opening
  view does (26-09-30-188 §3).** The console passes `--server` and `--clone`
  to every command it spawns, and scripts pass `--server` to choose an
  endpoint, so "typed" cannot be told from "inherited".
- **A local `--server` on `ls` shadowing the global one.** The same word would
  choose the endpoint in front of `ls` and filter after it.
- **Normalize `--source`** (relative paths, `~`, a URL with or without `.git`).
  Declined by the user: exact match, so what filters is what is printed.
- **Keep `--all` as a hidden no-op.** Beta; a removed flag fails loudly, and
  the only callers in the tree are skills updated with this change.
- **Keep the commands scoped to the directory and only widen `ls`.** Declined
  by the user: a discobox `ls` shows and `rm <name>` cannot find, because it
  was cut from another directory, is the disagreement this removes. (One on a
  registered server is still found by `rm` only with `--server`, as before.)
- **Narrow the first-argument guess of `shell`, `tools` and `cp`** to the
  discoboxes a person created, with a minimum length for an unprefixed short
  ID; and **make `apply`/`push` ask for `--dir` for a discobox another
  discobox created.** Both declined by the user for now; the risks they would
  close are recorded below.

## Consequences

- A bare `discobox attach` (or `shell`, `apply`, `push`) run where exactly one
  discobox exists in the project takes it, wherever it was cut from; with
  several it asks, where it used to take the directory's one. `apply` works
  against the directory the discobox recorded, not the one it is run in, and
  asks for `--dir` when the recorded host is not this machine's — a record,
  not a guard, for a discobox another discobox created (below).
- Names collide across directories more often than within one, so a name is
  refused as ambiguous where it used to resolve.
- The first argument of `shell`, `tools` and `cp` is now matched against
  every discobox. A short command word spelled in the ID alphabet (`ps`, `df`)
  can prefix-match a box's random ID, and a discobox that creates discoboxes
  can give one a command's name (`npm`), so the command runs in that box.
  Both were bounded by one directory's discoboxes before.
- `apply` and `push` trust the directory and host a discobox recorded, and a
  discobox that creates discoboxes writes those itself. A bare `apply`/`push`
  can now pick such a discobox from any directory, so its recorded directory
  can be any repository on the machine. Attach's source delivery already
  refuses a box-created discobox (26-10-10-545 §7); these do not.
- The SOURCE column and the picker's origin are text a discobox may have
  written, and are escaped before they are printed (`terminalSafe`).
- `discobox ls` in a checkout lists more than it did. The old listing — this
  machine's discoboxes for the directory, with its sourceless ones — has no
  single spelling; `--source <the repository root>` is its sourced part, on
  every machine that holds that path.
- `ls` and the console narrow differently: the console by folder — the
  origin key, or the GitHub repository a source resolved to
  ([26-10-14-585](26-10-14-585-a-console-folder-is-a-github-repository-and-servers-are-not-sections.md)) —
  and `ls` by source text. 26-10-14-585's "`ls` lists the origin key" is
  history once this lands: `ls` filters on no key.
