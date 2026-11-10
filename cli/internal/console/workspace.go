package console

import (
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A workspace is one discobox's sessions as a window holds them (ADR
// 26-11-07-876 §1): which of the box's sessions it opens and in which column,
// in what order, what each tab is called and numbered, what a tab's ending
// does, and what the workspace keeps while it is open — the sessions it is
// ending, its port forward, and the commits its automatic push was refused.
//
// It is the server's answer rather than a layout a window remembers: the exec
// listing says what is running and the session's own record says where it
// goes, so every window draws the same two columns. A renderer holds its own
// panes in it (Tabbed) and keeps the drawing, the focus, and the clock its
// poll and push loop run on; what each look reads and what its answer means
// are here.

// ListPollEvery is how often the listing is re-read while the console is
// open, and the machine's resources and the requests waiting on a person with
// it. A box's state changes without anything here asking it to — it starts,
// it stops, an upgrade appears — so the console follows the server rather than
// waiting to be told.
const ListPollEvery = 5 * time.Second

// ListStaleAfter is how old the rows on screen may get before they say how
// old they are (List.ListedAgo). Three refreshes: an ordinary poll leaves
// them at most one refresh and a round trip old, and a server that takes a
// few seconds to list is merely remote — but rows that have missed two
// refreshes in a row are rows that have stopped following the server, and
// drawn as though they had not, they are a window that looks stuck.
const ListStaleAfter = 3 * ListPollEvery

// WorkspacePollEvery is how often an open workspace re-reads the exec and
// service listings. It only has to catch sessions started elsewhere — the
// attach streams deliver every exit — so it is paced like a refresh rather
// than a keystroke. A parked workspace does not poll.
const WorkspacePollEvery = 2 * time.Second

// Side is one of a workspace's two columns.
type Side int

const (
	// SideTerminals is the left: the box's declared services, then its
	// harness terminals with the primary at their head.
	SideTerminals Side = iota
	// SideShells is the right: the shells, then the tools put away after
	// them.
	SideShells
)

// IsService reports whether a session runs one of the box's declared
// services (ADR 0070): read-only, and opened from the service listing rather
// than the exec listing.
func (e Exec) IsService() bool { return e.Service != "" }

// IsTool reports whether a session is a tool's (ADR 26-08-27-302): neither a
// terminal nor a shell, and put away after the shells when it is not shown.
func (e Exec) IsTool() bool { return e.Tool != "" }

// RunningTools are the tools a box's sessions are running, by ID: each with a
// live session carrying it (Exec.IsTool). A tool's tab is a view of exactly
// that session, and goes when it ends (Tab.End), so a console holding a
// tool's tab and a listing with its live session agree.
func RunningTools(execs []Exec) map[string]Exec {
	out := map[string]Exec{}
	for _, e := range execs {
		if e.IsTool() && e.Live {
			out[e.Tool] = e
		}
	}
	return out
}

// Side is the column a session is drawn in. The primary, the box's other
// harness terminals and its services go on the left — a service is the box
// running your own work, which belongs beside the harness working on it —
// and everything else on the right: shells, and the tools after them.
//
// It is the session's own record that answers: a terminal is created in
// harness mode and carries its harness, a service carries its service.
func (e Exec) Side() Side {
	if !e.IsTool() && (e.Primary || e.Harness != "" || e.IsService()) {
		return SideTerminals
	}
	return SideShells
}

// OpensAsTab reports whether the exec listing opens a session as a tab of its
// own: live, on a terminal, and none of the sessions that reach a workspace
// another way — the primary (opened whatever the listing says), a service
// (from the service listing, which also knows the ones not running), a pull
// request's files changed (drawn by the issue tab that started them).
func (e Exec) OpensAsTab() bool {
	return e.Live && e.Tty && e.ID != "" && !e.Primary && !e.IsService() && !e.FilesChanged
}

// Before orders a column's tabs: by when their sessions were created, oldest
// first, with the id as the tie-break so the order is stable whatever the
// listing's.
//
// Services are the exception, and sort ahead of everything else in their
// column, in the order the repository declares them: they are the box's own
// standing furniture, the same ones in the same order every time, and their
// declaration order is what their numbers mean, so it must not move with a
// restart. Tools sort after everything else, the other way round: a tool is
// put away to be out of the shells' way, and a shell opened later must not
// land beyond it.
func (e Exec) Before(o Exec) bool {
	if e.IsService() != o.IsService() {
		return e.IsService()
	}
	if e.IsTool() != o.IsTool() {
		return o.IsTool()
	}
	if e.IsService() && e.ServiceOrder != o.ServiceOrder {
		return e.ServiceOrder < o.ServiceOrder
	}
	if !e.CreatedAt.Equal(o.CreatedAt) {
		return e.CreatedAt.Before(o.CreatedAt)
	}
	return e.ID < o.ID
}

// Title is what to call a session's tab before its program names itself: a
// service's name (the one thing about it a person chose; its argv is the
// login shell every service shares), else the program it runs, else its
// harness, else the tail of its id.
func (e Exec) Title() string {
	if name := strings.TrimSpace(e.ServiceName); name != "" && e.IsService() {
		return name
	}
	if e.IsService() {
		return e.Service
	}
	if len(e.Command) > 0 {
		if base := path.Base(e.Command[0]); base != "" && base != "." && base != "/" {
			return base
		}
	}
	if e.Harness != "" {
		return e.Harness
	}
	if len(e.ID) > 6 {
		return e.ID[len(e.ID)-6:]
	}
	return e.ID
}

// Sessions is what a workspace opens from one exec listing beside its
// primary, in tab order: every session the listing opens as a tab, tools
// included. It is the rule for a window that holds nothing yet; an open
// workspace reconciles against what it holds (Workspace.Reconcile).
func Sessions(execs []Exec) []Exec {
	var out []Exec
	for _, e := range execs {
		if e.OpensAsTab() {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// Tab is one session as a workspace holds it: what a renderer's pane is, apart
// from how it draws.
type Tab struct {
	// ExecID keys the tab to the server session it draws, which is how a poll
	// tells a session it already shows from one it has to open. A service's
	// tab is keyed by its service (ServiceTabID), since it outlives any one
	// run.
	ExecID string
	// Title is what to call the tab until its program names itself.
	Title string
	// Created is when the session was created, which orders the tab.
	Created time.Time
	// Action is what the tab is, in the words the list uses: the attach (the
	// primary), a terminal, a shell, a service, or a tool's label.
	Action Interaction
	// Status is what the tab says over its program: a held tab's "finished",
	// a stopped service's state.
	Status string

	// Primary marks the box's primary terminal, the one tab whose ending ends
	// the workspace. It heads the terminals and is numbered 0.
	Primary bool
	// Harness is the harness the session runs, which puts it with the
	// terminals whatever it was asked for as.
	Harness string

	// Service is the declared service the tab draws, empty for every other
	// tab; it makes the tab read-only (a service runs on pipes). ServiceName is
	// its display name, kept so a command run from the tab can report on it;
	// ServiceOrder its place in the repository's declaration order, which
	// orders the service tabs; ServiceRun the run the tab was opened on, so a
	// poll can tell a tab still showing what the server reports from one whose
	// service has restarted, stopped or been fixed (Service.RunKey).
	Service      string
	ServiceName  string
	ServiceOrder int
	ServiceRun   string
	// Unread marks a service whose output moved on since it was last on
	// screen: the one tab you look away from on purpose says something
	// happened on its label.
	Unread bool

	// Tool is the tool the tab runs, empty for every other tab. A tool's tab
	// wears no number: it is a window over the workspace, put away rather
	// than moved between.
	Tool string

	// Files is a pull request's files changed, by its number
	// (DataSource.PullReview): discobox-review over its base and head. It is
	// a window of its own, never one the exec listing opens (Exec.FilesChanged),
	// so the tab that made it is the one that ends it. Typed is whether the
	// person has typed at it: quitting it then is how they say they are done
	// with it, and one that ends before they have ended on its own — an
	// error, a discobox-review too old — and is held to be read.
	Files int
	Typed bool

	// Exited is set when the tab's program finished and the tab was kept, so
	// its last screen can be read.
	Exited bool
}

// SessionTab is a session as the tab it opens as: the primary (opened under
// the virtual id), one the listing reports, or one asked for (a shell, a
// terminal). A create that failed carries no session record, so asked is
// what such a tab is called.
func SessionTab(e Exec, asked Interaction) Tab {
	t := Tab{ExecID: e.ID, Created: e.CreatedAt, Harness: e.Harness, Service: e.Service, ServiceName: e.ServiceName, ServiceOrder: e.ServiceOrder, Tool: e.Tool}
	switch {
	case e.ID == ExecPrimary:
		// No session record names it: until its harness titles its own
		// terminal, it is what it is, the attach.
		t.Primary, t.Action = true, InteractAttach
		return t
	case e.IsTool():
		// A tool is called by what it is. A window that knows the tool's
		// label calls it that instead (ToolTab).
		return ToolTab(e, e.Tool, e.Tool)
	case e.IsService():
		t.Action = InteractService
	case asked != "":
		t.Action = asked
	case e.Side() == SideTerminals:
		t.Action = InteractTerminal
	default:
		t.Action = InteractShell
	}
	t.Title = e.Title()
	return t
}

// ServiceTab is a declared service as its tab: its running process, or the
// card saying why there is none, which says so in Status. order is the
// service's place in the listing, which is its declaration order.
func ServiceTab(s Service, order int) Tab {
	t := Tab{
		ExecID: ServiceTabID(s.ID), Action: InteractService,
		Title:   s.DisplayName() + s.TabMark(),
		Service: s.ID, ServiceName: s.DisplayName(), ServiceOrder: order, ServiceRun: s.RunKey(),
	}
	if !s.Live() {
		t.Status = s.PaneStatus()
	}
	return t
}

// ServiceTabID is the ExecID a service's tab is keyed by: the service, not
// any one run of it.
func ServiceTabID(service string) string { return "service:" + service }

// ServiceOfTab is the service a tab's ExecID keys (ServiceTabID), and whether
// it keys one at all.
func ServiceOfTab(execID string) (string, bool) { return strings.CutPrefix(execID, "service:") }

// ListedServiceTabs is the tabs a service listing opens, in declaration
// order: one for every service worth a tab (Service.PaneWorthy), each on the
// run it reports. A tab held for a service missing here, or held on another
// run, has nothing left to draw (Workspace.ReconcileServices).
func ListedServiceTabs(services []Service) []Tab {
	var out []Tab
	for i, s := range services {
		if s.PaneWorthy() {
			out = append(out, ServiceTab(s, i))
		}
	}
	return out
}

// ToolTab is a tool's session as its tab, called by the tool's label.
func ToolTab(e Exec, tool, label string) Tab {
	return Tab{ExecID: e.ID, Created: e.CreatedAt, Action: Interaction(label), Title: label, Tool: tool}
}

// FilesTab is a pull request's files changed as its tab.
func FilesTab(e Exec, number int) Tab {
	title := "files changed #" + strconv.Itoa(number)
	return Tab{ExecID: e.ID, Created: e.CreatedAt, Action: Interaction("files changed"), Title: title, Files: number}
}

// Session is the tab as the session it orders by.
func (t *Tab) Session() Exec {
	return Exec{ID: t.ExecID, CreatedAt: t.Created, Primary: t.Primary, Harness: t.Harness, Service: t.Service, ServiceName: t.ServiceName, ServiceOrder: t.ServiceOrder, Tool: t.Tool}
}

// TakesImages reports whether an image pasted or dropped on the tab is carried
// into its box (DataSource.PutImage) and the path it landed at pasted in its
// place: any session in the box but a service's, which runs on pipes and takes
// nothing typed. A box has no clipboard of this machine's and cannot open its
// files, so this is how an agent in it is shown a screenshot.
func (t *Tab) TakesImages() bool { return t.ExecID != "" && t.Service == "" }

// TakesImageKeys reports whether the keys the agents bind to pasting an image
// (the paste image gesture) are taken on the tab, to carry the clipboard's
// image in: a harness terminal — the primary, or another terminal on its
// harness — never a shell, where Ctrl-V is quoted-insert and visual-block in
// vi, nor a tool.
func (t *Tab) TakesImageKeys() bool {
	return t.TakesImages() && t.Tool == "" && t.Side() == SideTerminals
}

// Side is the column the tab is drawn in: its session's (Exec.Side), and a
// terminal asked for is a terminal even when its create failed and there is
// no session record to say so.
func (t *Tab) Side() Side {
	if t.Tool == "" && t.Action == InteractTerminal {
		return SideTerminals
	}
	return t.Session().Side()
}

// Name is what to call the tab: what its program titles itself, else its
// Title, else what it is.
func (t *Tab) Name(program string) string {
	if program = strings.TrimSpace(program); program != "" {
		return program
	}
	if t.Title != "" {
		return t.Title
	}
	return string(t.Action)
}

// TabEnd is what a tab's program ending does to the workspace.
type TabEnd int

const (
	// EndsWorkspace: the primary ended, and the workspace with it — it is
	// the session the workspace is a view onto.
	EndsWorkspace TabEnd = iota
	// EndsTab: the tab goes. A tool is the program, not a transcript of it,
	// so it goes however it ended; any other tab goes when its stream broke,
	// and the reason is reported, since the screen that would have carried it
	// goes too.
	EndsTab
	// EndsHeld: the program finished, and the tab stays with its last screen
	// to be read until it is dismissed.
	EndsHeld
)

// End settles the tab's program ending, with the error its stream ended on,
// and says what that does. A held tab is marked so here.
func (t *Tab) End(err error) TabEnd {
	switch {
	case t.Primary:
		return EndsWorkspace
	case t.Tool != "" || err != nil:
		return EndsTab
	case t.Files != 0 && t.Typed:
		return EndsTab
	case t.Files != 0:
		t.Exited, t.Status = true, "discobox-review ended"
		return EndsHeld
	}
	t.Exited, t.Status = true, "finished"
	return EndsHeld
}

// Tabbed is a renderer's pane as the core holds it: its tab, and closing the
// view it draws (never the session).
type Tabbed interface {
	comparable
	TabState() *Tab
	Close()
}

// Column is one side of a workspace: a strip of tabs, one shown at a time.
// Both sides are the same thing, so the strip and its arithmetic are written
// once; what differs is what goes in them (Exec.Side).
type Column[P Tabbed] struct {
	Tabs []P
	// Active is the tab shown, and the one taking the keys while the column
	// has the focus. It is clamped wherever it is read.
	Active int
}

// Len is how many tabs the column holds.
func (c *Column[P]) Len() int { return len(c.Tabs) }

// Visible is the tab shown, or the zero P when the column is empty.
func (c *Column[P]) Visible() P {
	if len(c.Tabs) == 0 {
		var zero P
		return zero
	}
	return c.Tabs[min(max(c.Active, 0), len(c.Tabs)-1)]
}

// ByExec is the tab drawing a session, held tabs included, or the zero P.
func (c *Column[P]) ByExec(execID string) P {
	for _, p := range c.Tabs {
		if p.TabState().ExecID == execID {
			return p
		}
	}
	var zero P
	return zero
}

// Index is where a tab sits in the strip, or -1 when it is not in this one.
func (c *Column[P]) Index(p P) int {
	for i, s := range c.Tabs {
		if s == p {
			return i
		}
	}
	return -1
}

// Insert puts a tab where its session's order says it goes (Exec.Before),
// so the strip holds its order as the listing changes around it, and returns
// where it landed. focused says whether the keys are in this column: the tab
// being worked in stays the one being worked in, since an arrival must not
// move the keys to another session mid-word.
func (c *Column[P]) Insert(p P, focused bool) int {
	session := p.TabState().Session()
	at := len(c.Tabs)
	for i, s := range c.Tabs {
		if session.Before(s.TabState().Session()) {
			at = i
			break
		}
	}
	var zero P
	c.Tabs = append(c.Tabs, zero)
	copy(c.Tabs[at+1:], c.Tabs[at:])
	c.Tabs[at] = p
	if at <= c.Active && focused {
		c.Active++
	}
	c.Active = min(c.Active, len(c.Tabs)-1)
	return at
}

// Close closes one tab's view and takes it out of the strip. The session
// keeps running: ending one is the session's own to do.
func (c *Column[P]) Close(i int) {
	if i < 0 || i >= len(c.Tabs) {
		return
	}
	c.Tabs[i].Close()
	c.Tabs = append(c.Tabs[:i], c.Tabs[i+1:]...)
	if i < c.Active {
		c.Active--
	}
	c.Active = min(c.Active, max(len(c.Tabs)-1, 0))
}

// CloseAll closes every tab's view and empties the strip.
func (c *Column[P]) CloseAll() {
	for _, p := range c.Tabs {
		p.Close()
	}
	c.Tabs, c.Active = nil, 0
}

// Number is the number tab i wears, and whether it is a service's.
//
// There are two countings. A terminal or a shell is counted across the whole
// screen from base, where this column's numbers start (Workspace.TabBase), so
// one number means one tab on the whole screen. A service is counted within
// its column from one. Keeping the services out of the first counting is the
// point: they come and go on the box's schedule, and one starting or stopping
// must not renumber the shell you were reaching for. How a number is spelled
// as a key is each frontend's binding.
func (c *Column[P]) Number(i, base int) (n int, service bool) {
	service = c.Tabs[i].TabState().Service != ""
	for _, s := range c.Tabs[:i] {
		if (s.TabState().Service != "") == service {
			n++
		}
	}
	if service {
		return n + 1, true
	}
	return base + n, false
}

// Workspace is one box's workspace: its two columns of tabs and what the
// workspace keeps while it is attached, on screen or parked.
type Workspace[P Tabbed] struct {
	// Box is the discobox the workspace shows. Every tab is on it.
	Box Sandbox

	Terminals Column[P]
	Shells    Column[P]

	// Ending is the sessions this window has asked the server to end, so a
	// listing behind the kill does not open the tab straight back up. They
	// are kept for the workspace: exec ids are not reused, and listings
	// overlap, so no one answer proves the kill landed.
	Ending map[string]bool

	// Forward is the workspace's port forward while it is on screen; a
	// parked workspace has none, and gets one again when it is shown.
	Forward Forward

	// Push is the workspace's automatic push: what it holds against the
	// next look. PushGen is the generation the push loop runs under, the one
	// the workspace was opened in: it outlives the screen's, because a parked
	// workspace is still attached and still pushes.
	Push    AutoPush
	PushGen int
}

// Open starts a workspace on a box, holding nothing against it: a refusal or
// a kill belongs to the session it happened in. pushGen is the generation its
// push loop will run under.
func (w *Workspace[P]) Open(box Sandbox, pushGen int) {
	w.Box = box
	w.Ending = map[string]bool{}
	w.Push = AutoPush{Held: map[string]string{}}
	w.PushGen = pushGen
}

// Column is the column on one side.
func (w *Workspace[P]) Column(s Side) *Column[P] {
	if s == SideShells {
		return &w.Shells
	}
	return &w.Terminals
}

// Attached is every tab the workspace holds, left to right.
func (w *Workspace[P]) Attached() []P {
	return append(append([]P{}, w.Terminals.Tabs...), w.Shells.Tabs...)
}

// ByExec is the tab drawing a session on either side, or the zero P.
func (w *Workspace[P]) ByExec(execID string) P {
	if p := w.Terminals.ByExec(execID); p != *new(P) {
		return p
	}
	return w.Shells.ByExec(execID)
}

// Primary is the primary's tab, or the zero P while it has not arrived.
func (w *Workspace[P]) Primary() P {
	for _, p := range w.Terminals.Tabs {
		if p.TabState().Primary {
			return p
		}
	}
	var zero P
	return zero
}

// Parkable reports whether the workspace is worth keeping when it is
// switched away from: one whose primary has not arrived has nothing to keep,
// and is closed instead.
func (w *Workspace[P]) Parkable() bool { return w.Primary() != *new(P) }

// Park puts the workspace aside, attached: its tabs keep their streams and
// its push loop goes on, but its port forward goes with the screen that
// showed it.
func (w *Workspace[P]) Park() {
	if w.Forward != nil {
		_ = w.Forward.Close()
		w.Forward = nil
	}
}

// Release lets go of everything the workspace holds: every tab's view and
// its port forward. Every session keeps running.
func (w *Workspace[P]) Release() {
	w.Terminals.CloseAll()
	w.Shells.CloseAll()
	w.Park()
}

// Numbered is every tab that wears a number, left to right: all but the
// services.
func (w *Workspace[P]) Numbered() []P {
	var out []P
	for _, p := range w.Attached() {
		if p.TabState().Service == "" {
			out = append(out, p)
		}
	}
	return out
}

// ServiceTabs is every service's tab, in declaration order; the first wears
// 1.
func (w *Workspace[P]) ServiceTabs() []P {
	var out []P
	for _, p := range w.Attached() {
		if p.TabState().Service != "" {
			out = append(out, p)
		}
	}
	return out
}

// TabBase is where a column's numbers start: the terminals from 0, the
// primary among them, and the shells straight after them. The services sit
// in the left column but not in its numbers, which is what keeps a shell's
// number still while services start and stop around it.
func (w *Workspace[P]) TabBase(s Side) int {
	if s == SideTerminals {
		return 0
	}
	base := 0
	for _, p := range w.Terminals.Tabs {
		if p.TabState().Service == "" {
			base++
		}
	}
	return base
}

// Opens is what one exec listing asks an open workspace to open.
type Opens struct {
	// Primary is set when the primary is neither held nor opening: it is
	// opened from the first answer whatever that says, since the sandbox
	// resolves — and revives — it under the virtual id.
	Primary bool
	// Sessions is the terminals and shells to open, in tab order.
	Sessions []Exec
	// Tools is the tools to pick up, put away after the shells: a tool left
	// running by a window that has since gone is the same tool when it is
	// asked for again.
	Tools []Exec
	// NewShells is how many of these land in the shells column, tools
	// included, which is what the arrivals are sized for.
	NewShells int
}

// Reconcile is what an open workspace opens for one answer from the exec
// listing (nil when the listing failed): every session the listing opens as a
// tab that it does not already hold, is not ending, and the renderer is not
// already opening (opening, asked about a session or, for a tool, its tool).
//
// Nothing is ever closed here: the attach streams deliver their own exits,
// and a held tab is local and unknown to the listing, so each transition has
// one writer.
func (w *Workspace[P]) Reconcile(execs []Exec, opening func(Exec) bool) Opens {
	var o Opens
	primary := Exec{ID: ExecPrimary, Primary: true}
	o.Primary = w.Primary() == *new(P) && !opening(primary)
	for _, e := range Sessions(execs) {
		if w.ByExec(e.ID) != *new(P) || w.Ending[e.ID] {
			continue
		}
		if e.IsTool() {
			if !opening(e) && !w.holdsTool(e.Tool) {
				o.Tools = append(o.Tools, e)
				o.NewShells++
			}
			continue
		}
		if opening(e) {
			continue
		}
		o.Sessions = append(o.Sessions, e)
		if e.Side() == SideShells {
			o.NewShells++
		}
	}
	return o
}

// Tool is the tab running a tool, or the zero P. A tool's tab lives in the
// shells column, put away or not.
func (w *Workspace[P]) Tool(tool string) P {
	for _, p := range w.Shells.Tabs {
		if p.TabState().Tool == tool {
			return p
		}
	}
	var zero P
	return zero
}

func (w *Workspace[P]) holdsTool(tool string) bool { return w.Tool(tool) != *new(P) }

// ServiceOpen is one service's tab to open, at its place in the listing.
type ServiceOpen struct {
	Service Service
	Order   int
}

// ReconcileServices is what an open workspace changes for one answer from the
// service listing: the tabs to open, and the ones to close.
//
// Unlike the exec listing, it both opens and closes: a service is a
// declaration and the listing is the whole truth about it, so a tab whose
// service stopped on purpose, whose run moved on, or whose declaration was
// deleted has nothing left to draw. opening reports a service tab (by
// ServiceTabID) the renderer is already opening.
func (w *Workspace[P]) ReconcileServices(services []Service, opening func(tabID string) bool) (open []ServiceOpen, closing []P) {
	wanted := make(map[string]Tab, len(services))
	for _, t := range ListedServiceTabs(services) {
		wanted[t.Service] = t
	}
	declared := make(map[string]bool, len(services))
	for i, s := range services {
		declared[s.ID] = true
		id := ServiceTabID(s.ID)
		held := w.ByExec(id)
		want, ok := wanted[s.ID]
		switch {
		case !ok:
			// Stopped on purpose: its absence is the right thing to say, and
			// a tab to dismiss would not be.
			if held != *new(P) {
				closing = append(closing, held)
			}
		case opening(id):
		case held == *new(P):
			open = append(open, ServiceOpen{Service: s, Order: i})
		case held.TabState().ServiceRun != want.ServiceRun:
			// Its run moved on: the tab is opened again on the new one.
			closing = append(closing, held)
			open = append(open, ServiceOpen{Service: s, Order: i})
		}
	}
	for _, p := range w.Attached() {
		if service := p.TabState().Service; service != "" && !declared[service] {
			closing = append(closing, p)
		}
	}
	return open, closing
}

// ShortCommit is a commit as the console prints it.
func ShortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
