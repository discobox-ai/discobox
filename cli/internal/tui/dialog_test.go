package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A dialog takes most of a window with room to spare, and all of one without.
func TestADialogTakesMostOfTheWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		window int
		want   int
	}{
		{"wide", 120, 108},
		{"at the threshold", 80, 72},
		{"below the threshold, so all of it", 60, 60},
		{"narrower than the minimum, so still all of it", 40, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dialogWidth(tc.window); got != tc.want {
				t.Fatalf("dialogWidth(%d) = %d, want %d", tc.window, got, tc.want)
			}
		})
	}
}

// Whatever the policy works out to, it can never be wider than the terminal:
// a dialog that overflows is one with its right edge off screen.
func TestADialogNeverOutgrowsTheWindow(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	dialog := textDialog("Title", strings.Repeat("a word ", 200))
	// From the narrowest box there is: the border and padding alone are six
	// columns, so below seven there is no box to draw, only its chrome.
	for window := dialogChromeWidth + 1; window <= 200; window++ {
		if got := dialogWidth(window); got > window {
			t.Fatalf("dialogWidth(%d) = %d, wider than the window", window, got)
		}
		if got := lipgloss.Width(dialog.view(st, &zones{}, window, 40)); got > window {
			t.Fatalf("a dialog rendered at %d columns came out %d wide", window, got)
		}
	}
}

// The box's outside extent is what the policy asked for, so the sizing means
// what it says rather than what the border leaves of it.
func TestADialogIsDrawnAtTheWidthItWasGiven(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	dialog := textDialog("Title", "a short body")
	for _, window := range []int{120, 100, 80, 60} {
		if got := lipgloss.Width(dialog.view(st, &zones{}, window, 40)); got != dialogWidth(window) {
			t.Fatalf("at %d columns the box is %d wide, want %d", window, got, dialogWidth(window))
		}
	}
}

// A dialog is the only thing on screen, so it sits in the middle of it.
func TestADialogIsCentered(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	send(t, m, sizeMsg(120, 40), keyPress("f1"))

	lines := strings.Split(rawFrame(m), "\n")
	first, last := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		t.Fatal("the help dialog drew nothing")
	}
	// Centered vertically: the blank rows above and below differ by at most one,
	// which is what an odd number of spare rows leaves.
	above, below := first, len(lines)-1-last
	if diff := above - below; diff > 1 || diff < -1 {
		t.Fatalf("dialog has %d rows above and %d below, want it centered:\n%s", above, below, rawFrame(m))
	}
	// And horizontally, by the same measure on the widest row.
	widest := ""
	for _, line := range lines {
		if lipgloss.Width(line) > lipgloss.Width(widest) {
			widest = line
		}
	}
	left := lipgloss.Width(widest) - lipgloss.Width(strings.TrimLeft(widest, " "))
	right := 120 - lipgloss.Width(strings.TrimRight(widest, " "))
	if diff := left - right; diff > 1 || diff < -1 {
		t.Fatalf("dialog has %d columns left and %d right, want it centered", left, right)
	}
}

// Centering must not make the frame bigger than the terminal.
func TestACenteredDialogStillFitsTheTerminal(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	for _, size := range [][2]int{{120, 40}, {100, 24}, {80, 20}, {60, 16}} {
		send(t, m, sizeMsg(size[0], size[1]))
		if m.dialog == nil {
			send(t, m, keyPress("f1"))
		}
		frame := rawFrame(m)
		lines := strings.Split(frame, "\n")
		if len(lines) != size[1] {
			t.Fatalf("frame at %dx%d is %d rows, want %d", size[0], size[1], len(lines), size[1])
		}
		for i, line := range lines {
			if got := lipgloss.Width(line); got > size[0] {
				t.Fatalf("row %d at %dx%d is %d columns, want at most %d", i, size[0], size[1], got, size[0])
			}
		}
	}
}

// The body grows with the window: a card that scrolled in a short terminal
// should not still be scrolling in a tall one.
func TestALongBodyUsesTheHeightItIsGiven(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	body := strings.TrimRight(strings.Repeat("a line\n", 60), "\n")
	dialog := textDialog("Title", body)

	short := strings.Count(dialog.view(st, &zones{}, 100, 20), "\n")
	dialog.offset = 0
	tall := strings.Count(dialog.view(st, &zones{}, 100, 60), "\n")
	if tall <= short {
		t.Fatalf("body rows: %d in a 20-row window, %d in a 60-row one — want the taller window to show more", short, tall)
	}
}

// searchable is a scrolling text dialog with a body long enough to have
// something in it that is not already on screen.
func searchable() *dialog {
	var lines []string
	for i := range 60 {
		lines = append(lines, "line "+itoa(i))
	}
	lines[17] = "the leader detaches"
	lines[42] = "the leader quits"
	return textDialog("Title", strings.Join(lines, "\n"))
}

// draw renders the dialog, which is what counts the matches: the body is
// wrapped to the window, so how many lines hold a word is only known here.
func draw(d *dialog) string { return d.view(newStyles(false), &zones{}, 100, 24) }

// A subject too long for the card is cut on its own row rather than wrapped,
// and the question under it wraps in full: a card that cut the question off
// mid-sentence asked something nobody could read to the end (issue #37).
func TestDialogSubjectEllipsizesAndItsBodyWraps(t *testing.T) {
	t.Parallel()
	dir := "/Users/somebody/projects/" + strings.Repeat("nested/", 20) + "leaf-directory"
	d := actionsDialog("Copy this directory?", copyDirectoryBody, []action{
		{key: "false", label: "Do not copy the directory", enabled: true},
		{key: "true", label: "Copy the directory in", enabled: true},
	}, nil)
	d.subject = dir

	got := draw(d)
	var rows []string
	for _, row := range strings.Split(got, "\n") {
		if strings.Contains(row, "/Users/somebody") || strings.Contains(row, "leaf-directory") {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 || !strings.Contains(rows[0], "/Users/somebody") || !strings.Contains(rows[0], "leaf-directory") {
		t.Fatalf("the subject is not on one row with both its ends:\n%s", got)
	}
	if !strings.Contains(rows[0], "…") || strings.Count(got, "nested/") >= 20 {
		t.Fatalf("the subject was not cut from the middle:\n%s", got)
	}
	if !strings.Contains(got, "nothing checked out in it.") {
		t.Fatalf("the body was cut off before its end:\n%s", got)
	}
}

// / searches the body, and the tally says what it found while it is still
// being typed.
func TestSlashSearchesAScrollingBody(t *testing.T) {
	t.Parallel()
	d := searchable()
	draw(d)
	send := func(spec string) { d.update(keyPress(spec)) }

	send("/")
	if !d.typing {
		t.Fatal("/ did not open the search line")
	}
	for _, r := range "leader" {
		send(string(r))
	}
	if got := draw(d); !strings.Contains(got, "/leader") {
		t.Fatalf("the search line is not on the frame:\n%s", got)
	}
	if d.query != "leader" || d.matches != 2 {
		t.Fatalf("query %q with %d matches, want \"leader\" with 2", d.query, d.matches)
	}
	if !strings.Contains(draw(d), "the leader detaches") {
		t.Fatal("the body did not scroll to the first match")
	}
}

// Enter puts the line away and keeps the search: n and N walk the matches, and
// the footer still says what was searched for.
func TestNAndNWalkTheMatches(t *testing.T) {
	t.Parallel()
	d := searchable()
	draw(d)
	for _, spec := range []string{"/", "l", "e", "a", "d", "e", "r", "enter"} {
		d.update(keyPress(spec))
	}
	if d.typing {
		t.Fatal("Enter left the search line open")
	}
	d.update(keyPress("n"))
	if got := draw(d); !strings.Contains(got, "the leader quits") || !strings.Contains(got, "/leader") {
		t.Fatalf("n did not go to the second match:\n%s", got)
	}
	if d.match != 1 {
		t.Fatalf("match = %d, want the second", d.match)
	}
	// Past the end is the first again: two matches are a ring, not a list with
	// a wall at each end.
	d.update(keyPress("n"))
	draw(d)
	if d.match != 0 {
		t.Fatalf("match = %d after walking past the end, want the first", d.match)
	}
	d.update(keyPress("N"))
	draw(d)
	if d.match != 1 {
		t.Fatalf("match = %d after N from the first, want the last", d.match)
	}
}

// Esc belongs to the search line while one is open: it abandons the search and
// puts the body back where it was, rather than closing the dialog.
func TestEscAbandonsTheSearchAndKeepsYourPlace(t *testing.T) {
	t.Parallel()
	d := searchable()
	draw(d)
	for range 10 {
		d.update(keyPress("down"))
	}
	draw(d)
	was := d.offset

	for _, spec := range []string{"/", "l", "e", "a", "d", "e", "r"} {
		d.update(keyPress(spec))
	}
	draw(d)
	if d.offset == was {
		t.Fatal("the search did not move the body")
	}
	if _, closed := d.update(keyPress("esc")); closed {
		t.Fatal("Esc closed the dialog instead of the search line")
	}
	draw(d)
	if d.query != "" || d.matches != 0 {
		t.Fatalf("the search survived Esc: %q with %d matches", d.query, d.matches)
	}
	if d.offset != was {
		t.Fatalf("offset = %d after abandoning the search, want %d", d.offset, was)
	}
	// With no search open, Esc is the way out again.
	if _, closed := d.update(keyPress("esc")); !closed {
		t.Fatal("Esc did not close the dialog")
	}
}

// Backspacing past the start of the query is the same as never having asked.
func TestBackspacingOutOfTheSearchEndsIt(t *testing.T) {
	t.Parallel()
	d := searchable()
	draw(d)
	for _, spec := range []string{"/", "l", "backspace", "backspace"} {
		d.update(keyPress(spec))
	}
	if d.typing || d.query != "" {
		t.Fatalf("typing = %v, query = %q, want the search gone", d.typing, d.query)
	}
}

// A body that fits its window does not offer a search: everything in it is
// already on screen.
func TestAShortBodyDoesNotOfferASearch(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	short := textDialog("Title", "one line").view(st, &zones{}, 100, 24)
	if strings.Contains(short, "/ search") {
		t.Fatalf("a body that fits offered a search:\n%s", short)
	}
	if !strings.Contains(draw(searchable()), "/ search") {
		t.Fatal("a body that scrolls did not offer a search")
	}
}

// The matches are painted, and a painted row is still a row of the box: the
// background is drawn across the width the frame budgeted for it.
func TestASearchedDialogStaysInsideItsBox(t *testing.T) {
	t.Parallel()
	st := newStyles(true)
	for _, window := range []int{120, 100, 80, 60, 40, 20} {
		d := searchable()
		d.view(st, &zones{}, window, 24)
		for _, spec := range []string{"/", "l", "e", "a", "d", "e", "r"} {
			d.update(keyPress(spec))
		}
		for _, line := range strings.Split(d.view(st, &zones{}, window, 24), "\n") {
			if got := lipgloss.Width(line); got > dialogWidth(window) {
				t.Fatalf("at %d columns a searched row is %d wide, want at most %d",
					window, got, dialogWidth(window))
			}
		}
	}
}

// The help is what the search is for: from the window, F1 and / find a key
// without reading down the whole of it.
func TestTheHelpIsSearchable(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, sizeMsg(100, 30), keyPress("f1"), keyPress("/"))
	send(t, m, typeString("vscode")...)
	got := plainFrame(m)
	if !strings.Contains(got, "/vscode") {
		t.Fatalf("the search line is not on the frame:\n%s", got)
	}
	if !strings.Contains(got, "vscode and zed are different") {
		t.Fatalf("the help did not scroll to the match:\n%s", got)
	}
}

// The help is longer than the terminal drawing it, so c takes the whole of it
// rather than the part that happens to be on screen — and it is a letter again
// the moment the search line is open.
func TestTheHelpIsCopiedWhole(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	copies := make(chan string, 2)
	m.copyOS = func(text string) error { copies <- text; return nil }

	send(t, m, sizeMsg(100, 30), keyPress("f1"))
	if !strings.Contains(plainFrame(m), "c copies") {
		t.Fatalf("the card does not offer the key:\n%s", plainFrame(m))
	}
	send(t, m, keyPress("c"))
	select {
	case got := <-copies:
		if got != m.helpText() {
			t.Fatalf("copied %d bytes, want the whole help of %d", len(got), len(m.helpText()))
		}
	default:
		t.Fatal("c did not copy the help")
	}
	if m.dialog == nil {
		t.Fatal("copying should leave the help up to go on reading")
	}
	// The receipt is on the card. There is no status line under a dialog, so a
	// copy that only reported through one would report to nobody.
	if got := plainFrame(m); !strings.Contains(got, "copied") {
		t.Fatalf("the card does not say the copy happened:\n%s", got)
	}

	send(t, m, keyPress("/"), keyPress("c"))
	if got := plainFrame(m); !strings.Contains(got, "/c") {
		t.Fatalf("c in the search line is a letter, not the copy:\n%s", got)
	}
	select {
	case <-copies:
		t.Fatal("c in the search line copied instead of searching")
	default:
	}

	// Enter puts the search line away and keeps the query. The key still works
	// there, so the card still has to offer it.
	send(t, m, keyPress("enter"))
	if got := plainFrame(m); !strings.Contains(got, "c copies") {
		t.Fatalf("the offer went away with the search line still queried:\n%s", got)
	}
	send(t, m, keyPress("c"))
	select {
	case <-copies:
	default:
		t.Fatal("c did not copy with a query set")
	}
}

// The card's key line is cut to its room rather than past the edge, because
// viewHints marks where it drew: an offer past the cut would be a button drawn
// nowhere and pressable anyway. See REVIEW.md.
func TestTheCardsKeyLineIsFittedToNarrowWindows(t *testing.T) {
	t.Parallel()
	st := newStyles(true)
	// A query is what squeezes the row hardest: it takes its own cells off the
	// left before a single offer is drawn, and "no matches" is the widest tally
	// there is. Esc has to survive all of it.
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"no search", ""},
		{"a match", "body"},
		{"no matches", "nothing here matches this"},
	} {
		for _, window := range []int{120, 100, 80, 60, 48, 40, 30, 20} {
			d := textDialog("Keys", strings.Repeat("a line of the body\n", 200))
			d.copy = func(string) tea.Cmd { return nil }
			d.copied = true
			d.view(st, &zones{}, window, 24)
			if tc.query != "" {
				d.update(keyPress("/"))
				for _, r := range tc.query {
					d.update(keyPress(string(r)))
				}
				d.update(keyPress("enter"))
			}
			got := d.view(st, &zones{}, window, 24)
			for _, line := range strings.Split(got, "\n") {
				if w := lipgloss.Width(line); w > dialogWidth(window) {
					t.Fatalf("%s at %d columns: a row is %d wide, want at most %d\n%s",
						tc.name, window, w, dialogWidth(window), got)
				}
			}
			// Below the floor on inner the box has no inside to speak of and
			// lipgloss re-wraps everything to the width it really has — the
			// case view already carves out for the chevron. The row is built
			// to fit inner, so down there it is built to a width the box does
			// not have and there is nothing to promise about it.
			if dialogWidth(window)-dialogChromeWidth < max(dialogWidth(window)-dialogChromeWidth, 16) {
				continue
			}
			// Esc is the one offer that may never be trimmed or cut in half:
			// it is how the card is left.
			if !strings.Contains(got, "Esc closes") {
				t.Fatalf("%s at %d columns: the way out is not on the row:\n%s", tc.name, window, got)
			}
		}
	}
}

// longBody is more question than any window here has rows for.
func longBody() string {
	var lines []string
	for i := range 80 {
		lines = append(lines, "fact "+itoa(i))
	}
	return strings.Join(lines, "\n")
}

// Every kind of card fits the window it is drawn in, with what answers it on
// screen: a body longer than the window scrolls above the answers rather than
// pushing them off the bottom, where they could only be answered blind
// (issue #161).
func TestEveryKindOfCardKeepsItsAnswersInTheWindow(t *testing.T) {
	t.Parallel()
	menu := actionsDialog("Title", longBody(), []action{
		{key: "yes", press: "a", label: "Approve", enabled: true},
		{key: "no", press: "d", label: "Deny", enabled: true},
	}, nil)
	status := statusDialog("Title", longBody())
	form := formDialog("Title", newForm(textRow("field", "the field", "", "")), nil)
	form.body = longBody()
	for _, tc := range []struct {
		name   string
		dialog *dialog
		want   string
	}{
		{"a menu", menu, "Deny"},
		{"a question", confirmDialog("Title", longBody(), nil), "yes (Enter)"},
		{"a field", inputDialog("Title", longBody(), "", "", nil), "Enter accepts"},
		{"a form", form, "the field"},
		{"a report", status, "Esc stops watching"},
		{"a message", errorDialog("Title", longBody()), "Esc closes"},
		{"a text", textDialog("Title", longBody()), "Esc closes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, window := range []int{24, 40, 60} {
				text := ansi.Strip(tc.dialog.view(newStyles(false), &zones{}, 100, window))
				if rows := strings.Count(text, "\n") + 1; rows > window {
					t.Errorf("%d rows in a window of %d", rows, window)
				}
				if !strings.Contains(text, tc.want) {
					t.Errorf("%q is not on the card in a window of %d:\n%s", tc.want, window, text)
				}
				if !strings.Contains(text, "to scroll") {
					t.Errorf("a window of %d does not say the body scrolls:\n%s", window, text)
				}
			}
		})
	}
}

// The page keys walk a card's body to its end while its arrows go on choosing
// between the answers, and a card that fits is not offered a scroll it has no
// use for.
func TestThePageKeysScrollTheBodyOfACardWithAnswers(t *testing.T) {
	t.Parallel()
	menu := func(body string) *dialog {
		return actionsDialog("Title", body, []action{
			{key: "yes", press: "a", label: "Approve", enabled: true},
			{key: "no", press: "d", label: "Deny", enabled: true},
		}, nil)
	}
	d := menu(longBody())
	if text := draw(d); !strings.Contains(text, "fact 0\n") && !strings.Contains(text, "fact 0 ") {
		t.Fatalf("the card does not open at the top of its body:\n%s", text)
	}
	if !strings.Contains(draw(d), "PgUp/PgDn to scroll") {
		t.Fatalf("the card does not name the keys that scroll it:\n%s", draw(d))
	}
	for range 20 {
		if _, closed := d.update(keyPress("pgdown")); closed {
			t.Fatal("PgDn closed the card")
		}
		draw(d)
	}
	text := draw(d)
	if !strings.Contains(text, "fact 79") {
		t.Errorf("PgDn did not reach the end of the body:\n%s", text)
	}
	if !strings.Contains(text, "Approve") || !strings.Contains(text, "Deny") {
		t.Errorf("the answers left the card as it scrolled:\n%s", text)
	}
	d.update(keyPress("down"))
	if d.cursor != 1 {
		t.Errorf("down moved the cursor to %d, want the second answer", d.cursor)
	}
	d.update(keyPress("pgup"))
	if text := draw(d); strings.Contains(text, "fact 79") {
		t.Errorf("PgUp did not scroll back:\n%s", text)
	}

	short := menu("one fact")
	if text := draw(short); strings.Contains(text, "to scroll") {
		t.Errorf("a card that fits offers a scroll:\n%s", text)
	}
}

// A menu longer than the window shows the rows around its cursor and says
// there are more, so the row Enter runs is always one that can be read — and
// every row is still pressed where it is drawn.
func TestAMenuLongerThanTheWindowFollowsItsCursor(t *testing.T) {
	t.Parallel()
	var items []action
	for i := range 40 {
		items = append(items, action{key: "item-" + itoa(i), label: "secret " + itoa(i), enabled: true})
	}
	d := actionsDialog("Title", longBody(), items, nil)
	z := &zones{}
	view := func() string {
		z.reset()
		return ansi.Strip(d.view(newStyles(false), z, 100, 24))
	}
	text := view()
	if rows := strings.Count(text, "\n") + 1; rows > 24 {
		t.Fatalf("%d rows in a window of 24:\n%s", rows, text)
	}
	if !strings.Contains(text, "❯ secret 0 ") || !strings.Contains(text, "of 40") {
		t.Fatalf("the menu does not open on its first row and say how many there are:\n%s", text)
	}
	if !strings.Contains(text, "fact 0") {
		t.Fatalf("the menu left the body no rows:\n%s", text)
	}
	for range 39 {
		d.update(keyPress("down"))
		if text := view(); !strings.Contains(text, "❯ secret "+itoa(d.cursor)+" ") {
			t.Fatalf("the cursor is on row %d, which is not drawn:\n%s", d.cursor, text)
		}
	}
	if d.cursor != 39 {
		t.Fatalf("down stopped at row %d of 40", d.cursor)
	}
	// The window moves only when the cursor leaves it: going back up a row
	// keeps the rows below it on screen rather than scrolling them away.
	bottom := "secret 39 "
	d.update(keyPress("up"))
	if text := view(); !strings.Contains(text, "❯ secret 38 ") || !strings.Contains(text, bottom) {
		t.Fatalf("one row up scrolled the menu away from its last row:\n%s", text)
	}
	d.update(keyPress("down"))
	// The row marked for the pointer is the row the frame drew there.
	lines := strings.Split(view(), "\n")
	marks := 0
	for _, mark := range z.marks {
		// idx -1 is the menu's summary row, which is no action.
		if mark.what.kind != hitDialogItem || mark.what.idx < 0 {
			continue
		}
		marks++
		if want := "secret " + itoa(mark.what.idx) + " "; !strings.Contains(lines[mark.y], want) {
			t.Errorf("row %d is marked as %q and draws %q", mark.y, want, lines[mark.y])
		}
	}
	if marks == 0 {
		t.Error("no menu row is marked")
	}
}

// The row under a windowed menu that says there are more belongs to the menu:
// the wheel over it walks the menu rather than the body above, and a press on
// it runs nothing.
func TestTheWheelOverAMenusSummaryRowWalksTheMenu(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	var items []action
	for i := range 40 {
		items = append(items, action{key: "item-" + itoa(i), label: "secret " + itoa(i), enabled: true})
	}
	ran := ""
	m.dialog = actionsDialog("Title", longBody(), items, func(key string) tea.Cmd { ran = key; return nil })
	x, y := at(t, m, "of 40")
	offset := m.dialog.offset
	send(t, m, tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	if m.dialog.cursor == 0 {
		t.Error("the wheel over the summary row did not move the menu's cursor")
	}
	if m.dialog.offset != offset {
		t.Error("the wheel over the summary row scrolled the body")
	}
	send(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if ran != "" || m.dialog == nil {
		t.Errorf("a press on the summary row ran %q", ran)
	}
}

// A long menu under a short question takes the rows the question does not
// need: the half it is held to is only the body's when the body wants it.
func TestALongMenuTakesTheRowsAShortBodyLeaves(t *testing.T) {
	t.Parallel()
	var items []action
	for i := range 40 {
		items = append(items, action{key: "item-" + itoa(i), label: "secret " + itoa(i), enabled: true})
	}
	for _, body := range []string{"", "which one?"} {
		d := actionsDialog("Title", body, items, nil)
		text := ansi.Strip(d.view(newStyles(false), &zones{}, 100, 30))
		// A window of 30 allows the card 27 rows, and it uses them.
		if rows := strings.Count(text, "\n") + 1; rows != dialogHeight(30) {
			t.Errorf("body %q: the card is %d rows, want the %d it is allowed:\n%s", body, rows, dialogHeight(30), text)
		}
	}
}

// A card too narrow for its key line wraps it, and the wrapped row is counted:
// the row a narrow window loses is otherwise the one that says how to answer.
func TestANarrowCardStillFitsItsWindow(t *testing.T) {
	t.Parallel()
	menu := actionsDialog("Title", longBody(), []action{
		{key: "yes", press: "a", label: "Approve", enabled: true},
		{key: "no", press: "d", label: "Deny", enabled: true},
	}, nil)
	for _, d := range []*dialog{menu, inputDialog("Title", longBody(), "", "", nil), confirmDialog("Title", longBody(), nil)} {
		for width := 24; width <= 60; width++ {
			for _, window := range []int{16, 20, 24, 40} {
				text := ansi.Strip(d.view(newStyles(false), &zones{}, width, window))
				if rows := strings.Count(text, "\n") + 1; rows > window {
					t.Fatalf("kind %d at %d columns: %d rows in a window of %d:\n%s", d.kind, width, rows, window, text)
				}
			}
		}
	}
}

// A window made taller under a menu scrolled to its end is drawn at its new
// size at once: the menu's window is settled before the card is measured, so
// the first frame after a resize is not budgeted from the last one's rows.
func TestAWindowedMenuFitsTheFirstFrameAfterAResize(t *testing.T) {
	t.Parallel()
	var items []action
	for i := range 40 {
		items = append(items, action{key: "item-" + itoa(i), label: "secret " + itoa(i), enabled: true})
	}
	d := actionsDialog("Title", longBody(), items, nil)
	d.cursor = 39
	st := newStyles(false)
	d.view(st, &zones{}, 100, 24)
	for _, window := range []int{60, 50, 30, 24} {
		text := ansi.Strip(d.view(st, &zones{}, 100, window))
		if rows := strings.Count(text, "\n") + 1; rows > dialogHeight(window) {
			t.Fatalf("the first draw in a window of %d is %d rows, want at most %d:\n%s", window, rows, dialogHeight(window), text)
		}
	}
}

// A field wider than a narrow card wraps, and the key line under it is still
// pressed where it is drawn: its marks are placed by the rows the box draws,
// not by the lines written.
func TestAWrappedFieldDoesNotMoveTheKeyLinesMarks(t *testing.T) {
	t.Parallel()
	d := inputDialog("Title", "a question", "", strings.Repeat("x", 60), nil)
	for _, width := range []int{36, 44, 60, 100} {
		z := &zones{}
		lines := strings.Split(ansi.Strip(d.view(newStyles(false), z, width, 40)), "\n")
		found := false
		for _, mark := range z.marks {
			if mark.what.kind != hitKey || len(mark.what.keys) == 0 || mark.what.keys[0] != "esc" {
				continue
			}
			found = true
			if !strings.Contains(lines[mark.y], "Esc cancels") {
				t.Errorf("at %d columns Esc is marked on row %d, which draws %q", width, mark.y, lines[mark.y])
			}
		}
		if !found {
			t.Errorf("at %d columns the key line has no mark for Esc", width)
		}
	}
}

// A menu that chooses between sentences draws each one whole under its label
// rather than cutting it to what the label's row has left: a row ending in "…"
// hides the thing being chosen. Every row an item takes is that item's press,
// so the pointer on a wrapped line runs the item it belongs to.
func TestAMenuThatWrapsShowsEveryDetailWhole(t *testing.T) {
	t.Parallel()
	long := "Discoboxes whose primary source is discobox-ai/discobox, started to answer a person's request to list or look up their own GitHub repositories."
	items := []action{
		{key: "one", label: "This discobox", detail: "pinned by its ID", enabled: true},
		{key: "two", label: "Discoboxes doing this work on this repository", detail: long, enabled: true},
		{key: "three", label: "Any discobox", detail: "any discobox", enabled: false, why: "any discobox\nfailed, and cannot be chosen: no repository is recorded"},
	}
	d := actionsDialog("Title", "", items, nil)
	d.wrapDetails = true
	z := &zones{}
	text := ansi.Strip(d.view(newStyles(false), z, 80, 40))
	if strings.Contains(text, "…") {
		t.Fatalf("a detail was cut:\n%s", text)
	}
	words := strings.Join(strings.Fields(strings.NewReplacer("│", " ").Replace(text)), " ")
	for _, want := range []string{long, "failed, and cannot be chosen: no repository is recorded"} {
		if !strings.Contains(words, want) {
			t.Fatalf("the card does not carry %q whole:\n%s", want, text)
		}
	}
	// The marks cover each item's label row and the rows under it, in order,
	// and nothing else: a row marked for the wrong item is a press that runs
	// the choice beside the one pointed at.
	lines := strings.Split(text, "\n")
	rowsOf := map[int][]int{}
	for _, mark := range z.marks {
		if mark.what.kind == hitDialogItem {
			rowsOf[mark.what.idx] = append(rowsOf[mark.what.idx], mark.y)
		}
	}
	if len(rowsOf[1]) < 3 {
		t.Fatalf("the long item is marked on rows %v, want its label and each wrapped row", rowsOf[1])
	}
	for idx, label := range []string{"This discobox", "Discoboxes doing this work on this repository", "Any discobox"} {
		rows := rowsOf[idx]
		if len(rows) == 0 || !strings.Contains(lines[rows[0]], label) {
			t.Fatalf("item %d is marked on rows %v, the first of which draws %q", idx, rows, lines[rows[0]])
		}
		for i := 1; i < len(rows); i++ {
			if rows[i] != rows[i-1]+1 {
				t.Fatalf("item %d is marked on rows %v, want them together", idx, rows)
			}
		}
	}
	if last := rowsOf[1][len(rowsOf[1])-1]; !strings.Contains(lines[last], "repositories.") {
		t.Fatalf("the long item's last marked row draws %q, want the end of its sentence", lines[last])
	}
}

// A wrapped menu that does not fit its window goes back to one row an item,
// windowed around the cursor as any long menu is, so what Enter runs is still
// on screen; and a card that never asked to wrap is unchanged.
func TestAMenuThatWrapsFallsBackWhenItDoesNotFit(t *testing.T) {
	t.Parallel()
	var items []action
	for i := range 30 {
		// Two lines, as a list of uses or a failed proposal's reason is: one
		// row an item has to hold whatever the detail carries.
		items = append(items, action{key: "item-" + itoa(i), label: "choice " + itoa(i), detail: "· a sentence to read whole\n· and another under it", enabled: true})
	}
	d := actionsDialog("Title", "", items, nil)
	d.wrapDetails = true
	z := &zones{}
	text := ansi.Strip(d.view(newStyles(false), z, 80, 20))
	if rows := strings.Count(text, "\n") + 1; rows > 20 {
		t.Fatalf("%d rows in a window of 20:\n%s", rows, text)
	}
	if !strings.Contains(text, "❯ choice 0 ") || !strings.Contains(text, "of 30") {
		t.Fatalf("a menu too long to wrap is not windowed on its cursor:\n%s", text)
	}
	// The row marked for the pointer is the row the frame drew there: a press
	// on one choice must not run the one under it.
	lines := strings.Split(text, "\n")
	marks := 0
	for _, mark := range z.marks {
		if mark.what.kind != hitDialogItem || mark.what.idx < 0 {
			continue
		}
		marks++
		if want := "choice " + itoa(mark.what.idx) + " "; !strings.Contains(lines[mark.y], want) {
			t.Errorf("row %d is marked as %q and draws %q", mark.y, want, lines[mark.y])
		}
	}
	if marks == 0 {
		t.Error("no menu row is marked")
	}

	long := []action{
		{key: "a", label: "choice 0", detail: strings.Repeat("a sentence to read whole ", 6), enabled: true},
		{key: "b", label: "choice 1", detail: strings.Repeat("a sentence to read whole ", 6), enabled: true},
	}
	plain := actionsDialog("Title", "", long, nil)
	text = ansi.Strip(plain.view(newStyles(false), &zones{}, 80, 40))
	if !strings.Contains(text, "…") || strings.Count(text, "choice ") != 2 {
		t.Fatalf("a menu that did not ask to wrap changed:\n%s", text)
	}
}
