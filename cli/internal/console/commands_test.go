package console

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// servers is a data source that knows some servers and nothing else: all the
// registry asks of one.
type servers struct {
	DataSource
	gone map[string]bool
}

func (s servers) ForServer(name string) DataSource {
	if s.gone[name] {
		return nil
	}
	return s
}

// Every command is described, searchable and bound unambiguously. A new
// action cannot quietly acquire a shortcut without searchable metadata, and an
// alias two commands share would make the palette's space completion
// ambiguous (ADR 26-11-06-671 §4).
func TestEveryCommandIsDescribed(t *testing.T) {
	ids, aliases := map[string]bool{}, map[string]string{}
	for _, c := range Commands() {
		if c.ID == "" || c.Label == "" || c.Detail == "" || len(c.Aliases) == 0 {
			t.Fatalf("incomplete command: %+v", c)
		}
		if ids[c.ID] {
			t.Fatalf("duplicate ID %s", c.ID)
		}
		ids[c.ID] = true
		if c.Aliases[0] != c.Label {
			t.Errorf("%s: the label is not its first alias", c.ID)
		}
		for _, alias := range c.Aliases {
			alias = strings.ToLower(alias)
			if old, ok := aliases[alias]; ok && old != c.ID {
				t.Fatalf("ambiguous alias %s for %s and %s", alias, old, c.ID)
			}
			aliases[alias] = c.ID
		}
		if c.Lifecycle && (c.Target != TargetBox || len(c.Bindings.TUI.Keys) == 0) {
			t.Errorf("%s: a lifecycle command takes a box and has a list key", c.ID)
		}
	}
	for _, g := range Gestures() {
		if g.ID == "" || g.Label == "" {
			t.Fatalf("incomplete gesture: %+v", g)
		}
		if ids[g.ID] {
			t.Fatalf("gesture %s has a command's ID", g.ID)
		}
		ids[g.ID] = true
	}
}

// No key reaches two things: in the TUI a bare key, a sequence after the
// leader and a leaderless chord each name one command or gesture, and so does
// a desktop chord. A sequence after the leader is one key or a chord of two,
// and a chord's first key leads nothing else.
func TestEveryBindingIsUnambiguous(t *testing.T) {
	type bound struct {
		id string
		b  Bindings
	}
	var all []bound
	for _, c := range Commands() {
		all = append(all, bound{c.ID, c.Bindings})
	}
	for _, g := range Gestures() {
		all = append(all, bound{g.ID, g.Bindings})
	}
	bare, leader, chords, desk := map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
	claim := func(layer map[string]string, key, id, what string) {
		if old, ok := layer[key]; ok && old != id {
			t.Errorf("%s %q binds both %s and %s", what, key, old, id)
		}
		layer[key] = id
	}
	leads := map[string]bool{}
	for _, x := range all {
		for _, key := range x.b.TUI.Keys {
			claim(bare, key, x.id, "the bare key")
		}
		for _, seq := range x.b.TUI.Leader {
			if len(seq) == 0 || len(seq) > 2 {
				t.Errorf("%s: a leader sequence of %d keys", x.id, len(seq))
				continue
			}
			if len(seq) == 2 {
				leads[seq[0]] = true
			}
			claim(leader, strings.Join(seq, " "), x.id, "the leader sequence")
		}
		for _, key := range x.b.TUI.Chords {
			claim(chords, key, x.id, "the chord")
		}
		for _, key := range x.b.Desktop.Keys {
			claim(desk, key, x.id, "the desktop chord")
		}
	}
	for lead := range leads {
		if id, ok := leader[lead]; ok {
			t.Errorf("the leader's %q both runs %s and leads a chord", lead, id)
		}
	}
}

// Every command is in one of the desktop's menus, so the menu bar reaches
// every command by pointer as the palette does by name (ADR 26-11-07-598 §2);
// a gesture is not a command, and is in none.
func TestEveryCommandIsInADesktopMenu(t *testing.T) {
	for _, c := range Commands() {
		if !slices.Contains(Menus, c.Bindings.Desktop.Menu) {
			t.Errorf("%s is in the menu %q, which is none of the menu bar's", c.ID, c.Bindings.Desktop.Menu)
		}
	}
	for _, g := range Gestures() {
		if g.Bindings.Desktop.Menu != "" {
			t.Errorf("gesture %s is in the %s menu", g.ID, g.Bindings.Desktop.Menu)
		}
	}
}

// A numbered gesture's every sequence ends in its number.
func TestNumberedGesturesEndInADigit(t *testing.T) {
	for _, id := range []string{"pane", "service"} {
		g, ok := LookupGesture(id)
		if !ok {
			t.Fatalf("no gesture %s", id)
		}
		for _, seq := range g.Bindings.TUI.Leader {
			if last := seq[len(seq)-1]; len(last) != 1 || last[0] < '0' || last[0] > '9' {
				t.Errorf("%s: %v does not end in a digit", id, seq)
			}
		}
		for _, chord := range g.Bindings.Desktop.Keys {
			if last := chord[len(chord)-1]; last < '0' || last > '9' {
				t.Errorf("%s: %q does not end in a digit", id, chord)
			}
		}
	}
}

// A desktop chord is written with primary, and never adds shift to it: off
// macOS primary is Ctrl+Shift, so the two would be the same chord (ADR
// 26-11-07-598 §2).
func TestDesktopBindingsUsePrimaryWithoutShift(t *testing.T) {
	for _, c := range Commands() {
		for _, chord := range c.Bindings.Desktop.Keys {
			parts := strings.Split(chord, "+")
			if slices.Contains(parts, "primary") && slices.Contains(parts, "shift") {
				t.Errorf("%s: %q adds shift to primary", c.ID, chord)
			}
		}
	}
}

func TestConsoleCommandEligibility(t *testing.T) {
	r := NewRegistry(servers{})
	tests := []struct {
		state   State
		command string
		enabled bool
	}{
		{StateRunning, "stop", true}, {StateSuspended, "stop", true}, {StateStopped, "stop", false},
		{StateStopped, "start", true}, {StateSuspended, "start", true}, {StateArchived, "start", false},
		{StateArchived, "rename", false}, {StateArchived, "settings", false}, {StateArchived, "purge", true},
		{StateRunning, "purge", false}, {StateError, "repair", true},
	}
	for _, tt := range tests {
		box := Sandbox{ID: "box", State: tt.state}
		e, found := r.Entry(Context{}, tt.command, []Sandbox{box})
		if !found {
			t.Errorf("missing command %s", tt.command)
			continue
		}
		if e.Enabled != tt.enabled {
			t.Errorf("%s on %s = %v (%s)", tt.command, tt.state, e.Enabled, e.Why)
		}
	}
}

// Where a command is asked from can rule it out whatever its target, and a
// window opened on one box offers nothing for another.
func TestTheContextRulesCommandsOut(t *testing.T) {
	r := NewRegistry(servers{})
	mine := Sandbox{ID: "sbx_mine", State: StateRunning, HasRuntime: true}
	other := Sandbox{ID: "sbx_other", State: StateRunning, HasRuntime: true}
	tests := []struct {
		name    string
		ctx     Context
		id      string
		targets []Sandbox
		why     string
	}{
		{"a pane command with no pane", Context{}, "maximize", nil, "requires a workspace pane"},
		{"a pane command in a workspace", Context{WorkspaceShown: true, PaneFocused: true}, "maximize", nil, ""},
		{"the list's menu needs no pane", Context{}, "actions", nil, ""},
		{"a workspace's menu needs its pane", Context{WorkspaceShown: true}, "actions", nil, "requires a workspace pane"},
		{"the list's menu beside a workspace", Context{WorkspaceShown: true, ListHasKeys: true}, "actions", nil, ""},
		{"a batch from an attach window holding another box", Context{OneShot: &mine}, "stop", []Sandbox{mine, other}, "this window belongs to one box; open another with discobox attach"},
		{"all discoboxes outside an attach window", Context{}, "all", nil, "available in a wide attach window"},
		{"all discoboxes in a narrow attach window", Context{OneShot: &mine}, "all", nil, "the window is too narrow for the list beside the box"},
		{"all discoboxes in a wide attach window", Context{OneShot: &mine, TreeFits: true}, "all", nil, ""},
		{"a page with no tree beside the workspace", Context{HasWorkspace: true}, "harnesses", nil, "the pages open beside the list; this window has no tree"},
		{"a page in an attach window", Context{OneShot: &mine, TreeFits: true}, "options", nil, "open discobox console for this page"},
		{"another box from an attach window", Context{OneShot: &mine}, "stop", []Sandbox{other}, "this window belongs to one box; open another with discobox attach"},
		{"its own box from an attach window", Context{OneShot: &mine}, "stop", []Sandbox{mine}, ""},
		{"an issue with no tag", Context{}, "issue", []Sandbox{mine}, "no issue to show — tag this discobox issue=N, on a GitHub repository"},
		{"requests with none waiting", Context{}, "requests", []Sandbox{mine}, "nothing waiting on this discobox"},
		{"requests waiting", Context{Waiting: func(Sandbox) bool { return true }}, "requests", []Sandbox{mine}, ""},
		{"a box command on an archived box", Context{}, "tools", []Sandbox{{ID: "a", State: StateArchived}}, "the box is archived — unarchive it first"},
		{"a pane walk with no workspace", Context{}, "next pane", nil, "requires a workspace"},
		{"a pane walk from a page beside the tree", Context{Tree: true, TreeShown: true, PageShown: true}, "previous pane", nil, ""},
		{"the previous pane from a page with the list put away", Context{Tree: true, PageShown: true, HasWorkspace: true}, "previous pane", nil, "there is no list beside this page"},
		{"the next pane from the list beside a page, no workspace", Context{Tree: true, TreeShown: true, ListHasKeys: true}, "next pane", nil, ""},
		{"the next pane over a page", Context{Tree: true, PageShown: true, HasWorkspace: true}, "next pane", nil, "the page is in front of the panes; Esc puts it away"},
		{"the next pane from the list beside a workspace", Context{Tree: true, ListHasKeys: true, HasWorkspace: true}, "next pane", nil, ""},
		{"the previous pane from the list", Context{Tree: true, ListHasKeys: true, HasWorkspace: true}, "previous pane", nil, "the list is the leftmost; there is nothing left of it"},
		{"a box walk with no list beside the workspace", Context{HasWorkspace: true}, "next box", nil, "there is no list beside a workspace in this window"},
		{"a box walk beside the list", Context{HasWorkspace: true, Tree: true}, "previous box", nil, ""},
		{"a box command on the workspace's own box", Context{Workspace: &Sandbox{ID: "a"}}, "audit", []Sandbox{{ID: "a", State: StateError}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := r.Entry(tt.ctx, tt.id, tt.targets)
			if e.Enabled != (tt.why == "") || (tt.why != "" && e.Why != tt.why) {
				t.Fatalf("%s: enabled %v, why %q; want why %q", tt.id, e.Enabled, e.Why, tt.why)
			}
		})
	}
}

// recorder is a frontend that writes down what it was asked to do.
type recorder struct{ calls []string }

func (f *recorder) call(format string, args ...any) string {
	s := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, s)
	return s
}
func (f *recorder) Unavailable(why string) string          { return f.call("unavailable: %s", why) }
func (f *recorder) Cannot(c Command, why string) string    { return f.call("cannot %s: %s", c.ID, why) }
func (f *recorder) OfferUpgrade(box Sandbox) string        { return f.call("offer upgrade %s", box.ID) }
func (f *recorder) Attach(box Sandbox) string              { return f.call("attach %s", box.ID) }
func (f *recorder) Shell(box Sandbox) string               { return f.call("shell %s", box.ID) }
func (f *recorder) ReviewConfig(box Sandbox) string        { return f.call("review config %s", box.ID) }
func (f *recorder) Rename(box Sandbox) string              { return f.call("rename %s", box.ID) }
func (f *recorder) Settings(box Sandbox) string            { return f.call("settings %s", box.ID) }
func (f *recorder) Tools(box Sandbox) string               { return f.call("tools %s", box.ID) }
func (f *recorder) Issue(box Sandbox) string               { return f.call("issue %s", box.ID) }
func (f *recorder) PullRequest(box Sandbox) string         { return f.call("pr %s", box.ID) }
func (f *recorder) FilesChanged(box Sandbox) string        { return f.call("files %s", box.ID) }
func (f *recorder) Audit(box Sandbox) string               { return f.call("audit %s", box.ID) }
func (f *recorder) Services(box Sandbox) string            { return f.call("services %s", box.ID) }
func (f *recorder) CredentialRequests(box Sandbox) string  { return f.call("requests %s", box.ID) }
func (f *recorder) RejectedCredentials(box Sandbox) string { return f.call("rejections %s", box.ID) }
func (f *recorder) Do(verb Verb, targets []Sandbox) string {
	ids := make([]string, len(targets))
	for i, s := range targets {
		ids[i] = s.ID
	}
	return f.call("do %s %s", verb, strings.Join(ids, ","))
}
func (f *recorder) Confirm(_ Question, verb Verb, targets []Sandbox) string {
	ids := make([]string, len(targets))
	for i, s := range targets {
		ids[i] = s.ID
	}
	return f.call("confirm %s %s", verb, strings.Join(ids, ","))
}
func (f *recorder) NewBox() string         { return f.call("new") }
func (f *recorder) Credentials() string    { return f.call("credentials") }
func (f *recorder) Harnesses() string      { return f.call("harnesses") }
func (f *recorder) Quit() string           { return f.call("quit") }
func (f *recorder) Help() string           { return f.call("help") }
func (f *recorder) Refresh() string        { return f.call("refresh") }
func (f *recorder) Filters() string        { return f.call("servers") }
func (f *recorder) AllBoxes() string       { return f.call("all") }
func (f *recorder) ToggleArchived() string { return f.call("archived") }
func (f *recorder) RunOptions() string     { return f.call("options") }
func (f *recorder) Repaint() string        { return f.call("repaint") }
func (f *recorder) Palette() string        { return f.call("palette") }
func (f *recorder) Theme() string          { return f.call("theme") }
func (f *recorder) NewTerminal() string    { return f.call("terminal") }
func (f *recorder) EndSession() string     { return f.call("end") }
func (f *recorder) Maximize() string       { return f.call("maximize") }
func (f *recorder) Detach() string         { return f.call("detach") }
func (f *recorder) ToggleMouse() string    { return f.call("mouse") }
func (f *recorder) Actions() string        { return f.call("actions") }
func (f *recorder) DismissBanner() string  { return f.call("dismiss") }
func (f *recorder) NextPane() string       { return f.call("next pane") }
func (f *recorder) PreviousPane() string   { return f.call("previous pane") }
func (f *recorder) NextBox() string        { return f.call("next box") }
func (f *recorder) PreviousBox() string    { return f.call("previous box") }
func (f *recorder) ToggleSidebar() string  { return f.call("sidebar") }

// Every command dispatches to a flow of its own: a command in the registry
// that no frontend method answers would do nothing at all.
func TestEveryCommandDispatches(t *testing.T) {
	r := NewRegistry(servers{})
	box := Sandbox{ID: "sbx_one", State: StateRunning, HasRuntime: true, Upgrade: true,
		Repository: "https://github.com/acme/foo", Tags: []string{"issue=1", "pr=2"}}
	ctx := Context{HasWorkspace: true, WorkspaceShown: true, PaneFocused: true, Tree: true, TreeFits: true, Themed: true,
		Waiting: func(Sandbox) bool { return true }, Refused: func(Sandbox) bool { return true }}
	flows := map[string]string{}
	for _, c := range Commands() {
		var targets []Sandbox
		ctx := ctx
		target := box
		switch c.ID {
		case "all":
			ctx.OneShot = &box
		case "start", "unarchive", "purge":
			target.State = StateArchived
			if c.ID == "start" {
				target.State = StateStopped
			}
		}
		if c.Target == TargetBox {
			targets = []Sandbox{target}
		}
		f := &recorder{}
		got := Dispatch(r, Frontend[string](f), ctx, c.ID, targets)
		if got == "" || strings.HasPrefix(got, "unavailable") || strings.HasPrefix(got, "cannot") {
			t.Errorf("%s dispatched to %q", c.ID, got)
			continue
		}
		flow, _, _ := strings.Cut(got, " "+target.ID)
		if old, ok := flows[flow]; ok && c.verb == "" {
			t.Errorf("%s and %s enter the same flow %q", old, c.ID, flow)
		}
		flows[flow] = c.ID
	}
}

// Dispatch takes the box it is given, refuses before it enters a flow, and
// counts a use only once the flow is entered.
func TestDispatchRefusesBeforeItEnters(t *testing.T) {
	gone := Sandbox{ID: "sbx_gone", Server: "far", State: StateRunning, HasRuntime: true}
	running := Sandbox{ID: "sbx_one", State: StateRunning, HasRuntime: true}
	tests := []struct {
		name    string
		ctx     Context
		id      string
		targets []Sandbox
		want    string
	}{
		{"a box command with no box", Context{}, "stop", nil, "unavailable: nothing selected"},
		{"a verb the box cannot take", Context{}, "start", []Sandbox{running}, "cannot start: already on, or archived"},
		{"a box on a server that went away", Context{}, "stop", []Sandbox{gone}, `unavailable: server "far" is no longer available`},
		{"a box whose image is gone", Context{}, "attach", []Sandbox{{ID: "sbx_old", ImageUnavailable: true, Upgrade: true}}, "offer upgrade sbx_old"},
		{"a batch", Context{}, "stop", []Sandbox{running, {ID: "sbx_two", State: StateSuspended}}, "do stop sbx_one,sbx_two"},
		{"the given box, not another", Context{}, "tools", []Sandbox{running}, "tools sbx_one"},
		{"a window's command ignores a box it was handed", Context{}, "help", []Sandbox{gone}, "help"},
		{"another box from a one-box window", Context{OneShot: &running}, "tools", []Sandbox{{ID: "sbx_other", State: StateRunning, HasRuntime: true}}, "unavailable: this window belongs to one box; open another with discobox attach"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry(servers{gone: map[string]bool{"far": true}})
			f := &recorder{}
			if got := Dispatch(r, Frontend[string](f), tt.ctx, tt.id, tt.targets); got != tt.want {
				t.Fatalf("dispatch %s = %q, want %q", tt.id, got, tt.want)
			}
			wantUse := !strings.HasPrefix(tt.want, "unavailable") && !strings.HasPrefix(tt.want, "cannot") && !strings.HasPrefix(tt.want, "offer")
			if c, _ := Lookup(tt.id); !c.countedByFlow && (r.Usage(tt.id) == 1) != wantUse {
				t.Fatalf("%s counted %d uses", tt.id, r.Usage(tt.id))
			}
		})
	}
}

// The reason names the obstacle, not the row's state: the two ways to have no
// container are undone by different things.
func TestAttachWhyNamesTheObstacle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    Sandbox
		want string
	}{
		{"archived points at unarchive", Sandbox{State: StateArchived}, "unarchive"},
		{"no container points at repair", Sandbox{State: StateError}, "repair"},
		{"attachable has nothing to say", Sandbox{State: StateError, HasRuntime: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AttachWhy([]Sandbox{tc.s})
			if tc.want == "" {
				if got != "" {
					t.Fatalf("why = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("why = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// Moving focus is not a use (671 §6): a walk pressed in a run would otherwise
// put itself at the top of the palette.
func TestAWalkIsNotAUse(t *testing.T) {
	r := NewRegistry(servers{})
	ctx := Context{HasWorkspace: true, Tree: true, WorkspaceShown: true, PaneFocused: true}
	for _, id := range []string{"next pane", "previous pane", "next box", "previous box"} {
		Dispatch(r, Frontend[string](&recorder{}), ctx, id, nil)
		if n := r.Usage(id); n != 0 {
			t.Errorf("%s counted %d uses", id, n)
		}
	}
	Dispatch(r, Frontend[string](&recorder{}), ctx, "maximize", nil)
	if r.Usage("maximize") != 1 {
		t.Error("maximize was not counted")
	}
}
