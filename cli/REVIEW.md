# cli — review rules

## Writing to the screen

- **Work that runs under a full-screen program may not choose a stream.** A
  create runs inside the launcher's window, and `discobox run` runs one step
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

- **The status line owns its row while it is up.** Anything else written to that
  stream goes through `print`, `note`, or `suspend`; written past it, a line
  comes out with the spinner glued to its front.
