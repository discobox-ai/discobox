package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The form: one card that asks everything a decision needs, instead of a run of
// dialogs that ask one thing each.
//
// A pre-approval used to be seven questions in a row — scope, what it resolves
// against, how it may be used, the variable, the use, the host, the lifetime —
// and a sequence of modals is the wrong shape for that. Nothing can be revised
// without abandoning the run and starting again, the answers already given are
// off screen by the third question, and a person can only see the shape of what
// they are granting once it is granted.
//
// A form shows the whole decision at once, with rows appearing as the answers
// above them make them relevant: choosing "anything in the discobox" takes the
// variable and the use away, because a credential in the environment is not
// taken one use at a time.
//
// Rows are the same label/value column a card is read as, which is the point:
// the thing being built looks like the thing it will be when it is read back.

// choice is one option of a picker row.
type choice struct {
	key   string
	label string
	hint  string
}

// formRow is one line of a form: a label, and a value set either by typing or
// by choosing. choices make it a picker; without them it takes text.
type formRow struct {
	key   string
	label string
	// section is the rule this row sits under. A row repeating the section of
	// the row above it does not draw a second rule.
	section string
	hint    string

	choices []choice
	at      int

	input textinput.Model
	// suffix is the unit a typed value is in, drawn after it: "seconds".
	suffix string
	// masked hides what is typed, for a row that takes a credential.
	masked bool

	// when decides whether the row applies, out of the answers already given. A
	// nil when is a row that always applies.
	//
	// A row that does not apply is still drawn — dim, and stepped over — with
	// why in place of its value. Taking it off the card would make choosing an
	// OAuth credential produce four fields nobody knew were coming, and the
	// point of a form over a run of questions is being able to see what is
	// being asked before answering any of it.
	when func(f *form) bool
	// locked is a row this form never answers: what a credential already is, on
	// the card that edits the parts of it that can change.
	locked bool
	// why stands in for the value on a row that cannot be answered, saying
	// which it is: not applicable yet, or not editable at all.
	why string
	// required refuses an empty typed row on submit, saying why.
	required string
}

// form is the rows and where the cursor is in them.
type form struct {
	rows   []formRow
	cursor int
	// err is the refusal from the last submit, shown against the row that
	// caused it until it is answered.
	err string
}

func newForm(rows ...formRow) *form {
	f := &form{rows: rows}
	for i := range f.rows {
		if len(f.rows[i].choices) > 0 {
			continue
		}
		// The label is the prompt here: a "› " in front of every value would
		// be a second column of punctuation down a card that is already a
		// column.
		f.rows[i].input.Prompt = ""
		if f.rows[i].masked {
			f.rows[i].input.EchoMode = 1 // textinput.EchoPassword
		}
	}
	f.cursor = f.next(0, 1)
	f.focus()
	return f
}

// textRow is a row that takes typing, with what it starts as.
func textRow(key, label, placeholder, value string) formRow {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.SetValue(value)
	return formRow{key: key, label: label, input: ti}
}

// pickRow is a row that chooses between fixed options.
func pickRow(key, label string, choices ...choice) formRow {
	return formRow{key: key, label: label, choices: choices}
}

// answerable is whether a row takes an answer, given the answers above it. A
// row that does not is drawn all the same; see formRow.when.
func (f *form) answerable(i int) bool {
	if i < 0 || i >= len(f.rows) {
		return false
	}
	row := f.rows[i]
	if row.locked {
		return false
	}
	return row.when == nil || row.when(f)
}

// next is the row a move lands on, stepping over the ones that take no answer.
func (f *form) next(from, delta int) int {
	for i := from; i >= 0 && i < len(f.rows); i += delta {
		if f.answerable(i) {
			return i
		}
	}
	// Nothing that way: stay where the cursor already is, if that row still
	// takes an answer, and otherwise take the first that does.
	if f.answerable(f.cursor) {
		return f.cursor
	}
	for i := range f.rows {
		if f.answerable(i) {
			return i
		}
	}
	return 0
}

// focus puts the text cursor in the row under the form's cursor, and takes it
// out of every other. Only the focused row draws a caret.
func (f *form) focus() {
	for i := range f.rows {
		if len(f.rows[i].choices) > 0 {
			continue
		}
		if i == f.cursor {
			f.rows[i].input.Focus()
			continue
		}
		f.rows[i].input.Blur()
	}
}

func (f *form) move(delta int) {
	f.cursor = f.next(f.cursor+delta, delta)
	f.focus()
}

// value is what a typed row holds, trimmed.
func (f *form) value(key string) string {
	for _, row := range f.rows {
		if row.key == key {
			return strings.TrimSpace(row.input.Value())
		}
	}
	return ""
}

// chosen is the key of a picker row's current option.
func (f *form) chosen(key string) string {
	for _, row := range f.rows {
		if row.key == key && len(row.choices) > 0 {
			return row.choices[row.at].key
		}
	}
	return ""
}

// chosenLabel is that option as it is said to a person, for the report after
// the form is answered.
func (f *form) chosenLabel(key string) string {
	for _, row := range f.rows {
		if row.key == key && len(row.choices) > 0 {
			return row.choices[row.at].label
		}
	}
	return ""
}

// cycle moves a picker row's option along, wrapping at either end.
func (f *form) cycle(delta int) {
	row := &f.rows[f.cursor]
	if len(row.choices) == 0 {
		return
	}
	row.at = ((row.at+delta)%len(row.choices) + len(row.choices)) % len(row.choices)
	// A choice can take rows away or bring them back, and the cursor may be
	// standing on one that has just gone.
	f.cursor = f.next(f.cursor, 1)
	f.focus()
}

// submit checks the rows this form is asking and reports the first that is not
// answered, putting the cursor on it. Empty means the form is ready.
func (f *form) submit() string {
	for i, row := range f.rows {
		if !f.answerable(i) || row.required == "" || len(row.choices) > 0 {
			continue
		}
		if strings.TrimSpace(row.input.Value()) == "" {
			f.cursor, f.err = i, row.required
			f.focus()
			return row.required
		}
	}
	f.err = ""
	return ""
}

// update handles a key. A picker takes the arrows; every other key goes to the
// row being typed into, which is why the rows move on ↑↓ and Tab rather than on
// j and k.
func (f *form) update(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "up", "shift+tab":
		f.move(-1)
		return nil
	case "down", "tab":
		f.move(1)
		return nil
	}
	row := &f.rows[f.cursor]
	if len(row.choices) > 0 {
		switch keyName(msg) {
		case "left", "h":
			f.cycle(-1)
		case "right", "l", " ":
			f.cycle(1)
		}
		return nil
	}
	// Typing answers whatever the last refusal was about.
	f.err = ""
	var cmd tea.Cmd
	row.input, cmd = row.input.Update(msg)
	return cmd
}

func (f *form) paste(msg tea.PasteMsg) tea.Cmd {
	row := &f.rows[f.cursor]
	if len(row.choices) > 0 {
		return nil
	}
	var cmd tea.Cmd
	row.input, cmd = row.input.Update(msg)
	return cmd
}

// view draws the rows: the label column, the values beside it, and a rule
// wherever the section changes.
func (f *form) view(st *styles, inner int) string {
	labelW := 0
	for _, row := range f.rows {
		labelW = max(labelW, lipgloss.Width(row.label))
	}
	labelW = min(labelW, max(inner/3, 8))
	valueW := max(inner-labelW-4, 8)

	var b strings.Builder
	section := ""
	for i, row := range f.rows {
		if row.section != section {
			section = row.section
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			if section != "" {
				b.WriteString(sectionRule(st, section, inner))
				b.WriteString("\n")
			}
		}
		mark := "  "
		label := st.dimText
		if i == f.cursor {
			mark, label = st.key.Render("❯")+" ", st.cursorName
		}
		b.WriteString(padANSI(mark+padANSI(label.Render(truncate(row.label, labelW)), labelW)+"  "+
			f.viewValue(st, i, valueW), inner))
		b.WriteString("\n")
	}
	return b.String()
}

// viewValue is one row's answer: the option a picker is on, between the marks
// that say it can be changed, or the text being typed — and for a row that
// takes no answer, the value it already holds, or why it is not being asked.
func (f *form) viewValue(st *styles, i, valueW int) string {
	row := &f.rows[i]
	if !f.answerable(i) {
		// A row that does not apply says so, whatever it happens to hold: the
		// harness picker still knows which harness it was on, and drawing that
		// under a project-scoped grant would state a fact the grant does not
		// carry. A locked row is the other way round — it exists to show what
		// the credential already is — so it draws its value, unless the value
		// is one nothing may draw.
		text := row.why
		if row.locked {
			switch {
			case len(row.choices) > 0:
				text = row.choices[row.at].label
			case !row.masked && strings.TrimSpace(row.input.Value()) != "":
				text = row.input.Value()
			}
		}
		return st.dimText.Render(truncate(text, valueW))
	}
	if len(row.choices) > 0 {
		label := row.choices[row.at].label
		if len(row.choices) == 1 {
			// Nothing to cycle through: it is a fact about the grant rather
			// than a choice, and marks around it would offer a move that does
			// nothing.
			return st.name.Render(truncate(label, valueW))
		}
		style := st.name
		if i == f.cursor {
			style = st.dialogTitle
		}
		return st.dimText.Render("‹ ") + style.Render(truncate(label, max(valueW-4, 4))) + st.dimText.Render(" ›")
	}
	// The unit rides after the value, so the field is only as wide as what is
	// left once it has its cell.
	unit := ""
	if row.suffix != "" {
		unit = " " + row.suffix
	}
	row.input.SetWidth(max(valueW-lipgloss.Width(unit)-1, 8))
	value := row.input.View()
	if i != f.cursor && strings.TrimSpace(row.input.Value()) == "" {
		// A row nobody is typing into says what it wants rather than sitting
		// blank: a blank row reads as a field that has been answered emptily.
		// It says it as a description or as "e.g. …", never as a bare example —
		// a dim "github" beside "name" on a card whose dim rows are values a
		// credential already has reads as a name that was filled in.
		value = padANSI(st.dimText.Render(truncate(row.input.Placeholder, valueW)), lipgloss.Width(value))
	}
	return value + st.dimText.Render(unit)
}

// hintRows is the space kept under a form for the line that explains the row
// under the cursor, whatever that line turns out to say.
//
// It is a fixed two rows rather than as many as the sentence needs. The card is
// drawn from its content, so a hint that wrapped to three rows on one row of
// the form and one on the next made the whole window taller and shorter as the
// cursor moved down it — every row under the cursor shifting while somebody is
// reading them, on a card whose whole point is that it holds still.
const hintRows = 2

// hint is the line under the form: why the row under the cursor is being asked,
// or what the last submit refused. One sentence at a time, about the row being
// answered, is what a run of dialogs was spending a card each on.
//
// It always draws hintRows rows: blank ones when it has nothing to say, and a
// sentence cut with an ellipsis when it has more than fits.
func (f *form) hint(st *styles, inner int) string {
	style := st.dimText
	row := f.rows[f.cursor]
	hint := row.hint
	// A picker's options each explain themselves, so the line follows the
	// option rather than the row: what "one harness" means is not what "every
	// discobox in this project" means.
	if len(row.choices) > 0 && row.choices[row.at].hint != "" {
		hint = row.choices[row.at].hint
	}
	if f.err != "" {
		// The refusal replaces the explanation: the row is being told why it
		// was not accepted, which is the more urgent of the two things that
		// line can say.
		style, hint = st.statusER, f.err
	}

	lines := wrap(hint, inner)
	if len(lines) > hintRows {
		// What did not fit is folded back onto the last row it has and cut
		// there, so the sentence ends in an ellipsis rather than mid-word on a
		// row that is not drawn.
		tail := strings.Join(lines[hintRows-1:], " ")
		lines = append(lines[:hintRows-1], truncate(tail, inner))
	}
	for len(lines) < hintRows {
		lines = append(lines, "")
	}
	for i, text := range lines {
		lines[i] = style.Render(truncate(text, inner))
	}
	return strings.Join(lines, "\n")
}
