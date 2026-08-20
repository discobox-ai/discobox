package tui

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/termpane"
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

// Port is one TCP port a sandbox's own processes are listening on, and what it
// turned out to speak. The address it is bound on is not carried: a forward
// dials from inside the sandbox, where every reported port answers, so the
// number and the protocol are the whole of what is actionable.
type Port struct {
	Number   int
	Protocol string // http, https, tcp, or unknown while a probe has not answered
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

	// HasRuntime is the power axis, carried beside State because State
	// collapses it. The server keeps existence and power as two fields
	// (ADR 0034 §§1–2), and its displayState re-merges them error-first: a
	// latched ErrorMessage reads `error` however healthily the container is
	// running. That is the right thing to *draw* and the wrong thing to gate
	// on, so the row keeps the second axis.
	//
	// It is true once the pool agent has observed a container for this box —
	// running, stopped, or in transition. Absent is not `stopped`: it is a
	// sandbox no agent has ever reported on, which for an errored one means
	// the create never produced anything to report.
	HasRuntime bool

	// NameIsTitle marks a Name taken from the primary terminal's window title
	// rather than the sandbox's configured name. Rename edits the configured
	// name — which such a row is not showing — so it is disabled there.
	NameIsTitle bool

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

	// Ports are what the sandbox is serving right now, ordered by number.
	Ports []Port

	Usage   Usage
	Diff    DiffStat
	Created time.Time // when the sandbox was created
	Upgrade bool      // running an image older than its harness config resolves to
	Message string    // error detail, shown when the row is under the cursor
}

// Session is what the window knows about where it is running: the project and
// the directory every command it runs inherits.
//
// It is read once at startup. The project is a property of the session the way
// `discobox -p` is a property of a shell, not something the window changes. What
// the run options offer as a harness is not here — that is the harnesses, which
// are read on their own and change while the window is up.
type Session struct {
	Project        string
	DefaultProject string

	// Directory is the project directory `discobox run` would cut a sandbox from,
	// and Branch is what is checked out in it.
	Directory string
	Branch    string

	// Draft is the prompt that was left unsent in Directory when a window was
	// last open on it, and is what the composer opens holding. See
	// DataSource.SaveDraft.
	Draft string
}

// HarnessState is what a harness is set to, and so whether a discobox can be
// run on it. It is the one thing the harnesses screen is really about: a
// harness that has never been through its own setup has no credentials to work
// with.
type HarnessState string

const (
	// HarnessEnabled has been through its configure flow and can be run.
	HarnessEnabled HarnessState = "enabled"
	// HarnessDisabled has not, or has been taken back out of use.
	HarnessDisabled HarnessState = "disabled"
	// HarnessFailed tried and did not finish. The reason is on the harness.
	HarnessFailed HarnessState = "failed"
)

// Harness is one of the project's harness configs, as the harnesses screen
// draws it.
//
// It carries what the row needs, what the config card shows, and what decides
// which actions apply. The secrets are the environment variables the image
// declares and nothing more: which secret is bound to each costs a request of
// its own, so the card asks for it when it is opened. See
// DataSource.HarnessSecrets.
type Harness struct {
	ID    string
	Name  string
	Slug  string
	State HarnessState

	// Default is the harness a discobox is created on when nothing says otherwise.
	Default bool
	// BuiltIn harnesses come with the server rather than being registered by hand.
	BuiltIn bool
	// Shell is the reserved `shell` harness: a plain login shell, not a coding
	// harness. It runs like any other and is chosen like any other, but it is
	// not offered as a project's default, since defaulting to it is the same as
	// having no coding harness at all.
	Shell bool
	// Configurable is whether the image declares a setup command to run. The
	// reserved `shell` built-in declares none — it is a login shell with no
	// credentials to collect — and neither enabling nor disabling applies to
	// one, since the server refuses both.
	Configurable bool
	// Error is why the configure flow did not finish, when it did not.
	Error string

	Image  string
	Digest string

	Run      []string
	Relaunch []string

	Secrets []HarnessSecret
	Files   []HarnessFile

	Updated time.Time
}

// HarnessSecret is one environment variable a harness runs with.
//
// In a listing only the declaration is filled in: the name, whether it is
// required, and the group it is one of. HarnessSecrets fills in the rest —
// which of the project's secrets is actually bound to it — including the
// bindings the image never declared, which are the ones bound by hand.
type HarnessSecret struct {
	Name     string
	Required bool
	// OneOf names a group of alternatives, of which the harness needs one.
	OneOf string
	// Declared is false for a binding the image never asked for.
	Declared bool

	// SecretID is the project secret bound to this variable, empty when nothing
	// is. The rest describes it, and is filled in only when the secret is one
	// this project can see.
	SecretID   string
	SecretType string
	SecretName string
	Anonymous  bool
}

// HarnessFile is one file a harness carries into every discobox it runs in.
type HarnessFile struct {
	Path    string
	Content string

	// Configured marks a file the configure flow wrote. It overlays the
	// image-declared file of the same path, which is why it is listed first.
	Configured bool
	CreateOnly bool
	Template   bool
}

// ServiceVerb is a lifecycle action against one of a discobox's declared
// services. Like a HarnessVerb it runs against the API and returns, leaving the
// window up: a service is not something you sit in front of, so acting on one
// never takes the screen.
type ServiceVerb string

const (
	ServiceStart   ServiceVerb = "start"
	ServiceStop    ServiceVerb = "stop"
	ServiceRestart ServiceVerb = "restart"
)

// done is what the status line says once the verb has run.
func (v ServiceVerb) done(name string) string {
	switch v {
	case ServiceStart:
		return "started " + name
	case ServiceStop:
		return "stopped " + name
	default:
		return "restarted " + name
	}
}

// HarnessVerb is a harness action that runs against the API and returns,
// leaving the window up — the counterpart of Verb for the harnesses screen.
// Enabling is not among them: it is an interactive flow that owns the terminal.
type HarnessVerb string

const (
	HarnessDisable    HarnessVerb = "disable"
	HarnessSetDefault HarnessVerb = "set default"
)

// done is what the status line says once the verb has run.
func (v HarnessVerb) done(name string) string {
	if v == HarnessSetDefault {
		return name + " is now the default"
	}
	return "disabled " + name
}

// displayName is what the harness is called on screen: its name, the slug it is
// run by, or failing both the id, which is the one thing it always has.
func (h Harness) displayName() string {
	if name := strings.TrimSpace(h.Name); name != "" {
		return name
	}
	if slug := strings.TrimSpace(h.Slug); slug != "" {
		return slug
	}
	return h.ID
}

// flagName is what `discobox run --harness` takes for this harness.
func (h Harness) flagName() string {
	if slug := strings.TrimSpace(h.Slug); slug != "" {
		return slug
	}
	return strings.TrimSpace(h.Name)
}

func (s Sandbox) hasDiff() bool { return s.Diff.Known && s.Diff.Files > 0 }

// attachable reports whether there is a container to attach to. It asks the
// power axis, not the lifecycle one, because that is the question: the pool
// agent starts a stopped sandbox on demand when the attach arrives (ADR 0017
// §12), so anything it has observed a container for can be joined.
//
// The states are consulted only for the two answers power cannot give.
// Archived has no container by intent, whatever a stale report last said
// (ADR 0022 §5). Starting is a sandbox mid-create whose agent has not reported
// yet, and the attach waits for it rather than being refused by it (ADR 0039
// tier 1).
//
// Error is deliberately not on that list. A settled failure latches on the row
// and nothing clears it without new intent (ADR 0017 §4), so an errored box may
// have a perfectly good container — a failure on a later generation, or a
// transient one — and refusing it made archive/unarchive the only way to reach
// work that was never unreachable. The x and the reason still show; they just
// no longer bar the door. An error with no container is the case that is
// genuinely stuck, and it is the one this still refuses.
//
// Spelling this as a list of states is what ADR 0017 §4 warns against — the
// wedge it describes came from a guard written `State == "stopped"` when it
// meant "is anything relying on this container".
func (s Sandbox) attachable() bool {
	if s.State == StateArchived {
		return false
	}
	return s.HasRuntime || s.State == StateStarting
}

// repairable reports whether repair is offered for this box.
//
// It is the counterpart of attachable, and asks the same two axes for the
// opposite answer: a box with a latched error, or one nothing has ever reported
// a container for, is the wedge repair exists for (ADR 0035) — the first may be
// broken behind a container that looks fine, the second never got one built.
// Everything else is working, and repair is a rebuild, not a refresh.
//
// Two states answer for themselves, as they do in attachable. Archived has no
// container by intent, and unarchive is what brings it back; the server refuses
// repair on it with the same reasoning (ADR 0035). Starting is a create still
// in flight whose agent has not reported yet — its missing container is the
// operation running, not a wedge, and tearing it down is the opposite of
// waiting for it (ADR 0039 tier 1).
func (s Sandbox) repairable() bool {
	switch s.State {
	case StateArchived, StateStarting:
		return false
	}
	return s.State == StateError || !s.HasRuntime
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
	// empty for a plain shell. It is also which column the workspace draws the
	// session in: a harness terminal is a terminal and goes on the left, and a
	// shell goes on the right. See terminalExec.
	Harness string

	// Primary marks the sandbox's primary harness terminal, which is the first
	// of the workspace's left column and the session the screen is a view
	// onto.
	Primary bool

	// Service is the id of the declared service this exec runs, empty for
	// every session that is not one. A service is an exec the sandbox started
	// from the repository's `.discobox/services` (ADR 0063), and it reaches the
	// workspace through this same listing rather than a poll of its own.
	Service string
	// ServiceName is the service's display name, which is what its tab is
	// called. It rides along on the exec record so the tab strip needs nothing
	// but this listing to draw itself.
	ServiceName string

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

// RunRequest is what Enter in the prompt asks for: `discobox run`'s arguments, and
// nothing the command does not have.
type RunRequest struct {
	Prompt  string
	Harness string // empty is the project default

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
	VerbRepair    Verb = "repair"
	VerbArchive   Verb = "archive"
	VerbUnarchive Verb = "unarchive"
	VerbPurge     Verb = "purge"
)

// Interaction is an action that owns a terminal for as long as it runs. Every
// one of them is drawn in the window: attach and shell are terminals in the
// discobox and open the workspace screen onto them, and apply is a command of
// this CLI's own, drawn in a pane over whatever it was started from.
//
// Each takes exactly one discobox, which is what a pane shows.
type Interaction string

const (
	InteractAttach Interaction = "attach"
	InteractShell  Interaction = "shell"
	InteractApply  Interaction = "apply"
	// InteractTerminal is another of the discobox's own terminals, opened
	// beside the primary. It is not one of the list's actions — the list opens
	// a discobox, and which terminal of it you want is a question you can only
	// have once you are looking at the workspace — so it names the workspace's
	// leader-c pane rather than a key anything dispatches on.
	InteractTerminal Interaction = "terminal"
	// InteractService is a pane onto one of the discobox's declared services.
	// Nothing opens one: services arrive in the exec listing already running,
	// so this names what such a pane is rather than an action that creates it.
	InteractService Interaction = "service"
)

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

// ExitReporter is a Terminal that ran a command and can say how it ended.
//
// Only some can. A command this machine ran has a result, and a pane that read
// "finished" over a failed one would be a screen agreeing with itself while the
// output above it says otherwise. A session inside a discobox has no such
// answer: it ended because the shell in it ended, which is not a verdict on
// anything.
type ExitReporter interface {
	// ExitStatus is the command's exit code, and whether it has ended.
	ExitStatus() (code int, done bool)
}

// Binding is one sandbox port reachable at a local one, while a Forward is
// open. Local is what a browser on this machine connects to; Port is what the
// sandbox is serving on inside itself.
type Binding struct {
	Port  int
	Local int
}

// Forward is a running port forward onto one sandbox: every port it announces,
// held open at a local port for as long as the workspace showing it is.
//
// The window does not drive it. It has no address to dial with — a Port drops
// the bind address for the reason portsText gives — and nothing to decide: the
// set follows what the sandbox announces, which is the same thing the header is
// already drawn from. So the seam is "start one, draw what it bound, close it".
type Forward interface {
	// Bindings is what is forwarded right now, in sandbox-port order. It is
	// read while drawing, so it must be safe to call from the render.
	Bindings() []Binding

	// Events wakes the window when Bindings would answer differently. It
	// carries nothing: the window redraws from Bindings, and a per-connection
	// event has nothing for it to say. Closed when the forward is.
	Events() <-chan struct{}

	io.Closer
}

// DataSource is everything the window needs from the outside. It is implemented
// once, in the cli package, over the same API client and code paths the
// non-interactive commands use: the launcher runs `discobox`'s commands rather
// than a second implementation of them.
type DataSource interface {
	// Session is read once at startup, and is what the header and the run
	// options panel are drawn from.
	Session(ctx context.Context) (Session, error)

	// SaveDraft records the prompt as it stands against the folder it is being
	// typed in, so that closing the window mid-sentence does not throw the
	// sentence away: Session hands it back the next time a window opens on
	// that folder. An empty prompt drops the draft — there is nothing left to
	// come back to — and a folder is required, since a draft nothing can be
	// keyed by is one nothing can return.
	SaveDraft(ctx context.Context, folder, prompt string) error

	// MarkWelcomed records that the project has shown its introduction, so no
	// window on it opens on the welcome again. It is the project that
	// remembers, not this machine: see welcome.go.
	MarkWelcomed(ctx context.Context) error

	// List is the project's sandboxes, newest-created first.
	List(ctx context.Context) ([]Sandbox, error)

	// Run creates a sandbox and delivers its source, which is what Enter does.
	// It reports each step it passes through — the steps are this client's own
	// work, so nothing else can say which one is underway — and the window puts
	// them where it says what it is busy with.
	Run(ctx context.Context, req RunRequest, report func(string)) (Sandbox, error)

	// WatchProvisioning reports what a sandbox that is not usable yet is being
	// made to do, until ctx ends. It blocks, and is meant to run beside a wait
	// rather than instead of one: an attach blocks until every tier reports
	// ready, and this is the only thing that can say what it is blocked on.
	//
	// It reports nothing for a sandbox with nothing left to provision, so a
	// window that starts one against a running discobox draws no line at all.
	WatchProvisioning(ctx context.Context, sandboxID string, report func(string))

	// Dirty reports whether the source directory has uncommitted work, so the
	// window can settle --include-dirty=auto before it creates anything.
	Dirty(ctx context.Context, source string) (bool, error)

	// Do runs a lifecycle verb against one sandbox.
	Do(ctx context.Context, verb Verb, sandboxID string) error

	// Rename gives one sandbox a new name. It is not a Verb: a verb is a word
	// the window already has, and this one needs the name typed first.
	Rename(ctx context.Context, sandboxID, name string) error

	// OpenEditor opens one sandbox in VS Code, in a window of its own.
	//
	// It is neither a Verb nor an Interaction: it changes nothing about the
	// sandbox, and it takes no terminal — the editor is a separate program in a
	// separate window, and this returns as soon as it has been handed the
	// sandbox. The window stays exactly where it was, which is the point: the
	// terminal and the editor are two views of one sandbox, open at once.
	OpenEditor(ctx context.Context, sandboxID string) error

	// Harnesses is the project's harnesses, oldest first, which is the order
	// they were registered in. It is read at startup as well as by the harnesses
	// screen: the run options offer the harnesses it reports, so enabling one
	// makes it selectable without reopening the window.
	Harnesses(ctx context.Context) ([]Harness, error)

	// HarnessSecrets is one harness's environment variables with the project
	// secret bound to each, for the config card. It is a request of its own, so it
	// is made when the card is opened rather than for every row of a listing.
	HarnessSecrets(ctx context.Context, harnessID string) ([]HarnessSecret, error)

	// DoHarness runs a lifecycle verb against one harness.
	DoHarness(ctx context.Context, verb HarnessVerb, harnessID string) error

	// ConfigureHarness runs the harness's own interactive setup with the real
	// terminal's streams. The window is suspended for the duration: the flow
	// asks questions and the harness draws its own screen to ask them on.
	ConfigureHarness(ctx context.Context, harnessID string, stdin io.Reader, stdout, stderr io.Writer) error

	// EditHarnessFile opens one of the harness's files in the user's editor and
	// saves what it wrote back, reporting whether anything changed. The window
	// is suspended for it, for the same reason.
	EditHarnessFile(ctx context.Context, harnessID, path string, stdin io.Reader, stdout, stderr io.Writer) (bool, error)

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

	// Forward starts forwarding one sandbox's listening ports to local ports,
	// for as long as the returned Forward is open. The workspace opens one when
	// it opens and closes it when it detaches.
	Forward(ctx context.Context, sandboxID string) (Forward, error)

	// NewShell creates, attaches and starts a fresh interactive shell exec,
	// returning its identity along with the terminal so the tab it becomes is
	// keyed by the same id the listing will report.
	NewShell(ctx context.Context, sandboxID string, cols, rows int) (Exec, Terminal, error)

	// NewTerminal does the same for one of the sandbox's own harness
	// terminals: a session of the harness it already runs, created in terminal
	// mode so the listing reports it as a terminal and every window draws it
	// beside the primary. Which harness that is, is the sandbox's answer.
	NewTerminal(ctx context.Context, sandboxID string, cols, rows int) (Exec, Terminal, error)

	// Services is the sandbox's declared services, in declaration order,
	// running or not. The workspace's tabs already draw the running ones from
	// the exec listing; this is what can also see the ones that are not, and
	// is read when the menu is opened rather than polled.
	Services(ctx context.Context, sandboxID string) ([]Service, error)

	// DoService runs a lifecycle verb against one of the sandbox's declared
	// services. It returns when the sandbox has acted; what the service is
	// doing afterwards arrives through the exec listing like everything else,
	// so there is nothing here for the window to remember.
	DoService(ctx context.Context, verb ServiceVerb, sandboxID, serviceID string) error
}
