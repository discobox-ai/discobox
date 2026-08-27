# 0071. A tool session is an exec the launcher labeled

Status: Accepted

## Context

The launcher's workspace (`cli/internal/tui`) draws one discobox as the sessions
the server records: harness terminals on the left, shells on the right
([ADR 0054](0054-the-workspaces-columns-are-terminals-and-shells.md)). The
workspace mirrors the server rather than remembering what this window opened, so
a session started anywhere shows up on its own and reattaching redraws the same
screen anyone else's window would.

The tools picker (leader `o`) adds a third kind of session: `difftui` and the
`fresh` editor, both carried by the sandbox image, each run as one TTY exec in
the discobox's primary source directory and drawn as a window over the
workspace. That window outlives being looked at — putting it away leaves the
session running, and quitting the launcher entirely leaves it running too, so
the next attach has to find it again.

That last requirement is what forces the question. A tool session must be
distinguishable from a shell in `GET /execs`, or the poll draws a running
`difftui` as a stray shell tab and the picker offers to start a second one. And
it must say *which* tool it is, or a window with a diff and an editor open
cannot tell them apart.

Neither of the two answers the exec record already gives is that fact. The
sandbox tags harness terminals with `metadata.harnessId` because *it* resolved
the harness; it knows nothing about tools, and a plain exec with a command is
exactly what a tool session is to it.

ADR 0054 rejected exec metadata for the workspace's columns. This is the same
mechanism asked for a different fact, so the two have to be reconciled rather
than one quietly overruling the other.

## Decision

1. A tool session is a plain TTY exec created with the tool's command, carrying
   one metadata entry: `tool` = the tool's id (`diff`, `fresh`). No workdir is
   sent — an exec with no workdir already lands in the sandbox's primary source
   directory, which is where a diff and an editor belong.
2. The label is the launcher's, written at the wire
   (`cli/internal/cli.execToolMetadataKey`) and read back off the listing into
   `tui.Exec.Tool`. The sandbox stores it and hands it back unread.
3. A session with that label is neither a terminal nor a shell: it is taken out
   of the strip before the terminal/shell question is asked, so it wears no
   number and is never a tab.
4. Reattaching picks up every labeled session it finds and puts it away rather
   than showing it: attaching to a discobox should show you the discobox. The
   picker says which tools are running, and choosing one shows the pane that is
   already there.
5. Closing a tool ends the session — `DELETE /execs/{id}` — which is the one
   place the launcher ends a session rather than closing its own view of one.
   Putting it away ends nothing. They are different buttons for that reason.
6. Which tools exist and what they run is the launcher's table
   (`cli/internal/tui/tools.go`), not the server's. `vscode` is in it and has
   no command: it is a request that returns, and it is listed with the other
   two because it answers the same question.
7. A tool may declare **files** it carries (`ToolFile`). The copy lives on this
   machine, under `os.UserConfigDir()/discobox/tools/<tool>/<name>` — the
   config directory, not the state directory, because this is authored and is
   the only copy. It is created from the tool's `Default` the first time
   anything reads it, and edited in place in `$EDITOR` from the picker.
10. The local name and the delivered path are separate fields, and may differ.
    fresh's config is `config.jsonc` here and `~/.config/fresh/config.json`
    there: the tool dictates where it reads from, while the local extension is
    ours to pick and is how every editor decides how to color the file. The
    delivered copy carries a `// vim: set ft=jsonc :` modeline for the same
    reason, since its name is not ours to choose.
11. A file's destination may contain `{workspace}`, resolved *in the discobox*
    to its working directory encoded the way a per-project state directory
    names itself. It exists for state a tool keys on the project rather than on
    the user — fresh's trust decision — which cannot be a fixed path because
    only the discobox knows its own working directory.
12. fresh's workspace trust is recorded as trusted. A discobox is a new folder
    every time, so every one of them opens Restricted — no language servers, no
    environment activation — and nothing inside ever says otherwise.
8. `NewTool` puts those files into the discobox before starting the session,
   one exec per file, writing **only where nothing is there already**. The
   check and the write are the same step, inside the sandbox, so two windows
   opening the same tool cannot race. The content rides as its own argv
   element, so the script needs only `sh`, `printf` and `mkdir` — no encoding
   and no quoting to get wrong.
9. Delivery is create-only and one-way. The copy in a discobox belongs to the
   discobox; editing the local copy changes what the *next* discobox gets, not
   what any open one is using, and the window says so when an edit is saved.

## Consequences

- The tool id is now a cross-client contract: any window drawing a workspace has
  to read the same key, or it will draw someone else's diff as a shell. It is
  spelled once, at the boundary that speaks the wire.
- A tool session started outside the launcher — `discobox exec` running `difftui`
  by hand — carries no label and is a shell tab, correctly: nothing claims it,
  and nothing will reopen it as a tool.
- An id that no longer names a tool (an older window's, a tool since removed)
  attaches to nothing and stays out of the strip. It is invisible rather than
  misfiled, and its session keeps running until something ends it. Removing a
  tool from the table therefore strands any session of it, which is the cost of
  the table living on this side.
- Metadata was already stored durably by the sandbox agent and already returned
  by the listing, so nothing in the API, the control plane or the agent changed
  for this.
- A tool's config does not follow you to another machine, and a teammate opening
  fresh in your discobox gets the image's defaults rather than yours. That is
  the cost of the config being yours rather than the project's; the file is a
  real path on disk, so a dotfile manager can carry it if you want that.
- A discobox that already has the file keeps it forever, including one where the
  file was written by an earlier, worse default. Re-reading it means deleting the
  discobox's copy, which is a shell away and is not something a launch should do
  on its own.
- Trusting the source directory moves fresh's execution gate from "the folder"
  to "the discobox", which is where this system already draws it: the agent runs
  the repository's code in there by design, and an editor declining to start a
  language server is guarding the sandbox against the thing the sandbox exists
  to run. It is not a general endorsement of the repository — the decision is
  written per discobox, into that discobox, and dies with it.
- The `{workspace}` encoding is a second implementation of somebody else's
  private rule, in shell, and it fails silently: a wrong slug writes a file
  nothing ever reads. It is covered by tests that run the real script, and it is
  the price of reaching state a tool refuses to key on anything but the path.
- A tool whose files cannot be delivered does not start. That trades a rare hard
  failure for never silently coming up unconfigured, which for an editor you
  have configured is the failure you would not notice.

## Alternatives

**A naming convention on the command — treat any exec whose argv is `difftui`
as the diff tool.** Rejected: it makes the launcher's window layout depend on
what a program happens to be called, so `discobox exec difftui` typed in a shell
silently becomes a tool window, and a tool that takes arguments or is invoked
through a wrapper stops matching. The exec record is the place to say what a
session *is*, and a command is what it *runs*.

**A first-class exec kind in the API (`kind: tool`, `tool: diff`).** Rejected
for now: the sandbox cannot validate the value, would store and return it
unread, and would gain a field whose whole meaning lives in one client. That is
metadata with a schema change attached. It becomes the right answer the moment
anything other than this launcher has to act on toolness — the server scheduling
them, the agent restarting them — and this ADR should be superseded then.

**Client-side state, persisted per discobox on this machine.** Rejected for the
reason ADR 0054 gives: the workspace shows the discobox as the server has it.
Remembered state drifts the moment anything happens elsewhere, and it cannot
answer for a session this machine never opened — which is exactly the case that
matters here, since a tool session outlives the window that started it.

**A tool config as a project resource on the server, alongside harness config
files.** Rejected: it would follow you between machines and reach teammates,
which is real, but it puts "how I like my editor" into shared project state,
needs an API schema addition plus store and service work, and makes the control
plane know what a tool is — which §1–6 above exist to avoid. A per-user
preference is not a property of the project. If tool configs ever need to be
shared deliberately, that is a different feature and should be a different
record.

**A config checked into the repository** (`.disco/tools/fresh/config.json`),
delivered with the source like any other file. Rejected: it is the repository's
config rather than yours, every repository would need its own copy, and it would
land in diffs and reviews. The thing being configured is the person, not the
project.

**Overwriting the discobox's copy on every launch, so the local file is the
truth.** Rejected: a config edited inside a box — by you, or by the agent
working in it — would be silently reverted by the next launch, and an editor
that loses your settings when you reopen it is worse than one that ignores a
change you made elsewhere. Create-only makes the rule one sentence: the box
keeps what it has.

**Leaving trust to the one-time prompt in each discobox.** Rejected: it is one
prompt per discobox forever, and the answer is always the same because the
premise never changes — you made this box to work on this repository. A question
whose answer is structurally fixed is a question worth answering once.

**Why this is not the metadata ADR 0054 rejected.** That proposal put a
*layout* — which column a pane is drawn in — into the sandbox's durable state,
inventing a second answer to a question the exec record already answered. This
one records what the session *is*, a fact nothing else in the record carries,
and it is the only writer and the only reader. The layout follows from it rather
than being it.
