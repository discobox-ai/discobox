# 26-11-03-982 — A host tool is handed the CLI, server and project to set the discobox up

- **Status**: Accepted
- **Date**: 2026-11-03

  Accepted by the owner by merging #496, 2026-11-03.

- **Extends**: [0125](0125-tools-are-declared-in-files-the-way-services-are.md)
  §4, whose table of what a host tool is handed gains three rows; the rest of
  0125, and 0142, stand.
- **Issue**: #495

## Context

ADR 0125 §4 hands a `runs: host` tool the discobox as an ssh host, a working
tree and a git URL: enough to open an editor over ssh. People also want a tool
that *sets the box up* from their own machine before opening something: put a
program the image lacks into the box, and carry their own `~/.config/<tool>`
in, so a terminal tool (tuicr, lazygit, fresh) opens with their settings in
every new box.

A host script can do that only by calling `discobox` back: `discobox cp` for
the config, `discobox shell` to install and to open. Today it is not told
which binary is running it, which server the box is on, or which project.
`discobox` on its PATH may be another build. The user's default server and
project may not be the box's: `discobox ls` lists several servers, and the
project is commonly given as the `default` alias.

## Decision

### 1. Three more values

A host tool is also handed:

| Placeholder | Environment | Value |
| --- | --- | --- |
| `{discobox.cli}` | `DISCOBOX_CLI` | the running CLI binary |
| `{discobox.server}` | `DISCOBOX_SERVER` | the box's server, as an address any CLI resolves back to that server |
| `{discobox.project}` | `DISCOBOX_PROJECT` | the box's project ID, resolved, never an alias |

`DISCOBOX_SERVER` and `DISCOBOX_PROJECT` are the names the CLI already
reads as the defaults of `--server` and `--project`. So
`"$DISCOBOX_CLI" shell|cp "$DISCOBOX_ID" …` from the script reaches the
same box with no flags, and nothing new is invented for a script to parse.

### 2. Config is carried; programs are fetched by the box

The user's machine may be macOS or Windows, and the box Linux, on another
architecture. A tool carries in only configuration. A program is downloaded by
the box itself, into its `~/.cache` (a cache volume shared by the user's
boxes in the pool, ADR 0094, so a version downloads once), and linked into
`~/.local/bin`. This is guidance, which `discobox tools --help` gives with a
worked example. The CLI enforces nothing.

A host tool never names a path in the box from this machine, so it is handed
no home directory and no bin path. It relies on three things the discobox
already guarantees:

- **What runs in the box resolves the box's own paths.** The install step is a
  script sent with `"$DISCOBOX_CLI" shell "$DISCOBOX_ID" -- sh -s`, and its
  `$HOME`, `uname -m` and `~/.cache` are the box's, expanded there.
- **`~/.local/bin` is on the box's PATH.** The platform's manifest layer
  declares `%HOME%/.local/bin` in PATH (`sandbox-agent/image.json`), and a
  `discobox shell` command runs with it. So the tool opens the program by
  name: `"$DISCOBOX_CLI" shell "$DISCOBOX_ID" -- tuicr`.
- **`discobox cp` resolves a relative path in a box from its user's home,** not
  from a source's working tree, while `discobox shell` starts in the working
  tree. So `"$DISCOBOX_CLI" cp -r ~/.config/tuicr "$DISCOBOX_ID:.config/tuicr"`
  lands in the box's `~/.config` whatever that home is called.

### 3. The help is the reference

`discobox tools --help` documents:
- the front matter;
- what a host tool is handed;
- the worked example;
- the traps a first attempt hits (an executable script, `cp`'s scp
  semantics, `~` expanded on the host, no terminal for an installer's
  prompt).

An agent writes a tool from the help alone, without reading this ADR. A
user-side skill (#493) builds on it.

## Alternatives rejected

- **A new kind of declaration, a "provisioner" run when a box is created.**
  A host tool already runs the user's own program, from the user's own
  directory, against one box (0125 §5's trust rule holds unchanged). It lacked
  only these three values. Running one automatically at create time is a
  separate question, which this does not decide.
- **Copying the host's binaries into the box.** Wrong OS or architecture
  whenever the user's machine is not a Linux box like the sandbox. It also
  redoes a large copy on every box, where the cache keeps one download.
- **Passing the raw `--server` value and the project as given.** A
  registered name, or the `default` alias, can resolve differently for the
  child CLI than it did for the parent. Then the script is aimed at another
  box than the one the tool was opened on.
- **Handing the tool the box's home directory, or a bin path
  (`DISCOBOX_HOME`, `DISCOBOX_BIN`).** The host has no use for either. Every
  path in the box is either expanded by a command that runs there, or relative
  to the home that `cp` already resolves from. A value the host would splice
  into a remote command is one more thing to quote, and it can only drift from
  what the box itself says. If a tool ever needs one, `"$DISCOBOX_CLI" shell
  "$DISCOBOX_ID" -- sh -c 'echo $HOME'` asks the box.
- **A sandbox-side helper that installs from a tool's `files:`.** `files:`
  is for configuration a sandbox tool carries in. A host tool can already run
  anything in the box through the CLI, and putting the install there keeps
  the box's OS and architecture the box's to decide.
