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

## Listening on this machine

- **A local port this client is known by is one a box's forward must not
  take.** A box's listening ports are forwarded to the same number on this
  machine whenever the console or `discobox proxy` follows it, so a box that
  listens on that number while the listener is down is handed whatever its
  clients send — `discobox admin ahp`'s VS Code entry carries a token that
  drives every discobox. Such a port goes in `reservedLocalPorts`, which every
  `portforward.Options` takes. A forward Discobox does not make — VS Code's own,
  over Remote-SSH — is beyond it, and the user is told so.
- **A listener that takes a token in its URL listens on loopback only.** Plain
  `ws://` on another address hands the token to anyone on the network
  (`listenLoopback`).

## A client that goes

- **What a client's disconnect cleans up is that client's own.** The AHP host
  forgets drafts and exited shells when the client that made them goes
  (`Host.dropConn`, checking `owner`). Sweeping everything nobody watches at
  that moment removed another, still-connected client's draft, and its first
  message then went nowhere. Anything new that a disconnect forgets carries its
  owner and is checked against it.

## Reading an exec's attach

- **A call whose work outlives it runs under a context that outlives it.** An
  attach (`OpenExec`, `NewShell`, `NewTerminal`) lives exactly as long as the
  context it was opened with, and the console data source's `List` leaves a
  slow server's request running past the call to be drawn by the next poll. A
  request's context, or a timeout cancelled when the call returns, ends that
  work with it — `discobox admin ahp`'s screens died at once that way, and its
  listing dropped every slow server. Open under a long-lived context and bound
  only the open (`ahphost.Host.openContext`). A test fake that ignores its
  context cannot catch this; make it end with the context.

- **An exec's exit is the attach's error, not an interface.** `framedTerminal`
  ends with `io.EOF` for a clean exit and a `client.ExitError` for any other
  code; it is not a `tui.ExitReporter`. Code that waits for an exec to end
  matches those errors, and treats any other error as a lost attach to retry —
  a test whose fake stream never exits does not cover it.
