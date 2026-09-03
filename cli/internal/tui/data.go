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
// the host it runs on, and the disk as bytes.
//
// It comes from the pool agent's resource report (ADR 0071), which differences
// every sandbox in a pool over one tick — so these shares are comparable
// between rows in a way rates each sandbox computed for itself would not be.
//
// The two Known flags are separate because the two figures are measured on
// different schedules. CPU and memory arrive together every report; the disk is
// a walk of the sandbox's trees on the agent's own adaptive interval, so a
// sandbox can be measured for one and not yet the other. Neither is ever drawn
// as a zero when it is merely unmeasured, because a zero reads as "idle" and
// "0 B" as "holds nothing".
type Usage struct {
	// Known covers CPUPercent and MemoryPercent: false until the agent has two
	// samples to difference, which is the first report after it starts.
	Known bool
	// DiskKnown covers DiskBytes and DiskPercent, and lags Known on a sandbox
	// created since the last sweep. It is also the one that outlives running:
	// a stopped discobox holds its disk and reports it, where Known goes false
	// with the discobox that was being measured.
	DiskKnown bool

	CPUPercent int
	// MemoryBytes is resident size — what this discobox's processes are
	// holding in RAM, the sum of what top calls RES.
	//
	// It is bytes rather than a share because a share of the whole machine
	// answers a question about the machine, and the question about a row is
	// what that discobox is costing. And it is resident rather than virtual
	// because virtual is address space reserved rather than memory held: a
	// process that maps a large file inflates it without consuming anything,
	// which makes it useless for telling one discobox's weight from another's.
	//
	// It is also not the cgroup's charge, which the band's machine total uses.
	// The charge includes reclaimable page cache and is the right number to
	// read against the machine's capacity; this is the right number to read
	// against another discobox. The two do not sum to each other and are not
	// meant to.
	MemoryBytes int64
	// MemoryPercent and DiskPercent drive the color and are never drawn. A
	// share is how a figure gets noticed; the figure itself is what is read.
	MemoryPercent int
	DiskBytes     int64
	DiskPercent   int
}

// Resources is what Discobox has on this machine and what it is using of it.
//
// It is deliberately not called a pool. A pool is how the system is built —
// one machine's worth of capacity that discoboxes are scheduled into — and the
// person reading this window has one and has never heard of it. What they want
// to know is how much room they have left before starting another discobox.
//
// Used covers everything Discobox runs: the discoboxes themselves and the
// machinery beside them, the shared builder above all, which on a machine
// mid-build is most of it. Splitting those out is what the CLI's
// `discobox admin pool resources` is for; here they are one number, because
// the answer to "can I start another one" does not care which half is busy.
type Resources struct {
	// Known is false until the agent has two samples to difference, which is
	// the first report after it starts. The window draws nothing rather than
	// zeroes: an unmeasured machine is not an idle one.
	Known bool

	CPUVCPUs       float64
	CPUCapacity    float64
	MemoryBytes    int64
	MemoryCapacity int64
	// DiskFreeBytes is what is left on the filesystem Discobox stores into.
	// It leads the disk figures because the whole filesystem usually holds
	// more than Discobox, so what Discobox has taken says nothing on its own
	// about whether the next discobox will fit.
	DiskFreeBytes int64
	// DiskDataBytes is what the discoboxes themselves hold, summed — each
	// one's home, sources and nested container store, which are its own copies
	// rather than links into anything shared.
	DiskDataBytes int64
	// DiskCacheBytes is what Discobox holds that no discobox owns: the cache
	// every discobox shares and the builder's own store. Both are disposable
	// and rebuild themselves, which is what makes them worth telling apart
	// from the data — it is the half you can reclaim.
	DiskCacheBytes int64
	DiskKnown      bool
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

// Port is one TCP port a sandbox serves — observed listening, or declared by a
// service (ADR 0076) — and what it turned out to speak. The address it is bound
// on is not carried: a forward dials from inside the sandbox, where every
// reported port answers, so the number and the protocol are the whole of what
// is actionable.
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

	// PendingRequests is how many credential requests on this discobox are
	// waiting on a person. It is not read from the server with the row: the
	// model annotates it from the project's request listing, so one poll
	// answers the list and the workspace both.
	PendingRequests int

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

	// ConfigName is the name the sandbox is configured with, which is not
	// usually what Name shows: the server names a row by its primary
	// terminal's window title as soon as there is one, because what the
	// harness says the work is about tells two boxes apart better than two
	// generated names do. It is empty when the box was never named, and the
	// display name is then the id.
	//
	// It is carried because it is the handle the rest of the world knows the
	// box by — what rename edits, what a `disco` command takes — and the row
	// is not showing it. The status line says it under the cursor, and the
	// rename guard reads it (nameIsTitle).
	ConfigName string

	Harness string

	// Folder is the client directory the sandbox was started from. It is not a
	// column on the row — it is what the header's dropdown filters on, so every
	// row on screen already shares it.
	Folder string

	// Source is what the discobox was cut from, spelled the way `-C` takes it:
	// the client directory holding the repository, or the repository URL when
	// there was no local one. Empty for a discobox created with no source at
	// all. It is what the run options offer as sources to cut a new one from.
	Source string
	// SourceRemote marks a Source that is a repository URL rather than a
	// directory on a client. A remote source has no folder of its own, so a
	// discobox cut from one is filed under the directory the window is running
	// in — which is what decides where the list follows the source to.
	SourceRemote bool

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
	// ConfigReminder is harness-authored guidance shown outside its configure
	// terminal, so the terminal itself cannot be mistaken for a normal session.
	ConfigReminder string
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

// nameIsTitle reports whether the row's name came from the primary terminal's
// window title rather than from the name the box is configured with. Rename
// edits the configured name — which such a row is not showing — so it is
// disabled there.
//
// A box with no configured name is named by its id, which rename does change
// what is shown for, so only a differing name counts as a title.
func (s Sandbox) nameIsTitle() bool {
	return s.ConfigName != "" && s.Name != s.ConfigName
}

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
		return "ready"
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

	// Tool is the tool this session is running — "diff", "fresh" — and empty
	// for every session that is not one. A tool session is neither a terminal
	// nor a shell: it is a window of its own, and the id is what reopens the
	// right one after a minimize or a restart. It is a label the launcher put
	// on the exec when it created it (ADR 0071); the sandbox knows nothing
	// about tools.
	Tool string

	// Primary marks the sandbox's primary harness terminal, which is the first
	// of the workspace's left column and the session the screen is a view
	// onto.
	Primary bool

	// Service is the id of the declared service this exec runs, empty for
	// every session that is not one. A service is an exec the sandbox started
	// from the repository's `.discobox/services` (ADR 0070), and it reaches the
	// workspace through this same listing rather than a poll of its own.
	Service string
	// ServiceName is the service's display name, which is what its tab is
	// called.
	ServiceName string
	// ServiceOrder is where the service sits in the repository's declaration
	// order — the order `.discobox/services` lists in, which the numeric
	// filename prefix is for. Services are ordered by it rather than by when
	// their process happened to start, so the tab strip reads the way the
	// directory does and holds still across a restart.
	ServiceOrder int

	Tty bool

	// Live is whether the exec can be attached to: it exists and has not
	// exited, failed or been lost.
	Live bool

	// CreatedAt orders the tabs, oldest first, so they hold their places as
	// the listing changes around them.
	CreatedAt time.Time
}

// ToolFile is one file a tool carries into a discobox: a copy kept on this
// machine, put in place the first time that tool runs in a discobox that has
// none of its own.
//
// It is a default rather than a sync, and deliberately one-way and create-only.
// The discobox's copy belongs to the discobox — a config edited inside a box,
// by you or by the agent working in it, is not something a later launch should
// quietly overwrite — so editing the local copy changes what the *next*
// discobox gets, not what any box already carrying one has.
type ToolFile struct {
	// Tool is the tool that carries it, and Name is what the picker calls it.
	// Together they name the copy on this machine; where that is, is the
	// adapter's answer. See DataSource.ToolFilePath.
	Tool string
	Name string

	// Home is where it goes in the discobox, relative to the run user's home
	// directory — ".config/fresh/config.json". Relative because only the
	// discobox knows what its run user's home actually is.
	//
	// It may contain "{workspace}", which stands for the tool's working
	// directory encoded the way a per-project state directory names itself.
	// That is for the state a tool keys on the project rather than on the user
	// — fresh's trust decision — and it is resolved in the discobox, since the
	// working directory is another thing only the discobox knows. See
	// installToolFileScript.
	Home string

	// Default is what the local copy is created with the first time anything
	// asks for it, so the first edit opens on a starting point rather than on
	// an empty buffer and a documentation search.
	Default string
}

// ToolSpec is everything running a tool takes: what to run in the discobox, and
// the files to have in place before it starts. The picker's own columns — the
// key it answers to, the label it wears — stay in this package; this is the
// part the outside needs.
type ToolSpec struct {
	ID      string
	Command []string
	Files   []ToolFile
}

// ExecPrimary is the virtual exec id of the sandbox's primary terminal. The
// sandbox resolves it to the current primary session — and relaunches one that
// has stopped — so the workspace never has to know its concrete id.
const ExecPrimary = "primary"

// RunRequest is what Enter in the prompt asks for: `discobox run`'s arguments, and
// nothing the command does not have.
type RunRequest struct {
	// Prompt is what the harness is given to do, as the create takes it: the
	// arguments. The composer holds one piece of text and sends it as one
	// argument — splitting it would be inventing tokens nobody typed — while
	// `discobox run fix the tests` sends the three words the shell split.
	Prompt  []string
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

	// NoSource is `--no-source`: create the discobox with nothing checked out
	// in it. Source is empty with it — there is no directory to cut from — and
	// the discobox is still filed under the one the window is running in.
	NoSource bool

	// Include is `-i`: the extra sources brought into the same discobox beside
	// the primary one. The window offers no way to name one; it carries them
	// because `discobox run` opens the window on its own request and that
	// request is the whole command (WithRun).
	Include []string

	// SkipDeclaredSources is `--declared-sources=false`: leave out the sources
	// the primary source's repository declares in .discobox/sources.json. The
	// zero value brings them in, which is what both frontends do by default.
	SkipDeclaredSources bool
}

// SourceWorkspace is what a create would carry into a discobox from the source
// directory it is cut from.
type SourceWorkspace struct {
	// Directory is the source directory itself, which is what the question
	// about it names.
	Directory string
	// Repository is false when the directory is in no Git repository at all.
	// There is then no committed history to start from, so what is being asked
	// is whether the whole directory is copied in.
	Repository bool
	// Carries is whether there is anything to carry: uncommitted work in a
	// repository, or any content at all in a directory that is in none.
	Carries bool
	// Changes are the paths that differ from the checked-out commit, as the
	// create path reports them. They are what the question about them names,
	// so that "carry them in?" says which. Empty for a directory in no
	// repository, where the answer is the whole of it.
	Changes []string
}

// DirectoryTotal is how much of a directory copying it into a discobox would
// carry, as counted so far. Done is set once the count is final.
type DirectoryTotal struct {
	Bytes int64
	Files int64
	Done  bool
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

// CredentialRequest is one ask for a credential, waiting on a person. An agent
// makes them through the in-sandbox CLI (ADR 0031); the proxy makes bare ones
// when it meets a sentinel it cannot resolve. What separates them here is
// Uses: only an agent that asked can say what it wants the credential for, and
// only those requests carry a name, an env var, and a justification.
type CredentialRequest struct {
	ID        string
	SandboxID string
	// Name is the credential the agent asked for ("github"), which is not a
	// secret ID: choosing which secret answers it is the approval.
	Name          string
	EnvVar        string
	Host          string
	Type          string
	Justification string
	Uses          []string
	Created       time.Time
}

// FromAgent reports whether a person is being asked a question with reasons
// attached, rather than being told the proxy hit an unresolvable sentinel.
func (r CredentialRequest) FromAgent() bool { return len(r.Uses) > 0 }

// Secret is a project secret as the window needs it: enough to choose between
// them and to manage them, and never a value.
type Secret struct {
	ID   string
	Name string
	Type string
	// Host the secret is bound to, empty when nobody bound it. A bound secret
	// may only be granted for that host and the hosts beneath it, so the
	// approval picker offers the ones that cover the request first.
	Host string
	// MaxTTL is the longest a grant on it may live, and the lifetime a grant
	// takes when nobody names one. Zero is no limit: grants on this credential
	// may then live forever.
	MaxTTL time.Duration
	// Grants is how many live grants stand on it, filled in by the screen from
	// the project's grant listing rather than read per row.
	Grants  int
	Created time.Time
	Updated time.Time

	// OAuth is what an oauth credential is, without being it. Nil for a token,
	// and never the tokens themselves.
	OAuth *SecretOAuth
}

// SecretOAuth is the half of an OAuth credential that can be shown: where it
// renews, whose grant it is, what it may do, and when the access token goes
// stale.
type SecretOAuth struct {
	TokenURL             string
	ClientID             string
	Scopes               []string
	SubscriptionType     string
	AccessTokenExpiresAt time.Time
	Refreshable          bool
}

// GrantUse is one approved way to use a credential, as the window shows it.
type GrantUse struct {
	ID          string
	Description string
}

// NewGrant is a standing authorization somebody is creating by hand: a
// pre-approval, minted without a request to answer.
type NewGrant struct {
	SecretID string
	Scope    string
	ScopeKey string
	Host     string
	// TTLSeconds is how long it lives. Zero asks for a grant that never
	// expires, which a secret carrying a limit refuses.
	TTLSeconds int64
	// EnvVar and Uses make it the agent credentials shape: a credential
	// nothing in the discobox can read, which the in-sandbox CLI takes one use
	// at a time. Empty leaves the ordinary standing grant, which authorizes
	// the sentinel the discobox already holds.
	EnvVar string
	Uses   []string
}

// Grant is a standing authorization on a secret: who may use it, where it may
// be sent, and until when.
type Grant struct {
	ID       string
	SecretID string
	Scope    string
	ScopeKey string
	Host     string
	// Uses are the approved uses an agent credential grant carries, empty on a
	// plain standing grant. The ID travels with the description because it is
	// what an agent presents to `discobox-access run --use`, and reviewing a
	// grant is where somebody reads it.
	Uses      []GrantUse
	GrantedBy string
	Granted   time.Time
	// Expires is when the authorization lapses; zero never does.
	Expires time.Time
}

// NewSecret is a credential typed into the window: from the approval dialog,
// where the project has nothing that answers a request yet, or from the secrets
// screen.
//
// An OAuth credential is more than one field, and all of them are needed: the
// access token is what travels, and the rest is what the control plane spends
// to renew it when it goes stale.
type NewSecret struct {
	Name string
	Type string
	Host string

	// MaxTTLSeconds is the ceiling on grant lifetimes the credential is stored
	// with. It is set here rather than left to a second call because it is a
	// fact about the credential — how long consent to it may last — and a
	// secret that exists for even a moment without one is a secret whose
	// grants are limited by whatever the server's default happens to be.
	//
	// Zero is not absence: it is the answer "no limit", and grants on the
	// credential may then live forever.
	MaxTTLSeconds int64

	Value SecretValue
}

// SecretValue is a credential's material: one opaque token, or the several
// fields an oauth credential renews itself with.
//
// It travels as one thing because the server stores one value per secret and
// replaces it whole. Sending half of an oauth credential — a new refresh token
// without the access token beside it — would drop the rest, which is why
// changing any part of one means storing all of it again.
type SecretValue struct {
	Token string

	// What an oauth credential needs to renew itself.
	RefreshToken string
	TokenURL     string
	ClientID     string
	Scopes       []string
}

// SecretUpdate is what an edit says about a secret. Every field is a pointer
// because absence has to be spelled differently from the values themselves:
// an empty host releases a binding and a zero limit allows grants that never
// expire, so neither can stand for "unchanged".
//
// The type is not among them. A token and an oauth credential are stored and
// renewed differently, and the server has no answer for a secret that changes
// from one to the other — that is a new credential, and deleting the old one is
// somebody's decision rather than a side effect of an edit.
type SecretUpdate struct {
	Name          *string
	Host          *string
	MaxTTLSeconds *int64
	Value         *SecretValue
}

// Approval is what a person decided about a request: which secret answers it,
// and how long the grant it mints lives. Everything else — scope, the approved
// uses, the host — follows the request, which is what the approver read.
type Approval struct {
	RequestID string
	SecretID  string
	// TTLSeconds is how long the grant lives; zero takes the secret's limit,
	// which is also the lifetime nobody has to choose.
	TTLSeconds int64
}

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

	// Resources is what Discobox has on this machine and what it is using of
	// it, polled on the same beat as List. It is separate from List because it
	// is true with no discoboxes at all — which is exactly when someone is
	// deciding whether to start one.
	Resources(ctx context.Context) (Resources, error)

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

	// Workspace reports what a create would carry in from the source directory,
	// so the window can settle --include-dirty=auto before it creates anything.
	Workspace(ctx context.Context, source string) (SourceWorkspace, error)

	// MeasureDirectory starts counting what copying dir into a discobox would
	// carry. total reports the running count and is polled while the question
	// about it is on screen; stop ends a walk whose question has been answered,
	// and is safe to call on one that already finished.
	MeasureDirectory(ctx context.Context, dir string) (total func() DirectoryTotal, stop func())

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

	// OpenHarnessConfigure starts the harness's interactive setup on a terminal
	// the window draws in a dedicated configuration pane.
	OpenHarnessConfigure(ctx context.Context, harnessID string, cols, rows int) (Terminal, error)

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

	// ServiceLogs is the transcript of a service's current or last run, as the
	// bytes it wrote. It is read for a service that is not running, whose pane
	// has no stream to attach to and whose output is the point of looking at
	// it. A service that never ran has none, which is not an error.
	ServiceLogs(ctx context.Context, sandboxID, serviceID string) ([]byte, error)

	// DoService runs a lifecycle verb against one of the sandbox's declared
	// services. It returns when the sandbox has acted; what the service is
	// doing afterwards arrives through the exec listing like everything else,
	// so there is nothing here for the window to remember.
	DoService(ctx context.Context, verb ServiceVerb, sandboxID, serviceID string) error

	// NewTool does the same for a tool session: the spec's command, run in the
	// sandbox's primary source directory, labeled with the tool it is so that
	// this window — and the next one to attach — can tell it from a shell.
	// Which tools there are and what they run is this package's answer; see
	// tools.go.
	//
	// The spec's files are put in place first, and only where the discobox has
	// none of its own. A tool whose files could not be delivered does not
	// start: an editor that silently comes up unconfigured is worse than one
	// that says why it did not.
	NewTool(ctx context.Context, sandboxID string, spec ToolSpec, cols, rows int) (Exec, Terminal, error)

	// EndExec ends one exec session in the sandbox, killing what is running in
	// it. It is what closing a tool window does, and the one place this window
	// ends a session rather than closing its own view of one.
	EndExec(ctx context.Context, sandboxID, execID string) error

	// ToolFilePath is where a tool file's copy lives on this machine, whether
	// or not it exists yet. The picker shows it, so the file is findable and
	// editable from outside this window too. Empty when there is no path to be
	// had — no home directory to resolve it against.
	ToolFilePath(file ToolFile) string

	// EditToolFile opens that copy in the user's editor, creating it from the
	// file's Default when there is none yet, and reports whether what came back
	// differs. The window is suspended for it, the way editing a harness file
	// suspends it.
	EditToolFile(ctx context.Context, file ToolFile, stdin io.Reader, stdout, stderr io.Writer) (bool, error)
	// CredentialRequests is every credential request in the project still
	// waiting on a person, newest first. It is polled with the listing rather
	// than streamed: the client-facing event stream is gone (ADR 0061), and a
	// request is answered on human time anyway.
	CredentialRequests(ctx context.Context) ([]CredentialRequest, error)

	// Secrets is the project's secrets, for choosing which one answers a
	// request. It never carries a value.
	Secrets(ctx context.Context) ([]Secret, error)

	// CreateSecret stores a credential typed into the approval dialog and
	// returns it, so the approval that follows has a secret to name.
	CreateSecret(ctx context.Context, secret NewSecret) (Secret, error)

	// UpdateSecret changes what a secret says about itself: its name, the host
	// it may be sent to, how long consent to it may last, and the credential
	// itself. Everything left nil is left alone.
	//
	// It is one call because it is one endpoint and, from the window, one form:
	// a card whose rows were saved by a call each would half-apply when the
	// second one failed, and would report two things where a person did one.
	UpdateSecret(ctx context.Context, secretID string, update SecretUpdate) error

	// Grants lists the standing grants on one secret, or on every secret in the
	// project when secretID is empty.
	Grants(ctx context.Context, secretID string) ([]Grant, error)

	// CreateGrant mints a standing grant without a request behind it: the
	// pre-approval an operator makes because they already know the answer.
	CreateGrant(ctx context.Context, grant NewGrant) (Grant, error)

	// RevokeGrant withdraws one grant. The credential stops resolving at once —
	// the request that produced it stays approved, because it is history.
	RevokeGrant(ctx context.Context, grantID string) error

	// DeleteSecret removes a secret and everything standing on it.
	DeleteSecret(ctx context.Context, secretID string) error

	// ApproveCredentialRequest answers a request yes, minting the grant that
	// authorizes it. The server decides the scope and the approved uses from
	// the request itself.
	ApproveCredentialRequest(ctx context.Context, approval Approval) error

	// DenyCredentialRequest answers a request no. It is a complete answer, not
	// a dismissal: the asking agent is waiting on one.
	DenyCredentialRequest(ctx context.Context, requestID string) error
}
