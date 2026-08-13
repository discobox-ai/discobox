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

// DiffStat is what a sandbox has changed: committed and uncommitted tracked
// changes both, as `git diff --shortstat` counts them, against the spawn
// commit — forwarded to the merge base with upstream once the sandbox has
// fetched, so pulled commits do not count as its changes.
//
// It arrives with the listing: the sandbox-agent resolves the base and
// reports the stat with the rest of its status, so
// no git runs anywhere on the list's behalf — the fetch-per-row this replaced
// woke every stopped sandbox just to draw a column. Known separates "nothing
// changed" from "nothing reported yet".
type DiffStat struct {
	Known bool

	Added   int
	Deleted int
	Files   int
}

// GitState is where a sandbox's work sits right now, as its own agent last
// reported it: the branch and commit the primary source's working tree is on,
// whether anything there is uncommitted, and whether the head commit has been
// landed on a host by an apply.
//
// It arrives with the listing — the agent pushes it through the control plane —
// so unlike the diffstat it costs nothing to show. Known separates "nothing
// has reported yet", where the row falls back to the position the sandbox was
// spawned at, from a report of any kind.
type GitState struct {
	Known bool

	Branch  string
	Commit  string // head, short
	Dirty   bool   // uncommitted content in the working tree
	Applied bool   // clean, and the head commit was the last one applied

	// AppliedCommit is the host-side commit the apply produced, short. A
	// cherry-pick onto a different parent mints a new SHA, so this — not the
	// sandbox head — is the one findable in the local repository. Set only
	// when Applied.
	AppliedCommit string
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

	// Git is where the work sits now, when the sandbox's agent has reported;
	// the spawn fields above are the fallback until it does.
	Git GitState

	Usage   Usage
	Diff    DiffStat
	Created time.Time // when the sandbox was created
	Upgrade bool      // running an image older than its harness config resolves to
	Message string    // error detail, shown when the row is under the cursor
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

// base is where the sandbox's work sits in git. Once its agent has reported,
// that is the position the tree is on right now; until then it is the commit
// the sandbox was spawned from. The mark on it is the state of the work,
// most losable first: a star for uncommitted content — reported dirt, or the
// snapshot carried in at create — an up arrow for committed work no apply
// has landed, and a check for a head commit an apply has landed, which is
// the state where nothing in the sandbox would be lost. An applied row shows
// the host-side commit its apply produced rather than the sandbox head: that
// is the SHA findable in the local repository.
func (s Sandbox) base() string {
	branch, commit := s.Branch, s.Commit
	if s.Git.Known {
		branch, commit = s.Git.Branch, s.Git.Commit
		if s.Git.Applied && !s.Git.Dirty && s.Git.AppliedCommit != "" {
			commit = s.Git.AppliedCommit
		}
	}
	if branch == "" && commit == "" {
		return ""
	}
	out := branch + "@" + commit
	switch {
	case s.dirty():
		out += "*"
	case s.Git.Applied:
		out += "✓"
	case s.ahead():
		out += "⇡"
	}
	return out
}

// dirty reports whether the sandbox holds uncommitted content: what its agent
// reports once there is a report, the snapshot it was created over until then.
func (s Sandbox) dirty() bool {
	if s.Git.Known {
		return s.Git.Dirty
	}
	return s.Dirty
}

// ahead reports whether the sandbox holds committed work that no apply has
// landed anywhere: its reported head has moved off the commit it was spawned
// from, and is not the last applied one. It needs both commits to answer — a
// sandbox with no recorded spawn commit cannot say its head has moved.
func (s Sandbox) ahead() bool {
	return s.Git.Known && !s.Git.Dirty && !s.Git.Applied &&
		s.Git.Commit != "" && s.Commit != "" && s.Git.Commit != s.Commit
}

// changes is the git column's mark spelled out: the one-word state of the
// work, for the column beside the position so the mark never has to be
// decoded. The two agree by construction — same predicates, same order.
func (s Sandbox) changes() string {
	switch {
	case s.dirty():
		return "dirty"
	case s.Git.Applied:
		return "applied"
	case s.ahead():
		return "ahead"
	case s.Git.Known:
		return "clean"
	default:
		return "-"
	}
}

// Exec is one exec session in a sandbox, as the workspace's tab strip needs
// it: enough to key a tab, title it, order it, and decide whether it should be
// attached at all.
type Exec struct {
	ID string

	// Command is what the exec is running, for titling its tab: the startup
	// command a harness terminal types into its shell when there is one, the
	// argv otherwise.
	Command []string

	// Harness is the harness slug when this exec is a harness terminal, and
	// empty for a plain shell.
	Harness string

	// Primary marks the sandbox's primary harness terminal, which is the
	// workspace's left half rather than ever a tab.
	Primary bool

	Tty bool

	// Live is whether the exec can be attached to: it exists and has not
	// exited, failed or been lost.
	Live bool

	// CreatedAt orders the tabs, oldest first, so they hold their places as
	// the listing changes around them.
	CreatedAt time.Time
}

// ExecPrimary is the virtual exec id of the sandbox's primary terminal. The
// sandbox resolves it to the current primary session — and relaunches one that
// has stopped — so the workspace never has to know its concrete id.
const ExecPrimary = "primary"

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
// drawn in a pane inside the window. Apply runs a command that wants the real
// terminal, and the window steps aside for it.
type Interaction string

const (
	InteractAttach Interaction = "attach"
	InteractShell  Interaction = "shell"
	InteractApply  Interaction = "apply"
)

// paneable reports whether an interaction can be drawn in the window from the
// discobox list.
//
// Attach and shell are terminals in the discobox, and open the workspace
// screen.
//
// Apply is not among them here, because the list can act on several discoboxes
// at once and a pane shows one. On the workspace screen, where there is only
// ever the one, it runs like the others.
func (a Interaction) paneable() bool {
	switch a {
	case InteractAttach, InteractShell:
		return true
	default:
		return false
	}
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

	// List is the project's sandboxes, newest-created first.
	List(ctx context.Context) ([]Sandbox, error)

	// Run creates a sandbox and delivers its source, which is what Enter does.
	Run(ctx context.Context, req RunRequest) (Sandbox, error)

	// Dirty reports whether the source directory has uncommitted work, so the
	// window can settle --include-dirty=auto before it creates anything.
	Dirty(ctx context.Context, source string) (bool, error)

	// Do runs a lifecycle verb against one sandbox.
	Do(ctx context.Context, verb Verb, sandboxID string) error

	// Rename gives one sandbox a new name. It is not a Verb: a verb is a word
	// the window already has, and this one needs the name typed first.
	Rename(ctx context.Context, sandboxID, name string) error

	// Interact runs a terminal-owning action against the given sandboxes, with
	// the real terminal's streams. The window is suspended for the duration.
	Interact(ctx context.Context, action Interaction, sandboxIDs []string, stdin io.Reader, stdout, stderr io.Writer) error

	// Open connects a terminal for one of the CLI's own commands — apply —
	// sized to the overlay it is going into. The discobox's terminals come
	// through OpenExec and NewShell instead.
	Open(ctx context.Context, action Interaction, sandboxID string, cols, rows int) (Terminal, error)

	// Execs is a snapshot of a sandbox's exec sessions. The workspace polls it
	// while it is open; it is a snapshot rather than a stream because the
	// control plane has no exec event stream yet — exec state lives on the
	// sandbox, proxied through — and the seam is shaped so a stream can
	// replace the poll without moving anything else.
	Execs(ctx context.Context, sandboxID string) ([]Exec, error)

	// OpenExec attaches to one existing exec session, sized for the pane it
	// goes into. execID may be ExecPrimary, which the sandbox resolves — and
	// revives — itself.
	OpenExec(ctx context.Context, sandboxID, execID string, cols, rows int) (Terminal, error)

	// NewShell creates, attaches and starts a fresh interactive shell exec,
	// returning its identity along with the terminal so the tab it becomes is
	// keyed by the same id the listing will report.
	NewShell(ctx context.Context, sandboxID string, cols, rows int) (Exec, Terminal, error)
}
