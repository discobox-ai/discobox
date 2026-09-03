package tui

import (
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/termpane"
	"github.com/discobox-ai/x/selection"
)

// The window answers the mouse on every screen it draws on the alternate
// screen, not only where a pane is up. What a press means is looked up in the
// hit map the frame left behind (zones.go); what is left over — a press on
// nothing in particular — drives the chrome's selection, which is what stands
// in for the native one the terminal stopped doing the moment the mouse was
// reported. See ADR 0088.
//
// The opening prompt is the exception, and mouseMode is where it is made:
// it is drawn inline, in the shell's own scrollback, where a mouse coordinate
// is the terminal's rather than the frame's and the terminal's own selection
// is still the right one.

// clickRun is how close together two presses on the same cell must be to read
// as one gesture growing — the conventional desktop double-click window, and
// the same one the selection component uses for its word and line clicks.
const clickRun = 500 * time.Millisecond

// updateMouse routes an event to whatever the frame it was aimed at was
// showing, in the order view() draws: the introduction and the modals stand in
// place of the window, then the panes, then the window itself. Only the panes
// route by their own geometry; everything else is a lookup in the hit map.
func (m *Model) updateMouse(msg tea.MouseMsg) tea.Cmd {
	// A pointer moving with no button down is nobody's gesture: it is where
	// the pointer is resting, which is what draws a control as live.
	if ev, ok := msg.(tea.MouseMotionMsg); ok && ev.Button == tea.MouseNone {
		return m.hover(ev)
	}
	if m.inPanes() && !m.modalUp() {
		return m.routeMouse(msg)
	}
	return m.windowMouse(msg)
}

// hover records where the pointer is resting, so the next frame can draw
// whatever is under it as the control it is. The window asks the terminal for
// every move (mouseMode) and answers most of them here: a sandbox is sent the
// move only when it asked for motion itself, so one that subscribed to buttons
// alone receives no more than it did before the window wanted hover.
func (m *Model) hover(ev tea.MouseMotionMsg) tea.Cmd {
	m.hoverX, m.hoverY = ev.X, ev.Y
	if m.modalUp() || !m.inPanes() || m.mouseSeized {
		return nil
	}
	if p := m.focusedPane(); p != nil && p.term.MouseMode() == termpane.MouseAllMotion {
		return m.routeMouse(ev)
	}
	return nil
}

// modalUp is whether something stands in place of the window rather than
// inside it: the introduction, a dialog, or the run options. All three are
// drawn over everything, panes included, so all three take the mouse as well
// as the keys.
func (m *Model) modalUp() bool {
	return m.welcoming || m.dialog != nil || m.optionsOpen
}

// windowMouse is the mouse on one of the window's own screens.
func (m *Model) windowMouse(msg tea.MouseMsg) tea.Cmd {
	switch ev := msg.(type) {
	case tea.MouseWheelMsg:
		return m.wheelAt(ev)

	case tea.MouseClickMsg:
		switch ev.Button {
		case tea.MouseLeft:
			return m.leftPress(ev)
		case tea.MouseRight:
			return m.rightPress(ev)
		case tea.MouseMiddle:
			// What the middle button pastes everywhere else: the last thing
			// selected, which is X11's primary selection and not the
			// clipboard. See ADR 0088 §6.
			return m.pastePrimary()
		}
		return nil

	case tea.MouseMotionMsg:
		if ev.Button != tea.MouseLeft {
			return nil
		}
		if m.promptCapture {
			if where, ok := m.zones.find(hitPrompt); ok {
				m.prompt.ExtendSelection(ev.X-where.x, ev.Y-where.y)
			}
			return nil
		}
		if !m.chromeCapture {
			return nil
		}
		m.chromeSel.MouseDrag(ev.Y, ev.X)
		m.chromeShot = m.chromeSel.Text()
		return nil

	case tea.MouseReleaseMsg:
		if ev.Button != tea.MouseLeft {
			return nil
		}
		if m.promptCapture {
			m.promptCapture = false
			m.prompt.EndSelection()
			return m.tookSelection(m.prompt.SelectedText())
		}
		if !m.chromeCapture {
			return nil
		}
		m.chromeCapture = false
		if text, ok := m.chromeSel.MouseUp(); ok {
			m.chromeShot = m.chromeSel.Text()
			return m.tookSelection(text)
		}
		return nil
	}
	return nil
}

// leftPress answers a left button press: what the cell means, and then, unless
// the control took the gesture for itself, the start of a selection over the
// cells it landed on.
func (m *Model) leftPress(ev tea.MouseClickMsg) tea.Cmd {
	// Whatever was reported was about the last thing pressed, key or button.
	m.status, m.statusE = "", false
	m.statusGen++

	where, _ := m.zones.at(ev.X, ev.Y)
	clicks := m.countClick(ev.X, ev.Y)

	// The composer is a field, and a press in it is the caret moving rather
	// than anything about the window. Its selection is the textarea's own, so
	// that typing replaces it and Backspace deletes it. See ADR 0088 §2.
	if where.what.kind == hitPrompt {
		return m.pressPrompt(ev.X-where.x, ev.Y-where.y, clicks)
	}
	// A card's text field is a field too, and takes the caret the same way.
	if where.what.kind == hitInput {
		m.pressInput(where.what.idx, ev.X-where.x)
		m.clearSelections()
		return nil
	}

	cmd, taken := m.press(where.what, clicks)
	if taken {
		// A control that answered the press must not also start a drag-select
		// of its own label; see the credential banner, which was the first of
		// them.
		m.clearSelections()
		return cmd
	}

	// The grid must be the frame that was on screen at the moment of the
	// press.
	m.parseChrome(m.lastFrame)
	m.chromeCapture = true
	m.prompt.ClearSelection()
	m.clearPaneSelections(nil)
	m.chromeSel.MouseDown(ev.Y, ev.X, ev.Mod&tea.ModAlt != 0)
	m.chromeShot = m.chromeSel.Text()
	return cmd
}

// rightPress copies a showing selection, and otherwise opens the menu for
// whatever it landed on. The copy wins because it is the more expensive to
// lose: a right-click meant for the menu costs one more press, where one that
// threw away a finished selection costs the selection. See ADR 0088 §4.
func (m *Model) rightPress(ev tea.MouseClickMsg) tea.Cmd {
	if cmd := m.copyShowingSelection(); cmd != nil {
		return cmd
	}
	where, ok := m.zones.at(ev.X, ev.Y)
	if !ok {
		return nil
	}
	return m.menuFor(where.what)
}

// copyShowingSelection takes whatever selection is on screen, clears it and
// copies it, the way the right button does over a pane's own; see termpane's
// rightClickCopy. It reports nil when there was nothing showing.
func (m *Model) copyShowingSelection() tea.Cmd {
	var text string
	switch {
	case m.prompt.HasSelection():
		text = m.prompt.SelectedText()
	case m.chromeSel.Active():
		text = m.chromeSel.Text()
	default:
		return nil
	}
	m.clearSelections()
	if text == "" {
		return nil
	}
	return m.copyText(text, "copied")
}

// clearSelections drops every selection the window itself is drawing. One
// selection is on screen at a time, or two highlights race to answer the next
// copy.
func (m *Model) clearSelections() {
	m.chromeSel.Clear()
	m.chromeCapture = false
	m.prompt.ClearSelection()
	m.promptCapture = false
}

// tookSelection is a finished gesture: the text becomes what the middle button
// will paste — the window's own primary selection — and goes to the clipboard,
// which is what a completed selection has always done here.
func (m *Model) tookSelection(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	m.primaryText = text
	return m.copyText(text, "copied")
}

// pastePrimary puts the last selection back wherever text goes, exactly as a
// bracketed paste would: through updatePaste, so the composer's undo and the
// dialogs' inputs treat a middle click and a Ctrl-V as the same thing.
func (m *Model) pastePrimary() tea.Cmd {
	if m.primaryText == "" {
		return status("nothing has been selected to paste")
	}
	// Text has to land somewhere, and on the window's own screens that is the
	// composer — so a middle click puts the keyboard there as well, the way
	// clicking into a field does. A modal or a pane already has somewhere for
	// it to go and keeps it.
	if !m.modalUp() && !m.inPanes() && m.focus != focusPrompt {
		m.backToPrompt()
	}
	return m.updatePaste(tea.PasteMsg{Content: m.primaryText})
}

// countClick is how many presses in a row have landed on this cell: two is a
// double click, and the count resets as soon as the pointer moves or the run
// goes cold.
func (m *Model) countClick(x, y int) int {
	now := m.now()
	if x != m.clickX || y != m.clickY || now.Sub(m.clickAt) > clickRun {
		m.clicks = 0
	}
	m.clicks++
	m.clickX, m.clickY, m.clickAt = x, y, now
	return m.clicks
}

// press applies a left press to what the cell means, and reports whether the
// control answered it outright. A control that only points at something — a
// row, which the cursor moves to — does not: the gesture goes on into a
// selection, so the text of a row stays drag-selectable.
func (m *Model) press(what hit, clicks int) (tea.Cmd, bool) {
	switch what.kind {
	case hitKey:
		return m.pressKeys(what.keys), true

	case hitListKey:
		// The list's own band, pressed from wherever the keyboard is.
		m.focusListForMouse()
		return m.pressKeys(what.keys), true

	// A row press only points at the row, so the gesture goes on into a
	// selection; the second press is the one that acts, and takes it. A press
	// on the empty part of a list is neither: it says which list you are in.
	case hitRow:
		return m.pressRow(what.idx, clicks), clicks > 1 && what.idx >= 0

	case hitHarnessRow:
		return m.pressHarnessRow(what.idx, clicks), clicks > 1 && what.idx >= 0

	case hitSecretRow:
		return m.pressSecretRow(what.idx, clicks), clicks > 1 && what.idx >= 0

	case hitRequestRow:
		return m.pressRequestRow(what.idx, clicks), clicks > 1 && what.idx >= 0

	case hitOptionRow:
		m.opts.moveTo(what.idx)
		if clicks > 1 {
			return m.updateKey(keyPress("enter")), true
		}
		return nil, false

	case hitOptionCycle:
		m.opts.moveTo(what.idx)
		if what.delta < 0 {
			return m.updateKey(keyPress("left")), true
		}
		return m.updateKey(keyPress("right")), true

	case hitDialogItem:
		return m.pressDialogItem(what.idx), true

	case hitFormRow:
		// The row is answered where the keyboard is: a press moves the cursor
		// to it and focuses its field, which is what ↑ and ↓ do.
		if d := m.dialog; d != nil && d.kind == dlgForm {
			d.form.moveTo(what.idx)
		}
		return nil, true

	case hitFolder:
		// A dropdown opens when it is clicked. Reaching it and opening it are
		// two keys, because a keyboard has to get there first; a pointer is
		// already there.
		m.prompt.Blur()
		m.focus = focusFolder
		m.dialog = m.folderDialog()
		return nil, true

	case hitGit:
		// The leader's tools key is handled by the pane asynchronously, but a
		// pointer is already aimed at the diff choice. Build the same picker and
		// choose its d row through the dialog's key handler, preserving the
		// picker's availability check without flashing it on screen.
		_ = m.openTools()
		return m.updateKey(keyPress("d")), true

	case hitChips:
		// The strip names the run options, so it is the way into them.
		m.optionsOpen = true
		return nil, true
	}
	return nil, false
}

// pressKeys hands a hint's keystrokes to the same handler the keyboard reaches,
// in order. A chord is two presses and a click on one is those two presses: the
// leader is armed by the first exactly as a finger would arm it.
func (m *Model) pressKeys(keys []string) tea.Cmd {
	var cmds []tea.Cmd
	for _, k := range keys {
		if k == "" {
			continue
		}
		cmds = append(cmds, m.updateKey(keyPress(k)))
	}
	return tea.Batch(cmds...)
}

// menuFor is the right button's menu for what it landed on: the actions the
// row under it can take, which is the menu `.` opens on the same rows.
func (m *Model) menuFor(what hit) tea.Cmd {
	switch what.kind {
	case hitRow:
		if what.idx < 0 {
			return nil
		}
		m.pressRow(what.idx, 1)
		targets := m.list.targets()
		m.dialog = actionsDialog(actionTitle(targets), "", m.actions(targets), chooseAction(targets))
		return nil
	case hitHarnessRow:
		if what.idx < 0 {
			return nil
		}
		m.pressHarnessRow(what.idx, 1)
		return m.harnessMenu()
	}
	return nil
}

// pressRow puts the list's cursor on a row and, on the second press, acts on
// it — attach, which is what Enter does there. The row acted on is the one
// under the pointer rather than whatever is marked: a pointer names one row,
// and that is the whole of what it said.
func (m *Model) pressRow(idx, clicks int) tea.Cmd {
	m.focusListForMouse()
	if idx < 0 {
		return nil
	}
	rows := m.list.rows()
	if idx >= len(rows) {
		return nil
	}
	m.list.moveTo(idx)
	if clicks > 1 {
		return m.actOn("a", []Sandbox{rows[idx]})
	}
	return nil
}

// focusListForMouse gives the discobox list the keyboard because the pointer
// is in it. It is where clicking a row differs from moving to one: the press
// says which list you are working in as well as which row.
func (m *Model) focusListForMouse() {
	if m.focus == focusList {
		return
	}
	m.prompt.Blur()
	m.focus = focusList
	m.list.visited = true
}

func (m *Model) pressHarnessRow(idx, clicks int) tea.Cmd {
	if idx < 0 || idx >= len(m.harnesses.all) {
		return nil
	}
	m.harnesses.moveTo(idx)
	if clicks > 1 {
		return m.updateKey(keyPress("enter"))
	}
	return nil
}

func (m *Model) pressSecretRow(idx, clicks int) tea.Cmd {
	m.onRequests = false
	if idx < 0 || idx >= len(m.secrets.all) {
		return nil
	}
	m.secrets.moveTo(idx)
	if clicks > 1 {
		return m.updateKey(keyPress("enter"))
	}
	return nil
}

func (m *Model) pressRequestRow(idx, clicks int) tea.Cmd {
	m.onRequests = true
	if idx < 0 || idx >= len(m.requestRows.all) {
		return nil
	}
	m.requestRows.moveTo(idx)
	if clicks > 1 {
		return m.updateKey(keyPress("enter"))
	}
	return nil
}

// pressDialogItem runs the action a menu row names, the way Enter on it does.
// A row that cannot be taken is left alone: its reason is already written
// beside it, which is the whole of what pressing it would say.
func (m *Model) pressDialogItem(idx int) tea.Cmd {
	d := m.dialog
	if d == nil || idx < 0 || idx >= len(d.items) {
		return nil
	}
	if !d.items[idx].enabled {
		d.cursor = idx
		return nil
	}
	d.cursor = idx
	if d.action == nil {
		return nil
	}
	stays := d.items[idx].stays
	cmd := d.action(d.items[idx].key)
	if m.dialog == d && !stays {
		m.dialog = nil
	}
	return cmd
}

// pressInput puts the caret in a card's text field where the pointer is: the
// dialog's own field when idx is negative, and otherwise the form row's, which
// takes the cursor with it.
//
// A field that has scrolled sideways declines rather than guessing. The
// textinput keeps its horizontal offset to itself, so a value longer than the
// field is one this cannot place a caret in without being wrong — and a caret
// that lands somewhere other than where it was clicked is worse than one that
// did not move.
func (m *Model) pressInput(idx, col int) {
	d := m.dialog
	if d == nil {
		return
	}
	field := &d.input
	if idx >= 0 {
		if d.kind != dlgForm || idx >= len(d.form.rows) {
			return
		}
		d.form.moveTo(idx)
		if d.form.cursor != idx {
			// The row refused the cursor — it is not one that can be answered.
			return
		}
		field = &d.form.rows[idx].input
	}
	col -= lipgloss.Width(field.Prompt)
	value := field.Value()
	if col < 0 || lipgloss.Width(value) >= field.Width() {
		return
	}
	field.SetCursor(caretAt(value, col))
}

// caretAt is the rune a display column names, walked the way the field draws
// it: a column past the end is the end.
func caretAt(value string, col int) int {
	width := 0
	for i, r := range []rune(value) {
		next := lipgloss.Width(string(r))
		if width+next > col {
			return i
		}
		width += next
	}
	return len([]rune(value))
}

// pressPrompt puts the caret where the pointer is and opens a selection there.
// A double click takes the word under it and a third the line, which is what
// the same gestures do over every other field.
func (m *Model) pressPrompt(x, y, clicks int) tea.Cmd {
	m.clearSelections()
	if m.focus != focusPrompt {
		m.backToPrompt()
	}
	switch {
	case clicks >= 3:
		return m.selectPromptSpan(0, m.prompt.Width(), y)
	case clicks == 2:
		from, to := m.promptWord(x, y)
		return m.selectPromptSpan(from, to, y)
	}
	m.prompt.BeginSelection(x, y)
	m.promptCapture = true
	return nil
}

// selectPromptSpan selects a run of the composer's screen columns on one of
// its rows. The textarea takes a selection as the gesture that made it, so a
// span is drawn as the drag it would have been.
func (m *Model) selectPromptSpan(from, to, y int) tea.Cmd {
	m.prompt.BeginSelection(from, y)
	m.prompt.ExtendSelection(to, y)
	m.prompt.EndSelection()
	return m.tookSelection(m.prompt.SelectedText())
}

// promptWord is the run of word characters under a press, in the composer's
// own screen columns: the half-open span to select for a double click.
//
// It is walked in screen columns rather than in the buffer because that is the
// coordinate the textarea takes a selection in, and PositionAt is the only way
// across between the two. A column past the end of a line maps to the line's
// end, which holds no rune, so both walks stop there on their own.
func (m *Model) promptWord(x, y int) (int, int) {
	if !isWordCell(m.promptRuneAt(x, y)) {
		return x, x + 1
	}
	from := x
	for from > 0 && isWordCell(m.promptRuneAt(from-1, y)) {
		from--
	}
	to := x + 1
	for to < m.prompt.Width() && isWordCell(m.promptRuneAt(to, y)) {
		to++
	}
	return from, to
}

// promptRuneAt is the character the composer is drawing in one of its cells,
// or zero where there is none.
func (m *Model) promptRuneAt(x, y int) rune {
	at := m.prompt.PositionAt(x, y)
	lines := strings.Split(m.prompt.Value(), "\n")
	if at.Row < 0 || at.Row >= len(lines) {
		return 0
	}
	row := []rune(lines[at.Row])
	if at.Col < 0 || at.Col >= len(row) {
		return 0
	}
	return row[at.Col]
}

// isWordCell is what a double click in the composer takes as one word. It is
// the selection component's own rule — letters, digits, the underscore and the
// punctuation that glues a path, a URL or a flag together — so double clicking
// a word means the same thing in the field as it does over the rest of the
// frame.
func isWordCell(r rune) bool {
	if r == 0 {
		return false
	}
	if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	return strings.ContainsRune(selection.DefaultWordChars, r)
}

// wheelAt scrolls whatever the pointer is over. It never moves focus: the
// wheel scrolls what is under it, the way it does over a pane, so reading the
// list while a prompt is half typed leaves the keys in the prompt.
//
// A list scrolls by its cursor rather than by an offset of its own, because
// the offset follows the cursor (clamp) and a scroll position the cursor did
// not agree with would be snapped back by the next refresh.
func (m *Model) wheelAt(ev tea.MouseWheelMsg) tea.Cmd {
	lines := wheelLines(ev)
	if lines == 0 {
		return nil
	}
	where, ok := m.zones.at(ev.X, ev.Y)
	if !ok {
		// A dialog that scrolls is the whole card, and it is the only thing on
		// screen while it is up.
		if m.dialog != nil {
			m.dialog.scroll(-lines)
		}
		return nil
	}
	switch where.what.kind {
	case hitRow:
		m.list.move(-lines)
	case hitHarnessRow:
		m.harnesses.move(-lines)
	case hitSecretRow:
		m.secrets.move(-lines)
	case hitRequestRow:
		m.requestRows.move(-lines)
	case hitOptionRow, hitOptionCycle:
		// moveTo rather than move: the arrow keys wrap round the panel, and a
		// wheel that wrapped would jump from the last row to the first on the
		// way past the end.
		m.opts.moveTo(m.opts.cursor - lines)
	case hitFormRow:
		if d := m.dialog; d != nil && d.kind == dlgForm {
			d.form.move(-sign(lines))
		}
	case hitDialogItem:
		if d := m.dialog; d != nil {
			at := min(max(d.cursor-lines, 0), len(d.items)-1)
			d.cursor = d.nextEnabled(at, sign(-lines))
		}
	default:
		if m.dialog != nil {
			m.dialog.scroll(-lines)
		}
	}
	return nil
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	return 1
}

// keyPress is the key press a terminal would send for a keystroke, so a hint
// that names a key can be answered by handling that key — and so a test can
// press one. The two shapes are the ones keyName sees through: a printable
// character carries its text, and everything else is a code, with its
// modifiers read off the front of the name.
func keyPress(spec string) tea.KeyPressMsg {
	modifiers := map[string]tea.KeyMod{"ctrl": tea.ModCtrl, "alt": tea.ModAlt, "shift": tea.ModShift}
	var mod tea.KeyMod
	for {
		head, rest, found := strings.Cut(spec, "+")
		if !found || rest == "" {
			break
		}
		next, ok := modifiers[head]
		if !ok {
			break
		}
		mod |= next
		spec = rest
	}

	named := map[string]rune{
		"up":        tea.KeyUp,
		"down":      tea.KeyDown,
		"left":      tea.KeyLeft,
		"right":     tea.KeyRight,
		"enter":     tea.KeyEnter,
		"tab":       tea.KeyTab,
		"esc":       tea.KeyEscape,
		"backspace": tea.KeyBackspace,
		"delete":    tea.KeyDelete,
		"home":      tea.KeyHome,
		"end":       tea.KeyEnd,
		"pgup":      tea.KeyPgUp,
		"pgdown":    tea.KeyPgDown,
		"f1":        tea.KeyF1,
		"f2":        tea.KeyF2,
		"f3":        tea.KeyF3,
		"f4":        tea.KeyF4,
	}
	if code, ok := named[spec]; ok {
		return tea.KeyPressMsg{Code: code, Mod: mod}
	}
	if spec == " " {
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " ", Mod: mod}
	}
	runes := []rune(spec)
	if len(runes) != 1 {
		return tea.KeyPressMsg{}
	}
	if runes[0] >= 'A' && runes[0] <= 'Z' {
		// A capital letter is a shifted one on the wire, which is the shape
		// keyName exists to see through.
		return tea.KeyPressMsg{Code: runes[0] + 32, ShiftedCode: runes[0], Text: spec, Mod: mod | tea.ModShift}
	}
	if mod != 0 {
		// A modified key carries no text of its own: the terminal sends the
		// keystroke, and the name is what the window matches on.
		return tea.KeyPressMsg{Code: runes[0], Mod: mod}
	}
	return tea.KeyPressMsg{Code: runes[0], Text: spec}
}
