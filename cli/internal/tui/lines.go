package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A list typed a line at a time — a rule's policy, its repositories. The card's
// field is one line and each entry is one, so the list is built above the
// field rather than typed into it. ↑ and ↓ walk the field through the lines
// written, editing each in place; Enter keeps the edit, and on a line edited
// to nothing removes it. Esc never changes a word: it goes back a step with
// the list as it stands, the field's half-typed text included.
//
// A list may open with picks above its lines: entries the console already
// knows of — the repositories of the project's discoboxes and its rules —
// each chosen or not with Space or Enter. The cursor walks the picks, then
// the lines, then the field's new line. An entry typed that is one of the
// picks is that pick chosen, never a second line.

// listLine is one line of a list: what is drawn and edited, and what is sent.
// They differ for a line the server holds whose text had to be made safe to
// draw: until the person changes it, what is sent is the server's own text,
// so an edit elsewhere in the list never rewrites it.
type listLine struct {
	shown, value string
}

// typedLines are lines a person wrote, sent as written.
func typedLines(texts []string) []listLine {
	out := make([]listLine, 0, len(texts))
	for _, text := range texts {
		out = append(out, listLine{shown: text, value: text})
	}
	return out
}

// storedLines are lines the server holds: drawn as shown, sent as stored. A
// stored list that does not line up with the drawn one is not trusted, and
// the drawn text is what is sent.
func storedLines(shown, stored []string) []listLine {
	if len(stored) != len(shown) {
		return typedLines(shown)
	}
	out := make([]listLine, 0, len(shown))
	for i := range shown {
		out = append(out, listLine{shown: shown[i], value: stored[i]})
	}
	return out
}

// lineList is a list being written, where the cursor is in it, and what the
// field holds.
type lineList struct {
	// picks are entries offered to choose, and chosen which are.
	picks  []listLine
	chosen []bool
	// identity is what makes an entry typed the same as a pick; nil matches
	// none.
	identity func(string) string

	lines []listLine
	// pick is the pick the cursor is on, or -1; at is the line the field is
	// editing, len(lines) being the field's own new line at the foot.
	pick, at int
	// text is what the field holds, and fresh what the new line held while
	// the cursor was elsewhere.
	text, fresh string
	// held is a list somebody has been in, which is opened as they left it
	// rather than started again from the rule.
	held bool
}

// newLineList opens a list on its lines, with the field on a new line.
func newLineList(lines []listLine) lineList {
	return lineList{lines: lines, pick: -1, at: len(lines), held: true}
}

// newPickList opens a list with picks above its lines: the picks whose value
// is among chosen start chosen, and each line that is one of the picks is
// taken as that pick rather than kept as a line.
func newPickList(picks []listLine, chosen []string, lines []listLine, identity func(string) string) lineList {
	l := newLineList(nil)
	l.picks, l.identity = picks, identity
	l.chosen = make([]bool, len(picks))
	for i, pick := range picks {
		l.chosen[i] = slices.Contains(chosen, pick.value)
	}
	for _, line := range lines {
		l = l.add(line)
	}
	l.at = len(l.lines)
	return l
}

// pickOf is the pick an entry is, or -1.
func (l lineList) pickOf(entry string) int {
	if l.identity == nil {
		return -1
	}
	id := l.identity(entry)
	for i, pick := range l.picks {
		if id != "" && l.identity(pick.value) == id {
			return i
		}
	}
	return -1
}

// add puts one entry at the foot of the lines, or chooses the pick it is.
func (l lineList) add(entry listLine) lineList {
	if i := l.pickOf(entry.value); i >= 0 {
		l.chosen = slices.Clone(l.chosen)
		l.chosen[i] = true
		return l
	}
	l.lines = append(slices.Clone(l.lines), entry)
	return l
}

// shown is the list as it is drawn, and values as it is sent: the picks
// chosen, in their order, then the lines.
func (l lineList) shown() []string {
	var out []string
	for i, pick := range l.picks {
		if l.chosen[i] {
			out = append(out, pick.shown)
		}
	}
	for _, line := range l.lines {
		out = append(out, line.shown)
	}
	return out
}

func (l lineList) values() []string {
	var out []string
	for i, pick := range l.picks {
		if l.chosen[i] {
			out = append(out, pick.value)
		}
	}
	for _, line := range l.lines {
		out = append(out, line.value)
	}
	return out
}

// editing is whether the field is up on a line already written.
func (l lineList) editing() bool { return l.pick < 0 && l.at < len(l.lines) }

// put writes what the field holds into the line it is on: in its place, as
// however many lines it splits into, any that are picks chosen instead. A
// line whose text is unchanged keeps what it sends.
func (l lineList) put(entries []string) lineList {
	if len(entries) == 1 && entries[0] == l.lines[l.at].shown {
		return l
	}
	before, after := slices.Clone(l.lines[:l.at]), slices.Clone(l.lines[l.at+1:])
	l.lines = before
	for _, entry := range typedLines(entries) {
		l = l.add(entry)
	}
	l.lines = append(l.lines, after...)
	return l
}

// moved is the list with the cursor gone one way: up from the first line —
// or from the field, with no lines — into the picks, and down off the last
// pick onto the lines.
func (l lineList) moved(delta int) lineList {
	if l.pick >= 0 {
		l.pick += delta
		switch {
		case l.pick < 0:
			l.pick = 0
		case l.pick >= len(l.picks):
			l.pick, l.at = -1, 0
		}
	} else {
		l.at += delta
		if l.at < 0 {
			l.at = 0
			if len(l.picks) > 0 {
				l.pick = len(l.picks) - 1
			}
		}
		l.at = min(l.at, len(l.lines))
	}
	l.text = l.fresh
	if l.editing() {
		l.text = l.lines[l.at].shown
	}
	return l
}

// left is the list as the field leaves where it is, keeping what was typed
// there: the new line keeps it as it is, and a line written takes it in its
// place. A line emptied is left as it was — removing one is Enter's, said on
// the key line, never a move's. On a pick the field holds nothing typed.
func (l lineList) left(ask linesAsk, typed string) lineList {
	switch trimmed := strings.TrimSpace(typed); {
	case l.pick >= 0:
	case !l.editing():
		l.fresh = typed
	case trimmed != "":
		l = l.put(ask.entries(trimmed))
		l.at = min(l.at, len(l.lines))
	}
	return l
}

// linesAsk is how one list is asked for.
type linesAsk struct {
	title, answerLabel, placeholder, footer string
	// sections are drawn above the list; written labels an empty one.
	sections []section
	written  string
	// split turns one typed line into its entries; nil keeps it whole.
	split func(string) []string
	// sectionsFor are drawn after sections, from the list as it stands: what
	// changes as it is written, such as whether it is still what a model
	// wrote.
	sectionsFor func(lineList) []section
	// required is said when the list is finished empty; empty lets it be.
	required string
	// numbered heads each line with its number: a list read as entries in
	// order.
	numbered bool
}

func (ask linesAsk) entries(typed string) []string {
	if ask.split != nil {
		return ask.split(typed)
	}
	return []string{typed}
}

// askLines puts the list up. On a pick, Space or Enter chooses it or clears
// it. On the new line, Enter adds what was typed, and Enter on nothing hands
// the list to done — refused, when it is required and nothing is chosen or
// written. On a line written, Enter keeps the edit, in that line's place, and
// Enter on a line emptied removes it; the field goes back to the new line
// either way. ↑ and ↓ move the cursor, keeping what was typed on the line it
// leaves. Esc hands back the list as it stands, the field's text included,
// which is how going back loses nothing. refused is what was wrong with the
// last answer.
func (m *Model) askLines(ask linesAsk, list lineList, refused string, done func(lineList) tea.Cmd, back func(lineList) tea.Cmd) tea.Cmd {
	if !list.held {
		list = newLineList(list.lines)
	}
	again := func(next lineList, refused string) tea.Cmd { return m.askLines(ask, next, refused, done, back) }
	d := inputDialog(ask.title, "", ask.placeholder, list.text, func(value string) tea.Cmd {
		next := list
		next.text = ""
		if next.editing() {
			if value == "" {
				next.lines = slices.Delete(slices.Clone(next.lines), next.at, next.at+1)
			} else {
				next = next.put(ask.entries(value))
			}
			next.at, next.text, next.fresh = len(next.lines), next.fresh, ""
			return again(next, "")
		}
		if value != "" {
			for _, entry := range typedLines(ask.entries(value)) {
				next = next.add(entry)
			}
			next.at = len(next.lines)
			return again(next, "")
		}
		if len(next.values()) == 0 && ask.required != "" {
			return again(next, ask.required)
		}
		return done(next)
	})
	d.keyFirst = func(msg tea.KeyPressMsg, typed string) (tea.Cmd, bool) {
		key := keyName(msg)
		if list.pick >= 0 {
			// On a pick the field takes nothing: what keys do there is
			// choose and move.
			switch key {
			case " ", "space", "enter":
				next := list
				next.chosen = slices.Clone(next.chosen)
				next.chosen[next.pick] = !next.chosen[next.pick]
				return again(next, ""), true
			case "up":
				return again(list.moved(-1), ""), true
			case "down":
				return again(list.moved(1), ""), true
			}
			return nil, true
		}
		var delta int
		switch key {
		case "up":
			delta = -1
		case "down":
			delta = 1
		default:
			return nil, false
		}
		return again(list.left(ask, typed).moved(delta), ""), true
	}
	d.cancelTyped = func(typed string) tea.Cmd {
		if back == nil {
			return nil
		}
		kept := list
		if kept.pick < 0 {
			kept.text = typed
		}
		return back(kept)
	}

	// The picks, the lines and the new line are the card's rows, drawn and
	// marked as a menu's are, so the pointer answers them as the keys do: a
	// press on a pick ticks or clears it, on a line takes it up to edit. The
	// line being edited is its own editor, in its place (dialog.editor): every
	// row of it wrapped and the cursor in it, so a sentence is never seen
	// twice or cut. Every line reads as text; only the chevron and the cursor
	// say which is being edited.
	for i, pick := range list.picks {
		box := "[ ] "
		if list.chosen[i] {
			box = "[x] "
		}
		d.items = append(d.items, action{key: fmt.Sprintf("pick:%d", i), label: box + pick.shown, enabled: true})
	}
	// A numbered list's lines are headed 1., 2., 3. — right-aligned, and the
	// new line the next — with a wrapped line set in under its text.
	number := func(n int) string {
		if !ask.numbered {
			return ""
		}
		return fmt.Sprintf("%*d.", len(strconv.Itoa(len(list.lines)+1)), n)
	}
	for i, entry := range list.lines {
		d.items = append(d.items, action{key: fmt.Sprintf("line:%d", i), press: number(i + 1), label: entry.shown, enabled: true})
	}
	// The new line: what was half-typed there, or nothing, which the editor
	// shows as its placeholder when it is up.
	d.items = append(d.items, action{key: "line:new", press: number(len(list.lines) + 1), label: list.fresh, enabled: true})
	newLine := len(d.items) - 1
	d.inlineRows, d.editAt = true, -1
	d.cursor = list.pick
	if list.pick < 0 {
		d.cursor = len(list.picks) + list.at
		d.editAt = d.cursor
		editor := rowEditor(m.st, list.text, ask.placeholder)
		d.editor = &editor
	}
	d.itemPress = func(idx int, typed string) tea.Cmd {
		next := list.left(ask, typed)
		switch line := idx - len(next.picks); {
		case idx < len(next.picks):
			next.chosen = slices.Clone(next.chosen)
			next.chosen[idx] = !next.chosen[idx]
			next.pick = idx
			next.text = next.fresh
		case idx == newLine:
			next.pick, next.at, next.text = -1, len(next.lines), next.fresh
		case line < len(next.lines):
			next.pick, next.at = -1, line
			next.text = next.lines[line].shown
		}
		return again(next, "")
	}

	d.wrapDetails = true
	d.sections = slices.Clone(ask.sections)
	if ask.sectionsFor != nil {
		d.sections = append(d.sections, ask.sectionsFor(list)...)
	}
	// A title that holds still: which row is being edited is the chevron's
	// to say, not a header that changes under it.
	d.answerLabel = ask.written
	d.footer = ask.footer
	// Esc is second on every line, after the one key the position needs
	// most: a key line is cut from its tail, and the way out of a card is
	// the one thing it must always say.
	keep := pressing("Esc back, kept", "esc")
	switch {
	case list.pick >= 0:
		d.pasteRefused = "a paste goes in a line: ↓ to one first"
		d.keys = []hint{pressing("Space ticks", " "), keep, pressing("↑↓ move", "up", "down")}
	case list.editing():
		d.keys = []hint{pressing("Enter keeps; emptied, removes", "enter"), keep, pressing("↑↓ line", "up", "down")}
	default:
		d.keys = []hint{pressing("Enter adds; empty, finishes", "enter"), keep}
		switch {
		case len(list.lines) > 0:
			d.keys = append(d.keys, pressing("↑ edit a line", "up"))
		case len(list.picks) > 0:
			d.keys = append(d.keys, pressing("↑ tick a known one", "up"))
		}
	}
	d.emphasis = refused
	m.dialog = d
	return nil
}

// bareEditor is a textarea as a list's rows use one: no prompt, no line
// numbers, no limit, as tall as its text wraps to. It is the one wrapping
// rule for a list's lines, edited (rowEditor) or not (editorLines).
func bareEditor() textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Prompt = ""
	ta.DynamicHeight = true
	ta.MinHeight = 1
	return ta
}

// editorLines is a line of a list as the row editor would wrap it at width:
// the editor's own wrapping — which keeps the last column of a row for the
// cursor — so selecting a line to edit it never moves where it breaks.
func editorLines(text string, width int) []string {
	if text == "" {
		return []string{""}
	}
	ta := bareEditor()
	ta.SetStyles(textarea.Styles{})
	// As tall as the line wraps to, with no limit: this measures, and never
	// scrolls.
	ta.MaxHeight = 0
	ta.SetWidth(width)
	ta.SetValue(text)
	rows := strings.Split(ansi.Strip(ta.View()), "\n")
	out := rows[:0]
	for _, row := range rows {
		out = append(out, strings.TrimRight(row, " "))
	}
	for len(out) > 1 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// rowEditor is the editor a list draws in place of the line being edited:
// the composer's look with no chevron of its own (the row's is the card's),
// wrapping its text across as many rows as it takes, with the cursor at the
// end of what is there. Enter, ↑ and ↓ are the list's, taken before it.
func rowEditor(st *styles, text, placeholder string) textarea.Model {
	ta := bareEditor()
	ta.Placeholder = placeholder
	ta.MaxHeight = editorMaxRows
	styles := textarea.Styles{}
	if st.color {
		styles = textarea.DefaultDarkStyles()
	}
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Focused.Text = st.name
	styles.Cursor.Blink = false
	ta.SetStyles(styles)
	ta.SetValue(text)
	ta.MoveToEnd()
	ta.Focus()
	return ta
}
