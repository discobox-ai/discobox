# cli — review rules

## Writing to the screen

- **Work that runs under a full-screen program may not choose a stream.** A
  create runs inside the console's window, and `discobox new` runs one step
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
  its status line, outside any pane's emulator.

- **The status line owns its row while it is up.** Anything else written to that
  stream goes through `print`, `note`, or `suspend`; written past it, a line
  comes out with the spinner glued to its front.

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
