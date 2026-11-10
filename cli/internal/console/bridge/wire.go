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
// place of the bare forwards) and what a session's ending does (StreamEnded);
// 6, a box's services as tabs among its sessions, and the services command's
// menu and verbs; 7, the harnesses page (the harnesses command's harnesses,
// actions and card), a box's harness changes reviewed, saved and discarded,
// and edits answered with saved; 8, creating a box: the composer's draft and
// summary, the run options' forms with what they add up to, and a question's
// subject; 9, what a box's header says its agent is doing (Header.Agent); 10,
// a box's tools: the tools command's tools and addresses, a tool run or its
// session made, a tool's file edited, and the tool a tab runs (Tab.Tool); 11,
// a link pressed in a pane followed through the core (Follow), and the spec's
// link schemes; 12, images dropped on a pane as files (Upload.Files), every
// path an upload landed at (Result.Paths), which tabs take images and the
// image keys (Tab.Images, Tab.ImageKeys), and a folder dropped on the
// composer (the options command's Args.Source); 13, the secrets page (the
// credentials command's secrets, their bindings and cards, a secret's binding
// history, its actions and its words) and what a form is about (Form.About);
// 14, a question's count, sent again as it climbs (Question.Count); 15, the
// audit view: the timeline's runs, times and note, and the audit command's
// record read in full and its recordings; 16, a box's issue and pull request:
// the issue topic, the issue and pr commands' comment, close or reopen, and
// merge, a pull request's files changed as a session (TabFiles), and a
// question's answer offered with why it cannot be chosen (Choice.Why);
// 17, requests and approvals: a request's row words, the requests a secrets
// page answers (Result.Requests, the credentials command's answer action), a
// box's notice and its dismissal (Requests.Notices), requests just arrived
// (Requests.Arrived), automatic renewals (Requests.Said), a form's card and
// its actions (Form.Card, Form.Aside, Form.Actions, Form.Leave, Submit.Action,
// FormAction.Declines), and a
// detail line's bullet; 18, the audit view's search: the audit topic's query
// (Subscribe.Audit), the records it lets through with what it lights in each
// and its runs (AuditRecord.SourceLit, SummaryLit; AuditRun.SourceLit,
// SummaryLit), its tally (Audit.Tally), and what the timeline says of the
// records below a row held (Audit.Below).
const Protocol = 18

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
	// TypeUpload is images pasted or dropped on a box's session (app).
	TypeUpload Type = "upload"
	// TypeOpen attaches a stream the app allocated to a session, and
	// TypeClose detaches it, ending nothing in the box (app).
	TypeOpen  Type = "open"
	TypeClose Type = "close"
	// TypeStream is a stream's state: attached, failed, reconnecting,
	// reconnected (CLI). Its output and its end are frames on the stream.
	TypeStream Type = "stream"
	// TypeFollow is a link pressed in a pane, for the core to say where it
	// goes (app); a result answers it.
	TypeFollow Type = "follow"
	// TypeDraft is the prompt in the app's composer, to be kept for the next
	// window (app).
	TypeDraft Type = "draft"
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
	Draft       *Draft       `json:"draft,omitempty"`
	Follow      *Follow      `json:"follow,omitempty"`
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
	// Draft is the prompt the last window on this machine left in its
	// composer, which this one opens holding.
	Draft string `json:"draft,omitempty"`
}

// Follow is a link pressed in a pane: a program's (OSC 8) or text the app
// found on the screen. The core says where it goes, in a Result of the same
// ID: StatusDone with the URL to open — a URL printed in a box pointed at the
// port forwarded here for the box's header (the header topic), anything else
// as written — StatusEnter with the box a discobox link names, or
// StatusFailed with why it goes nowhere.
type Follow struct {
	ID  uint64 `json:"id"`
	URI string `json:"uri"`
	// Box is the key of the box the pane is one of, whose server a discobox
	// link that names none is on; empty for a pane that is no box's.
	Box string `json:"box,omitempty"`
}

// Draft is the prompt in the composer as it stands. The last window to say
// one is the one whose prompt the next window opens holding; an empty one
// clears it.
type Draft struct {
	Prompt string `json:"prompt"`
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
	// to do (Box). It says what an attach waits on, and runs until it is
	// unsubscribed — once the box is provisioned it says "connecting to the
	// terminal" — so the app ends it when the box's primary attaches.
	TopicProvisioning Topic = "provisioning"
	// TopicAudit is a box's audit timeline, followed (Box), narrowed to what
	// a search lets through (Audit).
	TopicAudit Topic = "audit"
	// TopicHeader is what a box's workspace header says of it, as the core
	// says it (console.Header), with its ports forwarded to this machine for
	// as long as the subscription lasts: the workspace's forward, which goes
	// with the screen showing it (Box).
	TopicHeader Topic = "header"
	// TopicSessions is what a box's workspace opens: its sessions and its
	// services' tabs, as the core orders and names them (Box).
	TopicSessions Topic = "sessions"
	// TopicCommands is every registry command as it stands where the app
	// asks from, for the boxes it names (Context, Targets): what its menus
	// list, enabled or with why not.
	TopicCommands Topic = "commands"
	// TopicPalette is what the palette finds for a query (Context, Palette):
	// the core's results, in the core's order.
	TopicPalette Topic = "palette"
	// TopicComposer is what a create from the composer would be made with,
	// as the line under it says it (console.RunOptions.Summary): sent again
	// whenever the run options change.
	TopicComposer Topic = "composer"
	// TopicIssue is the GitHub issue or pull request a box is tagged with,
	// as its tab draws it (Box, Issue): read as it is subscribed, again every
	// console.IssueRefreshEvery, soon after while GitHub has yet to say
	// whether a pull request can merge, and at once after a command here
	// comments on it, closes, reopens or merges it.
	TopicIssue Topic = "issue"
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
// The audit timeline is narrowed to what a search lets through (Audit): the
// app subscribes again under a new Sub for each query, before it ends the
// old, and a box's trails are read once for every subscription following
// them, so a new query narrows what was read rather than reading it again.
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
	Issue   *IssueQuery   `json:"issue,omitempty"`
	Audit   *AuditQuery   `json:"audit,omitempty"`
}

// AuditQuery is what the audit topic's search is (console.ParseQuery): the
// text typed, read as the TUI's audit screen reads it (console.AuditMatches).
type AuditQuery struct {
	Text string `json:"text,omitempty"`
}

// IssueQuery is which of a box's work the issue topic follows: its issue, or
// its pull request (Pull), by the number its tag gives (console.Sandbox.WorkNumber).
// Full is a read someone asked to see now, which the data source answers
// from GitHub rather than from what it read before; the app subscribes again
// under a new Sub to read again.
//
// Shown is the head of the pull request the app had shown the person before
// this subscription — a tab parked and back on screen — so a push made
// meanwhile is still said (console.ShownHead); empty for a tab that has
// shown none, or for a read the person asked for (Full), which shows them
// the head it brings.
type IssueQuery struct {
	Pull  bool   `json:"pull,omitempty"`
	Full  bool   `json:"full,omitempty"`
	Shown string `json:"shown,omitempty"`
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
	Composer     *Composer     `json:"composer,omitempty"`
	Issue        *Issue        `json:"issue,omitempty"`
}

// Issue is a GitHub issue or pull request as its tab draws it
// (console.Issue), with every rule's answer the core's: its badge, whether it
// can merge and why not, each event's words, the checks summed up, what each
// reviewer said. Every body is markdown made safe to render
// (console.MarkdownSource). Every string in it was written by whoever can
// comment on the issue. A time is said as how long ago it was when it was read
// (console.Ago), as the TUI's tab says it.
type Issue struct {
	// Label is what its tab is called (console.WorkLabel), and URL its page.
	Label string `json:"label"`
	URL   string `json:"url"`
	// Repository is owner/name.
	Repository  string       `json:"repository"`
	Number      int          `json:"number"`
	Pull        bool         `json:"pull,omitempty"`
	Title       string       `json:"title"`
	State       IssueState   `json:"state"`
	Badge       string       `json:"badge"`
	Mark        IssueMark    `json:"mark"`
	Author      string       `json:"author"`
	Opened      string       `json:"opened"`
	Comments    int          `json:"comments"`
	Body        string       `json:"body"`
	Reactions   string       `json:"reactions,omitempty"`
	Labels      []Label      `json:"labels"`
	Assignees   []string     `json:"assignees"`
	Type        string       `json:"type,omitempty"`
	Milestone   string       `json:"milestone,omitempty"`
	Locked      bool         `json:"locked,omitempty"`
	SubIssues   []IssueRef   `json:"subIssues"`
	SubTotal    int          `json:"subTotal,omitempty"`
	SubDone     int          `json:"subDone,omitempty"`
	PullRequest *PullRequest `json:"pullRequest,omitempty"`
	// Timeline is what happened after it was opened, oldest first, as the
	// page draws it: a merge's close left out (console.Issue.TimelineShows).
	Timeline []IssueItem `json:"timeline"`
	// Truncated is a timeline longer than was read: the rest is on its page.
	Truncated bool `json:"truncated,omitempty"`
	// Pushed is the head this subscription first showed, once a read since
	// brought another: what was read here is not what would be merged, and
	// Notice says so (console.PushedNotice) until the app reads again.
	Pushed string `json:"pushed,omitempty"`
	Notice string `json:"notice,omitempty"`
}

// IssueState is whether an issue is open or closed (a merged pull request is
// closed).
type IssueState string

// The states.
const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
)

// IssueMark is a state's mark (console.IssueMark): filled while there is
// something to do about it, in its tone.
type IssueMark struct {
	Filled bool              `json:"filled,omitempty"`
	Tone   console.IssueTone `json:"tone"`
}

// Label is one of an issue's labels, with the color its page draws it in:
// six hex digits, without the #.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// IssueRef is another issue or pull request, as a sub-issue list names it:
// its number, with its repository when that is not this one's.
type IssueRef struct {
	Number string    `json:"number"`
	Title  string    `json:"title"`
	Mark   IssueMark `json:"mark"`
}

// PullRequest is what a pull request's page shows beyond the issue it is.
type PullRequest struct {
	Draft     bool   `json:"draft,omitempty"`
	Merged    bool   `json:"merged,omitempty"`
	MergedBy  string `json:"mergedBy,omitempty"`
	MergedAgo string `json:"mergedAgo,omitempty"`
	// Head and Base are the branches, and HeadSHA and BaseSHA the commits that
	// were read: what a merge from here merges, and what the files changed
	// are measured between.
	Head         string `json:"head"`
	Base         string `json:"base"`
	HeadSHA      string `json:"headSha"`
	BaseSHA      string `json:"baseSha"`
	Commits      int    `json:"commits"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changedFiles"`
	// Merge is whether it can merge now (console.Issue.Merge), only while it
	// is open: Said is the sentence, Ready nothing in its way, Unknown GitHub
	// yet to say.
	Merge     *Merge     `json:"merge,omitempty"`
	Reviewers []Reviewer `json:"reviewers"`
	Checks    Checks     `json:"checks"`
}

// Merge is the merge box of an open pull request; a closed or merged one
// has none (console.MergeStanding.Shown).
type Merge struct {
	Ready   bool      `json:"ready,omitempty"`
	Unknown bool      `json:"unknown,omitempty"`
	Said    string    `json:"said,omitempty"`
	Mark    IssueMark `json:"mark"`
}

// Reviewer is one of the page's reviewers and what they said.
type Reviewer struct {
	Name string    `json:"name"`
	Said string    `json:"said"`
	Mark IssueMark `json:"mark"`
}

// Checks are the head commit's checks summed up (console.CheckSummary), and
// each that has not passed.
type Checks struct {
	Failing  int     `json:"failing,omitempty"`
	Pending  int     `json:"pending,omitempty"`
	Passed   int     `json:"passed,omitempty"`
	Partial  bool    `json:"partial,omitempty"`
	Unpassed []Check `json:"unpassed"`
}

// Check is one check that has not passed, and its page, where it has one.
type Check struct {
	Name string    `json:"name"`
	URL  string    `json:"url,omitempty"`
	Mark IssueMark `json:"mark"`
}

// IssueItemKind is what a timeline item is, which says how it is drawn.
type IssueItemKind string

// The item kinds.
const (
	// ItemComment is a comment, drawn as its markdown.
	ItemComment IssueItemKind = "comment"
	// ItemReview is a review: what it said, its summary, and its threads.
	ItemReview IssueItemKind = "review"
	// ItemCommit is a commit pushed: its short name and its subject.
	ItemCommit IssueItemKind = "commit"
	// ItemEvent is anything else that happened, said in Text.
	ItemEvent IssueItemKind = "event"
)

// IssueItem is one thing in the timeline: who, when, and what. Text is an
// event's words (console.Issue.EventText), a review's verdict
// (console.ReviewSaid, in Tone), or a commit's subject under its short name
// (Commit). Body is a comment's or a review's markdown.
type IssueItem struct {
	Kind      IssueItemKind     `json:"kind"`
	Actor     string            `json:"actor"`
	Ago       string            `json:"ago"`
	Text      string            `json:"text,omitempty"`
	Tone      console.IssueTone `json:"tone,omitempty"`
	Commit    string            `json:"commit,omitempty"`
	Body      string            `json:"body,omitempty"`
	Reactions string            `json:"reactions,omitempty"`
	Threads   []Thread          `json:"threads"`
}

// Thread is one conversation a review started on the diff: where
// (console.ThreadWhere), the lines it is under (console.ThreadHunk), and
// its comments.
type Thread struct {
	Where    string          `json:"where"`
	Hunk     []string        `json:"hunk"`
	Comments []ThreadComment `json:"comments"`
}

// ThreadComment is one comment of a review thread.
type ThreadComment struct {
	Author    string `json:"author"`
	Ago       string `json:"ago"`
	Body      string `json:"body"`
	Reactions string `json:"reactions,omitempty"`
}

// Composer is the summary of what a create would be made with: the harness,
// and every other run option somebody chose (console.RunOptions.Summary).
type Composer struct {
	Chips []Chip `json:"chips"`
}

// Chip is one part of it: its words, and whether it was chosen rather than a
// default, which the app draws lit.
type Chip struct {
	Text   string `json:"text"`
	Chosen bool   `json:"chosen,omitempty"`
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

// Requests is what waits on a person: every request and refused credential
// on every listed server, each listed box's notice as the window's dismissals
// leave it (console.Dismissals.NoticeFor), the requests that arrived since
// the subscription's last snapshot — none in its first: what was waiting as
// the window opened is the list's marks and the notices' to say — and what
// renewals a permission made since then said (console.RenewByPermit).
type Requests struct {
	Requests   []Request   `json:"requests"`
	Rejections []Rejection `json:"rejections"`
	Notices    []Notice    `json:"notices"`
	Arrived    []Arrival   `json:"arrived"`
	Said       []Said      `json:"said"`
}

// Request is one item waiting on a person. Box is the key of the box that
// asked, as the listing names it; empty for a request no listed box made.
// Subject, Where and Requester are what its row says (console.RequestSubject,
// CredentialRequest.Where, console.Requester), and Age how long it has
// waited, on a secrets page's rows alone.
type Request struct {
	ID            string              `json:"id"`
	Box           string              `json:"box,omitempty"`
	Server        string              `json:"server,omitempty"`
	Kind          console.RequestKind `json:"kind"`
	Name          string              `json:"name,omitempty"`
	EnvVar        string              `json:"envVar,omitempty"`
	Host          string              `json:"host,omitempty"`
	Justification string              `json:"justification,omitempty"`
	Created       time.Time           `json:"created"`
	Subject       string              `json:"subject"`
	Where         string              `json:"where,omitempty"`
	Requester     string              `json:"requester"`
	Age           string              `json:"age,omitempty"`
}

// Notice is what a box's workspace says above its panes (console.Notice):
// what it is about, beside it, what waits behind it, what pressing it does
// in words and as the command it runs on the box, and whether it is a person
// being waited on now, the one notice drawn as moving. A box with nothing to
// say has none.
type Notice struct {
	Box     string             `json:"box"`
	Kind    console.NoticeKind `json:"kind"`
	Subject string             `json:"subject"`
	Detail  string             `json:"detail,omitempty"`
	Behind  string             `json:"behind,omitempty"`
	Call    string             `json:"call"`
	Command string             `json:"command"`
	Urgent  bool               `json:"urgent,omitempty"`
}

// Arrival is a request that has just arrived, for the app to raise as a
// notification (console.RequestArrival): pressing it answers the request,
// through the credentials command's answer action on its server.
type Arrival struct {
	Request string `json:"request"`
	Server  string `json:"server,omitempty"`
	Box     string `json:"box,omitempty"`
	Title   string `json:"title"`
	Body    string `json:"body"`
}

// Said is what something the window did on its own says: a renewal a
// permission made, or why it could not (Warning).
type Said struct {
	Text    string `json:"text"`
	Warning bool   `json:"warning,omitempty"`
}

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

// Audit is a box's audit timeline: its most recent records, oldest first —
// under a search, those it lets through (console.AuditSearch); the rows they
// are drawn as (console.AuditRuns, over the records sent), every record in
// exactly one, in order; the trails the last read could not reach; and, with
// no record, what the timeline says (console.AuditReading before any read
// has finished, console.AuditNothing after, console.AuditNoMatch when a
// search hid every one). Tally is what a search says of what it let through
// (console.AuditTally), and set only under one. Below is what it says of the
// records newer than a row held off the newest.
type Audit struct {
	Records []AuditRecord `json:"records"`
	Runs    []AuditRun    `json:"runs"`
	Missing []string      `json:"missing"`
	Note    string        `json:"note,omitempty"`
	Tally   string        `json:"tally,omitempty"`
	Below   AuditBelow    `json:"below"`
}

// AuditBelow is what the timeline says of the records newer than the row
// the app's cursor holds off the newest, after their count, which is the
// app's (console.AuditBelowWords): One for one, Many for any other number —
// matches, under a search.
type AuditBelow struct {
	One  string `json:"one"`
	Many string `json:"many"`
}

// AuditRecord is one line of the timeline. Clock is when it was recorded, as
// the timeline says it (console.AuditTime) as of the snapshot: the timeline
// is sent again as the day turns, so a record of the day before says which.
// SourceLit and SummaryLit are what a search lights in its Source and its
// Summary (console.AuditLit), by rune index into each, in order.
type AuditRecord struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Clock        string    `json:"clock"`
	Source       string    `json:"source"`
	Summary      string    `json:"summary"`
	Group        string    `json:"group,omitempty"`
	GroupSummary string    `json:"groupSummary,omitempty"`
	SourceLit    []int     `json:"sourceLit,omitempty"`
	SummaryLit   []int     `json:"summaryLit,omitempty"`
}

// AuditRun is one row of the timeline: Records[First] alone, or the run
// First to Last folded behind one row that opens it, which says Summary
// (console.AuditRunSummary) at the time of its newest record. SourceLit and
// SummaryLit are what a search lights on that row — the newest record's
// Source, and Summary — as AuditRecord's are.
type AuditRun struct {
	First      int    `json:"first"`
	Last       int    `json:"last"`
	Summary    string `json:"summary,omitempty"`
	SourceLit  []int  `json:"sourceLit,omitempty"`
	SummaryLit []int  `json:"summaryLit,omitempty"`
}

// AuditText is what the audit command read: a record in full, as `discobox
// admin audit get` prints it, with the recordings it offers to read
// (console.AuditRecordDetail.AuditRecordings); or one of those recordings,
// as `discobox admin audit http --body` prints it, cut when it is long and
// saying so, and with none. Title is what it is shown under.
type AuditText struct {
	Record     string            `json:"record"`
	Part       console.AuditPart `json:"part,omitempty"`
	Title      string            `json:"title"`
	Text       string            `json:"text"`
	Recordings []AuditRecording  `json:"recordings"`
}

// AuditRecording is one recording a record offers to read: the part the
// audit command takes, and what it is called.
type AuditRecording struct {
	Part  console.AuditPart `json:"part"`
	Label string            `json:"label"`
}

// Header is what a box's workspace header says (console.Header): each
// field the core's answer, their order and colors the app's.
type Header struct {
	ID string `json:"id"`
	// Work is the issue and the pull request the box's tags number, each a
	// link to it, whose press opens its tab (Link.Command).
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
	// Agent is what the box's agent is doing (a program status state:
	// working, idle, blocked, done, error), and AgentMsg what it says of it
	// (console.Header.Agent): the core's choice between what the program in
	// the box's attached primary reports over OSC 7501 and the listing's
	// poll. None when there is nothing to say.
	Agent    string `json:"agent,omitempty"`
	AgentMsg string `json:"agentMsg,omitempty"`
}

// Link is something in the header a press opens in the browser; one without
// a URL is text.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
	// Command is the registry command a press on it runs in place of
	// opening the browser — issue or pr, for the work a box's tags number,
	// whose tab the console opens — leaving the browser to a press with the
	// platform's link modifier.
	Command string `json:"command,omitempty"`
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

// Sessions is what a box's workspace opens, by the core's rules, in tab order
// (console.Exec.Before): the tabs of its services worth one
// (console.ListedServiceTabs), the primary, and every session the exec listing
// opens as a tab. Running is false for a box that is not running: only its
// primary opens, and attaching it is what waits for the box.
//
// A session's tab is never closed by a snapshot: its stream says when it
// ends. A service's tab is, since the service listing is the whole truth
// about a declaration (console.Workspace.ReconcileServices): a service tab
// the app holds that is not listed, or is listed on another Run, has nothing
// left to draw, and goes.
type Sessions struct {
	Running  bool  `json:"running"`
	Sessions []Tab `json:"sessions"`
}

// Tab is one session as the app draws its tab. Kind is the column and what it
// is.
type Tab struct {
	Exec  string   `json:"exec"`
	Kind  TabKind  `json:"kind"`
	Title string   `json:"title"`
	Live  bool     `json:"live"`
	Argv  []string `json:"argv"`
	// Service is the declared service a service's tab draws; its Exec is
	// the service's (console.ServiceTabID), not any one run's. Run is the run
	// it draws (console.Service.RunKey): one that moves — a restart, a stop, a
	// fix — is a tab to open again. Status is what it says over its screen
	// while its service is not running: its state, its card under it.
	Service string `json:"service,omitempty"`
	Run     string `json:"run,omitempty"`
	Status  string `json:"status,omitempty"`
	// Tool is the tool a tool's tab runs, by its ID: what the tools menu
	// finds a running tool's tab by.
	Tool string `json:"tool,omitempty"`
	// Images is a tab an image pasted or dropped on is carried into the box
	// for (console.Tab.TakesImages); ImageKeys, one the paste image gesture is
	// taken on (console.Tab.TakesImageKeys).
	Images    bool `json:"images,omitempty"`
	ImageKeys bool `json:"imageKeys,omitempty"`
	// Files is the number of the pull request a files changed tab is of.
	Files int `json:"files,omitempty"`
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
	// TabFiles is a pull request's files changed, discobox-review over its
	// base and head: the app's own, ended when its tab is closed, which the
	// sessions topic never lists.
	TabFiles TabKind = "files"
)

// Service is one declared service, as the services menu lists it: its name,
// what it is doing and what it is for or why it cannot run (Detail), and its
// verbs as they stand for it (console.Service.Actions).
type Service struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Status  string          `json:"status"`
	Detail  string          `json:"detail,omitempty"`
	Actions []ServiceAction `json:"actions"`
}

// ServiceAction is one verb a service's menu offers, and why it cannot act
// when it cannot.
type ServiceAction struct {
	Verb    console.ServiceVerb `json:"verb"`
	Label   string              `json:"label"`
	Detail  string              `json:"detail"`
	Enabled bool                `json:"enabled"`
	Why     string              `json:"why,omitempty"`
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
// makes is attached to and the size of its pane (shell, terminal), the
// session it acts on (end, repaint), for services one of the box's services
// and the verb to run on it — without them, services lists them for its menu
// — and for new the prompt in the composer, which the box is given to do.
//
// The harnesses command reads the harnesses of Server (the list's server;
// empty for the primary) for its page, or, with a Harness and an Action, does
// that to it: Path is the file a files action edits. The review config
// command reads a box's harness changes for its review, or, with a Change and
// the Candidate the review showed, saves or discards that candidate.
//
// The credentials command reads the secrets of Server for its page, or does
// a SecretAction: new stores one, read reads them again for a page already
// open, and every other names the Secret it is done to — unbind the Binding
// it takes back.
type Args struct {
	Prompt    string                `json:"prompt,omitempty"`
	Stream    uint32                `json:"stream,omitempty"`
	Cols      int                   `json:"cols,omitempty"`
	Rows      int                   `json:"rows,omitempty"`
	Exec      string                `json:"exec,omitempty"`
	Service   string                `json:"service,omitempty"`
	Verb      console.ServiceVerb   `json:"verb,omitempty"`
	Server    string                `json:"server,omitempty"`
	Harness   string                `json:"harness,omitempty"`
	Action    console.HarnessAction `json:"action,omitempty"`
	Path      string                `json:"path,omitempty"`
	Change    console.HarnessChange `json:"change,omitempty"`
	Candidate string                `json:"candidate,omitempty"`
	// Tool names one of the box's tools for the tools command: with no File
	// it is run — handed the box, for one that runs on this machine, or its
	// session made on Stream — and with File, that file of it is edited.
	Tool string `json:"tool,omitempty"`
	File string `json:"file,omitempty"`
	// Source is a folder dropped on the composer: the options command with
	// one sets the Source row to it, checked as a source typed by hand is,
	// in place of asking the run options' form.
	Source string `json:"source,omitempty"`
	// Secret, SecretAction and Binding are the credentials command's, and
	// Request the request its answer action answers, on Server.
	Secret       string               `json:"secret,omitempty"`
	SecretAction console.SecretAction `json:"secretAction,omitempty"`
	Binding      string               `json:"binding,omitempty"`
	// Record names one record of the box's audit timeline for the audit
	// command, which reads it in full; with Part, it reads that recording of
	// it instead.
	Record string            `json:"record,omitempty"`
	Part   console.AuditPart `json:"part,omitempty"`
	// Act is what the issue and pr commands do to the box's issue or pull
	// request; without one they hand the app its tab. Comment is the comment
	// a comment posts, as written. Head is the commit the app showed, which a
	// merge merges — a branch that moved since is refused — and the files
	// command reviews, from Base; Pushed is the head it showed before a read
	// brought this one, when one did, which the merge question says.
	Act     IssueAct `json:"act,omitempty"`
	Comment string   `json:"comment,omitempty"`
	Head    string   `json:"head,omitempty"`
	Base    string   `json:"base,omitempty"`
	Pushed  string   `json:"pushed,omitempty"`
	Request string   `json:"request,omitempty"`
}

// IssueAct is what the issue and pr commands do.
type IssueAct string

// The acts.
const (
	// ActComment posts Args.Comment.
	ActComment IssueAct = "comment"
	// ActState closes or reopens it, as the core asks (console.Issue.StateQuestion).
	ActState IssueAct = "state"
	// ActMerge merges the pull request at Args.Head, as the core asks
	// (console.Issue.MergeQuestion).
	ActMerge IssueAct = "merge"
)

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
	// opening the box's workspace (Box). For new it is the box made, or, with
	// no Box, a prompt that is the window's way out (console.LeavesWindow):
	// the app leaves, as quit does.
	StatusEnter Status = "enter"
)

// Result ends a command (or an upload or a follow, by its ID).
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
	// Services are the box's declared services, in declaration order, read
	// as the services menu opens: what it lists.
	Services []Service `json:"services,omitempty"`
	// Paths are where an upload's images landed in the box, in order.
	Paths []string `json:"paths,omitempty"`
	// URL is where a followed link goes, for the app to open.
	URL string `json:"url,omitempty"`
	// Harnesses are the harnesses of the server the harnesses command read,
	// in the order they were registered: what its page lists, read again
	// after each action, which leaves the one it acted on as Harness. A
	// variant created is Harness.
	Harnesses []Harness `json:"harnesses,omitempty"`
	Harness   string    `json:"harness,omitempty"`
	// Card is a harness's config card (console.HarnessCard), for its config
	// action.
	Card []CardRow `json:"card,omitempty"`
	// Changes is a box's harness changes, read as its review opens.
	Changes *HarnessChanges `json:"changes,omitempty"`
	// Tools are a box's tools as the tools menu lists them
	// (console.PickerTools), read as it opens, and Addresses its two
	// addresses to copy beside them (console.AddressOffers).
	Tools     []Tool    `json:"tools,omitempty"`
	Addresses []Address `json:"addresses,omitempty"`
	// Secrets are the secrets of the server the credentials command read,
	// in the order it lists them: what its page lists, read again after each
	// action, which leaves the one it acted on as Secret. History is a
	// secret's binding history, newest first, for its history action.
	Secrets []Secret   `json:"secrets,omitempty"`
	Secret  string     `json:"secret,omitempty"`
	History []Decision `json:"history,omitempty"`
	// SecretWords are what the secrets page says beside its secrets, sent
	// with them and with a history.
	SecretWords *SecretWords `json:"secretWords,omitempty"`
	// Audit is what the audit command read of a record: the record in full,
	// or one of its recordings.
	Audit *AuditText `json:"audit,omitempty"`
	// Work is the issue or pull request the issue and pr commands open a tab
	// on: what its tab is called and its page, before the issue topic reads it.
	Work *Link `json:"work,omitempty"`
	// Requests are the requests waiting on the secrets page's server, oldest
	// first (console.RequestsOn), sent with its secrets.
	Requests []Request `json:"requests,omitempty"`
}

// Tool is one of a box's tools as the tools menu lists it: what it is called
// and what it is (running, when the box has its session live), whether it
// runs on this machine, why it cannot run when it cannot (console.Tool.Why),
// and the files it carries to edit.
type Tool struct {
	ID      string     `json:"id"`
	Label   string     `json:"label"`
	Detail  string     `json:"detail,omitempty"`
	Host    bool       `json:"host,omitempty"`
	Enabled bool       `json:"enabled"`
	Why     string     `json:"why,omitempty"`
	Files   []ToolFile `json:"files"`
}

// ToolFile is one file a tool carries, by its name, and what its row says of
// it (console.ToolFileDetail): where it lands in a box, and where its copy is
// on this machine.
type ToolFile struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// Address is one of a box's addresses to copy (console.AddressOffer): its
// label, and the address, or why there is none.
type Address struct {
	Label string `json:"label"`
	Value string `json:"value,omitempty"`
	Why   string `json:"why,omitempty"`
}

// SecretWords are what the secrets page says beside its secrets, in the
// core's words (console.NoSecrets, BindThisSecret, BindingHistoryTitle…):
// a page with none, the row that binds a secret read, and a history's head.
type SecretWords struct {
	Empty        string `json:"empty"`
	Bind         string `json:"bind"`
	BindDetail   string `json:"bindDetail"`
	HistoryTitle string `json:"historyTitle"`
	HistoryNote  string `json:"historyNote"`
	// Requests, Waiting and NothingWaiting head the page's requests table:
	// its title, its count, and what it says with none.
	Requests       string `json:"requests"`
	Waiting        string `json:"waiting"`
	NothingWaiting string `json:"nothingWaiting"`
}

// Secret is one of a server's secrets as the secrets page lists it: what its
// row says (console.SecretWhere, console.SecretStandingOf), what reading it
// says (console.DescribeSecret, console.BindingsAnswer), and its live
// bindings. Refused is why an upstream refuses it, when one does
// (console.RejectionSummary). Its value is never on the wire.
type Secret struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Where        string                 `json:"where"`
	Nowhere      bool                   `json:"nowhere,omitempty"`
	StandingText string                 `json:"standingText"`
	Standing     console.SecretStanding `json:"standing"`
	Age          string                 `json:"age,omitempty"`
	Refused      string                 `json:"refused,omitempty"`
	Card         []DetailSection        `json:"card"`
	Answer       string                 `json:"answer"`
	Bindings     []SecretBinding        `json:"bindings"`
}

// SecretBinding is one live binding of a secret: its row's label and detail
// (console.BindingLabel, BindingDetail), and the binding read in full
// (console.DescribeBinding).
type SecretBinding struct {
	ID     string          `json:"id"`
	Label  string          `json:"label"`
	Detail string          `json:"detail"`
	Card   []DetailSection `json:"card"`
}

// Decision is one decision of a secret's binding history: its row
// (console.BindingDecision.Label), the decision read in full
// (console.DescribeDecision), and the delegation it was handed on under read
// as a binding is, when that binding still stands.
type Decision struct {
	ID         string          `json:"id"`
	Label      string          `json:"label"`
	Card       []DetailSection `json:"card"`
	Delegation []DetailSection `json:"delegation"`
}

// DetailSection is a stretch of a card read rather than answered
// (console.DetailSection): a heading, its label and value rows, and the
// sentences under them.
type DetailSection struct {
	Label  string        `json:"label,omitempty"`
	Fields []DetailField `json:"fields"`
	Lines  []DetailLine  `json:"lines"`
}

// DetailField is a label and its value, in the value's tone.
type DetailField struct {
	Label string           `json:"label"`
	Value string           `json:"value"`
	Tone  console.CardTone `json:"tone"`
}

// DetailLine is a sentence under a section's fields, in its tone. A bullet
// hangs its wrapped rows under its text.
type DetailLine struct {
	Text   string           `json:"text"`
	Tone   console.CardTone `json:"tone"`
	Bullet bool             `json:"bullet,omitempty"`
}

// Harness is one of a server's harnesses as the harnesses page lists it: what
// its row says, its files for the files action's picker, and its actions as
// the core offers them (console.Harness.Actions).
type Harness struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
	BuiltIn bool   `json:"builtIn,omitempty"`
	Bundle  string `json:"bundle,omitempty"`
	// Age is how long ago it last changed, as the list says an age.
	Age     string         `json:"age,omitempty"`
	Secrets int            `json:"secrets,omitempty"`
	Files   []HarnessFile  `json:"files"`
	Actions []HarnessOffer `json:"actions"`
}

// HarnessFile is one of a harness's files: its path, its size in bytes, and
// what the core says of it (console.HarnessFile.Notes).
type HarnessFile struct {
	Path  string   `json:"path"`
	Size  int      `json:"size"`
	Notes []string `json:"notes"`
}

// HarnessOffer is one action as a harness's menu offers it, and why it
// cannot act when it cannot.
type HarnessOffer struct {
	Action  console.HarnessAction `json:"action"`
	Label   string                `json:"label"`
	Detail  string                `json:"detail"`
	Enabled bool                  `json:"enabled"`
	Why     string                `json:"why,omitempty"`
}

// CardRow is a row of a card (console.CardRow): its label, empty where it
// carries on the row above's; its value in spans; and a file's first lines
// under it, with More how many more it has.
type CardRow struct {
	Label string     `json:"label,omitempty"`
	Spans []CardSpan `json:"spans"`
	Lines []string   `json:"lines"`
	More  int        `json:"more,omitempty"`
}

// CardSpan is a stretch of a card row's value, in its tone.
type CardSpan struct {
	Text string           `json:"text"`
	Tone console.CardTone `json:"tone"`
}

// HarnessChanges is a box's harness changes as its review shows them
// (console.HarnessChanges): the candidate, where it stands, what it changes,
// what saving does, and what the review offers to do with it. Nothing is
// what the review says of a candidate carrying nothing to save, empty for one
// that carries something.
type HarnessChanges struct {
	Candidate   string        `json:"candidate,omitempty"`
	Summary     string        `json:"summary"`
	Nothing     string        `json:"nothing,omitempty"`
	Saving      string        `json:"saving"`
	Files       []ChangedFile `json:"files"`
	Credentials []string      `json:"credentials"`
	Diagnostics []string      `json:"diagnostics"`
	Offers      []ChangeOffer `json:"offers"`
}

// ChangedFile is one file a candidate adds, updates or removes, as its diff,
// headed as the core heads it (console.HarnessChangedFile.Label).
type ChangedFile struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Diff  string `json:"diff"`
}

// ChangeOffer is what the review offers to do with the candidate, and why it
// cannot when it cannot.
type ChangeOffer struct {
	Change  console.HarnessChange `json:"change"`
	Label   string                `json:"label"`
	Enabled bool                  `json:"enabled"`
	Why     string                `json:"why,omitempty"`
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
//
// Fields can grow as they are answered — a repeated run option gains a field
// once its last is filled — so a form sent again after a draft may hold more
// fields than before, matched to what the app holds by Key. Note is what the
// answers so far add up to (the command a create would run), said under the
// fields; Action is what the button that submits it says, empty for the
// app's own word.
type Form struct {
	ID      uint64  `json:"id"`
	Command uint64  `json:"command"`
	Title   string  `json:"title"`
	Fields  []Field `json:"fields"`
	Note    string  `json:"note,omitempty"`
	Action  string  `json:"action,omitempty"`
	// About is what the form is about, said above its fields, a paragraph
	// to a line: why a secret's edit was opened, what a binding is.
	About string `json:"about,omitempty"`
	// Card is what the form says beside its fields, read again for every
	// draft: what a request's card says as it is answered.
	Card []DetailSection `json:"card"`
	// Aside is said beside the title: which card of how many.
	Aside string `json:"aside,omitempty"`
	// Actions are the form's ways to submit, each by its key, in place of
	// the app's own one (Action): accept and deny, a request's JSON
	// (console.CardActions). The submit names the one pressed; only an
	// accepting one is checked, and a submit with none is refused.
	Actions []FormAction `json:"actions"`
	// Leave is what leaving the form does, said beside the key that does it,
	// when it is not the app's own word (a request's card leaves it
	// waiting).
	Leave string `json:"leave,omitempty"`
}

// FormAction is one of a form's ways to submit (console.CardOffer): its key,
// its label, and whether it declines, which is drawn as a refusal and never
// the default.
type FormAction struct {
	Key      console.CardAction `json:"key"`
	Label    string             `json:"label"`
	Declines bool               `json:"declines,omitempty"`
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
	// Why is why an answer a question offers cannot be chosen: shown, never
	// taken.
	Why string `json:"why,omitempty"`
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
	// Action is which of the form's Actions submitted it.
	Action console.CardAction `json:"action,omitempty"`
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
//
// Subject is what it is about, on a line of its own above the body. Count is
// what the count of a directory the answer turns on says so far
// (console.Question.Measure, console.DirectoryTotal.Said), drawn under the
// body: the question is sent again, with the same ID, each time it changes,
// and the app updates the question it is showing rather than asking another.
type Question struct {
	ID        uint64   `json:"id"`
	Command   uint64   `json:"command"`
	Title     string   `json:"title"`
	Subject   string   `json:"subject,omitempty"`
	Body      string   `json:"body"`
	Choices   []Choice `json:"choices"`
	DefaultNo bool     `json:"defaultNo,omitempty"`
	Count     string   `json:"count,omitempty"`
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

// Saved is the app's word that the person is finished with an edit, exactly
// once: Canceled when they gave it up, which saves nothing, and Error when it
// could not be opened.
type Saved struct {
	Edit     uint64 `json:"edit"`
	Canceled bool   `json:"canceled,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Upload is images for one of a box's sessions: an image pasted from the
// clipboard (Name and Data), or files dropped on its pane (Files). It is
// answered with a result, by ID, whose Paths are where the images are in the
// box, in order, for the app to paste in their place; a drop that is not all
// images is answered done with none, and the app pastes the files' paths as
// text, as a terminal does a dragged file.
type Upload struct {
	ID  uint64 `json:"id"`
	Box string `json:"box"`
	// Name is what the clipboard calls the image, and Data its bytes. The
	// image is written under a name of the core's (console.PastedImageName),
	// never this one.
	Name string `json:"name,omitempty"`
	Data []byte `json:"data,omitempty"`
	// Files are paths on this machine, as dropped. The CLI reads them, and
	// they are images only when every one is an image file
	// (clipimage.Files).
	Files []string `json:"files,omitempty"`
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
