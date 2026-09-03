package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The mouse is answered against the hit map the last frame left behind, so
// every one of these tests renders a frame, finds the words it means to press
// on that frame, and presses there. Reading the coordinates off the rendered
// window rather than computing them is the point: a test that recomputed the
// layout would agree with a hit map that had drifted from it.

// at is where text sits on the rendered frame: the first row it appears on and
// the column it starts at. It fails the test rather than returning nothing,
// because a press aimed at text that is not on screen is a broken test rather
// than a finding about the window.
func at(t *testing.T, m *Model, text string) (int, int) {
	t.Helper()
	for y, line := range strings.Split(plainFrame(m), "\n") {
		if x := strings.Index(line, text); x >= 0 {
			// Byte offset to cells: the frame is drawn with box-drawing runes
			// and the mark, so the two are not the same number.
			return ansi.StringWidth(line[:x]), y
		}
	}
	t.Fatalf("%q is not on the frame:\n%s", text, plainFrame(m))
	return 0, 0
}

// rowAt is where a discobox's row was drawn. A name too long for its column is
// ellipsized, so the row is found by as much of the name as fits rather than by
// the whole of it.
func rowAt(t *testing.T, m *Model, box Sandbox) (int, int) {
	t.Helper()
	name := box.Name
	if len(name) > 12 {
		name = name[:12]
	}
	return at(t, m, name)
}

// tap is one click of the left button, complete: the terminal reports a
// press and then a release, and a window that only handled one of them would
// leave a gesture open.
func tap(t *testing.T, m *Model, x, y int) *Model {
	t.Helper()
	return send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft},
	)
}

// doublePress is two clicks inside the double-click window, which is what the
// clock the model counts against is for.
func doublePress(t *testing.T, m *Model, x, y int) *Model {
	t.Helper()
	tap(t, m, x, y)
	return tap(t, m, x, y)
}

// slowClock makes every press its own gesture, for the tests that click twice
// in different places and mean two single clicks.
func slowClock(m *Model) {
	now := time.Now()
	m.now = func() time.Time {
		now = now.Add(time.Hour)
		return now
	}
}

// manySandboxes is a project with more discoboxes than the list can draw, for
// the tests about scrolling.
func manySandboxes(n int) []Sandbox {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	out := make([]Sandbox, 0, n)
	for i := range n {
		out = append(out, Sandbox{
			ID: "sbx_" + itoa(i), Name: "discobox number " + itoa(i),
			State: StateRunning, HasRuntime: true, Harness: "claude",
			Folder: "/src/disco2", Source: "/src/disco2",
			Branch: "main", Commit: "a3f9c21", Created: now,
		})
	}
	return out
}

func TestAPressOnARowPutsTheCursorOnIt(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	slowClock(m)
	showAllFolders(t, m)

	// The third row, named on the frame the press is aimed at.
	x, y := rowAt(t, m, boxes[2])
	tap(t, m, x, y)

	if m.focus != focusList {
		t.Fatalf("a press on a row should give the list the keyboard, focus = %v", m.focus)
	}
	if got := m.list.current(); got == nil || got.ID != boxes[2].ID {
		t.Fatalf("the cursor is on %v, want the row that was pressed (%s)", got, boxes[2].ID)
	}
}

// A press on a row is only a press on a row: it says which one you mean, and
// nothing about it runs.
func TestAPressOnARowStartsNothing(t *testing.T) {
	boxes := testSandboxes()
	ds := newFakeSource(boxes...)
	m := newTestModel(t, ds)
	slowClock(m)
	showAllFolders(t, m)

	x, y := rowAt(t, m, boxes[0])
	tap(t, m, x, y)

	if len(ds.opens) > 0 {
		t.Fatalf("one press opened %v; only the second should", ds.opens)
	}
}

// The second press is the one that opens it, the way Enter does.
func TestADoublePressOnARowAttaches(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	showAllFolders(t, m)

	x, y := rowAt(t, m, boxes[0])
	doublePress(t, m, x, y)

	if m.paneBox.ID != boxes[0].ID {
		t.Fatalf("the workspace opened on %q, want an attach to %s", m.paneBox.ID, boxes[0].ID)
	}
}

// The pointer names the row, whatever else is marked: a double click on one
// row of a marked set is about that row.
func TestADoublePressActsOnTheRowUnderThePointer(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	showAllFolders(t, m)
	// Mark the first row from the keyboard, then double-click the second.
	send(t, m, keyPress(" "))

	x, y := rowAt(t, m, boxes[1])
	doublePress(t, m, x, y)

	if m.paneBox.ID != boxes[1].ID {
		t.Fatalf("the workspace opened on %q, want the row pressed (%s)", m.paneBox.ID, boxes[1].ID)
	}
}

// The right button over a row is that row's menu — the one `.` opens.
func TestTheRightButtonOpensTheRowsMenu(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	slowClock(m)
	showAllFolders(t, m)

	x, y := rowAt(t, m, boxes[1])
	send(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})

	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %v, want the actions menu", m.dialog)
	}
	if !strings.Contains(dialogText(m), "attach") {
		t.Fatalf("the menu should offer the row's actions:\n%s", dialogText(m))
	}
}

// A hint that names a key is a button for that key, handled by the same
// handler the keyboard reaches.
func TestPressingAKeyHintPressesTheKey(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)

	x, y := at(t, m, "F3 harnesses")
	tap(t, m, x, y)

	if !m.harnessesOpen {
		t.Fatalf("pressing the header's F3 hint should open the harnesses screen")
	}
}

// The status line's offers are the screen's own keys, and they are buttons for
// them too.
func TestPressingAStatusHintActsOnTheList(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	slowClock(m)
	// Wide enough that the status line keeps every offer: it drops them from
	// the tail to fit, and Space is near the end of a long one.
	send(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	showAllFolders(t, m)

	x, y := at(t, m, "Space select")
	tap(t, m, x, y)

	if got := m.list.selectionCount(); got != 1 {
		t.Fatalf("selected %d discoboxes, want the one under the cursor", got)
	}
}

// The folder filter is a dropdown, and a dropdown opens when it is clicked.
func TestPressingTheFolderFilterOpensIt(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)

	x, y := at(t, m, m.session.Directory)
	tap(t, m, x, y)

	if m.dialog == nil {
		t.Fatalf("pressing the folder filter should open the dropdown")
	}
	if m.focus != focusFolder {
		t.Fatalf("focus = %v, want the filter", m.focus)
	}
}

// The strip under the composer names the run options, so it is the way in.
func TestPressingTheChipsOpensTheOptions(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)

	chips := ansi.Strip(m.opts.chips(m.st))
	x, y := at(t, m, strings.TrimSpace(chips))
	tap(t, m, x, y)

	if !m.optionsOpen {
		t.Fatalf("pressing the run summary should open the options panel")
	}
}

// The wheel scrolls what is under the pointer and leaves the keyboard alone: a
// prompt half typed is still where the keys go.
func TestTheWheelScrollsTheListWithoutTakingFocus(t *testing.T) {
	boxes := manySandboxes(40)
	m := newTestModel(t, newFakeSource(boxes...))

	_, y := rowAt(t, m, boxes[0])
	send(t, m, tea.MouseWheelMsg{X: 10, Y: y, Button: tea.MouseWheelDown})

	if m.focus != focusPrompt {
		t.Fatalf("the wheel took the keyboard: focus = %v", m.focus)
	}
	if m.list.cursor == 0 {
		t.Fatalf("the wheel should have moved the list on")
	}
}

// A drag across the frame selects the cells it crossed and copies them, which
// is what the terminal's own selection did before the window took the mouse.
func TestADragOverTheChromeSelectsAndCopies(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	slowClock(m)
	showAllFolders(t, m)

	var copied []string
	m.copyOS = func(text string) error {
		copied = append(copied, text)
		return nil
	}

	x, y := rowAt(t, m, boxes[0])
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft},
		tea.MouseMotionMsg{X: x + 2, Y: y, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: x + 2, Y: y, Button: tea.MouseLeft},
	)

	if len(copied) != 1 || copied[0] != "fix" {
		t.Fatalf("copied %q, want the cells that were dragged over (%q)", copied, "fix")
	}
}

// The middle button pastes the last selection, which is what it pastes
// everywhere else.
func TestTheMiddleButtonPastesTheLastSelection(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	slowClock(m)
	showAllFolders(t, m)
	m.copyOS = func(string) error { return nil }

	x, y := rowAt(t, m, boxes[0])
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft},
		tea.MouseMotionMsg{X: x + 2, Y: y, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: x + 2, Y: y, Button: tea.MouseLeft},
	)
	send(t, m, tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseMiddle})

	if got := m.prompt.Value(); got != "fix" {
		t.Fatalf("the composer holds %q, want the selection pasted into it (%q)", got, "fix")
	}
}

// A press in the composer is the caret moving, and a drag in it selects the
// text rather than the frame under it.
func TestADragInTheComposerSelectsItsText(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	m.copyOS = func(string) error { return nil }
	send(t, m, typeString("hello world")...)

	x, y := at(t, m, "hello world")
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft},
		tea.MouseMotionMsg{X: x + 5, Y: y, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: x + 5, Y: y, Button: tea.MouseLeft},
	)

	if got := m.prompt.SelectedText(); got != "hello" {
		t.Fatalf("the composer's selection is %q, want %q", got, "hello")
	}
}

// A double click in the composer takes the word under it.
func TestADoubleClickInTheComposerTakesAWord(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	m.copyOS = func(string) error { return nil }
	send(t, m, typeString("hello world")...)

	x, y := at(t, m, "world")
	doublePress(t, m, x+1, y)

	if got := m.prompt.SelectedText(); got != "world" {
		t.Fatalf("the composer's selection is %q, want the word under the pointer", got)
	}
}

// The mouse is reported once the window has the screen, and not before: the
// opening prompt is inline in the shell's own scrollback, where the terminal's
// selection is still the one that belongs.
func TestTheOpeningPromptLeavesTheMouseToTheTerminal(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	m.expanded = false

	if got := m.mouseMode(); got != tea.MouseModeNone {
		t.Fatalf("the inline prompt asks for %v, want the mouse left to the terminal", got)
	}
	m.expand()
	if got := m.mouseMode(); got == tea.MouseModeNone {
		t.Fatalf("the full window should report the mouse")
	}
}

// A menu row is a press: a menu whose rows only answer their letters is a menu
// the pointer cannot work.
func TestPressingAMenuRowRunsIt(t *testing.T) {
	boxes := testSandboxes()
	ds := newFakeSource(boxes...)
	m := newTestModel(t, ds)
	slowClock(m)
	showAllFolders(t, m)
	// The list's own menu, on the row the cursor is on.
	send(t, m, keyPress("."))

	// The detail, not the label: "archived" is in the reason on a row above.
	x, y := at(t, m, "put it away")
	tap(t, m, x, y)

	if m.dialog != nil {
		t.Fatalf("the menu should have run and closed, dialog kind = %v", m.dialog.kind)
	}
	if len(ds.did) != 1 || !strings.HasPrefix(ds.did[0], "archive ") {
		t.Fatalf("did = %v, want the row's archive", ds.did)
	}
}

// And a confirmation's two answers are buttons for their own letters.
func TestPressingAConfirmationAnswersIt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	slowClock(m)
	// Disabling a harness is the confirmation nearest to hand.
	send(t, m, keyPress("f3"), keyPress("d"))
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatalf("dialog = %v, want a confirmation to answer", m.dialog)
	}

	x, y := at(t, m, "y yes")
	tap(t, m, x, y)

	if m.dialog != nil {
		t.Fatalf("answering should take the confirmation down, dialog kind = %v", m.dialog.kind)
	}
	if len(ds.didHarness) == 0 {
		t.Fatalf("nothing was disabled")
	}
}

// The run options panel: a row is a press, and the arrows on it change the
// value the way the arrow keys do.
func TestPressingTheOptionArrowsChangesTheValue(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	send(t, m, keyPress("ctrl+o"))

	before := m.opts.opts[optHarness].display()
	// The arrow on the cursor row, which the panel opens on.
	x, y := at(t, m, "‹")
	tap(t, m, x, y)

	if got := m.opts.opts[optHarness].display(); got == before {
		t.Fatalf("the harness is still %q; the arrow should have changed it", got)
	}
}

// The harnesses screen is a list of things you act on, so its rows answer the
// pointer the way the discoboxes do.
func TestPressingAHarnessRowMovesItsCursor(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	send(t, m, keyPress("f3"))

	want := m.harnesses.all[2]
	x, y := at(t, m, want.displayName())
	tap(t, m, x, y)

	if got := m.harnesses.current(); got == nil || got.ID != want.ID {
		t.Fatalf("the cursor is on %v, want the harness that was pressed (%s)", got, want.ID)
	}
}

// The right button there opens what that harness can take, which is the menu
// `.` opens.
func TestTheRightButtonOpensTheHarnessMenu(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	send(t, m, keyPress("f3"))

	want := m.harnesses.all[1]
	x, y := at(t, m, want.displayName())
	send(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})

	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %v, want the harness menu", m.dialog)
	}
}

// The introduction takes one key, and the line that says so is a button for it.
func TestPressingTheWelcomeFooterDismissesIt(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	m.welcoming = true

	x, y := at(t, m, welcomeFooter)
	tap(t, m, x, y)

	if m.welcoming {
		t.Fatalf("pressing the footer should take the introduction down")
	}
}

// The secrets screen is two tables read as one, so a press has to say which of
// them it landed in as well as which row.
func TestPressingASecretRowMovesItsCursor(t *testing.T) {
	m, _ := secretsFixture(t)
	slowClock(m)

	x, y := at(t, m, "OpenAI key")
	tap(t, m, x, y)

	if m.onRequests {
		t.Fatalf("a press in the secrets table should leave the keys in it")
	}
	if got := m.secrets.current(); got == nil || got.Name != "OpenAI key" {
		t.Fatalf("the cursor is on %v, want the secret that was pressed", got)
	}
}

// And the second press opens what the row holds, the way Enter does.
func TestADoublePressOnASecretOpensIt(t *testing.T) {
	m, _ := secretsFixture(t)

	x, y := at(t, m, "OpenAI key")
	doublePress(t, m, x, y)

	if m.dialog == nil {
		t.Fatalf("a double press should open the secret")
	}
}

// A card being filled in answers the pointer too: a press moves the cursor to
// the row and focuses its field, which is what ↑ and ↓ do.
func TestPressingAFormRowMovesTheCursorToIt(t *testing.T) {
	m, _ := secretsFixture(t)
	slowClock(m)
	send(t, m, keyPress("n"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %v, want the new-secret card", m.dialog)
	}
	before := m.dialog.form.cursor

	// The row under the one the card opens on.
	label := m.dialog.form.rows[before+1].label
	x, y := at(t, m, label)
	tap(t, m, x, y)

	if got := m.dialog.form.cursor; got == before {
		t.Fatalf("the form cursor is still on %d; the press should have moved it", got)
	}
}
