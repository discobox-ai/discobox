package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// openCreateCard types a prompt and presses Enter, which puts the card up.
func openCreateCard(t *testing.T, m *Model) *createCard {
	t.Helper()
	send(t, m, typeString("fix the reaper")...)
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgCreate {
		t.Fatal("Enter in the prompt should put the create card up")
	}
	return m.dialog.create
}

// The harness is under the list and only Tab reaches it: ↑↓ stay on the
// sources, which is the one thing most creates choose between. It reads "auto"
// until somebody picks, and Enter on it creates with what was picked.
func TestTheCreateCardReachesTheHarnessByTab(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	card := openCreateCard(t, m)

	harness := m.opts.opts[optHarness]
	if got := card.display(harness); !strings.HasPrefix(got, "auto") {
		t.Fatalf("harness reads %q, want auto with nothing chosen", got)
	}
	for range card.sources() + 3 {
		send(t, m, keyPress("down"))
	}
	if card.row != -1 || card.cursor != card.sources() {
		t.Fatalf("row = %d cursor = %d, want Down to stop on the list's last row", card.row, card.cursor)
	}
	send(t, m, keyPress("home"), keyPress("tab"))
	if card.current() != harness {
		t.Fatal("Tab should land on the harness")
	}
	send(t, m, keyPress("right"))
	chosen := harness.selected()
	if !harness.changed() {
		t.Fatal("→ on the harness should choose one")
	}
	send(t, m, keyPress("enter"))
	if len(ds.runs) != 1 || ds.runs[0].Harness != chosen {
		t.Fatalf("runs = %+v, want one on %s", ds.runs, chosen)
	}
	if got := ds.runs[0].Source; got != "" {
		t.Fatalf("source = %q, want the window's own, which the list was left on", got)
	}
}

// Tab goes round — the list, each setting, the list — and Shift-Tab the other
// way, so the way into the settings is also the way out of them.
func TestTheCreateCardTabGoesRound(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	card := openCreateCard(t, m)

	for range card.rows() {
		send(t, m, keyPress("tab"))
	}
	if card.row != len(card.rows())-1 {
		t.Fatalf("row = %d, want the last setting", card.row)
	}
	send(t, m, keyPress("tab"))
	if card.row != -1 {
		t.Fatalf("row = %d, want Tab off the last setting back in the list", card.row)
	}
	send(t, m, keyPress("shift+tab"))
	if card.row != len(card.rows())-1 {
		t.Fatalf("row = %d, want Shift-Tab from the list on the last setting", card.row)
	}
}

// The server is offered only where there is one to choose, and moving it moves
// the header, as moving the panel's row does: the list shows where the create
// is going.
func TestTheCreateCardOffersTheServerWhenThereIsMoreThanOne(t *testing.T) {
	t.Parallel()
	one := newTestModel(t, newFakeSource(testSandboxes()...))
	if slices.Contains(openCreateCard(t, one).rows(), optServer) {
		t.Fatal("one server is nothing to choose between")
	}

	ds := newFakeSource(testSandboxes()...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)
	card := openCreateCard(t, m)
	for card.current() != m.opts.server() {
		send(t, m, keyPress("tab"))
	}
	send(t, m, keyPress("right"))
	if m.list.server != "beta" {
		t.Fatalf("the list is showing %q, want it to follow the row to beta", m.list.server)
	}
	send(t, m, keyPress("enter"))
	if len(ds.runs) != 1 || ds.runs[0].Server != "beta" {
		t.Fatalf("runs = %+v, want one on beta", ds.runs)
	}
}

// Skills and additional sources are the two repeated rows, where Enter asks
// for another value rather than creating, and the field goes back to the card.
// A source is checked before the row takes it, the way a typed primary is.
func TestTheCreateCardTakesSkillsAndAdditionalSources(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	card := openCreateCard(t, m)
	onto := func(idx int) {
		t.Helper()
		for card.current() != m.opts.opts[idx] {
			send(t, m, keyPress("tab"))
		}
	}

	onto(optUserSkills)
	send(t, m, keyPress(" "))

	onto(optSkills)
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("Enter on Skills should ask for a directory")
	}
	send(t, m, typeString("./team-skills")...)
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgCreate || m.dialog.create != card {
		t.Fatal("the field should go back to the card it was opened from")
	}

	onto(optInclude)
	ds.setSourceErr(errors.New("/src/typo does not exist"))
	send(t, m, keyPress("enter"))
	send(t, m, typeString("/src/typo")...)
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgInput || !m.dialog.err {
		t.Fatal("a source that is not there should come back to the field")
	}
	ds.setSourceErr(nil)
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgCreate {
		t.Fatal("a source that checks out should go back to the card")
	}
	if len(ds.runs) != 0 {
		t.Fatalf("runs = %+v, want nothing created by filling the rows in", ds.runs)
	}

	// Esc in a field is the card again, not the window.
	send(t, m, keyPress("enter"), keyPress("esc"))
	if m.dialog == nil || m.dialog.kind != dlgCreate {
		t.Fatal("Esc in the field should go back to the card")
	}

	send(t, m, keyPress("shift+tab"), keyPress("shift+tab"), keyPress("enter"))
	if len(ds.runs) != 1 {
		t.Fatalf("runs = %+v, want one", ds.runs)
	}
	run := ds.runs[0]
	if run.UserSkills == nil || !*run.UserSkills || !slices.Equal(run.SkillDirs, []string{"./team-skills"}) || !slices.Equal(run.Include, []string{"/src/typo"}) {
		t.Fatalf("run = %+v, want your skills, ./team-skills and /src/typo", run)
	}
	cmd := m.opts.command("fix it")
	for _, want := range []string{"--user-skills", "--skills ./team-skills", "-i /src/typo"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command = %q, want %q", cmd, want)
		}
	}
}

// Esc on the card creates nothing and leaves the prompt as it was typed.
func TestEscOnTheCreateCardCreatesNothing(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	openCreateCard(t, m)
	send(t, m, keyPress("esc"))
	if m.dialog != nil || len(ds.runs) != 0 {
		t.Fatalf("dialog = %v runs = %+v, want the card gone and nothing created", m.dialog, ds.runs)
	}
	if got := m.prompt.Value(); got != "fix the reaper" {
		t.Fatalf("prompt = %q, want it kept", got)
	}
}

// The list is pointed at with one press and answered with two; the settings
// take a press on their arrows.
func TestTheCreateCardAnswersThePointer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	card := openCreateCard(t, m)
	frame(m)

	at := func(kind hitKind, idx, delta int) zone {
		t.Helper()
		for _, mark := range m.zones.marks {
			if mark.what.kind == kind && mark.what.idx == idx && mark.what.delta == delta {
				return mark
			}
		}
		t.Fatalf("nothing marked for %v %d", kind, idx)
		return zone{}
	}
	second := at(hitCreateSource, 1, 0)
	send(t, m, tea.MouseClickMsg{X: second.x + 4, Y: second.y, Button: tea.MouseLeft})
	if card.cursor != 1 || len(ds.runs) != 0 {
		t.Fatalf("cursor = %d runs = %+v, want one press to point and not create", card.cursor, ds.runs)
	}
	frame(m)
	folded := at(hitCreateExpand, 0, 0)
	send(t, m, tea.MouseClickMsg{X: folded.x + 4, Y: folded.y, Button: tea.MouseLeft})
	if !card.expanded {
		t.Fatal("a press on the folded options should unfold them")
	}
	frame(m)
	forward := at(hitCreateCycle, optHarness, 1)
	send(t, m, tea.MouseClickMsg{X: forward.x, Y: forward.y, Button: tea.MouseLeft})
	if !m.opts.opts[optHarness].changed() {
		t.Fatal("a press on the harness's › should choose one")
	}
}

// The settings open folded, so what the card mostly shows is the sources; the
// first Tab unfolds them onto the harness, and they stay unfolded for the card.
func TestTheCreateCardFoldsItsOptionsUntilTab(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	card := openCreateCard(t, m)

	text := plainFrame(m)
	if card.expanded || strings.Contains(text, "Additional sources") || strings.Contains(text, "would run") {
		t.Fatalf("want the options folded away until Tab:\n%s", text)
	}
	if !strings.Contains(text, "Tab to show") {
		t.Fatalf("the folded line should say how to show them:\n%s", text)
	}
	send(t, m, keyPress("tab"))
	if !card.expanded || card.current() != m.opts.opts[optHarness] {
		t.Fatal("Tab should unfold the options onto the harness")
	}
	if text := plainFrame(m); !strings.Contains(text, "Additional sources") {
		t.Fatalf("want the options shown after Tab:\n%s", text)
	}
	for range card.rows() {
		send(t, m, keyPress("tab"))
	}
	if card.row != -1 || !card.expanded {
		t.Fatal("back in the list, the options should stay shown")
	}
}

// The User skills row left alone is client.yaml's run.userSkills, so the
// request names neither way; a command that said either way keeps saying it.
func TestUserSkillsLeftAloneAreTheConfigurations(t *testing.T) {
	t.Parallel()
	opts := newOptions(Session{Directory: "/src/disco2"})
	if got := opts.request("fix", "").UserSkills; got != nil {
		t.Fatalf("UserSkills = %v, want nil for the configuration to decide", *got)
	}
	opts.prefill(RunRequest{UserSkills: new(false)})
	got := opts.request("fix", "").UserSkills
	if got == nil || *got {
		t.Fatalf("UserSkills = %v, want false as the command gave it", got)
	}
	if cmd := opts.command("fix"); !strings.Contains(cmd, "--user-skills=false") {
		t.Fatalf("command = %q, want --user-skills=false", cmd)
	}
}
