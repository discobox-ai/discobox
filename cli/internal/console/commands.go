package console

import (
	"fmt"
	"slices"
)

// The command registry (ADR 26-11-06-671 §4, moved into the core by
// 26-11-07-876 §1): every command the console offers, by a stable ID, with what
// it is called, what it acts on, how each frontend reaches it (26-11-07-598
// §2), whether it can act now and why not, and its dispatch. Every key a
// frontend binds to reach a command or move between panes and boxes — the
// TUI's list, tree, leader and panes, the desktop's shortcuts and list — is a
// command's here or a gesture's (Gesture), and each frontend reads its key
// registration, hints, help, menus and palette from it (598 §3). A screen's
// own keys (an issue tab, a dialog, an open menu) are still that screen's.

// Target is what a command acts on.
type Target int

const (
	// TargetNone is a command for the window, not for a box: new discobox,
	// help, quit.
	TargetNone Target = iota
	// TargetBox takes one box, chosen explicitly — never inferred from what is
	// on screen. The box's lifecycle commands also take a batch, from the
	// list's selection.
	TargetBox
)

// Bindings is how each frontend reaches a command, keyed by frontend
// (598 §2). A command one frontend does not bind is reached there through the
// palette.
type Bindings struct {
	TUI     TUIBinding
	Desktop DesktopBinding
}

// TUIBinding is a command's keys in the terminal console.
type TUIBinding struct {
	// Keys are pressed bare, where the list or the workspace tree has the
	// keys. The first is the one a hint shows.
	Keys []string
	// Leader is the sequences pressed after the leader, from a workspace's
	// panes and the pages beside the tree: each one key, or a chord of two.
	// The first is the one a hint shows; any other reaches the same command on
	// another key (the arrows beside h and l).
	Leader [][]string
	// Repeats holds the leader open while its modifier is down, so a run of
	// the command is the leader once and then its key again and again: the
	// leader, then Ctrl-→ Ctrl-→ under a Ctrl leader.
	Repeats bool
	// Chords are pressed with no leader at all, from a pane as well: chords a
	// classic terminal cannot encode, so no program in a pane is sent them
	// (598 §3). One the window cannot act on where it is pressed goes on to
	// the program, as it would have unbound.
	Chords []string
	// Menu is reached after the leader through a menu: the menu's key, then
	// the entry's.
	Menu []string
}

// DesktopBinding is a command's chords in the desktop app. A chord is written
// with `primary`, which is ⌘ on macOS and Ctrl+Shift elsewhere, and never adds
// shift to primary (598 §2).
type DesktopBinding struct {
	Keys []string
	// Menu is the menu bar's menu the command is in (598 §2: the menu bar, or
	// an in-window equivalent). Every command is in one, so every command is
	// reached in the desktop by pointer as well as by the palette; a gesture
	// is in none.
	Menu Menu
}

// Menu is one of the desktop's menus.
type Menu string

// The menus, in the order the menu bar draws them (Menus).
const (
	MenuConsole Menu = "Console"
	MenuBox     Menu = "Box"
	// MenuSession is a workspace's sessions: a pane's own menu too.
	MenuSession Menu = "Session"
	MenuView    Menu = "View"
	MenuGo      Menu = "Go"
	MenuHelp    Menu = "Help"
)

// Menus is every menu, in the order the menu bar draws them.
var Menus = []Menu{MenuConsole, MenuBox, MenuSession, MenuView, MenuGo, MenuHelp}

// Command is one entry of the registry.
type Command struct {
	// ID is stable: parity scenarios, usage counts and every frontend's
	// bindings name commands by it.
	ID     string
	Label  string
	Detail string
	// Aliases are what the palette matches, the label first. A single-word
	// alias of a box command commits it as the palette's chip (671 §2), so no
	// two commands share one.
	Aliases []string
	Target  Target
	// Lifecycle marks the box's own actions — attach, shell, the verbs, rename,
	// settings, review config — which the box's action menu lists and which
	// take a batch.
	Lifecycle bool
	Bindings  Bindings

	// countedByFlow is a command whose flow counts its own use, because the
	// flow is also entered by gestures outside the registry (a page's row in
	// the tree, a menu). Dispatch counts every other command as it enters it.
	countedByFlow bool
	// verb is a lifecycle command's request to the server.
	verb Verb
	// opensBox is a command that needs the box's container, which an image
	// that is no longer available stops.
	opensBox bool
	// moves is a command that only moves focus — a pane along, a box down
	// the list — which is not a use: 671 §6 counts a command entering its
	// flow, not cursor movement, and a walk is pressed in runs that would put
	// it at the top of the palette.
	moves bool
}

// Entry is a command as it stands for one context and target: whether it can
// act, and why not.
type Entry struct {
	Command
	Enabled bool
	// Why is why it cannot act, when it cannot.
	Why string
	// Unavailable is why it cannot act from where it was asked at all, whatever
	// its target: a workspace command with no workspace, a page in a window
	// opened on one box.
	Unavailable string
}

// Context is what a frontend tells the registry about where a command is
// asked from. Navigation is the frontend's (876 §2); the registry is handed
// the parts of it a rule depends on.
type Context struct {
	Session Session
	// OneShot is the box a window opened on one box belongs to (a bare attach,
	// a run's own window); nil in a console.
	OneShot *Sandbox
	// Workspace is the box whose workspace the window holds, on screen or
	// behind a page; nil with none. Its own pages open there whatever the box
	// can do now, since nothing has to be attached to reach them.
	Workspace *Sandbox
	// HasWorkspace is a workspace open in the window, on screen or behind a
	// page; WorkspaceShown is one on screen, PaneFocused a pane of it with
	// the focus, and ListHasKeys the list beside it — a workspace or a page,
	// or expanded over them — with the keys instead.
	HasWorkspace, WorkspaceShown, PaneFocused, ListHasKeys bool
	// Tree is the list available beside the workspace, which the pages open
	// next to; TreeShown is it drawn there now, not put away; TreeFits is the
	// window wide enough for it.
	Tree, TreeShown, TreeFits bool
	// PageShown is a page (the harnesses, the secrets, the new card) in front
	// of the right side, with the keys. The walk between panes reads it and
	// ListHasKeys: the list is the leftmost thing on screen, and a page
	// covers the panes.
	PageShown bool
	// Waiting reports credential requests waiting on a box, and Refused a
	// refused credential shown for it. Nil reports none.
	Waiting, Refused func(Sandbox) bool
}

// Registry is the command registry of one window: the commands, and how often
// each has been used there (671 §6), which lives as long as the window does.
type Registry struct {
	ds    DataSource
	usage map[string]int
}

// NewRegistry is a window's registry over its data source.
func NewRegistry(ds DataSource) *Registry {
	return &Registry{ds: ds, usage: map[string]int{}}
}

// Use counts one use of a command, entering its flow.
func (r *Registry) Use(id string) { r.usage[id]++ }

// Usage is how often a command has been used in this window.
func (r *Registry) Usage(id string) int { return r.usage[id] }

// Commands is every command, in the order the registry lists them.
func Commands() []Command { return slices.Clone(registry) }

// Lookup is one command by ID.
func Lookup(id string) (Command, bool) {
	i := slices.IndexFunc(registry, func(c Command) bool { return c.ID == id })
	if i < 0 {
		return Command{}, false
	}
	return registry[i], true
}

// ForTUIKey is the command a key pressed bare in the TUI's list runs.
func ForTUIKey(key string) (Command, bool) {
	return find(func(b TUIBinding) bool { return slices.Contains(b.Keys, key) })
}

// ForTUILeader is the command one key pressed after the TUI's leader runs.
func ForTUILeader(key string) (Command, bool) {
	return find(func(b TUIBinding) bool {
		return slices.ContainsFunc(b.Leader, func(seq []string) bool { return len(seq) == 1 && seq[0] == key })
	})
}

// ForTUIChord is the command a leaderless chord runs in the TUI.
func ForTUIChord(key string) (Command, bool) {
	return find(func(b TUIBinding) bool { return slices.Contains(b.Chords, key) })
}

func find(bound func(TUIBinding) bool) (Command, bool) {
	i := slices.IndexFunc(registry, func(c Command) bool { return bound(c.Bindings.TUI) })
	if i < 0 {
		return Command{}, false
	}
	return registry[i], true
}

// Entries is every command as it stands in this context, for these targets:
// none for the window's own commands, one box from the palette or a pane, a
// batch from the list.
func (r *Registry) Entries(ctx Context, targets []Sandbox) []Entry {
	out := make([]Entry, 0, len(registry))
	for _, c := range registry {
		out = append(out, r.entry(ctx, c, targets))
	}
	return out
}

// Entry is one command as it stands in this context, for these targets.
func (r *Registry) Entry(ctx Context, id string, targets []Sandbox) (Entry, bool) {
	c, ok := Lookup(id)
	if !ok {
		return Entry{}, false
	}
	return r.entry(ctx, c, targets), true
}

func (r *Registry) entry(ctx Context, c Command, targets []Sandbox) Entry {
	e := Entry{Command: c, Enabled: true}
	one := len(targets) == 1
	switch {
	case c.Lifecycle:
		e.Enabled, e.Why = lifecycleEligible(c.ID, targets)
	case c.Target == TargetBox && one && ctx.Workspace != nil && sameBox(ctx.Session, targets[0], *ctx.Workspace):
		// Already open: the page opens in the workspace there is.
	case c.Target == TargetBox:
		// Anything else opens the box's workspace first, which attaching does.
		e.Enabled = one && targets[0].Attachable()
		e.Why = AttachWhy(targets)
	}
	switch c.ID {
	case "requests":
		e.Enabled = one && ctx.Waiting != nil && ctx.Waiting(targets[0])
		e.Why = "nothing waiting on this discobox"
	case "rejections":
		e.Enabled = one && ctx.Refused != nil && ctx.Refused(targets[0])
		e.Why = "nothing refused on this discobox"
	case "terminal", "end", "maximize", "detach", "mouse", "dismiss":
		if !ctx.PaneFocused || !ctx.WorkspaceShown {
			e.Unavailable = "requires a workspace pane"
		}
	case "actions":
		// Away from a workspace, or with the list's keys beside one, it is the
		// list's action menu, which needs no pane.
		if ctx.WorkspaceShown && !ctx.PaneFocused && !ctx.ListHasKeys {
			e.Unavailable = "requires a workspace pane"
		}
	case "all":
		switch {
		case ctx.OneShot == nil:
			e.Unavailable = "available in a wide attach window"
		case !ctx.TreeFits:
			e.Unavailable = "the window is too narrow for the list beside the box"
		}
	case "next pane":
		switch {
		case ctx.ListHasKeys:
			// The list hands the keys back to whatever is right of it: a
			// workspace, one still opening, or a page.
		case ctx.PageShown:
			e.Unavailable = "the page is in front of the panes; Esc puts it away"
		case !ctx.HasWorkspace:
			e.Unavailable = "requires a workspace"
		}
	case "previous pane":
		switch {
		case ctx.ListHasKeys:
			e.Unavailable = "the list is the leftmost; there is nothing left of it"
		case ctx.PageShown && !ctx.TreeShown:
			e.Unavailable = "there is no list beside this page"
		case !ctx.PageShown && !ctx.HasWorkspace && !ctx.Tree:
			e.Unavailable = "requires a workspace"
		}
	case "next box", "previous box", "sidebar":
		if !ctx.Tree {
			e.Unavailable = "there is no list beside a workspace in this window"
		}
	case "new", "credentials", "harnesses":
		if ctx.HasWorkspace && !ctx.Tree {
			e.Unavailable = "the pages open beside the list; this window has no tree"
		}
	}
	if ctx.OneShot != nil && slices.Contains([]string{"new", "credentials", "harnesses", "servers", "options"}, c.ID) {
		e.Unavailable = "open discobox console for this page"
	}
	if one && (c.ID == "issue" || c.ID == "pr" || c.ID == "files") {
		if _, ok := targets[0].WorkNumber(c.ID != "issue"); !ok {
			e.Enabled, e.Why = false, "no matching repository tag"
		}
	}
	if ctx.OneShot != nil && slices.ContainsFunc(targets, func(s Sandbox) bool { return !sameBox(ctx.Session, s, *ctx.OneShot) }) {
		e.Unavailable = "this window belongs to one box; open another with discobox attach"
	}
	if e.Unavailable != "" {
		e.Enabled, e.Why = false, e.Unavailable
	}
	return e
}

func sameBox(session Session, a, b Sandbox) bool {
	return a.ID == b.ID && session.ServerName(a.Server) == session.ServerName(b.Server)
}

// Frontend is what a frontend does for the commands it dispatches through the
// registry: one method per command, plus how it says a command cannot run.
// R is what a frontend's handler returns (the TUI's tea.Cmd). A command is
// added as a registry entry, a case in Dispatch and a method here together:
// the compiler holds every frontend to the method, and TestEveryCommandDispatches
// holds the registry to Dispatch. A frontend that cannot offer a command yet
// says so (598 §1).
type Frontend[R any] interface {
	// Unavailable says a command cannot run from where it was asked.
	Unavailable(why string) R
	// Cannot says a command cannot act on its targets, and why.
	Cannot(c Command, why string) R
	// OfferUpgrade is asked instead of opening a box whose image is no longer
	// available on its pool: upgrading it is the way out.
	OfferUpgrade(box Sandbox) R

	Attach(box Sandbox) R
	Shell(box Sandbox) R
	ReviewConfig(box Sandbox) R
	Rename(box Sandbox) R
	Settings(box Sandbox) R
	// Do sends a lifecycle verb for every target, in one batch.
	Do(verb Verb, targets []Sandbox) R
	// Confirm is asked instead of Do for a verb that asks first
	// (Confirmation): it puts q to the person, and sends the verb, as Do
	// would, only on AnswerYes.
	Confirm(q Question, verb Verb, targets []Sandbox) R

	Tools(box Sandbox) R
	Issue(box Sandbox) R
	PullRequest(box Sandbox) R
	FilesChanged(box Sandbox) R
	Audit(box Sandbox) R
	Services(box Sandbox) R
	CredentialRequests(box Sandbox) R
	RejectedCredentials(box Sandbox) R

	NewBox() R
	Credentials() R
	Harnesses() R
	Quit() R
	Help() R
	Refresh() R
	Filters() R
	AllBoxes() R
	ToggleArchived() R
	RunOptions() R
	Repaint() R
	Palette() R

	NewTerminal() R
	EndSession() R
	Maximize() R
	Detach() R
	ToggleMouse() R
	Actions() R
	DismissBanner() R

	// Moving between panes and boxes is the frontend's navigation (876 §2);
	// the registry holds the commands and their keys, and each frontend moves
	// its own focus.
	NextPane() R
	PreviousPane() R
	NextBox() R
	PreviousBox() R
	ToggleSidebar() R
}

// Dispatch runs one command against explicit targets: it refuses one with no
// box to act on, a box on a server that is gone, a command the context rules
// out, and a lifecycle command its targets cannot take; then it counts the use
// and enters the command's flow in the frontend — through the question a verb
// asks first, when it asks one (Confirmation). A box command's other
// preconditions — a tag to open, a request waiting — are its flow's to refuse,
// in that flow's own words; Entry still reports them, for a frontend to offer
// only what will act. A box command takes its targets as given and never falls
// back to what is on screen; a window's command takes none. An unknown ID does
// nothing.
func Dispatch[R any](r *Registry, f Frontend[R], ctx Context, id string, targets []Sandbox) R {
	var none R
	c, ok := Lookup(id)
	if !ok {
		return none
	}
	if c.Target == TargetNone {
		// A window's command takes no box, whichever the frontend had to hand.
		targets = nil
	}
	if c.Target == TargetBox && len(targets) == 0 {
		return f.Unavailable("nothing selected")
	}
	e := r.entry(ctx, c, targets)
	if c.Lifecycle {
		// Checked ahead of eligibility: a box whose image is gone has no
		// container to open, and eligibility's answer — repair — is the
		// heavier of the two ways out when an upgrade will do.
		if c.opensBox && len(targets) == 1 && targets[0].ImageUnavailable {
			return f.OfferUpgrade(targets[0])
		}
		if !e.Enabled {
			return f.Cannot(c, e.Why)
		}
	}
	for _, s := range targets {
		if r.ds.ForServer(s.Server) == nil {
			return f.Unavailable(fmt.Sprintf("server %q is no longer available", s.Server))
		}
	}
	if e.Unavailable != "" {
		return f.Unavailable(e.Unavailable)
	}
	if !c.countedByFlow && !c.moves {
		r.Use(c.ID)
	}
	if c.verb != "" {
		// What a verb asks before it is sent is the core's, and asked on
		// the way in: a frontend never sends one it was not answered on.
		if q, asks := Confirmation(c.verb, targets); asks {
			return f.Confirm(q, c.verb, targets)
		}
		return f.Do(c.verb, targets)
	}
	var box Sandbox
	if len(targets) > 0 {
		box = targets[0]
	}
	switch c.ID {
	case "attach":
		return f.Attach(box)
	case "shell":
		return f.Shell(box)
	case "review config":
		return f.ReviewConfig(box)
	case "rename":
		return f.Rename(box)
	case "settings":
		return f.Settings(box)
	case "tools":
		return f.Tools(box)
	case "issue":
		return f.Issue(box)
	case "pr":
		return f.PullRequest(box)
	case "files":
		return f.FilesChanged(box)
	case "audit":
		return f.Audit(box)
	case "services":
		return f.Services(box)
	case "requests":
		return f.CredentialRequests(box)
	case "rejections":
		return f.RejectedCredentials(box)
	case "new":
		return f.NewBox()
	case "credentials":
		return f.Credentials()
	case "harnesses":
		return f.Harnesses()
	case "quit":
		return f.Quit()
	case "help":
		return f.Help()
	case "refresh":
		return f.Refresh()
	case "servers":
		return f.Filters()
	case "all":
		return f.AllBoxes()
	case "archived":
		return f.ToggleArchived()
	case "options":
		return f.RunOptions()
	case "repaint":
		return f.Repaint()
	case "palette":
		return f.Palette()
	case "terminal":
		return f.NewTerminal()
	case "end":
		return f.EndSession()
	case "maximize":
		return f.Maximize()
	case "detach":
		return f.Detach()
	case "mouse":
		return f.ToggleMouse()
	case "actions":
		return f.Actions()
	case "dismiss":
		return f.DismissBanner()
	case "next pane":
		return f.NextPane()
	case "previous pane":
		return f.PreviousPane()
	case "next box":
		return f.NextBox()
	case "previous box":
		return f.PreviousBox()
	case "sidebar":
		return f.ToggleSidebar()
	}
	return none
}

// Lifecycle is the box's own actions as they stand for these targets: the
// action menu's entries. Unavailable ones stay with the reason, rather than
// disappearing and leaving you wondering where upgrade went.
func Lifecycle(targets []Sandbox) []Entry {
	var out []Entry
	for _, c := range registry {
		if c.Lifecycle {
			e := Entry{Command: c}
			e.Enabled, e.Why = lifecycleEligible(c.ID, targets)
			out = append(out, e)
		}
	}
	return out
}

// lifecycleEligible is whether a lifecycle command can act on every target,
// and why not.
func lifecycleEligible(id string, targets []Sandbox) (bool, string) {
	one := len(targets) == 1
	anyUpgrade, anyRepair, anyRunning, anyStopped := false, false, false, false
	anyArchived, allArchived := false, len(targets) > 0
	for _, s := range targets {
		anyUpgrade = anyUpgrade || s.Upgrade
		anyRepair = anyRepair || s.Repairable()
		// A suspended box takes both: stop powers it off, start thaws it.
		anyRunning = anyRunning || s.State == StateRunning || s.State == StateSuspended
		anyStopped = anyStopped || s.Startable()
		anyArchived = anyArchived || s.State == StateArchived
		allArchived = allArchived && s.State == StateArchived
	}
	switch id {
	case "attach", "shell":
		return one && targets[0].Attachable(), AttachWhy(targets)
	case "review config":
		return one && targets[0].State == StateRunning, "takes one running box"
	case "rename":
		// The name is written into the box (ADR 26-10-12-026), which is
		// started to take it. An archived one has nothing to start.
		if one && targets[0].State == StateArchived {
			return false, "an archived box cannot be started to take a name — unarchive it first"
		}
		return one, "takes exactly one box"
	case "settings":
		if one && targets[0].State == StateArchived {
			return false, "an archived box is unarchived before its settings change"
		}
		return one, "takes exactly one box"
	case "upgrade":
		return anyUpgrade, "already on the current image"
	case "repair":
		return anyRepair, "an archived box is unarchived, not repaired"
	case "stop":
		return anyRunning, "not running"
	case "start":
		return anyStopped, "already on, or archived"
	case "archive":
		return !allArchived, "already archived"
	case "unarchive":
		return anyArchived, "not archived"
	case "purge":
		return allArchived, "archive it first — purge only takes archived boxes"
	}
	return false, ""
}

// AttachWhy names what stops one box being opened, which for the two ways a
// box can have no container is not the same thing; empty when nothing does.
// Archived is reversible by intent and says so. Anything else with nothing to
// attach to is a create or rebuild that never produced a container, and a
// settled failure is never retried on its own (ADR 0017 §4) — repair is the
// intent that rebuilds it (ADR 0035).
//
// The state name is not the reason. "the box is error" described the row rather
// than the obstacle, and it was wrong as often as not: the boxes it refused
// mostly had a container and a stale error latched over it.
func AttachWhy(targets []Sandbox) string {
	if len(targets) != 1 {
		return "takes exactly one box"
	}
	box := targets[0]
	if box.ImageUnavailable {
		if box.Upgrade {
			return "its image is no longer available on its pool — upgrade moves it to its harness's current image"
		}
		return "its image is no longer available on its pool, and its harness has no newer image"
	}
	if box.Attachable() {
		return ""
	}
	if box.State == StateArchived {
		return "the box is archived — unarchive it first"
	}
	return "the box has no container; repair rebuilds it"
}

// tui and desktop are bindings for one command, written beside it.
func tui(keys ...string) TUIBinding { return TUIBinding{Keys: keys} }

// leader adds one sequence pressed after the leader; called again, another
// sequence for the same command.
func (b TUIBinding) leader(seq ...string) TUIBinding {
	b.Leader = append(slices.Clone(b.Leader), seq)
	return b
}

func (b TUIBinding) repeats() TUIBinding { b.Repeats = true; return b }

func (b TUIBinding) chords(keys ...string) TUIBinding { b.Chords = keys; return b }

func (b TUIBinding) menu(keys ...string) TUIBinding { b.Menu = keys; return b }

func desktop(keys ...string) DesktopBinding { return DesktopBinding{Keys: keys} }

// in puts a command in one of the desktop's menus.
func (b DesktopBinding) in(menu Menu) DesktopBinding { b.Menu = menu; return b }

// registry is every command, in the order hints, menus and the palette list
// them: the box's lifecycle first, as its action menu orders it.
var registry = []Command{
	{ID: "attach", Label: "attach", Detail: "join the harness terminal", Aliases: []string{"attach", "open"},
		Target: TargetBox, Lifecycle: true, opensBox: true,
		Bindings: Bindings{TUI: tui("a").leader("a"), Desktop: desktop("enter").in(MenuBox)}},
	{ID: "shell", Label: "shell", Detail: "open a shell in the box", Aliases: []string{"shell"},
		Target: TargetBox, Lifecycle: true, opensBox: true,
		Bindings: Bindings{TUI: tui("s").leader("s"), Desktop: desktop("primary+t").in(MenuSession)}},
	{ID: "review config", Label: "review config", Detail: "review and save local settings and logins", Aliases: []string{"review config"},
		Target: TargetBox, Lifecycle: true, countedByFlow: true,
		Bindings: Bindings{TUI: tui("H").leader("H"), Desktop: desktop().in(MenuBox)}},
	{ID: "rename", Label: "rename", Detail: "type a new name for the box", Aliases: []string{"rename"},
		Target: TargetBox, Lifecycle: true,
		Bindings: Bindings{TUI: tui("e").leader("e"), Desktop: desktop("f2").in(MenuBox)}},
	{ID: "settings", Label: "settings", Detail: "edit its configured name, harness, egress and discobox API access", Aliases: []string{"settings"},
		Target: TargetBox, Lifecycle: true,
		Bindings: Bindings{TUI: tui("E").leader("E"), Desktop: desktop().in(MenuBox)}},
	{ID: "upgrade", Label: "upgrade", Detail: "re-pin to the current harness image", Aliases: []string{"upgrade"},
		Target: TargetBox, Lifecycle: true, verb: VerbUpgrade,
		Bindings: Bindings{TUI: tui("u").leader("u"), Desktop: desktop().in(MenuBox)}},
	// Repair is R rather than r, which refreshes the list — and the shift is
	// no loss on an action that rebuilds a container, next to one that
	// redraws a table.
	{ID: "repair", Label: "repair", Detail: "rebuild the box on the current image, keeping its work", Aliases: []string{"repair"},
		Target: TargetBox, Lifecycle: true, verb: VerbRepair,
		Bindings: Bindings{TUI: tui("R").leader("R"), Desktop: desktop().in(MenuBox)}},
	{ID: "stop", Label: "stop", Detail: "power the box off, keeping its disk", Aliases: []string{"stop"},
		Target: TargetBox, Lifecycle: true, verb: VerbStop,
		Bindings: Bindings{TUI: tui("t").leader("t"), Desktop: desktop().in(MenuBox)}},
	{ID: "start", Label: "start", Detail: "power a stopped box back on, or thaw a suspended one", Aliases: []string{"start"},
		Target: TargetBox, Lifecycle: true, verb: VerbStart,
		Bindings: Bindings{TUI: tui("T").leader("T"), Desktop: desktop().in(MenuBox)}},
	{ID: "archive", Label: "archive", Detail: "put it away, disk and all, reversibly", Aliases: []string{"archive", "rm"},
		Target: TargetBox, Lifecycle: true, verb: VerbArchive,
		Bindings: Bindings{TUI: tui("x", "delete").leader("x"), Desktop: desktop().in(MenuBox)}},
	{ID: "unarchive", Label: "unarchive", Detail: "bring an archived box back", Aliases: []string{"unarchive"},
		Target: TargetBox, Lifecycle: true, verb: VerbUnarchive,
		Bindings: Bindings{TUI: tui("U").leader("U"), Desktop: desktop().in(MenuBox)}},
	{ID: "purge", Label: "purge", Detail: "destroy it and its data, for good", Aliases: []string{"purge"},
		Target: TargetBox, Lifecycle: true, verb: VerbPurge,
		Bindings: Bindings{TUI: tui("P").leader("P"), Desktop: desktop().in(MenuBox)}},

	{ID: "tools", Label: "tools", Detail: "open a diff viewer, an editor or your IDE on the box", Aliases: []string{"tools"},
		Target: TargetBox, countedByFlow: true,
		Bindings: Bindings{TUI: tui("o").leader("o"), Desktop: desktop("primary+alt+o").in(MenuBox)}},
	{ID: "issue", Label: "issue", Detail: "open the tagged issue", Aliases: []string{"issue"},
		Target:   TargetBox,
		Bindings: Bindings{TUI: tui("i").leader("i"), Desktop: desktop().in(MenuBox)}},
	{ID: "pr", Label: "pull request", Detail: "open the tagged pull request", Aliases: []string{"pull request", "pr"},
		Target:   TargetBox,
		Bindings: Bindings{TUI: tui("p").leader("p"), Desktop: desktop().in(MenuBox)}},
	{ID: "audit", Label: "audit", Detail: "follow everything the box has done, as it happens", Aliases: []string{"audit"},
		Target: TargetBox, countedByFlow: true,
		Bindings: Bindings{TUI: tui().leader("A"), Desktop: desktop().in(MenuBox)}},
	{ID: "new", Label: "new discobox", Detail: "open the new discobox card", Aliases: []string{"new discobox", "new"},
		countedByFlow: true,
		Bindings:      Bindings{TUI: tui("n").leader("n"), Desktop: desktop("primary+n").in(MenuConsole)}},
	{ID: "credentials", Label: "credentials", Detail: "manage credentials", Aliases: []string{"credentials", "secrets"},
		countedByFlow: true,
		Bindings:      Bindings{TUI: tui("f4").leader("f4"), Desktop: desktop().in(MenuConsole)}},
	{ID: "harnesses", Label: "harnesses", Detail: "manage harnesses", Aliases: []string{"harnesses"},
		countedByFlow: true,
		Bindings:      Bindings{TUI: tui("f3").leader("f3"), Desktop: desktop().in(MenuConsole)}},
	{ID: "quit", Label: "quit", Detail: "leave this window", Aliases: []string{"quit", "exit"},
		Bindings: Bindings{TUI: tui("q", "ctrl+c").leader("q"), Desktop: desktop("primary+q").in(MenuConsole)}},
	{ID: "help", Label: "help", Detail: "show the console keys", Aliases: []string{"help"},
		Bindings: Bindings{TUI: tui("f1", "?").leader("f1"), Desktop: desktop("f1").in(MenuHelp)}},
	{ID: "refresh", Label: "refresh", Detail: "read the boxes again", Aliases: []string{"refresh"},
		Bindings: Bindings{TUI: tui("r").leader("r"), Desktop: desktop("primary+r").in(MenuConsole)}},
	{ID: "servers", Label: "servers", Detail: "choose the server, folder or tags", Aliases: []string{"servers", "tags", "filters"},
		Bindings: Bindings{Desktop: desktop("primary+l").in(MenuView)}},
	{ID: "all", Label: "all discoboxes", Detail: "show every discobox, with this one beside them", Aliases: []string{"all discoboxes", "boxes"},
		Bindings: Bindings{TUI: tui().menu(".", "l"), Desktop: desktop().in(MenuView)}},
	{ID: "services", Label: "services", Detail: "view output, start, stop or restart a service", Aliases: []string{"services"},
		Target: TargetBox, countedByFlow: true,
		Bindings: Bindings{TUI: tui().leader("S", "0"), Desktop: desktop("primary+alt+s").in(MenuBox)}},
	{ID: "requests", Label: "credential requests", Detail: "answer this box's credential requests", Aliases: []string{"credential requests", "requests"},
		Target:   TargetBox,
		Bindings: Bindings{TUI: tui("C").leader("g"), Desktop: desktop().in(MenuBox)}},
	{ID: "rejections", Label: "rejected credentials", Detail: "remedy a refused credential", Aliases: []string{"rejected credentials", "rejections"},
		Target:   TargetBox,
		Bindings: Bindings{TUI: tui("K").leader("K"), Desktop: desktop().in(MenuBox)}},
	{ID: "files", Label: "files changed", Detail: "review the pull request's changes", Aliases: []string{"files changed", "diff"},
		Target:   TargetBox,
		Bindings: Bindings{TUI: tui("f").leader("f"), Desktop: desktop().in(MenuBox)}},
	{ID: "archived", Label: "show archived", Detail: "show or hide archived boxes", Aliases: []string{"show archived"},
		Bindings: Bindings{TUI: tui("A"), Desktop: desktop("primary+alt+a").in(MenuView)}},
	{ID: "options", Label: "run options", Detail: "choose defaults for the next discobox", Aliases: []string{"run options", "options"},
		Bindings: Bindings{TUI: tui("ctrl+o"), Desktop: desktop().in(MenuConsole)}},
	{ID: "repaint", Label: "repaint", Detail: "redraw and reread the console", Aliases: []string{"repaint"},
		Bindings: Bindings{TUI: tui("ctrl+l"), Desktop: desktop().in(MenuView)}},

	// These own a workspace pane rather than a box chosen from the list.
	{ID: "terminal", Label: "new terminal", Detail: "open another harness terminal", Aliases: []string{"new terminal"},
		Bindings: Bindings{TUI: tui().leader("c"), Desktop: desktop("primary+alt+t").in(MenuSession)}},
	{ID: "end", Label: "end terminal", Detail: "end the focused session", Aliases: []string{"end terminal"},
		Bindings: Bindings{TUI: tui().leader("X"), Desktop: desktop("primary+w").in(MenuSession)}},
	{ID: "maximize", Label: "maximize", Detail: "maximize or restore the focused column", Aliases: []string{"maximize"},
		Bindings: Bindings{TUI: tui().leader("z"), Desktop: desktop("primary+enter").in(MenuSession)}},
	{ID: "detach", Label: "detach", Detail: "leave the workspace, keeping sessions running", Aliases: []string{"detach"},
		Bindings: Bindings{TUI: tui().leader("d"), Desktop: desktop("primary+0").in(MenuView)}},
	{ID: "mouse", Label: "toggle mouse", Detail: "take or return the application's mouse", Aliases: []string{"toggle mouse"},
		Bindings: Bindings{TUI: tui().leader("m"), Desktop: desktop().in(MenuSession)}},
	{ID: "actions", Label: "actions", Detail: "open the focused box's action menu", Aliases: []string{"actions"},
		Bindings: Bindings{TUI: tui(".").leader("."), Desktop: desktop("shift+f10").in(MenuBox)}},
	{ID: "dismiss", Label: "dismiss banner", Detail: "dismiss the current notice", Aliases: []string{"dismiss banner"},
		Bindings: Bindings{TUI: tui().leader("b"), Desktop: desktop().in(MenuSession)}},
	{ID: "palette", Label: "command palette", Detail: "find a box or command", Aliases: []string{"command palette", "palette"},
		countedByFlow: true,
		Bindings:      Bindings{TUI: tui(":").leader("k"), Desktop: desktop("primary+k", "primary+p").in(MenuView)}},

	// Moving between panes and boxes. Where focus goes is each frontend's
	// (876 §2); the commands and their keys are the registry's like any other.
	// Off the leftmost pane is the list beside the workspace.
	{ID: "next pane", Label: "next pane", Detail: "move to the pane on the right", Aliases: []string{"next pane"},
		moves:    true,
		Bindings: Bindings{TUI: tui().leader("l").leader("right").repeats(), Desktop: desktop("ctrl+tab").in(MenuGo)}},
	{ID: "previous pane", Label: "previous pane", Detail: "move to the pane on the left, and off the leftmost to the list", Aliases: []string{"previous pane"},
		moves:    true,
		Bindings: Bindings{TUI: tui().leader("h").leader("left").repeats(), Desktop: desktop("ctrl+shift+tab").in(MenuGo)}},
	// The walk has leaderless chords as well: Ctrl-Shift-K and J reach no
	// program, since a classic terminal cannot encode them and sends Ctrl-K
	// and Ctrl-J instead — which are not these keys.
	{ID: "next box", Label: "next discobox", Detail: "switch to the discobox below in the list", Aliases: []string{"next discobox"},
		moves:    true,
		Bindings: Bindings{TUI: tui().leader("down").leader("j").repeats().chords("ctrl+shift+j"), Desktop: desktop("primary+alt+down").in(MenuGo)}},
	{ID: "previous box", Label: "previous discobox", Detail: "switch to the discobox above in the list", Aliases: []string{"previous discobox"},
		moves:    true,
		Bindings: Bindings{TUI: tui().leader("up").repeats().chords("ctrl+shift+k"), Desktop: desktop("primary+alt+up").in(MenuGo)}},
	{ID: "sidebar", Label: "toggle list", Detail: "show or hide the discobox list beside the workspace", Aliases: []string{"toggle list", "sidebar"},
		Bindings: Bindings{Desktop: desktop("primary+b").in(MenuView)}},
}

// Gesture is a binding that is not a command (671 §4): moving a cursor,
// choosing what it is on, jumping to a pane by its number. Each frontend acts
// on one itself; it is never searched for in the palette, counted or
// dispatched. Its keys are declared here beside the commands' so that every
// key a frontend's list, tree, leader and panes bind is in the registry, and
// classified.
//
// A numbered gesture's sequences end in a digit, which is the number it goes
// to as that frontend numbers what it goes to: the TUI's panes from 0, the
// primary, and its services from 1; the desktop's tabs from 1, and its
// services from 1.
type Gesture struct {
	ID       string
	Label    string
	Bindings Bindings
}

// Gestures is every gesture, in the order the registry lists them.
func Gestures() []Gesture { return slices.Clone(gestures) }

// LookupGesture is one gesture by ID.
func LookupGesture(id string) (Gesture, bool) {
	i := slices.IndexFunc(gestures, func(g Gesture) bool { return g.ID == id })
	if i < 0 {
		return Gesture{}, false
	}
	return gestures[i], true
}

// digits are the keys from one digit to another, each a sequence after
// whatever leads it.
func digits(lead string, from, to int) TUIBinding {
	var b TUIBinding
	for n := from; n <= to; n++ {
		if lead == "" {
			b = b.leader(fmt.Sprint(n))
		} else {
			b = b.leader(lead, fmt.Sprint(n))
		}
	}
	return b
}

// desktopDigits are the desktop's chords from one digit to another, each the
// digit after lead.
func desktopDigits(lead string, from, to int) DesktopBinding {
	var b DesktopBinding
	for n := from; n <= to; n++ {
		b.Keys = append(b.Keys, fmt.Sprintf("%s+%d", lead, n))
	}
	return b
}

var gestures = []Gesture{
	{ID: "up", Label: "move up", Bindings: Bindings{TUI: tui("up", "k"), Desktop: desktop("up")}},
	{ID: "down", Label: "move down", Bindings: Bindings{TUI: tui("down", "j"), Desktop: desktop("down")}},
	{ID: "page up", Label: "a screen up", Bindings: Bindings{TUI: tui("pgup")}},
	{ID: "page down", Label: "a screen down", Bindings: Bindings{TUI: tui("pgdown")}},
	{ID: "first", Label: "first", Bindings: Bindings{TUI: tui("home", "g"), Desktop: desktop("home")}},
	{ID: "last", Label: "last", Bindings: Bindings{TUI: tui("end", "G"), Desktop: desktop("end")}},
	{ID: "scroll left", Label: "read back along a long name", Bindings: Bindings{TUI: tui("left", "h")}},
	{ID: "scroll right", Label: "read on along a long name", Bindings: Bindings{TUI: tui("right", "l")}},
	{ID: "choose", Label: "choose", Bindings: Bindings{TUI: tui("enter")}},
	{ID: "back", Label: "back", Bindings: Bindings{TUI: tui("esc"), Desktop: desktop("escape", "right")}},
	{ID: "next field", Label: "next field", Bindings: Bindings{TUI: tui("tab")}},
	{ID: "previous field", Label: "previous field", Bindings: Bindings{TUI: tui("shift+tab")}},
	{ID: "select", Label: "select", Bindings: Bindings{TUI: tui(" "), Desktop: desktop("space")}},
	{ID: "range", Label: "select a range", Bindings: Bindings{TUI: tui("V"), Desktop: desktop("shift+space")}},
	{ID: "clear selection", Label: "clear the selection", Bindings: Bindings{TUI: tui("c"), Desktop: desktop("primary+d")}},
	{ID: "context menu", Label: "the row's menu", Bindings: Bindings{Desktop: desktop("contextmenu")}},
	{ID: "menu bar", Label: "the menu bar", Bindings: Bindings{Desktop: desktop("f10")}},
	{ID: "pane", Label: "a pane by its number", Bindings: Bindings{TUI: digits("", 0, 9), Desktop: desktopDigits("primary", 1, 9)}},
	{ID: "service", Label: "a service by its number", Bindings: Bindings{TUI: digits("S", 1, 9), Desktop: desktopDigits("primary+alt", 1, 9)}},
	// A pane's own keys on the desktop, which a terminal there answers
	// itself: the TUI's are the terminal it runs in.
	{ID: "copy", Label: "copy the selection", Bindings: Bindings{Desktop: desktop("primary+c")}},
	{ID: "paste", Label: "paste", Bindings: Bindings{Desktop: desktop("primary+v")}},
	{ID: "find", Label: "find in the scrollback", Bindings: Bindings{Desktop: desktop("primary+f")}},
	{ID: "scroll back", Label: "a screen back through the scrollback", Bindings: Bindings{Desktop: desktop("shift+pageup")}},
	{ID: "scroll on", Label: "a screen on through the scrollback", Bindings: Bindings{Desktop: desktop("shift+pagedown")}},
}
