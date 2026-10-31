package tui

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/pkg/sourceref"
	"github.com/discobox-ai/discobox/termpane"
	"github.com/discobox-ai/x/id"
)

// State is a sandbox's user-facing lifecycle state, narrowed to the six the
// launcher draws. The server reports more of them — stopping, archiving,
// deleting — but a picker only has to answer "can I act on this", and the
// transitional states answer that the same way the state they are heading for
// does.
//
// Repairing is kept apart from starting, which it is drawn like: a repair
// recorded and not yet landed, typed or automatic (ADR 26-10-12-711 §8). The
// box had failed, and the row says it is being rebuilt rather than that it is
// coming up for the first time.
type State string

const (
	StateRunning   State = "running"
	StateStarting  State = "starting"
	StateRepairing State = "repairing"
	StateStopped   State = "stopped"
	// StateSuspended is a box frozen whole, its memory kept (ADR 26-10-16-420):
	// up as far as acting on it goes — stop takes it — and off as far as
	// anything inside it answering, which a start or a use brings back.
	StateSuspended State = "suspended"
	StateArchived  State = "archived"
	StateError     State = "error"
)

// AutomaticRepair is the last repair of a discobox that nobody typed: something
// was done to it, it was found failed with no container, and it was rebuilt
// once (ADR 26-10-12-711). The zero value is a discobox with no such record.
//
// It is the record of a repair that is over. One still running is the row's
// state (StateRepairing) and its Message.
type AutomaticRepair struct {
	At      time.Time // when the repair landed, or was recorded if it failed
	Trigger string    // what was being done to the discobox: "exec attach", "ssh"
	Failure string    // the failure the repair answered, on one line
	// Failed is a repair that did not work: the discobox is in error on it,
	// and is not repaired automatically again.
	Failed bool
}

// Usage is what a sandbox is costing while it runs: cpu and memory as shares of
// the host it runs on, and the disk as bytes.
//
// It comes from the pool agent's resource report (ADR 0071 on resource
// accounting), which differences every sandbox in a pool over one tick — so
// these shares are comparable between rows in a way rates each sandbox computed
// for itself would not be.
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
// and whether anything there is uncommitted.
//
// It arrives with the listing — the agent pushes it through the control plane —
// so unlike the diffstat it costs nothing to show. Known separates "nothing
// has reported yet", where the row falls back to the position the sandbox was
// spawned at, from a report of any kind.
type GitState struct {
	Known bool

	Branch string
	Commit string // head, short
	Dirty  bool   // uncommitted content in the working tree
}

// Port is one TCP port a sandbox serves — observed listening, or declared by a
// service (ADR 0076) — and what it turned out to speak. The address it is bound
// on is not carried: a forward dials from inside the sandbox, where every
// reported port answers, so the number and the protocol are the whole of what
// is actionable.
type Port struct {
	Number int
	// UDP marks the UDP port of that number, which is a different port from
	// the TCP one and may be forwarded to a different local port (ADR 0109).
	UDP      bool
	Protocol string // http, https, tcp, udp, or unknown while a probe has not answered
	// ServiceID and ServiceName are the declaration this port came from, empty
	// for a port only discovery found.
	//
	// The id is what the header matches on: sandboxservices.DesktopID is drawn
	// as its own field rather than as a number in the protocol groups, because
	// a desktop is a different offer from a forwarded port. The name is what
	// that field is labeled with, so nothing here has to know what any
	// particular id is called.
	ServiceID   string
	ServiceName string
}

// Sandbox is the row model: what a picker needs to tell one sandbox from
// another, plus what the actions need to know about whether they apply.
//
// Where it came from — source, branch and commit — is what tells two sandboxes
// with similar names apart, so it is on the row rather than behind a key.
type Sandbox struct {
	ID    string
	Name  string
	State State

	// Server is the server the discobox is on, by the name the window lists it
	// under (ADR 0116 §4): what the header's server filter matches, and where
	// every action on the row is routed. Empty when there is only one, where
	// naming it says nothing.
	Server string

	// PendingRequests is how many credential requests on this discobox are
	// waiting on a person. It is not read from the server with the row: the
	// model annotates it from the project's request listing, so one poll
	// answers the list and the workspace both.
	PendingRequests int

	// Agent is what the program in the discobox's primary terminal last
	// reported of itself over OSC 7501 (ADR 26-10-16-424): working, idle,
	// blocked on a person, done, or failed. The zero State is no report, and
	// a discobox that is not running carries none.
	Agent programstatus.Status
	// AgentObservedAt is when the discobox's agent took the status Agent was
	// read from, on the sandbox's clock: what a fresher report heard on the
	// stream is measured against.
	AgentObservedAt time.Time

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
	// It is carried because it is the handle the box was created with, which
	// a `disco` command still takes, and the row is not showing it. The status
	// line says it under the cursor. Rename does not edit it: it sets the name
	// in the box's meta, which Name then shows (ADR 26-10-12-026).
	ConfigName string

	// GivenName is the name in the discobox's meta, the one rename sets, and
	// empty when nobody has named it. Name is it whenever it is set, so the
	// row does not draw it; it is here so rename can tell a name that can be
	// cleared from a title that cannot.
	GivenName string

	// Tags are the discobox's tags as it last reported them, spelled `key` or
	// `key=value` in key order, and Description is the first line of its
	// description (ADR 0136). The discobox holds both, in its meta file; these
	// are the copy the server keeps, so a stopped box still shows them.
	Tags        []string
	Description string

	Harness string
	// HarnessID is that harness's config ID. The slug above is what the row
	// shows; this is what matches a credential the window has been told is
	// refused, and what its configure flow is opened by.
	HarnessID string
	// HarnessName is that harness's name as a person reads it, which is what
	// a band about the harness calls it.
	HarnessName string
	// HarnessUncredentialed is a harness with a configure flow and no
	// credentials bound to it: every session on it runs signed out until it is
	// configured. See uncredentialed.go.
	HarnessUncredentialed bool

	// CreatedBy is the ID of the discobox that created this one, on the same
	// server, and empty for one a person created. The list draws it under
	// that discobox (sandboxList.nest).
	CreatedBy string

	// tree and under are the list's, set on each row as the rows are ordered
	// (sandboxList.nest): the branch drawn in front of the name of a row
	// nested under the discobox that created it, empty on a row nested under
	// nothing, and the row its tree hangs from, which is itself for that one.
	tree  string
	under boxRef

	// OriginKey is where the discobox is filed on the client that created it
	// (ADR 0111): that host and where its source came from, or the host alone
	// when it has none. It is not a column on the row — it is what the header
	// filter's folder matches, so every row on screen already shares it, bar
	// the ones nested under a creator filed there (sandboxList.nest).
	OriginKey string

	// OriginHostID identifies the machine the sandbox was created on, and
	// OriginHost is the hostname that machine reported for itself, which is
	// display only and may be empty. A folder's name does not answer this: two
	// machines can hold the same path, and the other's is a different folder
	// that reads the same. See Sandbox.elsewhere.
	OriginHostID string
	OriginHost   string

	// Source is what the discobox was cut from, spelled the way `run`'s SOURCE
	// takes it:
	// the client directory holding the repository, or the repository URL when
	// there was no local one. Empty for a discobox created with no source at
	// all. It is what the run options offer as sources to cut a new one from,
	// and what its folder is named by.
	Source string
	// SourceRemote marks a Source that is a repository URL rather than a
	// directory on a client.
	SourceRemote bool
	// SourceOriginKey is where a discobox cut from Source on this machine is
	// filed: what the list follows the Source row to when it is set to this
	// one. It is OriginKey itself for a discobox created here.
	SourceOriginKey string
	// Repository is the web address of the GitHub repository the source
	// belongs to, `https://github.com/owner/repo`, and empty when it is not
	// on GitHub or there is no source. It is what an `issue=` or `pr=` tag is
	// numbered in, which is what makes such a tag a link (Sandbox.tagURL).
	Repository string
	// RepositoryKey tells Repository apart from every other GitHub
	// repository, as `owner/repo` lower-cased: the same for a clone, the
	// shorthand and every spelling of the URL (sourceref.GitHubKey).
	// It is the folder the discobox is in when it is set (Sandbox.folder).
	RepositoryKey string

	Branch string
	Commit string // the commit it was spawned from, short
	Dirty  bool   // spawned from a snapshot on top of that commit

	// Pushable reports that new local commits here are this window's to send:
	// the discobox has a source delivered by pushing it, this machine is the
	// one it was pushed from, and it is in a state to take another push (ADR
	// 26-09-05-008 §2 on automatic push). Whether there are any new commits is a local
	// question, asked by the push itself and never by the listing.
	//
	// It is on the row because it is the gate the workspace's automatic push
	// reads on every beat, and reading it here costs the listing nothing: the
	// delivery, the origin host and the state are all already in the response.
	Pushable bool

	// AwaitsDelivery reports that the discobox is parked waiting for a source
	// this machine can deliver: its create stopped after it parked and before
	// the push was reported, and this machine is the one it was created on.
	// Opening the workspace delivers it first (ADR 26-09-24-005).
	AwaitsDelivery bool

	// Git is where the work sits now, when the sandbox's agent has reported;
	// the spawn fields above are the fallback until it does.
	Git GitState

	// Ports are what the sandbox is serving right now, ordered by number.
	Ports []Port

	Usage   Usage
	Diff    DiffStat
	Created time.Time // when the sandbox was created
	Upgrade bool      // running an image older than its harness config resolves to
	// Message is the detail behind the state, shown when the row is under the
	// cursor: the error a box settled on, or what a repair nobody typed is
	// answering while it runs.
	Message string
	// Provisioning is what the box reports of its provisioning, shown when the
	// row is under the cursor: the features still being provisioned, or the
	// ones that failed (ADR 26-10-16-423 §6). Empty once all are ready. It
	// was written by the box and made displayable already.
	Provisioning string
	// HarnessConfigPending reports a captured local settings or credential change.
	HarnessConfigPending bool

	// Repaired is the last automatic repair of this box, once it is over. It is
	// carried because a repair that works clears the failure it answered, and
	// this is then the only thing saying the box was rebuilt.
	Repaired AutomaticRepair

	// ImageUnavailable is a box that failed because its pool cannot obtain the
	// image it is pinned to, as the server classified the failure. Attaching
	// cannot get past that however often it is tried, so the window offers the
	// upgrade that re-pins it instead, when Upgrade says there is an image to
	// move it to (imageoffer.go).
	ImageUnavailable bool
}

// Session is what the window knows about where it is running: the project and
// the directory every command it runs inherits.
//
// It is read once at startup. The project is a property of the session the way
// DISCOBOX_PROJECT is a property of a shell, not something the window changes. What
// the run options offer as a harness is not here — that is the harnesses, which
// are read on their own and change while the window is up.
type Session struct {
	Project        string
	DefaultProject string

	// Home is this user's home directory, which a directory under it is
	// shown from as `~` (sourceref.Ref.Display); empty shows every path
	// whole.
	Home string

	// Directory is the directory the window is running in — its repository
	// root when it is in one — and Branch is what is checked out in it. It is
	// where the window's prompt draft is kept, and what the window cuts a
	// sandbox from by default.
	Directory string
	Branch    string

	// OriginKey is where a discobox cut from the window's own source is filed,
	// and HostKey is this machine's own key, which files its discoboxes with no
	// source (ADR 0111). Together they are the window's own folder when its
	// checkout is not on GitHub; when it is, the folder is RepositoryKey and
	// these bring in only what resolved to no repository (ADR 26-10-14-585).
	OriginKey string
	HostKey   string

	// RepositoryKey is the GitHub repository the window's own source belongs
	// to, resolved the way a discobox cut from it now would record it and
	// keyed as Sandbox.RepositoryKey is, and empty when it is not on GitHub.
	// It is what makes the window's folder the repository's.
	RepositoryKey string

	// HostID is this machine's client identity, which is what makes a
	// discobox created somewhere else recognizable as such. Empty where it
	// could not be resolved, and then no row claims to be from elsewhere:
	// marking every row is worse than marking none.
	//
	// Host is this machine's hostname, carried for the one thing the id
	// cannot answer: whether a row's recorded hostname is the one the person
	// is already sitting on. See Sandbox.elsewhere.
	HostID string
	Host   string

	// Draft is the prompt the last window on this machine left unsent, and is
	// what the composer opens holding. See DataSource.SaveDraft.
	Draft string

	// View is how the last window on this machine left the header's filters,
	// and is what the window opens on. The zero value is every server, every
	// folder and every tag. See DataSource.SaveView.
	View ListView

	// LastSource is the source the last create on this machine was cut from,
	// as the Source row spells it, and is what the row opens on and leads
	// with. Empty when nothing has been created yet. See
	// DataSource.SaveLastSource.
	LastSource string

	// ServerChosen is whether the command line named the server (--server,
	// which makes it the primary). What was named outranks what was saved:
	// naming one is saying which you mean.
	ServerChosen bool

	// Servers are the servers a discobox can be created on, by the names the
	// window lists them under, the primary first (ADR 0116 §5). Nil when there
	// is only the primary, and then the run options offer no choice.
	Servers []string
}

// ListView is the header's three filters as a window leaves them: the server,
// the folder and the tags the list is narrowed to, each empty for every one.
// The tags are in order, so two views holding the same ones are Equal.
//
// The folder is carried whole rather than by key alone, because the window
// opens on it before a listing has landed to name it from: its label is what
// the header draws and its source is what a create cuts from.
type ListView struct {
	Server       string
	FolderKey    string
	FolderLabel  string
	FolderSource string
	FolderLocal  bool
	Tags         []string
}

// Equal reports whether two views narrow the list the same way.
func (v ListView) Equal(o ListView) bool {
	return v.Server == o.Server && v.FolderKey == o.FolderKey && v.FolderLabel == o.FolderLabel &&
		v.FolderSource == o.FolderSource && v.FolderLocal == o.FolderLocal && slices.Equal(v.Tags, o.Tags)
}

// IsZero reports whether the view narrows nothing: every server, folder and
// tag, which is the default and so nothing to remember.
func (v ListView) IsZero() bool { return v.Equal(ListView{}) }

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

// SecretRejection is a credential the window knows does not work: an upstream
// refused it, the pool's proxy could not save it with a second value, and the
// server could not renew it (ADR 0132).
//
// The window's first job with one of these is attribution. From inside a
// discobox a refused credential looks exactly like Discobox failing to deliver
// one, and saying which credential was refused, and where, is what separates
// "this is broken" from "this token is dead". The second job is the remedy,
// which is why the harness is carried: a credential its configure flow made is
// replaced by configuring it again, and anything else by replacing the value.
type SecretRejection struct {
	// Server is the server the credential belongs to, by the name the window
	// lists it under, and where the remedy goes (ADR 0131 §1): a harness is
	// configured again on its own server, and a secret replaced there. Empty
	// when there is only one server.
	Server     string
	SecretID   string
	SecretName string
	SecretType string
	// Host is the destination that refused it. A credential can be good for one
	// host and dead at another, so the band names it.
	Host string
	// Reason is why a person is needed rather than another attempt:
	// unrefreshable, refresh-failed, or rejected-after-refresh.
	Reason string
	// EnvName is the variable the credential is delivered in, when something
	// names one. It is often what a person recognizes the credential by.
	EnvName string
	// SandboxID is the discobox that most recently ran into it, and
	// HarnessConfigID the harness whose configure flow owns the credential,
	// when one does. Between them they decide which workspace raises the band
	// and what pressing it opens.
	SandboxID         string
	HarnessConfigID   string
	HarnessConfigName string

	FirstSeen time.Time
	LastSeen  time.Time
}

// occurrence names this refusal among others: the credential, where it was
// refused, and when that was first seen — a refusal cleared and seen again is
// a new one. It is what a dismissal of the band remembers.
func (r SecretRejection) occurrence() string {
	return r.SecretID + "\x00" + r.Host + "\x00" + r.FirstSeen.UTC().Format(time.RFC3339Nano)
}

// harnessOwned reports whether configuring a harness again is the remedy.
func (r SecretRejection) harnessOwned() bool { return r.HarnessConfigID != "" }

// name is what to call the refused credential on one line: the variable it
// arrives in where there is one, since that is what a harness's own card and
// its error messages say, and the secret's name otherwise.
func (r SecretRejection) name() string {
	if r.EnvName != "" {
		return r.EnvName
	}
	if r.SecretName != "" {
		return r.SecretName
	}
	return r.SecretID
}

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
	// ConfigPorts are the local ports its configure flow forwards into the
	// configure sandbox at the same number — a sign-in's callback port. The
	// flow binds them; the window only asks ahead whether it can, so the
	// header can say so for the whole session. See DataSource.LocalPortsInUse.
	ConfigPorts []HarnessConfigPort
	// Error is why the configure flow did not finish, when it did not.
	Error string

	// Bundle is the harness bundle every discobox on it applies, or, for a
	// template with no image, ManifestFile is its manifest file (ADR
	// 26-10-16-423 §2); Digest is whichever's.
	Bundle       string
	ManifestFile string
	Digest       string
	// Platforms is what a manifest file's template runs on, as os/arch, and
	// Unhosted that no pool in the project can run the harness: none hosts
	// one of them and runs the kind of box it runs on (ADR 0145 §1, ADR
	// 26-10-09-106 §4). A discobox is created only on a harness some pool can run, so the
	// `run` picker leaves an unhosted one out; this screen lists every one.
	Platforms string
	Unhosted  bool

	Run      []string
	Relaunch []string

	Secrets []HarnessSecret
	Files   []HarnessFile

	Updated time.Time
}

// HarnessConfigPort is one port a harness's configure flow needs bound at its
// own number on this machine.
type HarnessConfigPort struct {
	// Port is zero when Ephemeral.
	Port int
	// Ephemeral stands for whatever ports the configure sandbox turns out to
	// listen on, which the flow forwards as it finds them. Nothing can be
	// checked for it before the flow opens.
	Ephemeral bool
	// Unavailable is what to tell the user when it cannot be: the harness's
	// own words, since only it knows what its sign-in can still do without
	// the port. Always filled in.
	Unavailable string
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
// runs is what the harness runs as: its bundle, or its manifest file.
func (h Harness) runs() string {
	if h.Bundle != "" {
		return h.Bundle
	}
	return h.ManifestFile
}

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
// tier 1). Repairing is the same answer for a rebuild: the server holds an
// action until a repair in flight lands (ADR 26-10-12-711 §6).
//
// Error is deliberately not on that list. A settled failure latches on the row
// and nothing clears it without new intent (ADR 0017 §4), so an errored box may
// have a perfectly good container — a failure on a later generation, or a
// transient one — and refusing it would make archive/unarchive the only way to
// reach work that was never unreachable. The x and the reason still show; they
// just do not bar the door. An error with no container is the case that is
// genuinely stuck, and it is the one this still refuses.
//
// Spelling this as a list of states is what ADR 0017 §4 warns against — the
// wedge it describes came from a guard written `State == "stopped"` when it
// meant "is anything relying on this container".
func (s Sandbox) attachable() bool {
	if s.State == StateArchived {
		return false
	}
	return s.HasRuntime || s.State == StateStarting || s.State == StateRepairing
}

// startable reports whether start is offered for this box: powered off, or
// frozen whole. Attaching to one starts it too (attachable).
func (s Sandbox) startable() bool {
	return s.State == StateStopped || s.State == StateSuspended
}

// live reports whether the box is running, or being brought up, so attaching
// to it starts nothing: a starting or repairing box is already being brought up,
// and the attach waits for it. Every other state either starts the box on an
// attach — stopped, suspended, and an error whose container may be off, since
// the state does not say (attachable) — or refuses one.
func (s Sandbox) live() bool {
	return s.State == StateRunning || s.State == StateStarting || s.State == StateRepairing
}

// repairable reports whether repair is offered for this box.
//
// Repair is offered on every box but an archived one. A latched error, or a box
// nothing has ever reported a container for, is the wedge repair exists for
// (ADR 0035), but a box can be broken behind a state that looks fine — a
// container that runs and a harness that does not, a create or a repair that
// will never land — and the person looking at it is the one who knows. The
// server takes a repair in any of those states. Archived has no container by
// intent, and unarchive is what brings it back; the server refuses repair on it
// with the same reasoning (ADR 0035).
func (s Sandbox) repairable() bool {
	return s.State != StateArchived
}

// elsewhere is what the row says about a discobox created under another client
// identity: the hostname that machine reported, qualified by the host id that
// actually identifies it, since a hostname is neither unique nor stable and the
// id is what every other part of Discobox names the machine by.
//
// It is empty for a discobox this identity created, which is the usual row.
// The qualifier is for the row nothing here started — a box from a laptop, in
// a folder whose path this machine happens to share — and it is the
// identity difference, so the row says where it came from
// before it is tried.
//
// A client identity is not quite a machine (`internal/hostid`): it is stored
// per user, and a config directory that does not survive the run — CI, a
// container — mints a new one next time. Such a row genuinely came from an
// identity that is not this one, and saying so is right; what would be wrong
// is naming the hostname for it, since "from wilma" while sitting on wilma
// reads as a mistake rather than as a second identity on wilma. So a recorded
// hostname that is this machine's own is dropped and the id says it alone.
//
// session.HostID empty is unknown rather than "no machine": nothing is marked
// at all, because a window that cannot say what is local cannot say what is
// remote either.
func (s Sandbox) elsewhere(session Session) string {
	if session.HostID == "" || s.OriginHostID == "" || s.OriginHostID == session.HostID {
		return ""
	}
	if s.OriginHost == "" || strings.EqualFold(s.OriginHost, session.Host) {
		return "from " + shortHostID(s.OriginHostID)
	}
	return "from " + s.OriginHost + " (" + shortHostID(s.OriginHostID) + ")"
}

// folder is the header folder a discobox is in (folder.go): its repository's
// when its source resolved to a GitHub repository when it was cut, and its
// origin key's otherwise — named by its source, or as having none, and by the
// machine it was created on when that is not this one. A discobox filed under
// the window's own key that resolved to no repository is in the window's
// folder; one that resolved to a repository is in that one's, even when the
// window's checkout now resolves to another, as a fork's does.
func (s Sandbox) folder(session Session) folder {
	local := session.HostID != "" && s.OriginHostID == session.HostID
	if s.RepositoryKey != "" {
		// What a create from the repository cuts from is what this one was
		// cut from when that can be cut from here — a URL, or a directory of
		// this machine's — and the repository itself otherwise.
		f := folder{key: s.RepositoryKey, label: repositoryLabel(s.RepositoryKey), source: s.Repository, local: local}
		if s.Source != "" && (s.SourceRemote || local) {
			f.source = s.Source
		}
		return f
	}
	if s.OriginKey != "" && s.OriginKey == session.OriginKey {
		return session.folder()
	}
	f := folder{key: s.OriginKey, label: sourceref.Display(s.Source, session.Home), source: s.Source, local: local}
	if s.Source == "" {
		// Nothing was cut from it, so there is nothing to cut from in it: the
		// panel falls back to the window's own source, the way it does for
		// every folder at once. The sentinel is the Source row's answer, not a
		// value anything may create from — sourceDir and the field's
		// placeholder would draw it verbatim.
		f.label = noSourceChoice
	}
	if from := s.elsewhere(session); from != "" {
		f.label += " · " + from
	}
	return f
}

// folderKey is the key of the folder a discobox is in: what the header's
// filter matches and the list's sections tell rows apart by.
func (s Sandbox) folderKey(session Session) string {
	if s.RepositoryKey != "" {
		return s.RepositoryKey
	}
	if s.OriginKey != "" && s.OriginKey == session.OriginKey {
		return session.folder().key
	}
	return s.OriginKey
}

// shortHostID is a host id the length a row can afford: the prefix and enough
// of the random part to tell two machines apart, since the full sixteen
// characters would spend most of a name column on a value nobody reads to the
// end. It is what tells two machines answering to the same hostname apart —
// two "ubuntu" laptops is the case the id is here for at all.
//
// A host id that is not a generated one is said whole: DISCOBOX_HOST_ID takes
// any string, and cutting somebody's own "ci" to six characters says nothing
// about how long it was.
func shortHostID(hostID string) string {
	if !id.IsGenerated(hostID) {
		return hostID
	}
	return hostID[:len(hostID)-id.RandomLength+6]
}

// up reports whether the sandbox is running anything, and so whether its usage
// figures would mean anything.
func (s Sandbox) up() bool {
	return s.State == StateRunning || s.State == StateStarting
}

// base is the reported Git position, or the spawn position until a report
// arrives. A star marks dirt and an up arrow marks commits ahead of spawn.
func (s Sandbox) base() string {
	branch, commit := s.Branch, s.Commit
	if s.Git.Known {
		branch, commit = s.Git.Branch, s.Git.Commit
	}
	if branch == "" && commit == "" {
		return ""
	}
	out := branch + "@" + commit
	switch {
	case s.dirty():
		out += "*"
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

// ahead reports clean committed work beyond the spawn commit.
func (s Sandbox) ahead() bool {
	return s.Git.Known && !s.Git.Dirty &&
		s.Git.Commit != "" && s.Commit != "" && s.Git.Commit != s.Commit
}

// changes is the git column's mark spelled out: the one-word state of the
// work, for the column beside the position so the mark never has to be
// decoded. The two agree by construction — same predicates, same order.
func (s Sandbox) changes() string {
	switch {
	case s.dirty():
		return "dirty"
	case s.ahead():
		return "ready"
	case s.Git.Known:
		return "clean"
	default:
		return "-"
	}
}

// standing is what a discobox's workspace header says of it
// (Sandbox.standing): nothing, ready for a person, or blocked on one.
type standing int

const (
	standingNone standing = iota
	standingReady
	standingBlocked
)

// standing is blocked when the discobox's pull request is (Sandbox.prStatus),
// when a credential request on it is waiting on a person — an agent asking
// for a credential is as stuck as a pull request waiting on somebody — or
// when its agent reports it is blocked on one, and ready when its pull
// request is ready for a human merge.
func (s Sandbox) standing() standing {
	switch {
	case s.PendingRequests > 0 || s.prStatus() == prBlocked || s.Agent.State == programstatus.Blocked:
		return standingBlocked
	case s.prStatus() == prReady:
		return standingReady
	default:
		return standingNone
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
	// on the exec when it created it (ADR 26-08-27-302 on tool sessions); the sandbox
	// knows nothing about tools.
	Tool string

	// FilesChanged marks a pull request's files changed, which its issue tab
	// started and draws in place of its conversation (DataSource.PullReview).
	// It is never a tab of its own.
	FilesChanged bool

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
	// discobox knows what its run user's home actually is. It is a fixed path:
	// state keyed on something only the discobox knows is the tool's own
	// script's to write.
	Home string

	// Default is what the local copy is created with the first time anything
	// asks for it, so the first edit opens on a starting point rather than on
	// an empty buffer and a documentation search.
	Default string
}

// Tool is one tool as the picker offers it: a declaration from the CLI, the
// discobox's image or primary source, or the user (ADR 0125). What it runs is
// the data source's to know; this is what the window needs to list it, bind a
// key to it, and tell a session from a request.
type Tool struct {
	ID string
	// Key is the key the declaration asks for, empty when it asks for none.
	// Whether it gets it is the window's call: a key something else on the
	// same card already answers to is not given away.
	Key    string
	Label  string
	Detail string
	// Host reports a tool that runs on this machine, handed the discobox. It is
	// a request that returns rather than a session with a window.
	Host bool
	// Files are what the tool carries into the discobox.
	Files []ToolFile
	// Problem is why the declaration cannot run, empty when it can.
	Problem string
}

// Addresses are the ways into a discobox that are not this window: the ssh
// command that opens a session in it, and the git URL its primary source
// answers on.
//
// They are strings to show and copy rather than parts to assemble, because
// what makes them worth printing is that they are already the whole thing —
// the point of `ssh mybox` is that there is no address, port, user or key to
// go and find. Both are empty when there is nothing to print, which is not an
// error: a discobox whose every host pattern was claimed by another one has no
// unambiguous name to be reached by, and one whose source never landed has no
// directory to clone from.
type Addresses struct {
	// SSH is the whole command, `ssh <name>`, rather than the host on its own.
	// It is what someone reads to learn that reaching a discobox is one word
	// in a shell, and pasting it into that shell should be all of it.
	SSH string
	// Git is a URL rather than a command, because a URL is what git takes
	// everywhere — clone, remote add, fetch — and only the first of those is a
	// guess about what the reader wants.
	Git string
}

// ExecPrimary is the virtual exec id of the sandbox's primary terminal. The
// sandbox resolves it to the current primary session — and relaunches one that
// has stopped — so the workspace never has to know its concrete id.
const ExecPrimary = "primary"

// Listing is what one refresh of the list found: the discoboxes, and the
// servers that were asked and did not answer, whose discoboxes are missing
// from it rather than gone (ADR 0116 §4). The primary is one of those servers:
// a window that polls keeps listing the servers that are up when one goes
// down. A server that has been asked and has not answered yet is not one of
// them — it is not known to be missing anything — it is in Waiting.
type Listing struct {
	Sandboxes   []Sandbox
	Unreachable []string
	// Waiting is the servers a poll asked and left in flight, with nothing of
	// theirs to show meanwhile: they are slow rather than missing, which is a
	// different thing to say and is said where their rows would be.
	Waiting []string
	// Listed is when each server whose rows are here last answered, keyed the
	// way Sandbox.Server names it — "" for a window with one server. Rows
	// are kept on screen while their server is slow to answer again, and how
	// old they have got is what the window says about them.
	Listed map[string]time.Time
}

// RunRequest is what Enter in the prompt asks for: `discobox run`'s arguments, and
// nothing the command does not have.
type RunRequest struct {
	Credentials        CredentialUI
	LendGitCredentials bool
	// Prompt is what the harness is given to do, as the create takes it: the
	// arguments. The composer holds one piece of text and sends it as one
	// argument — splitting it would be inventing tokens nobody typed — while
	// `discobox run . fix the tests` sends the three words the shell split.
	Prompt  []string
	Harness string // empty is the project default

	// IncludeDirty is `--include-dirty`: "", "true" or "false". Empty is auto,
	// which asks — and nothing in a full-screen window can ask, so the launcher
	// resolves it before the sandbox is created.
	IncludeDirty string
	// CopyDirectory answers the other question, for a source directory in no
	// Git repository: "true" copies it in, "false" leaves the discobox with no
	// source, and empty asks — with copying the answer it leads with. It has
	// no flag: `discobox run` asks it too, and copies with nobody to ask.
	CopyDirectory string

	Detach bool
	Env    []string
	Secret []string

	// Source is `run`'s SOURCE, DIR@REF, the directory and ref the sandbox is
	// cut from; empty is the window's own directory.
	Source string

	// NoSource is `--no-source`: create the discobox with nothing checked out
	// in it. Source is empty with it — there is no directory to cut from — and
	// the discobox is still filed under the one the window is running in.
	NoSource bool

	// Include is `-i`: the extra sources brought into the same discobox beside
	// the primary one, from the run options' Additional sources row or
	// `discobox run`'s own request (WithRun).
	Include []string

	// Bind and Delegate are `--bind` and `--delegate`, as given: the secrets
	// bound to the new discobox, and DiscoboxAPI `--discobox-api`. The window
	// offers no way to name them; it carries them because `discobox run` opens
	// the window on its own request and that request is the whole command
	// (WithRun).
	Bind        []string
	Delegate    []string
	DiscoboxAPI bool

	// SkillDirs and UserSkills are `--skills` and `--user-skills`, from the run
	// options' rows of those names or `discobox run`'s own request; nil for
	// one not given, which the create takes from the client's configuration
	// as `discobox run` would.
	SkillDirs  []string
	UserSkills *bool

	// NoProjectSkills is `--no-project-skills`: the project's skills
	// repositories are left out of this create. The zero value copies them
	// in, which is what every create does (ADR 26-10-14-894 §2).
	NoProjectSkills bool

	// SkipDeclaredSources is `--declared-sources=false`: leave out the sources
	// the primary source's repository declares in .discobox/settings.json. The
	// zero value brings them in, which is what both frontends do by default.
	SkipDeclaredSources bool

	// Pool is `--pool`: the pool to create the discobox on, by ID or name.
	// Empty is the project's default pool. Like Bind, only `discobox
	// new`'s own request carries one.
	Pool string

	// Server is the server to create the discobox on, by name: one of
	// Session.Servers. Empty is the primary, which is `--server` unset.
	Server string
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

// Interaction identifies a terminal or workspace action.
type Interaction string

const (
	InteractAttach Interaction = "attach"
	InteractShell  Interaction = "shell"

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

// ExitReporter provides the local harness configure command's exit status.
// Remote terminals report connection closure rather than a command verdict.
type ExitReporter interface {
	// ExitStatus is the command's exit code, and whether it has ended.
	ExitStatus() (code int, done bool)
}

// Binding is one sandbox port reachable at a local one, while a Forward is
// open. Local is what a browser on this machine connects to; Port is what the
// sandbox is serving on inside itself.
type Binding struct {
	Port int
	// UDP marks a binding of the sandbox's UDP port Port rather than its TCP
	// one.
	UDP   bool
	Local int
}

// portKey is how a forwarded port is looked up. A sandbox's TCP and UDP ports
// of one number are two ports, and their forwards can land on two local ones.
type portKey struct {
	number int
	udp    bool
}

func (p Port) key() portKey    { return portKey{number: p.Number, udp: p.UDP} }
func (b Binding) key() portKey { return portKey{number: b.Port, udp: b.UDP} }

// Forward is a running port forward onto one sandbox: every port it announces,
// held open at a local port for as long as the workspace showing it is.
//
// The window does not drive it. It has no address to dial with — a Port drops
// the bind address for the reason portsField gives — and nothing to decide: the
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

// AuditRecord is one record of a discobox's audit timeline, as `discobox admin
// audit ls` prints it: when, which trail, and what it says. Everything else
// about it is AuditDetail's to say.
type AuditRecord struct {
	// ID is what AuditDetail is asked for, and the ID `audit get` takes.
	ID   string
	Time time.Time
	// Source is the trail the record is in — http, creds, refresh, hooks or
	// execs — which is also what says who vouches for it (ADR 0130 §2).
	Source string
	// Summary is the record in one line, already safe to put on a terminal.
	Summary string
	// Group is what a run of records has in common when the timeline may fold
	// it into one row — an http exchange's method, status and origin — and
	// GroupSummary is that row's line. Consecutive records with the same Group
	// fold; an empty Group never does.
	Group        string
	GroupSummary string
}

// AuditRecordDetail is one record in full: the text `discobox admin audit get`
// prints, and what of it that text leaves out.
type AuditRecordDetail struct {
	Text string
	// Recordings are the parts of an http record the pool kept the bytes of
	// — AuditRequestBody, AuditResponseBody, AuditStream — which are unbounded
	// and so read on their own, with AuditBody.
	Recordings []string
}

// The recordings an http record can have, as AuditBody names them.
const (
	AuditRequestBody  = "request"
	AuditResponseBody = "response"
	AuditStream       = "stream"
)

// AuditUpdate is one delivery from a followed timeline.
type AuditUpdate struct {
	// Records are the records read since the last update, oldest first.
	Records []AuditRecord
	// Polled marks the end of one read of every trail, and Missing is then
	// what that read could not reach, a sentence each: empty says every trail
	// answered. A follower that has gone quiet because a trail stopped
	// answering has to be able to say so with no records to say it beside.
	Polled  bool
	Missing []string
}

// DataSource is everything the window needs from the outside. It is implemented
// once, in the cli package, over the same API client and code paths the
// non-interactive commands use: the launcher runs `discobox`'s commands rather
// than a second implementation of them.

// CredentialRequest is one item waiting on a person: an agent's ask for a
// credential, made through the in-sandbox CLI (ADR 0031) — an ask to use it
// names the variable it wants it in, an ask to delegate it names none — or,
// with Refresh or Trust set, an ask for a new value or for a host's
// certificate to be trusted.
type CredentialRequest struct {
	ID        string
	SandboxID string
	// Server is the server the request is waiting on, by the name the window
	// lists it under, and where everything that answers it goes (ADR 0131
	// §1): a request is answered with one of its own server's secrets. Empty
	// when there is only one server.
	Server string
	// Name is the credential the agent asked for ("github"), which is not a
	// secret ID: choosing which secret answers it is the approval.
	Name   string
	EnvVar string
	// Routes are where a credential request asks the credential to be sent;
	// Host is a trust's endpoint.
	Host          string
	Routes        []secretroute.Route
	Justification string
	// Raw is the request as the server sent it, as indented JSON.
	Raw string
	// AskedTTL is how long the agent asked to keep the credential, and the
	// lifetime the approval opens on. Zero is no ask, never forever.
	AskedTTL time.Duration
	// Delegate is an ask to delegate the credential to other discoboxes rather
	// than to use it; approving it binds it to the asking discobox to give on.
	Delegate bool
	Created  time.Time
	// Refresh is set on an item that asks for a new value of a token the
	// project already holds, because it went stale or was refused
	// (ADR 26-09-25-122 §4). Name is then the secret's name; the answer is a
	// value, not a binding.
	Refresh *RefreshAsk
	// Trust is set on an item that asks to trust a host for its discobox
	// rather than for a credential (ADR 0149). Host is then the endpoint,
	// Justification is why the agent says it needs it, and Name and EnvVar
	// are empty: the answer is not a secret but which certificate to pin.
	Trust *TrustAsk
}

// RefreshAsk is which token a refresh request wants a new value of, and why.
type RefreshAsk struct {
	SecretID string
	// Cause is "stale" — the value is past, or nearly past, its lifetime — or
	// "rejected" — an upstream refused it.
	Cause string
}

// ErrAlreadyAnswered is a renewal that lost the race to answer its refresh
// request: somebody else's value was written first, and nothing was written
// here. For the token it is a success.
var ErrAlreadyAnswered = errors.New("the refresh request was already answered")

// Renewal is a new value for a token, answering its refresh request. Either
// Command is run, on this machine, for the value, or Value is the value a
// person entered. Session says a permission given for the session answered
// it rather than a prompt, which the server records.
type Renewal struct {
	RequestID string
	SecretID  string
	Command   []string
	Value     string
	Session   bool
}

// TrustAsk is what a trust request offers to pin: the chain the pool's egress
// was shown, leaf first, and a CA the agent supplied, already checked by the
// pool to be one that chain verifies against.
type TrustAsk struct {
	Chain      []TrustCertificate
	SuppliedCA *TrustCertificate
	// DefaultPin is what the server pins when the approver names none, and
	// what the dialog offers first.
	DefaultPin TrustPin
}

// TrustCertificate is one certificate as a person reads it to decide.
type TrustCertificate struct {
	Subject    string
	Issuer     string
	Names      []string
	NotBefore  time.Time
	NotAfter   time.Time
	SHA256     string
	SPKISHA256 string
	IsCA       bool
	SelfSigned bool
}

// TrustPin names the certificate a trust verifies its host against: a CA the
// chain must verify to, or the host certificate's own public key.
type TrustPin struct {
	Kind   string
	SHA256 string
}

// Pin kinds, as the server names them.
const (
	TrustPinCA       = "ca"
	TrustPinLeafSPKI = "leaf-spki"
)

// TrustApproval is what a person decided about a trust request: which
// certificate to pin, and for how long. A trust always lapses, so TTLSeconds
// is never zero.
type TrustApproval struct {
	RequestID  string
	Pin        TrustPin
	TTLSeconds int64
}

// Secret is a project secret as the window needs it: enough to choose between
// them and to manage them, and never a value.
type Secret struct {
	ID   string
	Name string
	Type string
	// MaxTTL is the longest a binding of it may last, and the lifetime one
	// takes when nobody names one. Zero is no limit: bindings of this
	// credential may then last forever.
	MaxTTL time.Duration
	// AllowRaw is whether a person may hand a discobox its value itself
	// (ADR 26-10-27-840 §1), which an approval then offers.
	AllowRaw bool
	// Routes are where it may be sent, whoever holds it (ADR 26-10-27-880
	// §4): every request it is sent in must fall on one, the most specific
	// deciding. Empty sends it nowhere; `*` is any host (ADR 26-10-27-939).
	Routes  []secretroute.Route
	Created time.Time
	Updated time.Time

	// OAuth is what an oauth credential is, without being it. Nil for a token,
	// and never the tokens themselves.
	OAuth *SecretOAuth
	// Exchange is the same for an exchange credential: where its recipe
	// exchanges its fields, which fields it holds, and when its token goes
	// stale (ADR 26-10-08-452).
	Exchange *SecretExchange

	// RefreshCommand is the command a token suggests for its own renewal,
	// run only on a person's say-so (ADR 26-09-25-122). ValueTTL is how long a
	// value lasts, and StaleAt when the current one stops being trusted; zero
	// for a token that never goes stale.
	RefreshCommand []string
	ValueTTL       time.Duration
	StaleAt        time.Time

	// Format is the template the sentinels standing in for it are minted
	// from, and FormatSet whether a person chose it rather than it being read
	// from the value. A chosen one outlives the value it was set against.
	Format    string
	FormatSet bool
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

// SecretExchange is the half of an exchange credential that can be shown.
type SecretExchange struct {
	URL            string
	Fields         []string
	TokenExpiresAt time.Time
}

// Binding kinds: what a binding is on (ADR 26-10-27-880 §2), in the API's
// words.
const (
	BindingProject       = "project"
	BindingHarnessConfig = "harnessConfig"
	BindingSandbox       = "sandbox"
)

// NewBinding is a secret somebody is binding by hand, without a request to
// answer: to a discobox, a harness config, or the whole project.
type NewBinding struct {
	SecretID string
	// TargetKind is BindingProject, BindingHarnessConfig or BindingSandbox,
	// and TargetID the harness config or discobox; empty for the project.
	TargetKind string
	TargetID   string
	// Use delivers it in EnvVar; Delegate lets a discobox the binding covers
	// give it to the discoboxes it creates. At least one.
	Use      bool
	Delegate bool
	EnvVar   string
	// Routes narrow the secret's for this binding; none inherits them.
	Routes []secretroute.Route
	// TTLSeconds is how long it lasts. Zero asks for one that never lapses,
	// which a secret carrying a limit refuses.
	TTLSeconds int64
}

// SecretBinding is a secret made available to a target: existing and
// unexpired, it is the whole of the permission (ADR 26-10-27-880 §1).
type SecretBinding struct {
	ID         string
	SecretID   string
	SecretName string
	TargetKind string
	TargetID   string
	// Use is delivered in EnvVar; Delegate may be given on to the discoboxes
	// a discobox the binding covers creates.
	Use      bool
	Delegate bool
	EnvVar   string
	// RawDelivery is how the value itself is handed over, "env" or "file",
	// when it is rather than a sentinel (ADR 26-10-27-840).
	RawDelivery string
	// Routes narrow the secret's; none inherits them.
	Routes []secretroute.Route
	// ParentID is the delegation a discobox handed this on under; removing
	// that one removes this.
	ParentID  string
	CreatedBy string
	Created   time.Time
	// Expires is when the binding lapses; zero never does.
	Expires time.Time
}

// BindingFilter narrows a listing of bindings. Empty is every binding the
// server holds for the project.
type BindingFilter struct {
	// SecretID is the bindings of one secret.
	SecretID string
}

// SkillsListing is what the skills screen lists.
type SkillsListing struct {
	// Entries are the project's first, in the order a create adds them, then
	// client.yaml's.
	Entries []SkillsEntry
	// ClientFile is the client.yaml the directories are read from and written
	// to; empty when the client reads none.
	ClientFile string
}

// SkillsEntry is one skills source a new discobox gets.
type SkillsEntry struct {
	// Project is whether the project keeps it, on its server; otherwise it is
	// client.yaml's.
	Project bool
	// ID is the project entry's ID; empty for client.yaml's.
	ID string
	// Value is what it names, as --skills takes it: a Git URL[@REF][//PATH],
	// or an absolute directory.
	Value string
	// Label is Value as the window shows it, in the short spelling the add
	// form takes back (sourceref.Ref.Display); empty shows Value.
	Label string
	// Added is when the project entry was added; zero for client.yaml's.
	Added time.Time
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

	// MaxTTLSeconds is the ceiling on binding lifetimes the credential is
	// stored with. It is set here rather than left to a second call because it is a
	// fact about the credential — how long consent to it may last — and a
	// secret that exists for even a moment without one is a secret whose
	// bindings are limited by whatever the server's default happens to be.
	//
	// Zero is not absence: it is the answer "no limit", and bindings of the
	// credential may then last forever.
	MaxTTLSeconds int64

	// Routes are where it may be sent, whoever holds it; at least one.
	Routes []secretroute.Route

	// RefreshCommand makes the token one got from a command: when Value has no
	// token, the data source runs it here for the first one, and it is what a
	// person is offered to renew it with later. Nil is a token typed in.
	RefreshCommand []string
	// ValueTTLSeconds is how long a value lasts; nil takes the server's
	// default for a token with a command, and zero never goes stale.
	ValueTTLSeconds *int64

	// Format is the template its sentinels are minted from; empty reads it
	// from the value.
	Format string

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

	// Exchange is what an exchange credential stores, by its recipe's field
	// names; the server exchanges it for the token, so Token is then empty.
	Exchange map[string]string
}

// SecretUpdate is what an edit says about a secret. Every field is a pointer
// because absence has to be spelled differently from the values themselves:
// a zero limit allows bindings that never lapse, and an empty route list sends
// the secret nowhere, so neither can stand for "unchanged".
//
// The type is not among them. A token and an oauth credential are stored and
// renewed differently, and the server has no answer for a secret that changes
// from one to the other — that is a new credential, and deleting the old one is
// somebody's decision rather than a side effect of an edit.
type SecretUpdate struct {
	Name          *string
	MaxTTLSeconds *int64
	Value         *SecretValue
	// RefreshCommand replaces the command a token suggests for its renewal;
	// empty removes it.
	RefreshCommand *[]string
	// ValueTTLSeconds replaces how long a value lasts; zero never goes stale.
	ValueTTLSeconds *int64
	// Format sets the template its sentinels are minted from; empty goes back
	// to reading it from the value.
	Format *string
	// Routes replaces where it may be sent; empty sends it nowhere.
	Routes *[]secretroute.Route
}

// Approval is what a person decided about a request: which secret answers it,
// and how long the binding it makes lasts. Everything else — the discobox, the
// variable, the routes — follows the request, which is what the approver read.
type Approval struct {
	RequestID string
	SecretID  string
	// TTLSeconds is how long the binding lasts, and it is always said: zero
	// is forever, not "whatever the secret's limit happens to be". An approval
	// that left it out was making a binding whose lifetime nobody on this side
	// had seen. See credentials.go.
	TTLSeconds int64
	// Raw grants the value itself, "env" or "file", rather than a sentinel:
	// the person's choice, never the request's (ADR 26-10-27-840 §2).
	Raw string
}

// SourcePush is what one of a discobox's push-delivered sources did when the
// window last looked at it: what the local branch resolves to now, and whether
// that moved the discobox's origin.
//
// A source with nothing to send reports the commit both ends already hold and
// nothing else, so "checked, and there was nothing" and "sent" are the same
// shape.
type SourcePush struct {
	Slug   string
	Branch string
	// Remote is the remote the discobox's checkout sees Branch under:
	// discobox, or origin in a discobox created before a pool cached Git.
	Remote string
	// Commit is the local tip resolved for this source, whether or not it was
	// sent. It is what the window records against a failure, so the same
	// refused push is not attempted again every beat.
	Commit string
	// Pushed reports that the discobox's origin moved.
	Pushed bool
	// UpToDate reports that the origin already holds this commit, however it
	// got there. It is what releases a source the window is holding after a
	// refusal that somebody then answered by hand — `discobox push --force`
	// moves the lease rather than the branch, so nothing else about this
	// source would change.
	UpToDate bool
	// Err is why this source did not push. Having nothing to send is not one:
	// that is the ordinary answer, and it is not an error.
	Err error
}

// Issue is a GitHub issue or pull request as its page shows it, for the issue
// tab. Every string in it was written by whoever can comment on the issue, so
// the data source hands it over made safe to draw: no control character, and
// no format character a terminal acts on or that misleads — a bidirectional
// override or mark — reaches the screen as itself. Two kinds of text, two
// rules. What is written to be read on the page (a title, a body, a commit's
// message) may carry the zero-width joiner and non-joiner an emoji
// sequence and several scripts are made with, and has lost the invisible hints
// at where a line may break. Everything else is a name or code (a login, a
// branch, a label, a path, a diff, a hunk): every invisible character in it arrives
// escaped, so two that differ are never drawn as one.
//
// A body is markdown, and the character references in it are still to be
// decoded: whoever renders one passes it through markdownSource first.
type Issue struct {
	// Repository is owner/name.
	Repository string
	Number     int
	Title      string
	// State is "open" or "closed"; StateReason says how it closed —
	// completed, not_planned, duplicate — and is empty for an open one.
	State       string
	StateReason string
	// Type is the issue's type, where the organization uses them.
	Type      string
	Author    string
	Body      string
	CreatedAt time.Time
	Labels    []IssueLabel
	Assignees []string
	Milestone string
	Locked    bool
	// Comments is how many comments the issue has, whether or not the
	// timeline was read as far as all of them.
	Comments  int
	Reactions IssueReactions
	// PullRequest is set when the number is a pull request's — to GitHub every
	// pull request is an issue — and Pull is then the rest of what its page
	// shows.
	PullRequest bool
	Pull        *PullRequest
	// SubIssues are the issues this one is broken into, as far as they were
	// read; SubIssuesTotal and SubIssuesDone are GitHub's own count of them
	// and of those closed, which a list cut short does not change.
	SubIssues      []IssueRef
	SubIssuesTotal int
	SubIssuesDone  int
	// Timeline is everything that happened after it was opened, oldest first.
	Timeline []IssueEvent
	// Truncated is set when the timeline was longer than was read, or a pull
	// request's review comments were: the threads past there are not here.
	Truncated bool
}

// IssueLabel is one label, with the color its page draws it in: six hex
// digits, without the #.
type IssueLabel struct {
	Name  string
	Color string
}

// IssueRef is another issue or pull request, as a timeline event or a
// sub-issue list names it.
type IssueRef struct {
	// Repository is owner/name.
	Repository  string
	Number      int
	Title       string
	State       string
	PullRequest bool
	Merged      bool
}

// IssueReactions are the counts under a comment, keyed by GitHub's names:
// +1, -1, laugh, hooray, confused, heart, rocket, eyes.
type IssueReactions map[string]int

// IssueEvent is one item of the timeline. Kind is GitHub's own event name —
// "commented", "labeled", "closed", "cross-referenced" — so a kind the tab
// has no words for still says what it was.
type IssueEvent struct {
	Kind  string
	Actor string
	At    time.Time
	// Body and Reactions are a comment's.
	Body      string
	Reactions IssueReactions
	// Label is what a labeled or unlabeled event moved.
	Label *IssueLabel
	// Subject is who an assignment or a review request was about, the
	// milestone a milestoning moved, the reason a lock gave, or the commit a
	// reference, a close, a merge or a push came from.
	Subject string
	// Message is a pushed commit's subject line.
	Message string
	// Review is what a review said — approved, changes_requested, commented,
	// dismissed — with its summary in Body, and Threads are the conversations
	// it started on the diff.
	Review  string
	Threads []ReviewThread
	// From and To are a rename's titles.
	From, To string
	// StateReason is how a close closed it.
	StateReason string
	// Source is the issue or pull request a cross-reference came from.
	Source *IssueRef
}

// PullRequest is what a pull request's page shows beyond the issue it also is.
type PullRequest struct {
	Draft    bool
	Merged   bool
	MergedBy string
	MergedAt time.Time
	// Head and Base are the branches as the page names them, and HeadSHA the
	// commit that was read: the one a merge from here merges. BaseSHA is
	// where the base was at the same read, which with HeadSHA is what the
	// files changed are measured between.
	Head, Base string
	HeadSHA    string
	BaseSHA    string
	// HeadRef is the head's branch alone — Head without a fork's owner — as
	// the repository it is in, and a discobox working on it, names it.
	HeadRef      string
	Commits      int
	Additions    int
	Deletions    int
	ChangedFiles int
	// Mergeable is GitHub's word for whether it can merge: clean, dirty
	// (conflicts), behind, blocked, unstable, draft, or unknown.
	Mergeable string
	// MergeMethods are the ways the repository merges — merge, squash, rebase
	// — and empty when it would not say.
	MergeMethods []string
	Reviewers    []PullReviewer
	// Checks are the head commit's, and ChecksPartial is set when not all of
	// them could be read: the ones here are not the whole list.
	Checks        []PullCheck
	ChecksPartial bool
}

// PullReviewer is one of the page's reviewers. State is "requested" for one
// yet to answer, and otherwise what their last review said: approved,
// changes_requested, commented, or dismissed.
type PullReviewer struct {
	Name  string
	State string
}

// PullCheck is one check on the head commit. State is pending, success,
// failure, canceled, skipped, or neutral; URL is its own page, when it has
// one.
type PullCheck struct {
	Name  string
	State string
	URL   string
}

// ReviewThread is one conversation a review started on a line of the diff.
type ReviewThread struct {
	Path string
	Line int
	// Outdated is set when the diff has moved on from under it.
	Outdated bool
	// Hunk is the diff it was written against, ending at its line.
	Hunk     string
	Comments []ReviewComment
}

// ReviewComment is one comment of a review thread.
type ReviewComment struct {
	Author    string
	At        time.Time
	Body      string
	Reactions IssueReactions
}

// ErrNotRunning is what Execs and Services answer for a discobox that is not
// running. The server answers those reads from what the discobox last posted
// rather than start it (ADR 26-10-11-657), and the workspace must not take
// that for what is in the discobox now: it opens a tab for every live session
// a listing reports, and a session the discobox last reported running went
// down with it. Attaching would also start the discobox, and the poll must
// never be what does that — a discobox stopped under an open workspace stays
// stopped.
var ErrNotRunning = errors.New("the discobox is not running")

type DataSource interface {
	// Session is read once at startup, and is what the header and the run
	// options panel are drawn from.
	Session(ctx context.Context) (Session, error)

	// SaveDraft records the prompt as it stands, so that closing the window
	// mid-sentence does not throw the sentence away: Session hands it back the
	// next time a window opens. An empty prompt drops the draft — there is
	// nothing left to come back to.
	SaveDraft(ctx context.Context, prompt string) error

	// SaveView records the header's filters, so the next window opens on
	// them: Session hands them back as Session.View. The zero view is the
	// default, and saving it drops what was kept.
	SaveView(ctx context.Context, view ListView) error

	// SaveLastSource records the source a create was just cut from, as the
	// Source row spells it, so the next window opens cutting from it: Session
	// hands it back as Session.LastSource.
	SaveLastSource(ctx context.Context, source string) error

	// MarkWelcomed records that the project has shown its introduction, so no
	// window on it opens on the welcome again. It is the project that
	// remembers, not this machine: see welcome.go.
	MarkWelcomed(ctx context.Context) error

	// List is the project's sandboxes, newest-created first, across every
	// server the window lists, with the servers that did not answer
	// (ADR 0116 §4). It is polled, so it answers with what the servers have
	// said rather than waiting on every one of them: the same rows may come
	// back twice, and a server that is slow is listed when it answers.
	List(ctx context.Context) (Listing, error)

	// Resources is what Discobox has on this machine and what it is using of
	// it, polled on the same beat as List. It is separate from List because it
	// is true with no discoboxes at all — which is exactly when someone is
	// deciding whether to start one.
	Resources(ctx context.Context) (Resources, error)

	// Run creates a sandbox and delivers its source, which is what Enter does.
	// It reports each step it passes through — the steps are this client's own
	// work, so nothing else can say which one is underway — and the window puts
	// them where it says what it is busy with.
	//
	// warn is told what the create left out or found wrong that the person
	// should hear about and that is no step of the work: uncommitted changes it
	// did not carry in, a repository settings file it could not read all of.
	// The window keeps it on screen past the steps that follow it.
	//
	// accepted is called once, the moment the server has taken the create and
	// before the source is delivered: from then on the discobox exists whatever
	// the rest of the run does, so the prompt it was given can no longer be
	// lost and the window lets go of it. A create the server refused never
	// calls it.
	Run(ctx context.Context, req RunRequest, report, warn func(string), accepted func()) (Sandbox, error)

	// WatchProvisioning reports what a sandbox that is not usable yet is being
	// made to do, until ctx ends. It blocks, and is meant to run beside a wait
	// rather than instead of one: an attach blocks until every tier reports
	// ready, and this is the only thing that can say what it is blocked on.
	//
	// It reports nothing for a sandbox with nothing left to provision, so a
	// window that starts one against a running discobox draws no line at all.
	WatchProvisioning(ctx context.Context, sandboxID string, report func(string))

	// DeliverSource delivers the source a discobox is still waiting for, when
	// this machine can, and does nothing otherwise: the delivery `discobox push`
	// performs against a parked discobox, run by an attach before it dials
	// (ADR 26-09-24-005). report is told each step as it begins. It returns once the
	// discobox is free to start, or with why it could not be delivered.
	DeliverSource(ctx context.Context, sandboxID string, report func(string), credentials CredentialUI) error

	// Workspace reports what a create would carry in from the source directory,
	// so the window can ask the questions still open before it creates anything.
	Workspace(ctx context.Context, source string) (SourceWorkspace, error)

	// ResolveSource checks a source named by hand — a directory, a repository
	// URL, or DIR@REF — and reports it the way the create path will take it:
	// with ~ expanded and the directory made absolute, and whatever ref was
	// typed left on the end. A local directory that is not there is an error
	// rather than a value, because the window is still holding the field it
	// was typed into and this is the last point at which it can be corrected;
	// the alternative is a create that fails on it seconds later, with the
	// field long gone and the path to retype from memory. It comes back with
	// where a discobox cut from it here is filed, since a directory's key is
	// its repository root's and not what was typed.
	ResolveSource(ctx context.Context, source string) (Source, error)

	// MeasureDirectory starts counting what copying dir into a discobox would
	// carry. total reports the running count and is polled while the question
	// about it is on screen; stop ends a walk whose question has been answered,
	// and is safe to call on one that already finished.
	MeasureDirectory(ctx context.Context, dir string) (total func() DirectoryTotal, stop func())

	// Do runs a lifecycle verb against one sandbox.
	Do(ctx context.Context, verb Verb, sandboxID string, credentials CredentialUI) error

	// Rename gives one sandbox a new name: the one in its meta, which its row
	// shows from then on whatever its harness titles the terminal. It is not a
	// Verb: a verb is a word the window already has, and this one needs the
	// name typed first.
	Rename(ctx context.Context, sandboxID, name string) error

	// Tools is every tool one sandbox can be worked on with: the CLI's and the
	// user's, merged with what the sandbox's image and primary source declare.
	// It asks the sandbox, so it is a call rather than a field of the listing.
	Tools(ctx context.Context, sandboxID string) ([]Tool, error)

	// RunHostTool hands one sandbox to a tool that runs on this machine.
	//
	// It is neither a Verb nor an Interaction: it changes nothing about the
	// sandbox, and it takes no terminal — the tool is a separate program,
	// usually in a separate window, and this returns once it has been handed
	// the sandbox. The window stays exactly where it was, which is the point:
	// the terminal and the editor are two views of one sandbox, open at once.
	RunHostTool(ctx context.Context, sandboxID, toolID string) error

	// PushSources sends this machine's new commits into the origin repositories
	// the discobox's push-delivered sources fetch from — the transport
	// `discobox push` performs, with no flags (ADR 0058 §5, ADR 26-09-05-008 §3 on
	// automatic push). Nothing in the discobox moves: it gains origin/<branch>,
	// and whoever is working in it rebases when they choose.
	//
	// held names, per source slug, a commit whose push has already failed.
	// Such a source is resolved but not sent again while it names that same
	// commit, so a standing refusal — a stale lease, an unrelated history —
	// costs a ref read rather than a rejected transfer on every beat.
	//
	// It is called only for a discobox whose row is Pushable, and it is
	// answered locally when nothing has been committed since the last call:
	// resolving a branch tip against the lease dials nothing.
	//
	// The returned error is the call failing as a whole — the discobox could
	// not be read, the git route could not be opened. A source that could not
	// be pushed reports on its own SourcePush instead, because the others
	// still can be.
	PushSources(ctx context.Context, sandboxID string, held map[string]string) ([]SourcePush, error)

	// Harness changes are safe previews; writes always name the reviewed candidate.
	HarnessChanges(ctx context.Context, sandboxID string) (HarnessChanges, error)
	SaveHarnessChanges(ctx context.Context, sandboxID, candidateID string, force bool) (string, error)
	DiscardHarnessChanges(ctx context.Context, sandboxID, candidateID string) (string, error)

	// Harnesses is one server's harnesses, oldest first, which is the order
	// they were registered in. It is read at startup as well as by the harnesses
	// screen, for the server the next create goes to: the run options offer
	// what it reports, so enabling one makes it selectable without reopening
	// the window.
	//
	// Every harness and secret call names its server by the name the window
	// lists it under, empty or the primary's for the primary (ADR 0131 §3):
	// the configuration they read and change is per server, and nothing in
	// their arguments says which one otherwise.
	Harnesses(ctx context.Context, server string) ([]Harness, error)

	// HarnessSecrets is one harness's environment variables with the project
	// secret bound to each, for the config card. It is a request of its own, so it
	// is made when the card is opened rather than for every row of a listing.
	HarnessSecrets(ctx context.Context, server, harnessID string) ([]HarnessSecret, error)

	// DoHarness runs a lifecycle verb against one harness.
	DoHarness(ctx context.Context, server string, verb HarnessVerb, harnessID string) error

	// OpenHarnessConfigure starts the harness's interactive setup on a terminal
	// the window draws in a dedicated configuration pane.
	OpenHarnessConfigure(ctx context.Context, server, harnessID string, cols, rows int) (Terminal, error)

	// LocalPortsInUse reports which of the given local ports cannot be bound
	// on this machine right now. The window asks just before it opens a
	// configure flow that declares ports (Harness.ConfigPorts), so the pane's
	// header can carry the harness's own warning for as long as the flow runs
	// — the flow itself binds them a moment later and says the same thing in
	// its terminal, where a full-screen sign-in then paints over it.
	LocalPortsInUse(ctx context.Context, ports []int) []int

	// EditHarnessFile opens one of the harness's files in the user's editor and
	// saves what it wrote back, reporting whether anything changed. The window
	// is suspended for it, for the same reason.
	EditHarnessFile(ctx context.Context, server, harnessID, path string, stdin io.Reader, stdout, stderr io.Writer) (bool, error)

	// Execs is a snapshot of a sandbox's exec sessions. The workspace polls it
	// while it is open; it is a snapshot rather than a stream because the
	// control plane has no exec event stream yet — exec state lives on the
	// sandbox, proxied through — and the seam is shaped so a stream can
	// replace the poll without moving anything else.
	//
	// It answers ErrNotRunning for a sandbox that is not running, and so does
	// Services: see there.
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

	// FollowAudit reads one discobox's audit trails as one timeline, the way
	// `discobox admin audit ls --follow` reads them: the newest records,
	// delivered oldest first, and then each record as it is recorded, until
	// ctx ends. It blocks, and report is called from the goroutine running it.
	// A trail that cannot be read is named in an update rather than failing
	// the rest; an error is the whole read failing.
	FollowAudit(ctx context.Context, sandboxID string, report func(AuditUpdate)) error

	// AuditDetail is one record of that timeline in full, as `discobox admin
	// audit get` prints it. It is a call of its own because the timeline
	// carries a line per record and a record can be a page of headers.
	AuditDetail(ctx context.Context, sandboxID, recordID string) (AuditRecordDetail, error)

	// AuditBody is one recording of an http record — a body, or an upgraded
	// stream — as `discobox admin audit http --body` prints it, escaped for
	// the terminal. A body is unbounded, so a long one comes back cut, saying
	// so and how to read the rest.
	AuditBody(ctx context.Context, sandboxID, recordID, part string) (string, error)

	// Issue is one GitHub issue as its page shows it — the issue, its
	// sub-issues, and its whole timeline of comments and events, and for a
	// pull request its reviewers, checks, review threads and diff (Issue.Pull)
	// — read as the person at this machine. repository is the web address Sandbox.Repository
	// carries. Without full, the data source may answer a re-read from what it
	// read before when GitHub says nothing has changed since; full is a read
	// someone asked to see now.
	Issue(ctx context.Context, repository string, number int, full bool) (Issue, error)

	// CommentOnIssue posts a comment on a GitHub issue or pull request, as the
	// person at this machine.
	CommentOnIssue(ctx context.Context, repository string, number int, body string) error

	// SetIssueState closes a GitHub issue or pull request, or reopens it, as
	// the person at this machine: state is "closed" or "open", and reason how
	// an issue closed — "completed", "not_planned" — or what reopened it
	// ("reopened"). A pull request has no reason.
	SetIssueState(ctx context.Context, repository string, number int, state, reason string) error

	// MergePullRequest merges a GitHub pull request, as the person at this
	// machine: method is "merge", "squash" or "rebase", and sha the head commit
	// that was read (PullRequest.HeadSHA), so a branch that has moved on since
	// is refused rather than merged unseen.
	MergePullRequest(ctx context.Context, repository string, number int, method, sha string) error

	// PullReview creates, attaches and starts discobox-review in the sandbox
	// over a pull request's files changed: from pull's base to its head
	// (PullRequest.BaseSHA, HeadSHA), read from the commits the sandbox has,
	// under the review its git directory keeps. It is labeled as one
	// (Exec.FilesChanged), which keeps the workspace from opening it as a tab:
	// the issue tab that asked draws it, and ends it.
	PullReview(ctx context.Context, sandboxID string, pull PullRequest, cols, rows int) (Exec, Terminal, error)

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

	// NewTool does the same for a tool session: the tool's command, run in the
	// sandbox's primary source directory, labeled with the tool it is so that
	// this window — and the next one to attach — can tell it from a shell.
	// Which tool that is, is looked up again by id, so what runs is what the
	// declarations say now.
	//
	// The tool's files are put in place first, and only where the discobox has
	// none of its own. A tool whose files could not be delivered does not
	// start: an editor that silently comes up unconfigured is worse than one
	// that says why it did not.
	NewTool(ctx context.Context, sandboxID, toolID string, cols, rows int) (Exec, Terminal, error)

	// EndExec ends one exec session in the sandbox, killing what is running in
	// it. It is what ending a shell, a tool or an extra terminal does (endPane),
	// and the one place this window ends a session rather than closing its own
	// view of one.
	EndExec(ctx context.Context, sandboxID, execID string) error

	// PutImage writes an image pasted on this machine into the sandbox, under
	// a fresh name built from name, and answers with the absolute path it is
	// at there. It is how a pasted screenshot reaches an agent that cannot see
	// this machine's clipboard or files (imagepaste.go): the path is what is
	// pasted in its place.
	PutImage(ctx context.Context, sandboxID, name string, data []byte) (string, error)

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

	// Addresses is how to reach one discobox from a shell on this machine.
	//
	// Resolving them refreshes the project's managed ssh_config, the same file
	// `discobox admin ssh-config --write` owns, so what comes back is a
	// command that works rather than one that would have worked if something
	// else had been run first. That is also why it is a call and not a field
	// on Sandbox: it writes, it can fail, and it is worth neither on every row
	// of a listing nor on every poll.
	Addresses(ctx context.Context, sandboxID string) (Addresses, error)

	// CredentialRequests is every request in the project still waiting on a
	// person — for a credential, or to trust a host (ADR 0149 §7) — on every
	// server the window lists, newest first,
	// each naming its server (ADR 0131 §1). A server that did not answer
	// contributes none. It is polled with the listing rather
	// than streamed: the client-facing event stream is gone (ADR 0061), and a
	// request is answered on human time anyway.
	CredentialRequests(ctx context.Context) ([]CredentialRequest, error)

	// SecretRejections is every credential an upstream has refused that its
	// server cannot renew (ADR 0132), on every server the window lists, each
	// naming its server — the same reach the credential inbox has (ADR 0131
	// §1), because a discobox on a second server takes the same 401 and is
	// fixed with that server's own harness or secret. A server that did not
	// answer contributes none. It is polled beside the inbox, and for the same
	// reason: a dead credential is dealt with on human time, and the
	// client-facing event stream is gone (ADR 0061).
	SecretRejections(ctx context.Context) ([]SecretRejection, error)

	// Skills is the skills every new discobox gets from this window: the
	// project's skills sources on one server, in the order a create adds them,
	// then client.yaml's run.skills, which are this machine's whatever server
	// the window is on (ADR 26-10-14-894).
	Skills(ctx context.Context, server string) (SkillsListing, error)

	// AddSkills files one value where it belongs, read as --skills reads it: a
	// Git URL[@REF][//PATH] in the project's skills sources, a directory in
	// client.yaml.
	AddSkills(ctx context.Context, server, value string) (SkillsEntry, error)

	// RemoveSkills takes one entry out of where it is kept.
	RemoveSkills(ctx context.Context, server string, entry SkillsEntry) error

	// Secrets is one server's secrets, for choosing which one answers a
	// request and for the secrets screen. It never carries a value.
	Secrets(ctx context.Context, server string) ([]Secret, error)

	// CreateSecret stores a credential typed into the approval dialog and
	// returns it, so the approval that follows has a secret to name.
	CreateSecret(ctx context.Context, server string, secret NewSecret) (Secret, error)

	// UpdateSecret changes what a secret says about itself: its name, where
	// it may be sent, how long consent to it may last, and the credential
	// itself. Everything left nil is left alone.
	//
	// It is one call because it is one endpoint and, from the window, one form:
	// a card whose rows were saved by a call each would half-apply when the
	// second one failed, and would report two things where a person did one.
	UpdateSecret(ctx context.Context, server, secretID string, update SecretUpdate) error

	// Bindings lists the bindings the filter names: of one secret, or — with
	// an empty filter — every one the server holds for the project.
	Bindings(ctx context.Context, server string, filter BindingFilter) ([]SecretBinding, error)

	// CreateBinding binds a secret without a request behind it: the binding
	// an operator makes because they already know the answer.
	CreateBinding(ctx context.Context, server string, binding NewBinding) (SecretBinding, error)

	// DeleteBinding takes one binding back, with every binding a discobox
	// handed on under it. The credential stops resolving at once — the request
	// that produced it stays approved, because it is history. It returns how
	// many handed-on bindings went with it.
	DeleteBinding(ctx context.Context, server, bindingID string) (int, error)

	// DeleteSecret removes a secret and every binding of it.
	DeleteSecret(ctx context.Context, server, secretID string) error

	// ApproveCredentialRequest answers a request yes, binding the secret to
	// the discobox that asked. The server takes the variable and the routes
	// from the request itself.
	ApproveCredentialRequest(ctx context.Context, server string, approval Approval) error

	// DenyCredentialRequest answers a request no. It is a complete answer, not
	// a dismissal: the asking agent is waiting on one.
	DenyCredentialRequest(ctx context.Context, server, requestID string) error

	// RefreshSecret writes a new value for a token, answering its refresh
	// request: running the renewal's command here for it, or sending the value
	// a person entered.
	RefreshSecret(ctx context.Context, server string, renewal Renewal) error

	// DismissRefresh closes a refresh request without writing a value.
	DismissRefresh(ctx context.Context, server, requestID string) error

	// ApproveTrustRequest answers a trust request yes, pinning the chosen
	// certificate for the asking discobox alone (ADR 0149).
	ApproveTrustRequest(ctx context.Context, server string, approval TrustApproval) error

	// DenyTrustRequest answers a trust request no.
	DenyTrustRequest(ctx context.Context, server, requestID string) error
}

// where is where a request asks to reach, as a person reads it: every route a
// credential request names, or a trust's endpoint.
func (r CredentialRequest) where() string {
	if len(r.Routes) > 0 {
		return routesText(r.Routes)
	}
	return r.Host
}

// whereRows is where, one route to a row, for a card's field: a column of
// routes is read route by route, where a "; "-joined line of them is not.
func (r CredentialRequest) whereRows() string {
	if len(r.Routes) > 0 {
		return routeRows(r.Routes)
	}
	return r.Host
}

// routesText is routes as a person types them, each as secretroute.Format
// writes it, joined by "; ": a comma is part of a route, between its hosts,
// methods, paths, fields or tools.
func routesText(routes []secretroute.Route) string {
	return strings.Join(formatRoutes(routes), "; ")
}

// routeRows is routes as routesText writes them, one to a row.
func routeRows(routes []secretroute.Route) string {
	return strings.Join(formatRoutes(routes), "\n")
}

func formatRoutes(routes []secretroute.Route) []string {
	out := make([]string, 0, len(routes))
	for _, route := range routes {
		out = append(out, secretroute.Format(route))
	}
	return out
}
