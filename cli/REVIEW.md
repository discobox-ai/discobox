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
  sequence in it is an instruction to the reader's terminal.

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
- **A child process that names a server sets `DISCOBOX_TOKEN` outright.**
  Start its environment from `App.childCLIEnv`, never bare `os.Environ()`, and
  say which token it carries: the App's own for the ProxyCommand, harness
  configure and the console's own commands (`openLocalCommand`), none for a
  host tool, whose CLI is authorized by its transport.
  An App aimed at a registered server carries no token (`forServer`), so an
  inherited `DISCOBOX_TOKEN` was given for the primary, and beside a
  `DISCOBOX_SERVER` naming another server it sends the primary's bearer token
  there. The ProxyCommand had this right; the host script's environment, which
  started handing `DISCOBOX_SERVER` (ADR 26-11-03-982), first did not.
- **What the CLI reads back goes to a script, never to an editor.**
  `DISCOBOX_SERVER` and `DISCOBOX_PROJECT` are `--server` and `--project` to
  every `discobox` run with them, and a `.yaml` host tool is usually an editor
  whose terminals inherit its environment for the life of the window, so
  `Remote.Environ` is a script's alone (ADR 0125 §4); a `.yaml` tool takes the
  values as placeholders in its own args.
- **A file this CLI puts into a pool-shared volume, or tells anyone to fetch
  into one, is checked before it runs.** `~/.cache` is shared by every box in
  the pool that runs as the same sandbox uid (ADR 0094 §2 keys it on the uid
  alone, so on a shared pool that can be another person's box), so any of them
  can plant an executable there first. Code here that would run a file from it
  delivers it elsewhere instead (`deliverToolScript`), and help, docs or a skill
  that tell someone to download into it pin the file's sha256, check it, and run
  a checked copy in the box's own home, never the cached file or a link to it;
  an `-x` test proves nothing. The first `tools --help` worked example missed
  this (#513). It governs what this package delivers and recommends; the
  image's own cache volumes that hold programs (`~/.vscode-server`,
  `~/.rustup`, ...) are the image's decision, not this rule's.
- **The console runs a host tool with no terminal.**
  `apiDataSource.RunHostTool` gives it empty stdin and discards its output, so
  anything that documents a host tool's console key (`tools --help`, a skill)
  says it suits a tool that opens its own window; a terminal program runs
  through `discobox tools <id>`.
- **A backport rechecks the CLI it describes against its own line.** Help
  text, docs and tests cherry-picked from main name main's command shapes
  (`tools TOOL [DISCOBOX]`, `admin harness`), and a call site main deleted is
  one the cherry-pick never touches: 0.18's `openLocalCommand` kept a bare
  `os.Environ()` after #505's `childCLIEnv` change. Read every usage string
  and every sibling of a changed call against the release branch.
