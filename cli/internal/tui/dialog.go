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
	// dlgForm is the whole of a decision on one card: rows that are typed into
	// or chosen between, with the ones a given answer makes irrelevant taken
	// away. See form.
	dlgForm
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

	// titleRight is what rides on the right of the title row: the ID or the
	// age of the thing being read. It is on the title rather than in the
	// fields because it identifies the card rather than saying something about
	// its subject — and because a column of fields is for what has to be
	// compared, which an opaque ID never is.
	titleRight string

	// sections are the body with its structure kept: named blocks of aligned
	// label/value fields, drawn as a column rather than dissolved into
	// sentences. A dialog whose subject is a set of facts — what a credential
	// is, what a grant covers, what an agent is asking for — carries these
	// instead of body, and the two are drawn one after the other where a
	// dialog has both.
	sections []section

	// form is the rows of a dlgForm, and submit what to do with them once they
	// are answered. The form is handed over whole rather than as a map of
	// values: what it holds is typed by the rows themselves.
	form   *form
	submit func(f *form) tea.Cmd

	// answerLabel is the question, drawn as a rule immediately above whatever
	// answers it: the menu rows, the input line, or the y/n pair. It is the one
	// sentence a dialog must be read for, so it sits against the thing that
	// answers it rather than at the end of a paragraph above.
	answerLabel string
	cursor      int
	offset      int
	input       textinput.Model
	err         bool

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

	// keys is the line under the card saying what the keys here do, made
	// pressable the way every other key line in the window is. footer, above
	// it, is prose — the caveat the question carries — and stays text.
	keys []hint

	// onCancel runs when the dialog is dismissed without answering. Most
	// dialogs have nothing to do there, but a question whose two answers are
	// both answers — carry the uncommitted changes, or do not — has to hear
	// "no" as well as "yes".
	onCancel func() tea.Cmd
}

// action is one row of the action menu.
//
// key identifies the row and is what the menu's callback receives; press is the
// key that runs it without moving to it, and is the only one of the two ever
// drawn. They are separate because most of what this window offers to choose
// between is identified by something nobody can type — a secret's ID, a grant's
// ID, a scope's wire name — and a menu that printed those in the column where
// keystrokes go was offering keys that do not exist while wrecking the
// alignment of every row beside them.
type action struct {
	key     string
	press   string
	label   string
	detail  string
	enabled bool
	why     string // why it is not available, when it is not
}

// tone is how a value is painted. A caller says what a line means and the
// theme decides what that looks like, so the dialogs do not each reach into
// the palette for themselves.
type tone int

const (
	tonePlain tone = iota
	toneDim
	toneAccent
	toneAlert
	toneOK
)

func (t tone) style(st *styles) lipgloss.Style {
	switch t {
	case toneDim:
		return st.dimText
	case toneAccent:
		return st.dialogTitle
	case toneAlert:
		return st.statusER
	case toneOK:
		return st.statusOK
	default:
		return st.name
	}
}

// field is one label/value row. An empty label continues the row above it,
// which is how a field with several values — the uses on a request — stays one
// column rather than becoming a list beside a heading.
type field struct {
	label string
	value string
	tone  tone
}

// line is a row of a section that is not a label/value pair: a bullet under a
// heading, or a sentence that has to be read rather than scanned.
type line struct {
	text   string
	tone   tone
	bullet bool
}

// section is one named block of a dialog body: a rule carrying its name, the
// fields under it, and the lines under those.
type section struct {
	label  string
	fields []field
	lines  []line
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
	// No character limit. The longest thing typed into one of these is a
	// credential, and a credential is as long as whoever issued it made it —
	// a limit here would take a pasted token and silently store the front of
	// it, which fails later as a wrong password rather than here as a refusal.
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
// formDialog asks everything at once. Enter answers the whole card, so a form
// is for a decision with one outcome — a grant made, a secret stored — rather
// than for a menu of things that each do something different.
func formDialog(title string, f *form, submit func(f *form) tea.Cmd) *dialog {
	return &dialog{kind: dlgForm, title: title, form: f, submit: submit}
}

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
				if it.press != "" && it.press == keyName(msg) && it.enabled && d.action != nil {
					return d.action(it.key), true
				}
			}
		}
		return nil, false

	case dlgForm:
		if keyName(msg) == "enter" {
			// A form that is not answered stays up with the reason on it: the
			// alternative is a dialog that closes and reports a refusal onto
			// the screen behind it, having thrown away everything typed.
			if why := d.form.submit(); why != "" {
				return nil, false
			}
			if d.submit == nil {
				return nil, true
			}
			cmd := d.submit(d.form)
			// A submit that refused — a lifetime that is not a number — says so
			// on the form, and the form stays up holding everything typed.
			if d.form.err != "" {
				return nil, false
			}
			return cmd, true
		}
		return d.form.update(msg), false

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

// paste takes bracketed pasted text.
//
// A terminal reports a paste as one message of its own rather than as the keys
// it would have taken to type, so a dialog that only heard key presses heard
// nothing at all from a paste — and the dialogs that ask for a credential are
// exactly the ones nobody types into.
func (d *dialog) paste(msg tea.PasteMsg) tea.Cmd {
	switch d.kind {
	case dlgForm:
		return d.form.paste(msg)
	case dlgInput:
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return cmd
	case dlgMessage, dlgText:
		// Only the search line takes text; the body itself is not editable.
		if d.typing {
			d.setQuery(d.query + msg.Content)
		}
	}
	return nil
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
	// dialogPadLeft and dialogPadTop are where the content starts inside that
	// box, which is what turns a line of the card into a cell of the frame for
	// the hit map. See zones.go.
	dialogPadLeft = 1 + 2
	dialogPadTop  = 1 + 1
)

func (d *dialog) view(st *styles, z *zones, width, height int) string {
	boxWidth := dialogWidth(width)
	inner := max(boxWidth-dialogChromeWidth, 16)

	var b strings.Builder
	titleStyle := st.dialogTitle
	if d.err {
		titleStyle = st.statusER.Bold(true)
	}
	title := titleStyle.Render(truncate(d.title, inner))
	if d.titleRight != "" {
		title = spread(title, st.dimText.Render(d.titleRight), inner)
	}
	b.WriteString(title)
	b.WriteString("\n\n")

	// What the body may take: the box's allowance, less its chrome, the title
	// and the blank under it, and the three rows the scroll hint and footer
	// need under it.
	maxBody := max(dialogHeight(height)-dialogChromeHeight-2-3, 3)
	scrolling := d.kind == dlgText || d.kind == dlgMessage
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
	if lines := d.bodyLines(st, room); len(lines) > 0 {
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

	// The question, on a rule against the thing that answers it. The blank row
	// above it is the one the title already left when there is no body between
	// them, rather than a second one under it.
	answer := func() {
		if !strings.HasSuffix(b.String(), "\n\n") {
			b.WriteString("\n")
		}
		if d.answerLabel != "" {
			b.WriteString(answerRule(st, d.answerLabel, inner))
			b.WriteString("\n")
		}
	}
	// The footer is the caveat the question carries. It wraps rather than being
	// cut: a sentence that says what a credential may be used for is not one to
	// lose the end of.
	footer := func() {
		if d.footer == "" {
			return
		}
		b.WriteString("\n")
		for i, wrapped := range wrap(d.footer, inner) {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(st.dimText.Render(truncate(wrapped, inner)))
		}
	}
	// The key line under it, drawn and marked exactly as the status line's is:
	// a hint that names a key is a button for that key here too, and a card
	// whose keys were only readable would be the one surface in the window
	// where they are not. It gives up offers from the tail to fit.
	keys := func(fallback ...hint) {
		line := d.keys
		if len(line) == 0 {
			line = fallback
		}
		line = fitHints(line, hintSep, inner)
		if len(line) == 0 {
			return
		}
		b.WriteString("\n")
		z.push(dialogPadLeft, strings.Count(b.String(), "\n")+dialogPadTop)
		b.WriteString(viewHints(st, z, line, 0, hintSep))
		z.pop()
	}

	switch d.kind {
	case dlgConfirm:
		answer()
		yes, no := "yes", "no"
		if d.defaultNo {
			no += " (Enter)"
		} else {
			yes += " (Enter)"
		}
		// The two answers, each a button for its own letter.
		answerRow := strings.Count(b.String(), "\n") + dialogPadTop
		z.mark(keyHit("y"), dialogPadLeft, answerRow, lipgloss.Width("y "+yes), 1)
		z.mark(keyHit("n"), dialogPadLeft+lipgloss.Width("y "+yes)+3, answerRow, lipgloss.Width("n "+no), 1)
		b.WriteString(st.key.Render("y"))
		b.WriteString(" ")
		b.WriteString(yes)
		b.WriteString("   ")
		b.WriteString(st.key.Render("n"))
		b.WriteString(" ")
		b.WriteString(no)
		if d.footer != "" {
			b.WriteString("\n")
			footer()
		}
		keys()
	case dlgStatus:
		// No answer to give, so the key line is the whole of what can be done
		// here: stop watching, or — in a window that is the command it is
		// waiting for — leave. See Model.waitDialog.
		footer()
		keys(pressing("Esc stops watching", "esc"))
	case dlgMessage, dlgText:
		b.WriteString("\n")
		z.push(dialogPadLeft, strings.Count(b.String(), "\n")+dialogPadTop)
		b.WriteString(truncate(d.viewSearch(st, z, inner), inner))
		z.pop()
	case dlgActions:
		answer()
		// Every row of a menu is a press, marked where the card put it: a menu
		// whose rows only answer their letters is a menu the pointer cannot
		// work.
		itemsTop := strings.Count(b.String(), "\n") + dialogPadTop
		b.WriteString(d.viewItems(st, z, itemsTop, inner))
		footer()
		keys(pressing("Enter runs the highlighted action", "enter"), pressing("Esc cancels", "esc"))
	case dlgForm:
		answer()
		b.WriteString(d.form.view(st, z, strings.Count(b.String(), "\n")+dialogPadTop, inner))
		// The hint keeps its rows whether or not it has anything to say, so
		// moving down the form does not resize the card under the cursor.
		b.WriteString("\n")
		b.WriteString(d.form.hint(st, inner))
		b.WriteString("\n")
		footer()
		keys(says("↑↓ moves"), says("←→ chooses"), pressing("enter accepts", "enter"), pressing("esc cancels", "esc"))

	case dlgInput:
		answer()
		// The field is a field: a press in it puts the caret where the
		// pointer is, rather than only bringing the card into focus it
		// already has.
		field := d.input.View()
		z.mark(hit{kind: hitInput, idx: -1}, dialogPadLeft, strings.Count(b.String(), "\n")+dialogPadTop, lipgloss.Width(field), 1)
		b.WriteString(field)
		b.WriteString("\n")
		footer()
		keys(pressing("Enter accepts", "enter"), pressing("Esc cancels", "esc"))
	}

	return st.dialog.Width(boxWidth).Render(b.String())
}

// bodyLines is everything between the title and the answer, wrapped to the
// width it will be drawn at: the prose body, and under it the sections.
//
// It is one list of lines rather than two blocks because a text dialog scrolls
// and searches its body, and a card built out of sections has to scroll and be
// searched the same way a paragraph does.
func (d *dialog) bodyLines(st *styles, room int) []string {
	var lines []string
	if d.body != "" {
		lines = wrap(d.body, room)
		// Wrapping is not enough on its own: a line the wrapper could not break
		// — the help text's key columns are one long run of spaces and words —
		// comes back wider than the box, and lipgloss wraps it again into a row
		// the height was not budgeted for. One row over is a frame one row
		// taller than the terminal.
		for i, text := range lines {
			lines[i] = truncate(text, room)
		}
	}
	if len(d.sections) == 0 {
		return lines
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	return append(lines, d.sectionLines(st, room)...)
}

// sectionLines draws the structured body: a rule carrying each section's name,
// the fields under it in one column, and the lines under those.
//
// The label column is measured across every section rather than per section, so
// the values stand in one column down the whole card. Reading a card is
// comparing what it says against what you expected it to say, and a column that
// moves under each heading is a column that has to be found again each time.
func (d *dialog) sectionLines(st *styles, room int) []string {
	labelW := 0
	for _, sec := range d.sections {
		for _, f := range sec.fields {
			labelW = max(labelW, lipgloss.Width(f.label))
		}
	}
	// A label column past a third of the card leaves nothing to hold the
	// values, so it is cut and the labels are truncated into it instead.
	labelW = min(labelW, max(room/3, 8))
	valueW := max(room-labelW-2, 8)
	blank := strings.Repeat(" ", labelW+2)

	var out []string
	for _, sec := range d.sections {
		if len(out) > 0 {
			out = append(out, "")
		}
		if sec.label != "" {
			out = append(out, sectionRule(st, sec.label, room))
		}
		for _, f := range sec.fields {
			label := padANSI(st.dimText.Render(truncate(f.label, labelW)), labelW) + "  "
			for i, text := range wrap(f.value, valueW) {
				if i > 0 {
					label = blank
				}
				out = append(out, label+f.tone.style(st).Render(truncate(text, valueW)))
			}
		}
		if len(sec.fields) > 0 && len(sec.lines) > 0 {
			// A sentence under a table is not another row of it.
			out = append(out, "")
		}
		for _, l := range sec.lines {
			// A bullet hangs its wrapped rows under the text rather than under
			// the mark, so a list of uses reads as a list however long each
			// one runs.
			lead, hang := "", ""
			if l.bullet {
				lead, hang = "· ", "  "
			}
			for i, text := range wrap(l.text, max(room-2, 8)) {
				if i > 0 {
					lead = hang
				}
				out = append(out, l.tone.style(st).Render(truncate(lead+text, room)))
			}
		}
	}
	return out
}

// sectionRule is a section's name on a rule that runs to the edge of the card.
func sectionRule(st *styles, label string, room int) string {
	head := "── " + label + " "
	if w := lipgloss.Width(head); w < room {
		return st.dimText.Render(head + strings.Repeat("─", room-w))
	}
	return st.dimText.Render(truncate(head, room))
}

// answerRule is the section rule for the question itself: the same rule, with
// the label in the window's accent, because it is the line the dialog is up to
// be read for.
func answerRule(st *styles, label string, room int) string {
	head := "── "
	tail := " "
	if w := lipgloss.Width(head + label + tail); w < room {
		tail += strings.Repeat("─", room-w)
	}
	return st.dimText.Render(head) + st.dialogTitle.Render(truncate(label, max(room-4, 4))) + st.dimText.Render(tail)
}

// viewItems draws the menu rows: the key that runs each one, the label, and
// what choosing it means.
//
// The key column is as wide as the widest key and no wider, and a menu whose
// rows have no keys — one choosing between secrets, or grants, or discoboxes —
// has no column at all rather than a ragged one full of IDs.
func (d *dialog) viewItems(st *styles, z *zones, top, inner int) string {
	pressW := 0
	for _, it := range d.items {
		pressW = max(pressW, lipgloss.Width(it.press))
	}
	keyCol := 0
	if pressW > 0 {
		keyCol = pressW + 2
	}
	// The label column fits the longest label rather than a fixed width:
	// action names are a word, but the folder dropdown's are paths, and a
	// path cut off at fourteen cells is not a path you can choose between.
	labelW := 14
	for _, it := range d.items {
		labelW = max(labelW, lipgloss.Width(it.label))
	}
	labelW = min(labelW, max(inner-keyCol-24, 14))

	var b strings.Builder
	for i, it := range d.items {
		// The row is a press, and it says so under the pointer: a menu that
		// only answered its letters is a menu the pointer cannot work, and one
		// that answered the pointer silently is one nobody tries twice. The
		// chevron stays the cursor's — what Enter runs — so the two never say
		// the same thing.
		z.markRow(hit{kind: hitDialogItem, idx: i}, top+i, inner+2*dialogPadLeft)
		bar, label := " ", st.dimText
		if i == d.cursor && it.enabled {
			bar, label = st.key.Render("❯"), st.cursorName
		} else if it.enabled && z.hovering(0, top+i, inner+2*dialogPadLeft, 1) {
			label = st.hover
		}
		key, detail := "", it.detail
		if pressW > 0 {
			key = padANSI(st.key.Render(it.press), pressW) + "  "
			if !it.enabled {
				key = padANSI(st.dimText.Render(it.press), pressW) + "  "
			}
		}
		if !it.enabled && it.why != "" {
			detail = it.why
		}
		row := bar + " " + key + label.Render(pad(it.label, labelW)) + "  " +
			st.dimText.Render(truncate(detail, max(inner-keyCol-labelW-4, 8)))
		b.WriteString(padANSI(row, inner))
		b.WriteString("\n")
	}
	return b.String()
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
func (d *dialog) viewSearch(st *styles, z *zones, inner int) string {
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
		found := st.key.Render("/") + d.query + st.dimText.Render("  "+d.tally()+"  ")
		return found + viewHints(st, z, []hint{
			pressing("n / N next, previous", "n"),
			pressing("/ search", "/"),
			pressing("Esc closes", "esc"),
		}, lipgloss.Width(found), hintSep)
	}
	line := []hint{pressing("Esc closes", "esc")}
	if d.overflow {
		line = []hint{pressing("/ search", "/"), pressing("Esc closes", "esc")}
	}
	return viewHints(st, z, line, 0, hintSep)
}

// tally is which match the body is on, of how many.
func (d *dialog) tally() string {
	if d.matches == 0 {
		return "no matches"
	}
	return itoa(d.match+1) + "/" + itoa(d.matches)
}
