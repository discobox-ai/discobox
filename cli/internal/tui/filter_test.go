package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The header says what the list is narrowed to and nothing about what it
// leaves open: everything is one phrase, and each narrowed filter is named,
// widest first.
func TestTheHeaderDescribesTheActiveFilters(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", taggedSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	filterTo(t, m, console.AllFolders)
	if got := headerLine(m); !strings.HasSuffix(got, "all discoboxes ▾") {
		t.Fatalf("header = %q, want it to say nothing is narrowed", got)
	}
	own := m.session.Folder().Label
	filterTo(t, m, "alpha", own, "#wip")
	if got, want := headerLine(m), "server alpha · "+own+" · #wip ▾"; !strings.HasSuffix(got, want) {
		t.Fatalf("header = %q, want it to end %q", got, want)
	}
}

// The card changes every filter at once: Space marks a choice in each group
// without moving the list, and Enter applies what is marked, together.
func TestTheFilterCardAppliesEveryMarkOnEnter(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", taggedSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)
	filterTo(t, m, console.AllFolders)

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgFilter {
		t.Fatal("Enter on the filter should open its card")
	}
	view := dialogText(m)
	for _, group := range []string{"Server", "Folder", "Tag"} {
		if !strings.Contains(view, "── "+group) {
			t.Errorf("the card has no %s group:\n%s", group, view)
		}
	}
	own := m.session.Folder()
	markFilter(t, m, "alpha")
	markFilter(t, m, own.Label)
	markFilter(t, m, "#wip")
	if m.list.Filter.Server != "" || m.list.Filter.Folder.Key != "" || len(m.list.Filter.Tags) != 0 {
		t.Fatalf("the list moved before Enter: server %q, folder %q, tags %q", m.list.Filter.Server, m.list.Filter.Folder.Label, m.list.Filter.Tags)
	}

	send(t, m, keyPress("enter"))
	if m.dialog != nil {
		t.Fatal("Enter should close the card")
	}
	if m.list.Filter.Server != "alpha" || m.list.Filter.Folder.Key != own.Key || !slices.Equal(m.list.Filter.Tags, []string{"wip"}) {
		t.Fatalf("server %q, folder %q, tags %q, want alpha, the window's own and wip", m.list.Filter.Server, m.list.Filter.Folder.Label, m.list.Filter.Tags)
	}
	for _, s := range m.list.Rows() {
		if s.Server != "alpha" || s.OriginKey != own.Key || !m.list.Filter.Tagged(s) {
			t.Fatalf("row %s is outside the filter", s.ID)
		}
	}
}

// Esc leaves the card with nothing applied, whatever was marked on it.
func TestEscDropsTheMarks(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	was := m.list.Filter.Folder.Key

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("enter"))
	markFilter(t, m, console.AllFolders)
	send(t, m, keyPress("esc"))
	if m.dialog != nil || m.list.Filter.Folder.Key != was {
		t.Fatalf("folder = %q after Esc, want it unchanged", m.list.Filter.Folder.Label)
	}
	if m.focus != focusFilter {
		t.Fatalf("focus = %v, want the filter the card was opened from", m.focus)
	}
}

// Each count is what that choice would list alongside the others marked, so
// marking a tag recounts the folders above it.
func TestTheCardCountsAgainstWhatIsMarked(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	filterTo(t, m, console.AllFolders)
	m.dialog = m.filterDialog()
	detail := func(label string) string {
		for _, row := range m.dialog.filter.rows() {
			if row.label == label {
				return row.detail
			}
		}
		t.Fatalf("no %q on the card", label)
		return ""
	}
	if got := detail(console.AllFolders); !strings.HasPrefix(got, plural(len(m.list.Rows()), "box", "boxes")) {
		t.Fatalf("all folders = %q, want every row counted", got)
	}
	markFilter(t, m, "#ticket=ENG-12")
	if got := detail(console.AllFolders); !strings.HasPrefix(got, "1 box") {
		t.Fatalf("all folders = %q with #ticket=ENG-12 marked, want the one box carrying it", got)
	}
}

// Over the secrets screen with the list on every server, the card marks the
// primary the screen shows, and an Enter that changed nothing leaves the list
// on every server rather than narrowing it to the primary behind the screen.
func TestAnUntouchedCardOverTheConfigScreensMovesNothing(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)
	send(t, m, keyPress(secretsKey))

	m.dialog = m.filterDialog()
	if got := dialogText(m); !strings.Contains(got, "● alpha") {
		t.Fatalf("the card does not mark the primary the screen shows:\n%s", got)
	}
	send(t, m, keyPress(" "), keyPress("enter"))
	if m.list.Filter.Server != "" {
		t.Fatalf("the list is on %q after an untouched card, want every server", m.list.Filter.Server)
	}
}

// A card taller than the window is drawn as a window over its rows that keeps
// the cursor on screen and says how much is out of it.
func TestTheFilterCardScrollsInAShortWindow(t *testing.T) {
	t.Parallel()
	var boxes []console.Sandbox
	for i := range 12 {
		dir := "/src/project-" + itoa(i)
		boxes = append(boxes, cutFrom(console.Sandbox{ID: "sbx_" + itoa(i), Name: "box " + itoa(i), State: console.StateRunning, Tags: []string{"t" + itoa(i)}}, dir))
	}
	m := newTestModel(t, newFakeSource(boxes...))
	m.dialog = m.filterDialog()

	const height = 20
	view := func() string { return ansi.Strip(m.dialog.view(m.st, &m.zones, 100, height)) }
	if got := strings.Count(view(), "\n") + 1; got > height {
		t.Fatalf("the card is %d lines in a %d-line window:\n%s", got, height, view())
	}
	if !strings.Contains(view(), "↓") || !strings.Contains(view(), "more") {
		t.Fatalf("the card does not say there is more below:\n%s", view())
	}
	for range len(m.dialog.filter.rows()) {
		send(t, m, keyPress("down"))
	}
	last := m.dialog.filter.rows()[m.dialog.filter.cursor].label
	if got := view(); !strings.Contains(got, "❯ ○ "+last) && !strings.Contains(got, "❯ ● "+last) {
		t.Fatalf("the cursor's row %q is off screen:\n%s", last, got)
	}
	if got := strings.Count(view(), "\n") + 1; got > height {
		t.Fatalf("the card is %d lines in a %d-line window", got, height)
	}
}

// Enter takes the row under the cursor along with whatever Space marked, so
// one change is ↓ Enter and several are Space in the other groups first.
func TestEnterPicksTheRowUnderTheCursor(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	filterTo(t, m, console.AllFolders)

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("enter"))
	markFilter(t, m, m.session.Folder().Label)
	p := m.dialog.filter
	for p.rows()[p.cursor].label != "#wip" {
		send(t, m, keyPress("down"))
	}
	send(t, m, keyPress("enter"))
	if m.list.Filter.Folder.Key != m.session.OriginKey || !slices.Equal(m.list.Filter.Tags, []string{"wip"}) {
		t.Fatalf("folder %q, tags %q, want the folder Space marked and the tag Enter was on", m.list.Filter.Folder.Label, m.list.Filter.Tags)
	}
}
