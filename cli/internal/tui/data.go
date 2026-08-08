package tui

import (
	"context"
	"io"
	"time"

	"github.com/obot-platform/discobox/termpane"
)

// State is a sandbox's user-facing lifecycle state, narrowed to the five the
// launcher draws. The server reports more of them — stopping, archiving,
// deleting — but a picker only has to answer "can I act on this", and the
// transitional states answer that the same way the state they are heading for
// does.
type State string

const (
	StateRunning  State = "running"
	StateStarting State = "starting"
	StateStopped  State = "stopped"
	StateArchived State = "archived"
	StateError    State = "error"
)

// Usage is what a sandbox is costing while it runs: cpu and memory as shares of
// their quota, and the disk as bytes.
//
// Nothing reports this yet. Known is false until something does, and the row
// draws dots rather than three zeroes, which would read as "idle" instead of
// "not measured". The column is here because the shape of the row is the thing
// being designed, and a column added later moves everything beside it.
type Usage struct {
	Known bool

	CPUPercent    int
	MemoryPercent int
	DiskBytes     int64
	DiskPercent   int
}

// DiffStat is what a sandbox has changed against the commit it was cut from.
//
// It is not part of a sandbox listing: the server does not track it, so it
// costs a git invocation inside the sandbox to find out. The list fetches it in
// the background for the rows on screen, and Known separates "nothing changed"
// from "not asked yet".
type DiffStat struct {
	Known bool

	Added   int
	Deleted int
	Files   int
}

// Sandbox is the row model: what a picker needs to tell one sandbox from
// another, plus what the actions need to know about whether they apply.
//
// The origin — folder, branch and commit — is what tells two sandboxes with
// similar names apart, so it is on the row rather than behind a key.
type Sandbox struct {
	ID    string
	Name  string
	State State

	Harness string

	// Folder is the client directory the sandbox was started from. It is not a
	// column on the row — it is what the header's dropdown filters on, so every
	// row on screen already shares it.
	Folder string
	Branch string
	Commit string // the commit it was spawned from, short
	Dirty  bool   // spawned from a snapshot on top of that commit

	Usage    Usage
	Diff     DiffStat
	LastUsed time.Time // last attach, exec or apply
	Upgrade  bool      // running an image older than its harness config resolves to
	Message  string    // error detail, shown when the row is under the cursor
}

// Session is what the window knows about where it is running: the project and
// directory every command it runs inherits, and the choices the run options
// panel offers.
//
// It is read once at startup. The project is a property of the session the way
// `disco -p` is a property of a shell, not something the window changes.
type Session struct {
	Project        string
	DefaultProject string

	// Directory is the project directory `disco run` would cut a sandbox from,
	// and Branch is what is checked out in it.
	Directory string
	Branch    string

	// Harnesses are the project's configured harness slugs, most useful first,
	// and DefaultHarness is the one an unset --harness resolves to.
	Harnesses      []string
	DefaultHarness string
}

func (s Sandbox) hasDiff() bool { return s.Diff.Known && s.Diff.Files > 0 }

func (s Sandbox) attachable() bool {
	return s.State == StateRunning || s.State == StateStarting || s.State == StateStopped
}

// up reports whether the sandbox is running anything, and so whether its usage
// figures would mean anything.
func (s Sandbox) up() bool {
	return s.State == StateRunning || s.State == StateStarting
}

// base is the commit the sandbox was spawned from. A star marks the ones
// carrying uncommitted work that was snapshotted on top of it.
func (s Sandbox) base() string {
	if s.Branch == "" && s.Commit == "" {
		return ""
	}
	out := s.Branch + "@" + s.Commit
	if s.Dirty {
		out += "*"
	}
	return out
}

// RunRequest is what Enter in the prompt asks for: `disco run`'s arguments, and
// nothing the command does not have.
type RunRequest struct {
	Prompt  string
	Harness string // empty is the project default

	// NoHarness asks for a sandbox with no agent in it, just a shell. It is a
	// different answer from an empty Harness, which takes the project default.
	NoHarness bool

	// IncludeDirty is `--include-dirty`: "", "true" or "false". Empty is auto,
	// which asks — and nothing in a full-screen window can ask, so the launcher
	// resolves it before the sandbox is created.
	IncludeDirty string

	Detach bool
	Env    []string
	Secret []string

	// Source is `-C DIR@REF`, the directory and ref the sandbox is cut from.
	Source string
}

// Verb is a lifecycle action that runs against the API and returns: no terminal
// is taken, so the window stays up and reports the outcome on its status line.
type Verb string

const (
	VerbStart     Verb = "start"
	VerbStop      Verb = "stop"
	VerbUpgrade   Verb = "upgrade"
	VerbArchive   Verb = "archive"
	VerbUnarchive Verb = "unarchive"
	VerbPurge     Verb = "purge"
)

// Interaction is an action that owns a terminal for as long as it runs.
//
// Two of them — attach and shell — are terminals in their own right, and are
// drawn in a pane inside the window. The rest run a command that wants the real
// terminal, a pager most of all, and the window steps aside for those.
type Interaction string

const (
	InteractAttach Interaction = "attach"
	InteractShell  Interaction = "shell"
	InteractDiff   Interaction = "diff"
	InteractApply  Interaction = "apply"
	InteractStatus Interaction = "status"
)

// paneable reports whether an interaction can be drawn in a pane from the
// discobox list.
//
// Attach and shell are terminals in the discobox. Diff and status are this
// CLI's own commands, given a terminal of their own so they can be drawn beside
// one — they read and print, so a pane is exactly where they belong.
//
// Apply is not among them here, because the list can act on several discoboxes
// at once and a pane shows one. On the pane screen, where there is only ever
// the one, it runs like the others; see Interaction.slotted.
func (a Interaction) paneable() bool {
	switch a {
	case InteractAttach, InteractShell, InteractDiff, InteractStatus:
		return true
	default:
		return false
	}
}

// slotted reports whether the interaction is one of the pane screen's two
// standing terminals rather than a command that runs and finishes.
//
// This is the whole shape of that screen: a spot for the harness and a spot for
// a shell, one of each, either of which may be empty and can be opened where it
// stands. Everything else — diff, status, apply — takes the screen for as long
// as it runs and gives it back, so the two terminals are still there, still
// running, still where they were.
func (a Interaction) slotted() bool {
	return a == InteractAttach || a == InteractShell
}

// holdsOnExit reports whether a pane should stay on screen after what was
// running in it finishes.
//
// A shell that exits is gone, and so is a harness session that ends: the pane
// has nothing left to show. A command that ran, printed and returned is the
// opposite — `disco status` on a clean tree is over in a moment, and a pane that
// vanished with it would be a screen you never got to read.
func (a Interaction) holdsOnExit() bool {
	return !a.slotted()
}

// TerminalConnectionState is what the transport underneath a pane is doing. A
// reconnect is invisible in the output — the stream simply carries on — so it is
// reported separately for the pane to say so.
type TerminalConnectionState int

const (
	TerminalReconnecting TerminalConnectionState = iota
	TerminalReconnected
)

// TerminalEvent is one such report.
type TerminalEvent struct {
	State TerminalConnectionState
}

// Terminal is a sandbox terminal the window draws itself: a stream termpane can
// render, plus the connection events that never appear in its output.
type Terminal interface {
	termpane.Stream

	// Events reports connection state changes. It is never closed while the
	// terminal is open, and a receive that nobody is waiting on is dropped
	// rather than blocking the transport.
	Events() <-chan TerminalEvent
}

// DataSource is everything the window needs from the outside. It is implemented
// once, in the cli package, over the same API client and code paths the
// non-interactive commands use: the launcher runs `disco`'s commands rather
// than a second implementation of them.
type DataSource interface {
	// Session is read once at startup, and is what the header and the run
	// options panel are drawn from.
	Session(ctx context.Context) (Session, error)

	// List is the project's sandboxes, most recently used first.
	List(ctx context.Context) ([]Sandbox, error)

	// DiffStat is what one sandbox has changed. It runs git inside the sandbox,
	// so the list only asks for the rows it is showing.
	DiffStat(ctx context.Context, sandboxID string) (DiffStat, error)

	// Run creates a sandbox and delivers its source, which is what Enter does.
	Run(ctx context.Context, req RunRequest) (Sandbox, error)

	// Dirty reports whether the source directory has uncommitted work, so the
	// window can settle --include-dirty=auto before it creates anything.
	Dirty(ctx context.Context, source string) (bool, error)

	// Do runs a lifecycle verb against one sandbox.
	Do(ctx context.Context, verb Verb, sandboxID string) error

	// Interact runs a terminal-owning action against the given sandboxes, with
	// the real terminal's streams. The window is suspended for the duration.
	Interact(ctx context.Context, action Interaction, sandboxIDs []string, stdin io.Reader, stdout, stderr io.Writer) error

	// Open connects a terminal the window will draw in a pane, sized to the
	// pane it is going into. Every interaction but a multi-discobox apply comes
	// through here.
	Open(ctx context.Context, action Interaction, sandboxID string, cols, rows int) (Terminal, error)
}
