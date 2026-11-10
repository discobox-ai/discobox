# 26-11-10-203 — The console shows a box's files beside its workspace, and moves them by drag, drop and key

- **Status**: Accepted
- **Date**: 2026-11-10
- Accepted by the owner by merging #923, 2026-11-10
- **Issue**: [#920](https://github.com/discobox-ai/discobox/issues/920)
- **Relates to**: [26-10-22-499](26-10-22-499-a-discobox-has-a-files-api-that-acts-as-its-user.md) (the files API the panel reads and writes through),
  [26-11-04-684](26-11-04-684-a-lead-runs-child-commands-and-copies-files-through-the-files-api.md) (`discobox cp` over that API, whose copy the panel reuses),
  [26-10-16-535](26-10-16-535-a-pasted-image-is-carried-into-the-discobox-as-a-file.md) (a pasted local path while a pane has the keys).

## Context

Checked against `main` at `29c26c358ad1ec25d6a7707ee68933c981d288b2`:

- The console's workspace (`cli/internal/tui`) shows a box's terminals and,
  on its left, the workspace tree. Nothing in it shows the box's files; a
  person reaches them through a shell, `discobox cp`, or an agent.
- The files API (ADR 26-10-22-499) lists, stats, moves and copies paths in a
  box as its user, and imports and exports copy archives (`files/archive`).
  `discobox cp` (`copyOperands`, `cli/internal/cli/cp.go`) already copies
  local files and directories into a box and out of it over that API.
- A terminal has no drag and drop. The mouse reports presses, motion and
  releases inside the window only. A file dragged onto the terminal from the
  desktop arrives as a **paste of its local path**; most terminals send no
  motion while that drag crosses the window. Nothing can be dragged *out* of
  a terminal onto the desktop.
- While a pane has the keys, a pasted local image path is already carried
  into the box as a file (ADR 26-10-16-535); any other paste is the pane's.
- The box's listing says whether its tree is dirty and how large its diff is
  (`Sandbox.Git`, `Sandbox.Diff`), but nothing reports git's status per file.
- `sandboxCommandOutput` (`cli/internal/cli/sandbox_execs.go`) runs a short
  command in a box to its end and reads its output — the PR review's git
  lookups and the tools start check use it. It creates an exec and never
  deletes it. The sandbox agent removes an exec only on an explicit delete
  (`execs.Manager.Delete`, `sandbox-agent/execs/manager.go`); there is no
  pruning, so every captured command leaves its record, transcript and
  runtime files in the box for the box's life.

## Decision

### 1. A files panel down the right of the workspace

`leader F` (also the `filetree` command, and an entry in the box menu,
`leader .`) opens a panel of the box's files on the right of the workspace,
rooted at the primary terminal's working directory (falling back to `~`),
read one directory at a time through the files API. It is laid out like the
tree: its cells come off the window's width, it draws against its own hit
zones, and the mouse over it is answered before the left tree shifts
coordinates. What it shows of a box is that box's, so a parked box keeps its
open folders and cursor; whether the panel is up, and whether it has the
keys, is the window's. The panel has the leader's `F` and `h`; any other
leader key hands the keys back and reaches the focused pane as its chord.

### 2. A drag inside the window is a held press on a row

A press on a row starts a drag the window owns until release:

- released where it started: a click (open or close a folder; a double click
  pastes the path into the focused pane);
- released on a folder row: a **move** into it through the files API, a
  **copy** with Ctrl or Alt held. Neither ever replaces what is at the
  destination; a clash is an error, not an overwrite;
- released on a pane: the path, shell-quoted, is pasted into that pane.

A press while a drag is held, or motion with no button down, ends the drag
unfinished: its release happened outside the window, and finishing it would
move a file to wherever the next click lands.

### 3. A drop from the desktop is a paste, and only while the panel has the keys

While the panel has the keys, a paste whose every entry is an existing local
path (`clipimage.LocalPaths`, at most 200) is a drop: the files and
directories are copied into the folder under the panel's cursor through
`discobox cp -r`'s own copy, so a directory goes over as one copy archive.
Any other paste with the panel's keys copies nothing and says so on the
status line. While a pane has the keys,
a paste stays the pane's, ADR 26-10-16-535's image paths included.

### 4. A file leaves by a key

`d` on a row copies that file or directory out of the box into this
machine's downloads directory — `$XDG_DOWNLOAD_DIR` when it names one,
otherwise `~/Downloads` when it exists, otherwise the home directory — under
its own name, or `name (2).ext` and on when that is taken, never over
something already there. The name must be one `copyarchive.Name` accepts. The
copy is `discobox cp -r BOX:PATH LOCAL`'s.

### 5. Git's status is asked of the box, and asked again only when it may have moved

The panel draws git's per-file status over the tree: a changed file's letter
(git's own porcelain letter) at its row's end and its name in that change's
color, and a dot on each directory with a change beneath it, in the color of
the strongest. It is read in the box, through a new
`DataSource.RunCommand` (run a command to its end and capture stdout, stderr
and the exit code): `git rev-parse --show-toplevel` from the panel's root,
then `git status --porcelain=v1 -z --untracked-files=all` there. A root in no
repository draws nothing; a failed read keeps what was drawn.

It is read when the panel opens or takes the keys, on `r`, after every move,
copy or drop the panel makes, and otherwise only when the listing's git state
for the box (branch, commit, dirty, diffstat) has changed since the last
read — never on every poll.

### 6. A captured command deletes its exec

`sandboxCommandOutput` deletes the exec it created once the output is read,
or once the run fails, on a context detached from the caller's (which may be
what ended the run) with its own 30-second bound. A failed delete is not the
caller's error. This covers every caller: the panel's git reads, the PR
review's lookups and the tools start check. The exec's lifecycle events are
kept, as `Manager.Delete` keeps them; its record, transcript and runtime
files go.

## Alternatives rejected

- **Dropping at the pointer's position.** The panel could drop into the row
  under the last reported pointer. Most terminals report no motion while an
  outside drag crosses the window, so that position is stale, and a path
  pasted for an agent would be taken for a drop onto whatever row the
  pointer last crossed. The cursor is what the person last chose.
- **Treating any pasted local paths as a drop, whoever has the keys.** A
  path pasted into a pane is how a person hands an agent a file, and
  ADR 26-10-16-535 already gives image paths there a meaning. A drop would
  take that gesture away from every pane.
- **Keyboard cut and paste instead of a drag.** It works without a mouse,
  but a move between two visible folders is one gesture with a drag and
  four keys without. The keys (`y` copies a path, `p` pastes it into the
  pane) stay for what they are good at.
- **Overwriting on a move, copy, drop or download.** Every one of these is
  a single gesture with no confirmation, so a slip would silently replace a
  file. Refusing (inside the box) and numbering (on this machine) make the
  gesture safe to get wrong.
- **Asking where to save a download.** A prompt for a destination is more
  general, but the panel has no text field, and the downloads directory is
  where a person looks first for something just fetched. `discobox cp` remains for
  anywhere else.
- **Re-reading git on every poll.** One `git status` every two seconds per
  open panel is an exec, a process and a log in the box each time, for an
  answer that rarely changes; the listing's git state is a free signal for
  when it may have.
- **Reporting per-file git status from the box's own poll.** The sandbox
  agent already reads git for the listing's summary. Carrying every changed
  path through the pool to the server and every client would grow every
  box's posted state for one panel's benefit. Deferred: revisit if another
  client (the desktop app, a web console) needs per-file status, or if the
  signature gating proves too coarse in practice (it misses a second new
  untracked file until the next real change or `r`).
- **Leaving captured execs and pruning in the sandbox agent instead.** A
  retention policy in the agent would cover every client, but nothing yet
  says which execs are disposable: harness terminals and services are execs
  too, and outlive any age-based rule. The CLI knows its own captured
  commands are disposable the moment it has read them. Deferred: revisit if
  another client starts leaving short-lived execs behind.

## Consequences

- The console gains a panel and a `RunCommand` and `DownloadFile`,
  `TransferFile`, `UploadFiles` and `ListFiles` on its `DataSource`; the CLI
  implements them over the files API and `copyOperands`.
- A box no longer accumulates an exec per captured command; one that already
  has them keeps them until deleted.
- A captured command's transcript can no longer be read back after the
  fact. Nothing does today; its lifecycle events remain.
- A paste with the panel's keys is never typed anywhere: the panel has no
  field that takes text.
- The git overlay depends on `git` in the box; a box without it, or a root
  in no repository, draws the tree without marks.
