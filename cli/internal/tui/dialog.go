package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type dialogKind int

const (
	dlgMessage dialogKind = iota
	dlgConfirm
	dlgActions
	dlgInput
	dlgText
	// dlgStatus is a dialog nobody answers: it reports what is happening and
	// takes itself down when that finishes. See statusDialog.
	dlgStatus
)

// dialog is the single modal layer. Everything that is not the list or the
// prompt — the action menu, a confirmation, the help — is one of these.
type dialog struct {
	kind  dialogKind
	title string
	body  string
	items []action
	// footer is the line under a menu, which says what choosing one of its
	// rows does. Empty takes the action menu's wording.
	footer string
	cursor int
	offset int
	input  textinput.Model
	err    bool

	// emphasis is the one line a question turns on — the size of a directory
	// being measured behind it — drawn under the body in the window's accent
	// rather than buried in the sentence explaining it. It is rewritten while
	// the dialog is up, so it is its own field rather than part of body.
	emphasis string

	// defaultNo makes Enter mean no rather than yes, for a question whose
	// costly answer is yes. Only dlgConfirm reads it.
	defaultNo bool

	// altKey is a second thing a menu row can be asked for, on a key of its
	// own beside the one that runs it: the tools picker's "edit this tool's
	// files". Empty means the menu has only the one action per row. alt is
	// handed the highlighted row's key, since that is what identifies a row to
	// whoever built the menu.
	altKey string
	alt    func(key string) tea.Cmd

	// The "/" search over a body too long to read down. query is what has been
	// typed, match is which of the lines holding it the body is on, and typing
	// says the line at the foot is still being typed into.
	//
	// matches and seek belong to the draw: only it knows how the body wrapped
	// at this width, so it is the draw that counts the matches and scrolls to
	// one, and a key press only says which one it wants.
	query   string
	match   int
	matches int
	typing  bool
	seek    bool
	// resume is where the body sat when the search line opened, to go back to
	// if the search is abandoned.
	resume int
	// overflow is whether the last draw had more body than window. A dialog
	// that fits does not offer a search: there is nothing in it to look for
	// that is not already on screen.
	overflow bool

	// action receives the result: the chosen action's key, the entered text,
	// or "yes" for a confirmed question. It is not called on cancel.
	action func(result string) tea.Cmd

	// onCancel runs when the dialog is dismissed without answering. Most
	// dialogs have nothing to do there, but a question whose two answers are
	// both answers — carry the uncommitted changes, or do not — has to hear
	// "no" as well as "yes".
	onCancel func() tea.Cmd
}

// action is one row of the action menu, and also the definition of the letter
// that runs it directly from the list.
type action struct {
	key     string
	label   string
	detail  string
	enabled bool
	why     string // why it is not available, when it is not
}

func errorDialog(title, body string) *dialog {
	return &dialog{kind: dlgMessage, title: title, body: body, err: true}
}

func confirmDialog(title, body string, act func(string) tea.Cmd) *dialog {
	return &dialog{kind: dlgConfirm, title: title, body: body, action: act}
}

func actionsDialog(title, body string, items []action, act func(string) tea.Cmd) *dialog {
	d := &dialog{kind: dlgActions, title: title, body: body, items: items, action: act}
	d.cursor = d.nextEnabled(0, 1)
	return d
}

func inputDialog(title, body, placeholder, value string, act func(string) tea.Cmd) *dialog {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.SetValue(value)
	ti.Focus()
	ti.CharLimit = 200
	ti.SetWidth(44)
	ti.Prompt = "› "
	return &dialog{kind: dlgInput, title: title, body: body, input: ti, action: act}
}

// statusDialog reports an operation the user started and now waits on.
//
// It has no answer to give, so it has no buttons and Enter does not dismiss it:
// the thing it describes is what ends it. Escape still cancels, because a wait
// nobody can leave is a trap — and canceling a wait for a discobox that is
// coming up anyway costs the view of it, not the discobox.
func statusDialog(title, body string) *dialog {
	return &dialog{kind: dlgStatus, title: title, body: body}
}

func textDialog(title, body string) *dialog {
	return &dialog{kind: dlgText, title: title, body: body}
}

func (d *dialog) cancel() tea.Cmd {
	if d.onCancel == nil {
		return nil
	}
	return d.onCancel()
}

func (d *dialog) nextEnabled(from, delta int) int {
	if len(d.items) == 0 {
		return 0
	}
	i := from
	for range d.items {
		if i >= 0 && i < len(d.items) && d.items[i].enabled {
			return i
		}
		i += delta
		if i < 0 {
			i = len(d.items) - 1
		}
		if i >= len(d.items) {
			i = 0
		}
	}
	return from
}

// update handles a key press, returning a command and whether to close.
func (d *dialog) update(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if keyName(msg) == "esc" {
		// While the search line is open Esc belongs to it: the way out of a
		// search you did not mean to start is not the way out of the help.
		if d.typing {
			d.endSearch()
			return nil, false
		}
		return d.cancel(), true
	}

	switch d.kind {
	case dlgStatus:
		// Nothing to answer. Keys fall through to the window underneath rather
		// than being swallowed by a dialog that has no use for them.
		return nil, false
	case dlgMessage, dlgText:
		return d.updateText(msg)

	case dlgConfirm:
		switch strings.ToLower(keyName(msg)) {
		case "enter":
			if d.defaultNo {
				return d.cancel(), true
			}
			if d.action != nil {
				return d.action("yes"), true
			}
			return nil, true
		case "y":
			if d.action != nil {
				return d.action("yes"), true
			}
			return nil, true
		case "n", "q":
			return d.cancel(), true
		}
		return nil, false

	case dlgActions:
		// The second action comes before the row keys, because a menu that
		// offers one has reserved that key from every row of itself.
		if d.altKey != "" && keyName(msg) == d.altKey && d.alt != nil {
			if d.cursor < 0 || d.cursor >= len(d.items) {
				return nil, false
			}
			return d.alt(d.items[d.cursor].key), true
		}
		switch keyName(msg) {
		case "up", "k":
			d.cursor = d.nextEnabled(max(d.cursor-1, 0), -1)
		case "down", "j":
			d.cursor = d.nextEnabled(min(d.cursor+1, len(d.items)-1), 1)
		case "enter":
			if d.cursor < len(d.items) && d.items[d.cursor].enabled && d.action != nil {
				return d.action(d.items[d.cursor].key), true
			}
			return nil, false
		case "q":
			return nil, true
		default:
			for _, it := range d.items {
				if it.key == keyName(msg) && it.enabled && d.action != nil {
					return d.action(it.key), true
				}
			}
		}
		return nil, false

	case dlgInput:
		if keyName(msg) == "enter" {
			if d.action != nil {
				return d.action(strings.TrimSpace(d.input.Value())), true
			}
			return nil, true
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return cmd, false
	}
	return nil, false
}

// updateText handles a key on a scrolling body: the search line while one is
// open, and otherwise the keys that walk the body and the matches in it.
func (d *dialog) updateText(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if d.typing {
		switch keyName(msg) {
		case "enter":
			// The search survives the line: the matches are what was being
			// looked for, and n and N walk them with the foot given back to
			// what the keys are.
			d.typing = false
		case "backspace":
			query := []rune(d.query)
			if len(query) == 0 {
				// Deleting past the start of a search is not having asked.
				d.endSearch()
				return nil, false
			}
			d.setQuery(string(query[:len(query)-1]))
		case "up":
			d.scroll(-1)
		case "down":
			d.scroll(1)
		default:
			// Everything a terminal reports as text goes into the query,
			// space included; a modified key is a command, not a letter.
			if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
				d.setQuery(d.query + msg.Text)
			}
		}
		return nil, false
	}

	switch keyName(msg) {
	case "/":
		d.resume, d.typing = d.offset, true
		d.setQuery("")
	case "n":
		if d.query != "" {
			d.match, d.seek = d.match+1, true
		}
	case "N":
		if d.query != "" {
			d.match, d.seek = d.match-1, true
		}
	case "enter", " ", "q":
		return nil, true
	case "up", "k":
		d.scroll(-1)
	case "down", "j":
		d.scroll(1)
	}
	return nil, false
}

// setQuery is a change to what is being looked for. The body goes to the first
// match rather than the next one after where it is scrolled to: a search of the
// help is "where is this written", which is answered from the top.
func (d *dialog) setQuery(query string) {
	d.query, d.match, d.seek = query, 0, true
}

// endSearch abandons a search: the line goes, the matches go with it, and the
// body returns to where it was when the line opened — a search that found
// nothing you wanted should not also have lost your place.
func (d *dialog) endSearch() {
	d.typing, d.query, d.match, d.matches, d.seek = false, "", 0, 0, false
	d.offset = d.resume
}

// scroll moves the body. Only the draw knows how many rows the body wrapped to
// at this width, so it is the draw that stops it at the end.
func (d *dialog) scroll(delta int) {
	d.offset = max(d.offset+delta, 0)
}

// find is which of the wrapped body lines hold the search text, matched against
// the text alone: the harness card carries color, and a query should not have
// to be typed around the escapes in it.
func (d *dialog) find(lines []string) []int {
	if d.query == "" {
		return nil
	}
	needle := strings.ToLower(d.query)
	var found []int
	for i, line := range lines {
		if strings.Contains(strings.ToLower(ansi.Strip(line)), needle) {
			found = append(found, i)
		}
	}
	return found
}

// How much of the window a dialog takes.
//
// A dialog is the only thing on screen while it is up, so the old fixed 90
// columns left most of a wide terminal empty around a card that was scrolling.
// It takes most of the window instead, and all of a window small enough that a
// margin costs more than the frame gives.
//
// Height is an allowance rather than a size: a dialog grows into it and stops
// at its content, so the config card fills the screen and "Disable Codex?"
// stays the size of the question.
const (
	dialogFillPercent = 90
	// Below these the dialog takes the whole window rather than most of it.
	dialogFullWidthBelow  = 80
	dialogFullHeightBelow = 24
	// dialogMinWidth is the narrowest worth drawing: the action rows put a key,
	// a label column and a reason on one line. A window narrower than this gets
	// all of it, which is the most there is to give.
	dialogMinWidth = 48
)

// dialogWidth and dialogHeight are the box's outside extent, border included.
func dialogWidth(window int) int  { return dialogExtent(window, dialogFullWidthBelow, dialogMinWidth) }
func dialogHeight(window int) int { return dialogExtent(window, dialogFullHeightBelow, 0) }

func dialogExtent(window, fullBelow, minimum int) int {
	if window <= 0 {
		return minimum
	}
	extent := window
	if window >= fullBelow {
		extent = window * dialogFillPercent / 100
	}
	// Never wider than the window: on a terminal narrower than the minimum, the
	// whole of it is all there is.
	return min(max(extent, minimum), window)
}

// dialogChrome is what the box costs around its content: the border and the
// padding either side. See styles.dialog.
const (
	dialogChromeWidth  = 2 + 2*2
	dialogChromeHeight = 2 + 2*1
)

func (d *dialog) view(st *styles, width, height int) string {
	boxWidth := dialogWidth(width)
	inner := max(boxWidth-dialogChromeWidth, 16)

	var b strings.Builder
	titleStyle := st.dialogTitle
	if d.err {
		titleStyle = st.statusER.Bold(true)
	}
	b.WriteString(titleStyle.Render(truncate(d.title, inner)))
	b.WriteString("\n\n")

	// What the body may take: the box's allowance, less its chrome, the title
	// and the blank under it, and the three rows the scroll hint and footer
	// need under it.
	maxBody := max(dialogHeight(height)-dialogChromeHeight-2-3, 3)
	scrolling := d.kind == dlgText || d.kind == dlgMessage
	if d.body != "" {
		// A scrolling body keeps a column for the chevron, the way every list
		// in this window points at the row you are on. It is there whether or
		// not a search is open, so opening one shifts nothing: a body that
		// re-wrapped under the search line would move the very text the search
		// had just found.
		//
		// Not on a box too narrow to have an inside — below the floor on inner,
		// lipgloss is already re-wrapping the body to the width it really has,
		// and an indent survives that wrap and takes the row past the window.
		chevron := scrolling && boxWidth-dialogChromeWidth >= inner
		room := inner
		if chevron {
			room -= lipgloss.Width(dialogMarkOff)
		}
		lines := wrap(d.body, room)
		// Wrapping is not enough on its own: a line the wrapper could not break
		// — the help text's key columns are one long run of spaces and words —
		// comes back wider than the box, and lipgloss wraps it again into a row
		// the height was not budgeted for. One row over is a frame one row
		// taller than the terminal.
		for i, line := range lines {
			lines[i] = truncate(line, room)
		}
		if scrolling {
			b.WriteString(d.viewBody(st, lines, inner, maxBody, chevron))
		} else {
			b.WriteString(strings.Join(lines, "\n"))
		}
		b.WriteString("\n")
	}

	if d.emphasis != "" {
		b.WriteString("\n")
		b.WriteString(st.dialogTitle.Render(truncate(d.emphasis, inner)))
		b.WriteString("\n")
	}

	switch d.kind {
	case dlgConfirm:
		b.WriteString("\n")
		yes, no := "yes", "no"
		if d.defaultNo {
			no += " (Enter)"
		} else {
			yes += " (Enter)"
		}
		b.WriteString(st.key.Render("y"))
		b.WriteString(" ")
		b.WriteString(yes)
		b.WriteString("   ")
		b.WriteString(st.key.Render("n"))
		b.WriteString(" ")
		b.WriteString(no)
	case dlgStatus:
		b.WriteString("\n")
		b.WriteString(st.dimText.Render("Esc stops watching"))
	case dlgMessage, dlgText:
		b.WriteString("\n")
		b.WriteString(truncate(d.viewSearch(st, inner), inner))
	case dlgActions:
		b.WriteString("\n")
		// The label column fits the longest label rather than a fixed width:
		// action names are a word, but the folder dropdown's are paths, and a
		// path cut off at fourteen cells is not a path you can choose between.
		labelW := 14
		for _, it := range d.items {
			labelW = max(labelW, lipgloss.Width(it.label))
		}
		labelW = min(labelW, max(inner-24, 14))
		for i, it := range d.items {
			bar, label := " ", st.dimText
			if i == d.cursor && it.enabled {
				bar, label = st.key.Render("❯"), st.cursorName
			}
			key, detail := st.key.Render(it.key), it.detail
			if !it.enabled {
				key = st.dimText.Render(it.key)
				if it.why != "" {
					detail = it.why
				}
			}
			row := bar + " " + key + "  " + label.Render(pad(it.label, labelW)) + " " +
				st.dimText.Render(truncate(detail, max(inner-labelW-6, 8)))
			b.WriteString(padANSI(row, inner))
			b.WriteString("\n")
		}
		footer := d.footer
		if footer == "" {
			footer = "Enter runs the highlighted action · Esc cancels"
		}
		b.WriteString(st.dimText.Render(truncate(footer, inner)))
	case dlgInput:
		b.WriteString("\n")
		b.WriteString(d.input.View())
		b.WriteString("\n\n")
		b.WriteString(st.dimText.Render("Enter accepts · Esc cancels"))
	}

	return st.dialog.Width(boxWidth).Render(b.String())
}

// dialogMark is the chevron column down the left of a scrolling body: the match
// the search is on, and nothing on every other row.
const (
	dialogMarkOn  = "❯ "
	dialogMarkOff = "  "
)

// viewBody draws the scrolling half of a text dialog: the rows in view, the
// matches under the search painted, and the hint that says there is more.
//
// The matches are counted here rather than at the key that asked for them
// because the body is wrapped to the window: which line a word falls on, and so
// how many lines hold it, is only known once there is a width to wrap to.
func (d *dialog) viewBody(st *styles, lines []string, inner, maxBody int, chevron bool) string {
	found := d.find(lines)
	d.matches = len(found)
	if len(found) > 0 {
		// n and N walk the index off either end, and a resize changes what it
		// counts against, so it is wrapped back into range here.
		d.match = ((d.match % len(found)) + len(found)) % len(found)
		if d.seek {
			// A third of the way down rather than at the top, so a match
			// arrives with the lines above it that say which section it is in.
			d.offset = max(found[d.match]-maxBody/3, 0)
		}
	}
	d.seek = false
	d.offset = min(max(d.offset, 0), max(len(lines)-maxBody, 0))
	end := min(d.offset+maxBody, len(lines))

	current := -1
	if len(found) > 0 {
		current = found[d.match]
	}
	marked := make(map[int]bool, len(found))
	for _, i := range found {
		marked[i] = true
	}

	on, off := "", ""
	if chevron {
		on, off = st.key.Render(dialogMarkOn), dialogMarkOff
	}

	var b strings.Builder
	for i := d.offset; i < end; i++ {
		if i > d.offset {
			b.WriteString("\n")
		}
		// The two bands are the list's own: a row the search is pointing at,
		// and a row it merely found, told apart the way the row under the
		// cursor is told apart from the rows a command would act on.
		switch {
		case i == current:
			b.WriteString(highlight(st, padANSI(on+lines[i], inner), colBothBG))
		case marked[i]:
			b.WriteString(highlight(st, padANSI(off+lines[i], inner), colSelectedBG))
		default:
			b.WriteString(off)
			b.WriteString(lines[i])
		}
	}
	d.overflow = len(lines) > maxBody
	if d.overflow {
		b.WriteString("\n")
		b.WriteString(st.dimText.Render("  … ↑/↓ to scroll"))
	}
	return b.String()
}

// viewSearch is the row under a scrolling body: the search line while one is
// being typed, and otherwise what the keys there are.
func (d *dialog) viewSearch(st *styles, inner int) string {
	if d.typing {
		// The tally rides on the right of the line rather than waiting for
		// Enter, so a query that matches nothing says so while there are still
		// letters to take back.
		return spread(st.key.Render("/")+d.query+st.key.Render("▏"), st.dimText.Render(d.tally()), inner)
	}
	if d.query != "" {
		// The query stays on the line with the search put away, because the
		// rows below it are painted and n and N are live: a footer that forgot
		// what was searched for would leave both unexplained.
		return st.key.Render("/") + d.query +
			st.dimText.Render("  "+d.tally()+" · n / N next, previous · / search · Esc closes")
	}
	if d.overflow {
		return st.dimText.Render("/ search · Esc closes")
	}
	return st.dimText.Render("Esc closes")
}

// tally is which match the body is on, of how many.
func (d *dialog) tally() string {
	if d.matches == 0 {
		return "no matches"
	}
	return itoa(d.match+1) + "/" + itoa(d.matches)
}
