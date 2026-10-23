# cli — review rules

## Writing to the screen

- **Work that runs under a full-screen program may not choose a stream.** A
  create runs inside the console's window, and `discobox run` runs one step
  ahead of the terminal it is about to attach. Anything either of them calls
  that has something to say takes a `noteFunc` (`statusline.go`) and reports
  through it; it never takes a `*cobra.Command` in order to reach
  `ErrOrStderr()`, and never reaches for `os.Stderr`. Prefer passing a
  `context.Context` and the sink, so writing to the wrong screen is impossible
  rather than merely discouraged — see `writeProjectSSHConfig`.

- **A line a command prints is its answer; everything else is a note.** The
  discobox `-d` printed, the stanzas `admin ssh-config` emitted, "Created
  discobox …, attaching when it is ready" — those are what was asked for. Which
  key got enrolled, which `ssh_config` got rewritten, what a declared source
  resolved to: those are notes, and where they go is the caller's call
  (`printedNotes`, `statusLine.note`, the window's busy line).

- **Nothing may be left standing above a terminal handover.** A row printed
  before an attach is content the discobox's own full-screen program never drew
  and will not redraw, so everything it paints lands shifted. The status line
  exists for this — it rewrites one row and takes it back down — so narrate on
  it and clear before the stream changes hands.

- **Text a discobox wrote is escaped before it is printed.** A judge's reason,
  an argv, a hook payload, a prompt, and every ID or name that arrived beside
  them: anything composed inside a discobox goes through `terminalSafe`
  (`audit.go`) on its way to a terminal, and JSON carrying it through
  `writeTerminalSafeJSON`. It is display data, and printed raw a control
  sequence in it is an instruction to the reader's terminal. That includes a
  sandbox command's stderr folded into an error: the console puts the error on
  its status line, outside any pane's emulator. A repository's `.discobox`
  files count too, local or remote: a box can push the commit a create reads,
  and a JSON key or value decodes `\u001b` to a raw escape. Quote what they
  name (`sandboxcreate.quoteKeys`, `%q`) and escape the line it lands in.
  An API error the sandbox agent answered with is that text too: moving a
  step off an exec onto an API call (the files API, ADR 26-10-22-499) moves
  the box's words out of a stderr that was escaped into an error message that
  is not, unless the call's error is (`sandboxFileResponse`).

- **Escape the form of a line that is drawn.** A line kept in two forms —
  its text, and the tokens or spans it is drawn from — is escaped in the form
  the screen draws: the policy rendering's tokens and notes, not only its
  text, and a raw JSON view through `writeTerminalSafeJSON`, not an encoder
  that passes C1 controls and bidirectional overrides. Twice one form was
  escaped while the other reached the screen.
- **The status line owns its row while it is up.** Anything else written to that
  stream goes through `print`, `note`, or `suspend`; written past it, a line
  comes out with the spinner glued to its front.

## Flags

- **A flag's usage backquotes only the name of its value.** pflag reads the
  first backquoted text in a usage as that name and prints it after the flag
  (`--body ID`), so a command quoted that way makes a boolean flag look as if
  it took one: `--policy-as-asked discobox secret request get`. Quote a
  command in usage text with plain quotes.

## Command names and arguments

The rules are DESIGN.md's "Command Naming", and `command_conventions_test.go`
enforces the ones a tree walk can see. These are the ones it cannot:

- **A placeholder is checked against the resolver behind it.** `Use` saying
  `DISCOBOX` promises a name works. `trust ls DISCOBOX` called
  `resolveSandboxID`, which takes IDs only, and nothing caught it. Read which
  `resolve*` the command calls before accepting its `Use`.
- **A root command takes a discobox through `resolveListedSandbox`** (or
  `resolveSandboxReference` over a listing): the NAME `discobox ls` prints, an
  ID or a short ID, with a full ID skipping the listing. `resolveSandboxID` is
  the raw `admin box` forms' rule.
- **Which positional is which is decided by position and flags, never by
  what an argument looks like.** `run`'s first argument is SOURCE unless
  `--no-source` is given; a SOURCE that does not resolve is an error naming
  the form that works, not a guess that it was a prompt. Only zero arguments
  open a picker. `shell` and `tools ssh` still guess; a change that touches
  their argument handling should not copy them.
- **`-q` prints what the group's other verbs take back,** not necessarily an
  ID: a remote's name, a harness secret's env name, a skills entry the way
  `skills rm` takes it.
- **A rename keeps the old spelling as an alias, and teaches only the new
  one.** Skills (`.agents/skills`, `sandbox-agent/features/*/skills`,
  `access/skills`), bats tests, harness hints (`harness/*/image.json`,
  `configure.sh`), design docs and the argv the console builds
  (`tui_local.go`, `tui/options.go`) move with it. The `run` rename also
  missed, on a first pass:
  - format strings (`"%[1]s new"`, `"%s new %q"`);
  - the bare word in help prose ("By default new opens…");
  - the credential judge's prompt (`pkg/judge/recognized.go`), which describes
    the CLI's commands. It names every spelling, because approved purposes are
    worded in the old one.

  Leave what records history: ADRs, the recorded judge-eval cases
  (`test/judge-evals`), and audit argv in tests. Leave persisted
  configuration keys (`client.yaml`'s `new:`) too. Tests drive the canonical
  name. A blind replace of a word hits more than the command, so read every
  replaced line:
  - quoted words, such as dialog actions and fixtures;
  - format strings, as `%d new keys` became `%d run keys` in a test's failure
    message.

## Handing the sandbox's paths to host tools

- **A folder from the server's record is checked before a host tool gets it**
  (`sandboxSSHFolder`): absolute in the sandbox, not a Windows share, no
  control character. A host tool reads it on the far side of an SSH host
  (`ssh-remote+<host><folder>`), where `@attacker.example/x` is another host,
  and the record is not the client's alone: a box can create boxes, and an
  archive is anyone's (ADR 26-10-15-417). Anything new that puts a sandbox path
  into a host-side URL or command goes through the same check.
- **A URL joins a sandbox path to its host through `/`.** `tools.Remote`'s
  `{workdir.urlpath}` always begins with one, a drive path written
  `/C:/src/app`: a Windows path begins with its drive, and `C:\x@host` read as
  authority is another host.
- **A command printed for the user to paste is not guarded yet.** `apply`'s
  next steps put a sandbox directory into `git -C <dir>` unquoted. Quote it for
  the user's shell before adding another such command, and do not treat this
  section as covering them.

## Crossing into Windows from WSL

- **Ask Windows through `internal/wsl`, never with a copy of it.** Detection,
  `wslpath`, finding a Windows program (`cmd.exe`, `powershell.exe`) and
  reading a Windows variable each have a detail a copy forgets — a
  distribution with `appendWindowsPath=false`, a probe that never returns — and
  four copies had drifted into two meanings of "is this WSL" before they were
  folded into one.
- **WSL is the two variables WSL sets, never the kernel's release string.**
  That string says "microsoft" in every container on a WSL2 kernel (Docker
  Desktop, a wslc pool's boxes), where there is no Windows to reach, and the
  CLI's tests clear the variables to stay out of a developer's real Windows
  profile.

## Reading files a paste names

- **The box can write what gets pasted.** OSC 52 puts its text on the user's
  clipboard, and its screen is what the user copies from, so pasted text that
  leads to reading a file on this machine checks what is actually opened — an
  image by name and bytes (`clipimage.ReadFile`), on a local drive — and never
  trusts the text that named it. A `file://` link's fragment, a `.png` symlink
  and a Windows share path all led somewhere else.
