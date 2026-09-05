package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// Every mark a frame makes has to be on that frame, over something drawn. An
// origin pushed wrong lands a control off the window or on blank cells, where
// nothing looks broken and the press simply misses — which is the failure this
// whole mechanism has to be watched for.
func TestEveryMarkLandsOnSomethingDrawn(t *testing.T) {
	screens := map[string]func(t *testing.T) *Model{
		"the discobox list": func(t *testing.T) *Model {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			showAllFolders(t, m)
			return m
		},
		"the harnesses": func(t *testing.T) *Model {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			return send(t, m, keyPress("f3"))
		},
		"the secrets": func(t *testing.T) *Model {
			m, _ := secretsFixture(t)
			return m
		},
		"the run options": func(t *testing.T) *Model {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			return send(t, m, keyPress("ctrl+o"))
		},
		"an actions menu": func(t *testing.T) *Model {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			showAllFolders(t, m)
			return send(t, m, keyPress("."))
		},
		"a card": func(t *testing.T) *Model {
			m, _ := secretsFixture(t)
			return send(t, m, keyPress("n"))
		},
		"the introduction": func(t *testing.T) *Model {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			m.welcoming = true
			return m
		},
	}

	// At two widths, because the narrow one is where a column drops off the
	// end and a hit map computed a second time would come apart from the frame.
	for name, open := range screens {
		for _, width := range []int{120, 72} {
			t.Run(fmt.Sprintf("%s at %d", name, width), func(t *testing.T) {
				assertMarksLand(t, open(t), width)
			})
		}
	}
}

func assertMarksLand(t *testing.T, m *Model, width int) {
	t.Helper()
	send(t, m, tea.WindowSizeMsg{Width: width, Height: 40})
	lines := strings.Split(plainFrame(m), "\n")
	if len(m.zones.marks) == 0 {
		t.Fatalf("the frame marked nothing at all")
	}
	for _, z := range m.zones.marks {
		if z.y < 0 || z.y+z.height > len(lines) {
			t.Errorf("a %v mark spans rows %d..%d of a %d-row frame", z.what.kind, z.y, z.y+z.height-1, len(lines))
			continue
		}
		if z.x < 0 || z.x+z.width > m.width {
			t.Errorf("a %v mark spans columns %d..%d of a %d-column frame", z.what.kind, z.x, z.x+z.width-1, m.width)
			continue
		}
		// A hint is a word made pressable, so its cells hold that word. The
		// wider marks are rows and fields, which are blank often enough — an
		// empty list, a prompt nobody has typed in.
		if z.what.kind != hitKey && z.what.kind != hitListKey {
			continue
		}
		row := []rune(lines[z.y])
		if z.x+z.width > len(row) || strings.TrimSpace(string(row[z.x:z.x+z.width])) == "" {
			t.Errorf("a %v mark for %v covers no text at %d,%d in:\n%s", z.what.kind, z.what.keys, z.x, z.y, lines[z.y])
		}
	}
}

// A control the pointer is resting on says so before it is pressed, so what
// can be clicked is answerable by moving the mouse rather than by clicking to
// find out.
func TestAHintShadesUnderThePointer(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	plain := plainFrame(m)
	x, y := at(t, m, "F3 harnesses")
	send(t, m, tea.MouseMotionMsg{X: x, Y: y})

	if got := plainFrame(m); got != plain {
		t.Fatalf("hovering changed the text on the frame, not just its color:\n%s", got)
	}
	row := strings.Split(rawFrame(m), "\n")[y]
	if !strings.Contains(row, m.st.hover.Render("F3 harnesses")) {
		t.Fatalf("the hint under the pointer is not drawn as live:\n%q", row)
	}

	// And it gives the shade back when the pointer moves off it.
	send(t, m, tea.MouseMotionMsg{X: 0, Y: y})
	if row := strings.Split(rawFrame(m), "\n")[y]; strings.Contains(row, m.st.hover.Render("F3 harnesses")) {
		t.Fatalf("the hint is still live with the pointer elsewhere:\n%q", row)
	}
}

// The window asks for every move so it can do that, and answers them itself: a
// sandbox that subscribed to buttons alone is sent no more than it was.
func TestABareMoveIsTheWindowsOwn(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if got := m.mouseMode(); got != tea.MouseModeAllMotion {
		t.Fatalf("the window asks for %v, want every move so a control can shade under the pointer", got)
	}
}

// A card's key line is a key line like any other: the offers on it are buttons
// for their keys, because a card is the one surface where they would otherwise
// only be readable.
func TestPressingADialogsKeyLineAnswersIt(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	showAllFolders(t, m)
	send(t, m, keyPress("."))

	x, y := at(t, m, "Esc cancels")
	tap(t, m, x, y)

	if m.dialog != nil {
		t.Fatalf("pressing the menu's own Esc offer should close it, kind = %v", m.dialog.kind)
	}
}

// And a menu row says so under the pointer, without taking the chevron from
// the row Enter would run.
func TestAMenuRowShadesUnderThePointer(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	showAllFolders(t, m)
	send(t, m, keyPress("."))

	before := m.dialog.cursor
	x, y := at(t, m, "put it away")
	send(t, m, tea.MouseMotionMsg{X: x, Y: y})

	row := strings.Split(rawFrame(m), "\n")[y]
	if !strings.Contains(row, m.st.hover.Render(pad("archive", 14))) {
		t.Fatalf("the row under the pointer is not drawn as live:\n%q", row)
	}
	if m.dialog.cursor != before {
		t.Fatalf("hovering moved the cursor to %d; the chevron says what Enter runs", m.dialog.cursor)
	}
}

// A card's text field takes the caret from a press, the way the composer does.
func TestPressingACardsFieldPlacesTheCaret(t *testing.T) {
	boxes := testSandboxes()
	m := newTestModel(t, newFakeSource(boxes...))
	slowClock(m)
	showAllFolders(t, m)
	send(t, m, keyPress("e")) // rename opens the name it already has
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatalf("dialog = %v, want the rename field", m.dialog)
	}
	if got := m.dialog.input.Position(); got != len(boxes[0].Name) {
		t.Fatalf("the caret opens at %d, want the end of the name", got)
	}

	x, y := at(t, m, boxes[0].Name)
	tap(t, m, x+4, y)

	if got := m.dialog.input.Position(); got != 4 {
		t.Fatalf("the caret is at %d, want where the pointer was (4)", got)
	}
}

// And a card's key line closes it, on the cards that are only read.
func TestPressingACardsCloseOffer(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	send(t, m, keyPress("f1"))
	if m.dialog == nil {
		t.Fatalf("F1 did not open the help")
	}

	// The key line, not the same words in the prose above it: the help
	// explains its own search, so "Esc closes" appears in the body too.
	line := "/ search" + hintSep + "c copies" + hintSep + "Esc closes"
	x, y := at(t, m, line)
	tap(t, m, x+lipgloss.Width("/ search"+hintSep+"c copies"+hintSep), y)

	if m.dialog != nil {
		t.Fatalf("pressing the card's own Esc offer should close it")
	}
}

// A form row that is typed into takes the caret too, and the cursor with it.
func TestPressingAFormFieldPlacesTheCaret(t *testing.T) {
	m, _ := secretsFixture(t)
	slowClock(m)
	send(t, m, keyPress("n"))

	row := -1
	for i, r := range m.dialog.form.rows {
		if len(r.choices) == 0 && m.dialog.form.answerable(i) {
			m.dialog.form.moveTo(i)
			m.dialog.form.rows[i].input.SetValue("github")
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatal("the card has no field to type into")
	}

	x, y := at(t, m, "github")
	tap(t, m, x+3, y)

	if got := m.dialog.form.cursor; got != row {
		t.Fatalf("the form cursor is on %d, want the row that was pressed (%d)", got, row)
	}
	if got := m.dialog.form.rows[row].input.Position(); got != 3 {
		t.Fatalf("the caret is at %d, want where the pointer was (3)", got)
	}
}

// One press changes a value, on any row rather than only the one the keyboard
// cursor is on: the arrow columns are kept on every row that can be stepped,
// so the press that lights them is the press that uses them.
func TestOneArrowPressChangesAnyOptionRow(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	send(t, m, keyPress("ctrl+o"))
	if m.opts.cursor == optDetach {
		t.Fatalf("the panel opens on the row this test means to reach past")
	}

	before := m.opts.opts[optDetach].display()
	_, y := at(t, m, m.opts.opts[optDetach].label)
	x := 0
	for _, z := range m.zones.marks {
		if z.what.kind == hitOptionCycle && z.what.idx == optDetach && z.what.delta > 0 {
			x = z.x
		}
	}
	if x == 0 {
		t.Fatalf("the row has no arrow to press")
	}
	tap(t, m, x, y)

	if got := m.opts.opts[optDetach].display(); got == before {
		t.Fatalf("the value is still %q; one press on its arrow should have changed it", got)
	}
	if m.opts.cursor != optDetach {
		t.Fatalf("the cursor is on %d, want the row that was pressed", m.opts.cursor)
	}
}

// And the value does not move as the pointer crosses it: the arrow columns are
// there whether or not they are lit.
func TestTheOptionValuesDoNotShiftUnderThePointer(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	send(t, m, keyPress("ctrl+o"))

	x, y := at(t, m, m.opts.opts[optDetach].display())
	send(t, m, tea.MouseMotionMsg{X: x, Y: y})

	if got, _ := at(t, m, m.opts.opts[optDetach].display()); got != x {
		t.Fatalf("the value moved from column %d to %d when the pointer arrived", x, got)
	}
}

// The run options are one of the modal surfaces, so its key line is a key line
// like every other: the offers on it are buttons for their keys.
func TestPressingTheOptionsKeyLineLeaves(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	slowClock(m)
	send(t, m, keyPress("ctrl+o"))

	x, y := at(t, m, "Esc back to the prompt")
	tap(t, m, x, y)

	if m.optionsOpen {
		t.Fatalf("pressing the panel's own Esc offer should close it")
	}
}
