package bridge

import (
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The wire (ADR 26-11-07-289 §2–3): every JSON message either side sends, as
// one envelope naming its type and carrying that type's body under the same
// name. These structs are the wire: its JSON Schema (wire.schema.json) is
// generated from them, and the app's Rust types from the schema (genwire,
// generate.go). No field is ever null: a list is [] when empty, and a value
// that is absent is omitted.

// Protocol is the bridge's version, carried by hello and welcome. The CLI
// pins the exact app it runs (983 §2), so two that differ are a development
// override: the app says which side is older and exits. 3 made the listing
// the core's list (rows, notes, the filter and its choices); 4 added the
// commands and palette topics; 5, a workspace's header (the header topic, in
// place of the bare forwards) and what a session's ending does (StreamEnded).
const Protocol = 5

// Type names a message.
type Type string

// The messages, and which side sends each.
const (
	// TypeHello opens the bridge (app).
	TypeHello Type = "hello"
	// TypeWelcome answers it with the protocol, the spec and the session (CLI).
	TypeWelcome Type = "welcome"
	// TypeSubscribe and TypeUnsubscribe start and stop a topic (app).
	TypeSubscribe   Type = "subscribe"
	TypeUnsubscribe Type = "unsubscribe"
	// TypeSnapshot is a topic's value, whole, on subscribe and on every
	// change (CLI).
	TypeSnapshot Type = "snapshot"
	// TypeCommand runs a registry command (app).
	TypeCommand Type = "command"
	// TypeResult ends a command or an upload (CLI).
	TypeResult Type = "result"
	// TypeForm is a form a command needs filled (CLI); TypeSubmit fills it
	// (app); TypeFieldErrors is the core's refusal of what was submitted
	// (CLI).
	TypeForm        Type = "form"
	TypeSubmit      Type = "submit"
	TypeFieldErrors Type = "fieldErrors"
	// TypeQuestion is a question a running command asks (CLI); TypeAnswer
	// answers it (app).
	TypeQuestion Type = "question"
	TypeAnswer   Type = "answer"
	// TypeProgress is a running command's report, warning or acceptance
	// (CLI).
	TypeProgress Type = "progress"
	// TypeEdit is a file for the app to open in the user's editor (CLI);
	// TypeSaved is the app's word that it was saved (app).
	TypeEdit  Type = "edit"
	TypeSaved Type = "saved"
	// TypeUpload is a pasted or dropped image for a box (app).
	TypeUpload Type = "upload"
	// TypeOpen attaches a stream the app allocated to a session, and
	// TypeClose detaches it, ending nothing in the box (app).
	TypeOpen  Type = "open"
	TypeClose Type = "close"
	// TypeStream is a stream's state: attached, failed, reconnecting,
	// reconnected (CLI). Its output and its end are frames on the stream.
	TypeStream Type = "stream"
)

// Message is one JSON message: its type, and the body of that type.
type Message struct {
	Type Type `json:"type"`

	Hello       *Hello       `json:"hello,omitempty"`
	Welcome     *Welcome     `json:"welcome,omitempty"`
	Subscribe   *Subscribe   `json:"subscribe,omitempty"`
	Unsubscribe *Unsubscribe `json:"unsubscribe,omitempty"`
	Snapshot    *Snapshot    `json:"snapshot,omitempty"`
	Command     *Command     `json:"command,omitempty"`
	Result      *Result      `json:"result,omitempty"`
	Form        *Form        `json:"form,omitempty"`
	Submit      *Submit      `json:"submit,omitempty"`
	FieldErrors *FieldErrors `json:"fieldErrors,omitempty"`
	Question    *Question    `json:"question,omitempty"`
	Answer      *Answer      `json:"answer,omitempty"`
	Progress    *Progress    `json:"progress,omitempty"`
	Edit        *Edit        `json:"edit,omitempty"`
	Saved       *Saved       `json:"saved,omitempty"`
	Upload      *Upload      `json:"upload,omitempty"`
	Open        *Open        `json:"open,omitempty"`
	Close       *Close       `json:"close,omitempty"`
	Stream      *Stream      `json:"stream,omitempty"`
}

// Hello is the app's first message.
type Hello struct {
	Protocol int `json:"protocol"`
}

// Welcome answers hello. Spec and Session are sent only when the protocols
// match: another version's fields may not parse.
type Welcome struct {
	Protocol int           `json:"protocol"`
	Spec     *console.Spec `json:"spec,omitempty"`
	Session  *Session      `json:"session,omitempty"`
}

// Session is what the window is opened on.
type Session struct {
	// PrimaryServer is the configured server's name.
	PrimaryServer string `json:"primaryServer"`
	Project       string `json:"project,omitempty"`
	// Directory and Branch are where the CLI was run, which a create cuts
	// from.
	Directory string `json:"directory,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Host      string `json:"host,omitempty"`
}

// Topic is what a subscription follows.
type Topic string

// The topics.
const (
	// TopicListing is the boxes, across every server.
	TopicListing Topic = "listing"
	// TopicResources is what Discobox has on this machine and is using.
	TopicResources Topic = "resources"
	// TopicRequests is the credential and approval requests waiting on a
	// person, and the credentials refused.
	TopicRequests Topic = "requests"
	// TopicProvisioning is what a box that is not usable yet is being made
	// to do (Box).
	TopicProvisioning Topic = "provisioning"
	// TopicAudit is a box's audit timeline, followed (Box).
	TopicAudit Topic = "audit"
	// TopicHeader is what a box's workspace header says of it, as the core
	// says it (console.Header), with its ports forwarded to this machine for
	// as long as the subscription lasts: the workspace's forward, which goes
	// with the screen showing it (Box).
	TopicHeader Topic = "header"
	// TopicSessions is what a box's workspace opens: its sessions and
	// services, as the core orders and names them (Box).
	TopicSessions Topic = "sessions"
	// TopicCommands is every registry command as it stands where the app
	// asks from, for the boxes it names (Context, Targets): what its menus
	// list, enabled or with why not.
	TopicCommands Topic = "commands"
	// TopicPalette is what the palette finds for a query (Context, Palette):
	// the core's results, in the core's order.
	TopicPalette Topic = "palette"
)

// Subscribe starts following a topic. Sub is the app's, unique for the
// bridge's life; Box is the box key, for a topic about one box. Filter is
// what the listing is narrowed to, which a renderer holds as the TUI holds
// its List's (console/DESIGN.md, "The list"): the app subscribes again under
// a new Sub to change it, and ends the old. Without one the listing opens on
// the filter this machine's last window left (console.ListView). Reveal, a
// box's key, narrows it instead to what shows that box whatever the filter
// hid (console.Revealing): a view the palette navigates to, which is never
// saved over the person's filter.
//
// The commands and the palette are about where the app asks from (Context)
// and, for the commands, the boxes it names (Targets). They are read from the
// last listing, never the servers, and sent again as it changes; the app
// subscribes again under a new Sub when what it asks changes — each query of
// the palette, a menu opened somewhere else.
type Subscribe struct {
	Sub     uint32        `json:"sub"`
	Topic   Topic         `json:"topic"`
	Box     string        `json:"box,omitempty"`
	Filter  *Filter       `json:"filter,omitempty"`
	Reveal  string        `json:"reveal,omitempty"`
	Context *Context      `json:"context,omitempty"`
	Targets []string      `json:"targets,omitempty"`
	Palette *PaletteQuery `json:"palette,omitempty"`
}

// PaletteQuery is what the palette asks (console.PaletteQuery): the text
// typed, at most one chip — a command by its ID, or a box by its key — and
// whether archived boxes are in scope. Current is the key of the box the
// window is on, and Recent the boxes it has gone to, latest first: navigation
// is the app's, and what an empty query ranks by.
type PaletteQuery struct {
	Text     string   `json:"text,omitempty"`
	Command  string   `json:"command,omitempty"`
	Box      string   `json:"box,omitempty"`
	Archived bool     `json:"archived,omitempty"`
	Current  string   `json:"current,omitempty"`
	Recent   []string `json:"recent,omitempty"`
}

// Unsubscribe stops one.
type Unsubscribe struct {
	Sub uint32 `json:"sub"`
}

// Snapshot is a topic's value, whole. Error is why it could not be read this
// time; the last value stands until one can. Exactly one value is set for the
// topic, unless Error is.
type Snapshot struct {
	Sub   uint32 `json:"sub"`
	Topic Topic  `json:"topic"`
	Error string `json:"error,omitempty"`

	Listing      *Listing      `json:"listing,omitempty"`
	Resources    *Resources    `json:"resources,omitempty"`
	Requests     *Requests     `json:"requests,omitempty"`
	Provisioning *Provisioning `json:"provisioning,omitempty"`
	Audit        *Audit        `json:"audit,omitempty"`
	Header       *Header       `json:"header,omitempty"`
	Sessions     *Sessions     `json:"sessions,omitempty"`
	Commands     *Commands     `json:"commands,omitempty"`
	Palette      *Palette      `json:"palette,omitempty"`
}

// Commands is every registry command as it stands, in the registry's order.
type Commands struct {
	Entries []Entry `json:"entries"`
}

// Palette is what a query found, in the order the palette lists it. Archived
// is whether archived boxes were searched: asked for, or brought in by a chip
// that acts only on them, which the app keeps. Commit is the chip the query's
// text committed as it was typed (console.PaletteCommit): the rows are the
// committed query's, and the app takes its chip and text in place of its own.
// Partial and Stale are what the search says about the servers it searched
// (console.List.Partial, Stale): those it has no rows from yet or could not
// reach, and those whose rows are behind — every server, whatever the list's
// filter.
type Palette struct {
	Rows     []PaletteRow   `json:"rows"`
	Archived bool           `json:"archived,omitempty"`
	Commit   *PaletteCommit `json:"commit,omitempty"`
	Partial  []string       `json:"partial"`
	Stale    []Note         `json:"stale"`
}

// PaletteRow is one result: a box by its key, or a command as it stands; Why
// is why it cannot be chosen, when it cannot.
type PaletteRow struct {
	Box     string `json:"box,omitempty"`
	Command *Entry `json:"command,omitempty"`
	Why     string `json:"why,omitempty"`
}

// PaletteCommit is a command committed as the palette's chip, and the text
// left after it.
type PaletteCommit struct {
	Command string `json:"command"`
	Text    string `json:"text"`
}

// Listing is the box listing as the console shows it (console.List): every
// box listed, and the rows the filter shows, in the order they are drawn.
type Listing struct {
	// Boxes is every box listed, on every server and whatever the filter:
	// what a box is looked up in, and what the palette searches.
	Boxes []Box `json:"boxes"`
	// Rows are the boxes the filter shows, as drawn: folder sections when
	// Grouped, and in each a box followed by the boxes it created.
	Rows []Row `json:"rows"`
	// Grouped is whether the rows are drawn in sections, one per folder.
	Grouped bool `json:"grouped"`
	// Notes are what the list says about servers after its rows.
	Notes []Note `json:"notes"`
	// Filter is what the rows are narrowed to.
	Filter Filter `json:"filter"`
	// Label is how the filter reads: "all discoboxes", or what it narrows
	// the rows to. Narrowed is whether it leaves any box out by what it is
	// (console.List.Narrowed): an empty list then says nothing matches, not
	// that there is nothing.
	Label    string `json:"label"`
	Narrowed bool   `json:"narrowed,omitempty"`
	// Choices are every way to narrow the rows, counted against the filter
	// as it stands (console.List.FilterChoices).
	Choices []FilterChoice `json:"choices"`
	// Archived is how many more rows showing the archived would draw.
	Archived int `json:"archived"`
	// Unreachable are servers that did not answer; Waiting, ones still being
	// asked with nothing of theirs to show.
	Unreachable []string `json:"unreachable"`
	Waiting     []string `json:"waiting"`
}

// Row is one box's place in the list (console.List.Rows, Nesting): its
// section, how deep it is nested under the box that created it, and what it
// says in place of its name when picked out.
type Row struct {
	// Box is the box's key.
	Box string `json:"box"`
	// Section is the key of the folder section it is drawn in, and
	// SectionTitle how that section is headed.
	Section      string `json:"section"`
	SectionTitle string `json:"sectionTitle"`
	// Depth is how many creators it is nested under; Last, whether it is its
	// creator's last row; Rules, for each level between the top and its
	// creator, whether a branch carries on past it there.
	Depth int    `json:"depth"`
	Last  bool   `json:"last,omitempty"`
	Rules []bool `json:"rules"`
	// Remark is what the row says in place of its name when picked out.
	Remark *Remark `json:"remark,omitempty"`
	// Origin is the machine it was created on, when that is not this one.
	Origin string `json:"origin,omitempty"`
}

// Remark is console.Remark: the text, and the command that answers it when
// one does, with what doing so does as a verb. The app spells the command's
// key with its own binding.
type Remark struct {
	Text    string `json:"text"`
	Command string `json:"command,omitempty"`
	Verb    string `json:"verb,omitempty"`
}

// Note is a line the list draws about a server rather than a box.
type Note struct {
	Server string `json:"server"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Filter is what the listing is narrowed to (console.Filter): a server, a
// folder (by its key; none is every folder), the tags a box must carry, and
// whether the archived are listed.
type Filter struct {
	Server       string   `json:"server,omitempty"`
	FolderKey    string   `json:"folderKey,omitempty"`
	FolderLabel  string   `json:"folderLabel,omitempty"`
	FolderSource string   `json:"folderSource,omitempty"`
	FolderLocal  bool     `json:"folderLocal,omitempty"`
	Tags         []string `json:"tags"`
	Archived     bool     `json:"archived,omitempty"`
}

// FilterChoice is one way to narrow the listing (console.FilterChoice): the
// filter marking it makes, and for a marked tag the one letting it go.
type FilterChoice struct {
	Group   console.ChoiceGroup `json:"group"`
	Label   string              `json:"label"`
	Detail  string              `json:"detail"`
	Marked  bool                `json:"marked,omitempty"`
	Several bool                `json:"several,omitempty"`
	Mark    Filter              `json:"mark"`
	Unmark  Filter              `json:"unmark"`
}

// Box is one box as the app draws its row.
type Box struct {
	// Key is what the app names the box by in every message: its ID when one
	// server is configured, the server's name and the ID otherwise. Opaque to
	// the app, which shows ID.
	Key      string        `json:"key"`
	ID       string        `json:"id"`
	Server   string        `json:"server,omitempty"`
	Name     string        `json:"name"`
	State    console.State `json:"state"`
	Agent    string        `json:"agent,omitempty"`
	AgentMsg string        `json:"agentMsg,omitempty"`
	Harness  string        `json:"harness"`
	Source   string        `json:"source"`
	Tags     []string      `json:"tags"`
	Branch   string        `json:"branch,omitempty"`
	Commit   string        `json:"commit,omitempty"`
	// Dirty is uncommitted work; Ahead, clean commits beyond the spawn
	// commit (console.Sandbox.Uncommitted, Ahead).
	Dirty   bool `json:"dirty,omitempty"`
	Ahead   bool `json:"ahead,omitempty"`
	Added   int  `json:"added,omitempty"`
	Deleted int  `json:"deleted,omitempty"`
	// CPU and MemoryBytes are what a box that is up is using; none for one
	// that is not.
	CPU          int       `json:"cpu,omitempty"`
	MemoryBytes  int64     `json:"memoryBytes,omitempty"`
	Created      time.Time `json:"created"`
	Message      string    `json:"message,omitempty"`
	Provisioning string    `json:"provisioning,omitempty"`
	// Attachable is whether the box has a container to open now.
	Attachable bool `json:"attachable"`
}

// Resources is the machine's capacity and use. Known is false until there is
// something measured to show.
type Resources struct {
	Known          bool    `json:"known"`
	CPUVCPUs       float64 `json:"cpuVcpus"`
	CPUCapacity    float64 `json:"cpuCapacity"`
	MemoryBytes    int64   `json:"memoryBytes"`
	MemoryCapacity int64   `json:"memoryCapacity"`
	DiskKnown      bool    `json:"diskKnown"`
	DiskFreeBytes  int64   `json:"diskFreeBytes"`
	DiskDataBytes  int64   `json:"diskDataBytes"`
	DiskCacheBytes int64   `json:"diskCacheBytes"`
}

// Requests is what waits on a person.
type Requests struct {
	Requests   []Request   `json:"requests"`
	Rejections []Rejection `json:"rejections"`
}

// Request is one item waiting on a person. Box is the key of the box that
// asked, as the listing names it.
type Request struct {
	ID            string      `json:"id"`
	Box           string      `json:"box"`
	Server        string      `json:"server,omitempty"`
	Kind          RequestKind `json:"kind"`
	Name          string      `json:"name,omitempty"`
	EnvVar        string      `json:"envVar,omitempty"`
	Host          string      `json:"host,omitempty"`
	Justification string      `json:"justification,omitempty"`
	Created       time.Time   `json:"created"`
}

// RequestKind is what a request waits on a person for.
type RequestKind string

// The request kinds.
const (
	// RequestCredential is a credential the box asked for.
	RequestCredential RequestKind = "credential"
	// RequestRefresh is a credential to sign in again for.
	RequestRefresh RequestKind = "refresh"
	// RequestTrust is a host whose certificate the box asked to trust.
	RequestTrust RequestKind = "trust"
	// RequestAdoption is a box asking to be adopted.
	RequestAdoption RequestKind = "adoption"
	// RequestAPIAccess is a box asking for access to the Discobox API.
	RequestAPIAccess RequestKind = "apiAccess"
)

// Rejection is a credential refused where a person is needed. Boxes are the
// listed boxes it affects (console.RejectionsFor): a harness's credential is
// refused for every box running that harness.
type Rejection struct {
	Boxes      []string  `json:"boxes"`
	Server     string    `json:"server,omitempty"`
	SecretID   string    `json:"secretId"`
	SecretName string    `json:"secretName"`
	Host       string    `json:"host"`
	Reason     string    `json:"reason"`
	EnvName    string    `json:"envName,omitempty"`
	LastSeen   time.Time `json:"lastSeen"`
}

// Provisioning is what a box is being made to do now; empty once nothing is
// reported.
type Provisioning struct {
	Step string `json:"step"`
}

// Audit is a box's audit timeline: its most recent records, oldest first,
// and the trails the last read could not reach.
type Audit struct {
	Records []AuditRecord `json:"records"`
	Missing []string      `json:"missing"`
}

// AuditRecord is one line of the timeline.
type AuditRecord struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Source       string    `json:"source"`
	Summary      string    `json:"summary"`
	Group        string    `json:"group,omitempty"`
	GroupSummary string    `json:"groupSummary,omitempty"`
}

// Header is what a box's workspace header says (console.Header): each
// field the core's answer, their order and colors the app's.
type Header struct {
	ID string `json:"id"`
	// Work is the issue and the pull request the box's tags number, each a
	// link to it.
	Work []Link `json:"work"`
	// Blocked is what a box waiting on a person says in place of the git
	// word, empty for one that is not.
	Blocked string `json:"blocked,omitempty"`
	// Base is the work's git position; Changes its state as a word (dirty,
	// ready, clean), empty when there is nothing to say. Tone is how both
	// read, and Ready a ready pull request over ready work, which reads as
	// done.
	Base    string  `json:"base,omitempty"`
	Changes string  `json:"changes,omitempty"`
	Tone    GitTone `json:"tone"`
	Ready   bool    `json:"ready,omitempty"`
	// Diff is whether the box has reported a change, and Added and Deleted
	// the lines it changed.
	Diff    bool `json:"diff,omitempty"`
	Added   int  `json:"added,omitempty"`
	Deleted int  `json:"deleted,omitempty"`
	// Desktop is the box's graphical desktop, once its forward has bound it.
	Desktop *Link `json:"desktop,omitempty"`
	// Ports is what the box serves, by protocol, web first; a forwarded web
	// port links to its local end.
	Ports []PortGroup `json:"ports"`
	// Tags is the box's tags the header says nothing of otherwise, labeled.
	Tags []string `json:"tags"`
	// Unforwarded is why the box's ports could not be forwarded, when they
	// could not: they are listed, unlinked.
	Unforwarded string `json:"unforwarded,omitempty"`
}

// Link is something in the header a press opens in the browser; one without
// a URL is text.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// GitTone is how a header reads the work's git position and word.
type GitTone string

// The tones.
const (
	// ToneQuiet is clean work, or work nothing is known about.
	ToneQuiet GitTone = "quiet"
	// ToneDirty is uncommitted work.
	ToneDirty GitTone = "dirty"
	// ToneAhead is committed work beyond what the box was cut from.
	ToneAhead GitTone = "ahead"
)

// PortGroup is one protocol's ports, in port order.
type PortGroup struct {
	Protocol string `json:"protocol"`
	Ports    []Link `json:"ports"`
}

// Sessions is what a box's workspace opens, by the core's rules: its
// sessions in tab order, the primary first, and its declared services.
// Running is false for a box that is not running: only its primary opens,
// and attaching it is what waits for the box.
type Sessions struct {
	Running  bool      `json:"running"`
	Sessions []Tab     `json:"sessions"`
	Services []Service `json:"services"`
}

// Tab is one session as the app draws its tab. Kind is the column and what it
// is.
type Tab struct {
	Exec  string   `json:"exec"`
	Kind  TabKind  `json:"kind"`
	Title string   `json:"title"`
	Live  bool     `json:"live"`
	Argv  []string `json:"argv"`
}

// TabKind is what a session is, which says the column its tab is in.
type TabKind string

// The tab kinds.
const (
	// TabPrimary is the box's primary session, its harness.
	TabPrimary TabKind = "primary"
	// TabTerminal is another harness terminal.
	TabTerminal TabKind = "terminal"
	// TabShell is a shell.
	TabShell TabKind = "shell"
	// TabTool is a tool's session.
	TabTool TabKind = "tool"
	// TabService is a declared service's session.
	TabService TabKind = "service"
)

// Service is one declared service.
type Service struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Exec is the session it runs in, when it has one.
	Exec   string `json:"exec,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Command runs a registry command. ID is the app's, unique for the bridge's
// life, and is what its result, forms, questions and progress name. Targets
// are box keys, explicit: a box command never falls back to what is on
// screen.
type Command struct {
	ID      uint64   `json:"id"`
	Command string   `json:"command"`
	Targets []string `json:"targets"`
	Context Context  `json:"context"`
	Args    Args     `json:"args"`
}

// Context is where the command is asked from (console.Context): navigation
// is the app's, and the core is handed the parts of it its rules read.
type Context struct {
	// Workspace is the key of the box whose workspace the window holds.
	Workspace      string `json:"workspace,omitempty"`
	HasWorkspace   bool   `json:"hasWorkspace,omitempty"`
	WorkspaceShown bool   `json:"workspaceShown,omitempty"`
	PaneFocused    bool   `json:"paneFocused,omitempty"`
	ListHasKeys    bool   `json:"listHasKeys,omitempty"`
	Tree           bool   `json:"tree,omitempty"`
	TreeShown      bool   `json:"treeShown,omitempty"`
	TreeFits       bool   `json:"treeFits,omitempty"`
	PageShown      bool   `json:"pageShown,omitempty"`
}

// Args are what a command takes beyond its targets: the stream a session it
// makes is attached to and the size of its pane (shell, terminal), and the
// session it acts on (end, repaint).
type Args struct {
	Stream uint32 `json:"stream,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Exec   string `json:"exec,omitempty"`
}

// Status is how a command ended.
type Status string

// The statuses.
const (
	// StatusDone is a command the core carried out; Note says what came of
	// it.
	StatusDone Status = "done"
	// StatusFailed is one refused or that failed; Why says why.
	StatusFailed Status = "failed"
	// StatusCanceled is one whose form or question was dismissed.
	StatusCanceled Status = "canceled"
	// StatusEnter is one whose flow is the app's: the core has ruled it can
	// run, and the app enters it — moving focus, opening a page or a menu,
	// opening the box's workspace (Box).
	StatusEnter Status = "enter"
)

// Result ends a command (or an upload, by its ID).
type Result struct {
	ID     uint64 `json:"id"`
	Status Status `json:"status"`
	Why    string `json:"why,omitempty"`
	Note   string `json:"note,omitempty"`
	// Box is the box the flow entered is about.
	Box string `json:"box,omitempty"`
	// Session is the session a shell or terminal made, attached to the
	// command's stream.
	Session *Tab `json:"session,omitempty"`
	// Entries are what the palette and the action menu list, as they stand
	// for the command's context and targets.
	Entries []Entry `json:"entries,omitempty"`
	// Path is where an upload landed in the box.
	Path string `json:"path,omitempty"`
}

// Entry is one command as it stands: whether it can act, and why not. Uses is
// how often this window has used it, which ranks the palette.
type Entry struct {
	Command string `json:"command"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Enabled bool   `json:"enabled"`
	Why     string `json:"why,omitempty"`
	Uses    int    `json:"uses,omitempty"`
}

// Form is a form a command needs filled, described by the core. ID is the
// CLI's; it is sent again, same ID, with Applies recomputed, after a draft is
// submitted.
type Form struct {
	ID      uint64  `json:"id"`
	Command uint64  `json:"command"`
	Title   string  `json:"title"`
	Fields  []Field `json:"fields"`
}

// Field is one field of a form (console.Field). Value is what it holds as
// the core reads it: as it opens (console.Field.Opens — a typed value as one
// line, a picker's chosen key), or after a draft what the answers so far make
// it. Applies is whether it takes an answer given the answers so far; one that
// does not is drawn with Why in place of its value.
type Field struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Section     string   `json:"section,omitempty"`
	Hint        string   `json:"hint,omitempty"`
	Choices     []Choice `json:"choices"`
	Value       string   `json:"value"`
	Placeholder string   `json:"placeholder,omitempty"`
	Suffix      string   `json:"suffix,omitempty"`
	Masked      bool     `json:"masked,omitempty"`
	Locked      bool     `json:"locked,omitempty"`
	Applies     bool     `json:"applies"`
	Why         string   `json:"why,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// Choice is one option of a picker field, or an answer to a question.
type Choice struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// Submit fills a form. Answers are the fields the person changed, by key;
// every other field answers what it opened holding, so an untouched field is
// never an edit. Cancel dismisses it. Draft asks only for the form again with
// Applies recomputed for these answers, and submits nothing.
type Submit struct {
	Form    uint64            `json:"form"`
	Answers map[string]string `json:"answers"`
	Cancel  bool              `json:"cancel,omitempty"`
	Draft   bool              `json:"draft,omitempty"`
}

// FieldErrors is why submitted answers were refused; the form stays up
// holding them. An error with no Field is about the answers as a whole.
type FieldErrors struct {
	Form   uint64       `json:"form"`
	Errors []FieldError `json:"errors"`
}

// FieldError is one refusal.
type FieldError struct {
	Field string `json:"field,omitempty"`
	Why   string `json:"why"`
}

// Question is what a running command asks (console.Question). With Choices it
// is a pick of one; with none, a yes or no answered "yes" or "no".
type Question struct {
	ID        uint64   `json:"id"`
	Command   uint64   `json:"command"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Choices   []Choice `json:"choices"`
	DefaultNo bool     `json:"defaultNo,omitempty"`
}

// Answer answers a question, exactly once: Key, or Declined when it cannot be
// asked. A yes or no dismissed is answered "no", never declined (console's
// Question): only a pick of one is declined when it is dismissed.
type Answer struct {
	Question uint64 `json:"question"`
	Key      string `json:"key,omitempty"`
	Declined bool   `json:"declined,omitempty"`
}

// Progress is what a running command says: a step, a warning, or the box a
// create made the moment the server took it.
type Progress struct {
	Command  uint64 `json:"command"`
	Text     string `json:"text,omitempty"`
	Warning  bool   `json:"warning,omitempty"`
	Accepted *Box   `json:"accepted,omitempty"`
}

// Edit is a file on this machine for the app to open in the user's editor.
type Edit struct {
	ID      uint64 `json:"id"`
	Command uint64 `json:"command"`
	Path    string `json:"path"`
	Title   string `json:"title"`
}

// Saved is the app's word that the editor is done with an edit: Error when it
// could not be opened or saved.
type Saved struct {
	Edit  uint64 `json:"edit"`
	Error string `json:"error,omitempty"`
}

// Upload is an image pasted or dropped on a box. It is answered with a
// result, by ID, whose Path is where the image is in the box.
type Upload struct {
	ID   uint64 `json:"id"`
	Box  string `json:"box"`
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// Open attaches the stream the app allocated to one of a box's sessions,
// sized for its pane. Frames the app sends on it before it is attached are
// kept until it is, and dropped if it fails.
type Open struct {
	Stream uint32 `json:"stream"`
	Box    string `json:"box"`
	Exec   string `json:"exec"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
}

// Close detaches a stream: the app no longer shows it. The session goes on in
// the box; ending one is the end command.
type Close struct {
	Stream uint32 `json:"stream"`
}

// StreamState is where a stream stands.
type StreamState string

// The stream states.
const (
	StreamAttached     StreamState = "attached"
	StreamFailed       StreamState = "failed"
	StreamReconnecting StreamState = "reconnecting"
	StreamReconnected  StreamState = "reconnected"
	// StreamEnded is the session's program ending, and End what that does
	// to the workspace (console.Tab.End). The stream's Exit frame follows.
	StreamEnded StreamState = "ended"
)

// TabEnd is what a session's ending does to its workspace (console.TabEnd).
type TabEnd string

// The endings.
const (
	// EndWorkspace: the primary ended, and the workspace with it.
	EndWorkspace TabEnd = "workspace"
	// EndTab: the tab goes; Why says why when its stream broke.
	EndTab TabEnd = "tab"
	// EndHeld: the program finished, and the tab stays with its last screen
	// to be read until it is dismissed; Status is what it says over it.
	EndHeld TabEnd = "held"
)

// Stream says a stream's state changed. A failed stream is forgotten. End
// is what an ending, or a failure to attach, does to the workspace; Status is
// what a held tab says over its last screen.
type Stream struct {
	Stream uint32      `json:"stream"`
	State  StreamState `json:"state"`
	Why    string      `json:"why,omitempty"`
	End    TabEnd      `json:"end,omitempty"`
	Status string      `json:"status,omitempty"`
}
