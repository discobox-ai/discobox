package tui

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The form: one card that asks everything a decision needs, instead of a run of
// dialogs that ask one thing each.
//
// A binding is seven questions — what it is on, which harness or discobox, what
// it is for, the variable, the routes, the lifetime — and a sequence
// of modals is the wrong shape for that. Nothing can be revised
// without abandoning the run and starting again, the answers already given are
// off screen by the third question, and a person can only see the shape of what
// they are binding once it is bound.
//
// A form shows the whole decision at once, with rows appearing as the answers
// above them make them relevant: choosing "anything in the discobox" takes the
// variable and the use away, because a credential in the environment is not
// taken one use at a time.
//
// Rows are the same label/value column a card is read as, which is the point:
// the thing being built looks like the thing it will be when it is read back.

// formRow is one field of a form as it is being answered: the core's
// description of it (console.Field), and the state of answering it here — the
// option a picker is on, or the text typed so far.
type formRow struct {
	console.Field

	at    int
	input textinput.Model
}

// form is the rows and where the cursor is in them. What the rows ask, when
// each applies and whether the answers are accepted is the core's
// (console.Form); the form holds the answers and draws them.
type form struct {
	// listRoom bounds an open picker when its dialog must retain decisions. Zero is unbounded.
	listRoom     int
	rowRoom      int
	windowCursor int
	choiceRow    int
	rows         []formRow
	cursor       int
	// listing is a picker row opened as a list, every option on its own line
	// under the row, with listAt the one highlighted. Enter opens it, so the
	// options can be read together rather than met one at a time by ←→.
	listing bool
	listAt  int
	// err is the refusal from the last submit, shown against the row that
	// caused it until it is answered.
	err string
}

// newForm opens a form on the core's fields, each holding what it opens on.
func newForm(fields ...console.Field) *form {
	f := &form{rows: make([]formRow, len(fields))}
	for i, field := range fields {
		row := formRow{Field: field}
		if field.Picker() {
			row.at = max(slices.IndexFunc(field.Choices, func(c console.Choice) bool { return c.Key == field.Opens() }), 0)
			f.rows[i] = row
			continue
		}
		row.input = textinput.New()
		row.input.Placeholder = field.Placeholder
		row.input.SetValue(field.Value)
		// The label is the prompt here: a "› " in front of every value would
		// be a second column of punctuation down a card that is already a
		// column.
		row.input.Prompt = ""
		if field.Masked {
			row.input.EchoMode = 1 // textinput.EchoPassword
		}
		f.rows[i] = row
	}
	f.cursor = f.next(0, 1)
	f.focus()
	return f
}

// answers are what the rows hold, by key, as the core reads them.
func (f *form) answers() console.Answers {
	a := make(console.Answers, len(f.rows))
	for _, row := range f.rows {
		if row.Picker() {
			a[row.Key] = row.Choices[row.at].Key
			continue
		}
		a[row.Key] = row.input.Value()
	}
	return a
}

// answer is the core's verdict on what the form holds: q's answer, or false
// with the refusal on the form, against the row it names.
func answer[T any](f *form, q console.Form[T]) (T, bool) {
	v, refused := q.Answer(f.answers())
	if refused != nil {
		f.refuse(refused)
		return v, false
	}
	f.err = ""
	return v, true
}

// refuse puts a refusal on the form, and the cursor on the row it is about.
func (f *form) refuse(r *console.Refusal) {
	f.err = r.Why
	for i, row := range f.rows {
		if r.Field != "" && row.Key == r.Field && f.answerable(i) {
			f.cursor = i
			f.focus()
		}
	}
}

// answerable is whether a row takes an answer, given the answers above it. A
// row that does not is drawn all the same; see console.Field.When.
func (f *form) answerable(i int) bool {
	if i < 0 || i >= len(f.rows) {
		return false
	}
	return f.rows[i].Applies(f.answers())
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
		if f.rows[i].Picker() {
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

// moveTo puts the cursor on one row, which is what a press on it means. A row
// nobody may answer takes the next one that can be, the way moving onto it
// would.
func (f *form) moveTo(i int) {
	f.cursor = f.next(min(max(i, 0), len(f.rows)-1), 1)
	f.focus()
}

// value is what a typed row holds, trimmed.
func (f *form) value(key string) string {
	for _, row := range f.rows {
		if row.Key == key && !row.Picker() {
			return strings.TrimSpace(row.input.Value())
		}
	}
	return ""
}

// chosen is the key of a picker row's current option.
func (f *form) chosen(key string) string {
	for _, row := range f.rows {
		if row.Key == key && row.Picker() {
			return row.Choices[row.at].Key
		}
	}
	return ""
}

// onPicker is whether the cursor is on a row with options to choose between,
// where Enter lists them rather than accepting the form.
func (f *form) onPicker() bool {
	return len(f.rows) > 0 && len(f.rows[f.cursor].Choices) > 1
}

// openList opens the cursor's picker as a list, on the option it holds.
func (f *form) openList() {
	if !f.onPicker() {
		return
	}
	f.listing, f.listAt = true, f.rows[f.cursor].at
}

// closeList closes the list, leaving the option as it was.
func (f *form) closeList() { f.listing = false }

// updateList is a key while a picker is open as a list: ↑↓ walk it, Enter
// takes the highlighted option. Esc is the dialog's, which closes the list
// before it would close the card.
func (f *form) updateList(msg tea.KeyPressMsg) {
	row := &f.rows[f.cursor]
	switch keyName(msg) {
	case "up", "k", "shift+tab":
		f.listAt = max(f.listAt-1, 0)
	case "down", "j", "tab":
		f.listAt = min(f.listAt+1, len(row.Choices)-1)
	case "home":
		f.listAt = 0
	case "end":
		f.listAt = len(row.Choices) - 1
	case "enter", " ":
		f.pick(f.listAt)
	}
}

// pick takes an option off the open list, as ←→ would have landed on it.
func (f *form) pick(at int) {
	row := &f.rows[f.cursor]
	if at < 0 || at >= len(row.Choices) {
		return
	}
	f.listing = false
	row.at = at
	f.cursor = f.next(f.cursor, 1)
	f.focus()
}

// set replaces what a typed row holds.
func (f *form) set(key, value string) {
	for i := range f.rows {
		if f.rows[i].Key == key && !f.rows[i].Picker() {
			f.rows[i].input.SetValue(value)
		}
	}
}

// choose moves a picker row to the option with the given key.
func (f *form) choose(key, option string) {
	for i := range f.rows {
		if f.rows[i].Key != key {
			continue
		}
		for j, c := range f.rows[i].Choices {
			if c.Key == option {
				f.rows[i].at = j
			}
		}
	}
}

// chosenLabel is that option as it is said to a person, for the report after
// the form is answered.
func (f *form) chosenLabel(key string) string {
	for _, row := range f.rows {
		if row.Key == key && row.Picker() {
			return row.Choices[row.at].Label
		}
	}
	return ""
}

// cycle moves a picker row's option along, wrapping at either end.
func (f *form) cycle(delta int) {
	row := &f.rows[f.cursor]
	if !row.Picker() {
		return
	}
	row.at = ((row.at+delta)%len(row.Choices) + len(row.Choices)) % len(row.Choices)
	// A choice can take rows away or bring them back, and the cursor may be
	// standing on one that has just gone.
	f.cursor = f.next(f.cursor, 1)
	f.focus()
}

// update handles a key. A picker takes the arrows; every other key goes to the
// row being typed into, which is why the rows move on ↑↓ and Tab rather than on
// j and k.
func (f *form) update(msg tea.KeyPressMsg) tea.Cmd {
	if f.listing {
		f.updateList(msg)
		return nil
	}
	switch keyName(msg) {
	case "up", "shift+tab":
		f.move(-1)
		return nil
	case "down", "tab":
		f.move(1)
		return nil
	}
	row := &f.rows[f.cursor]
	if row.Picker() {
		switch keyName(msg) {
		case "left", "h":
			f.cycle(-1)
		case "right", "l", " ":
			f.cycle(1)
		case "enter":
			f.openList()
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
	if row.Picker() {
		return nil
	}
	var cmd tea.Cmd
	row.input, cmd = row.input.Update(msg)
	return cmd
}

// view draws the rows: the label column, the values beside it, and a rule
// wherever the section changes.
func (f *form) view(st *styles, z *zones, top, inner int) string {
	markStart := z.count()
	focusRow := 0
	labelW := 0
	for _, row := range f.rows {
		labelW = max(labelW, lipgloss.Width(row.Label))
	}
	labelW = min(labelW, max(inner/3, 8))
	valueW := max(inner-labelW-4, 8)

	draw := func(hoverOffset int) string {
		var b strings.Builder
		section := ""
		for i, row := range f.rows {
			if row.Section != section {
				section = row.Section
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				if section != "" {
					b.WriteString(sectionRule(st, section, inner))
					b.WriteString("\n")
				}
			}
			// A row is answered where it is drawn: the sections mean a row's place
			// on the card is not its place in the form, so the mark is made here,
			// against the line the builder is actually on. See zones.go.
			y := top + strings.Count(b.String(), "\n")
			if i == f.cursor || (f.cursor < 0 && i == f.windowCursor) {
				focusRow = y - top
			}
			z.markRow(hit{kind: hitFormRow, idx: i}, y, inner+2*dialogPadLeft)

			mark := "  "
			label := st.dimText
			switch {
			case i == f.cursor:
				mark, label = st.key.Render("❯")+" ", st.cursorName
			case f.answerable(i) && z.hovering(0, y-hoverOffset, inner+2*dialogPadLeft, 1):
				// Under the pointer, and answerable: a row you can press says so.
				label = st.hover
			}
			// A row that is typed into takes a caret from a press, so the value
			// column is marked as the field it is. A picker's ‹ › are the cursor
			// row's own keys and the row mark under them is enough.
			if f.answerable(i) && len(row.Choices) == 0 {
				z.mark(hit{kind: hitInput, idx: i}, dialogPadLeft+labelW+4, y, valueW, 1)
			}
			b.WriteString(padANSI(mark+padANSI(label.Render(truncate(row.Label, labelW)), labelW)+"  "+
				f.viewValue(st, i, valueW), inner))
			b.WriteString("\n")
			if f.listing && i == f.cursor {
				b.WriteString(f.viewList(st, z, top+strings.Count(b.String(), "\n"), labelW, inner))
				focusRow = f.choiceRow - top
			}
		}
		return b.String()
	}
	output := draw(0)
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if f.rowRoom > 0 && len(lines) > f.rowRoom {
		start := min(max(focusRow-f.rowRoom/2, 0), len(lines)-f.rowRoom)
		if start > 0 {
			// The first draw measured the viewport. Render hover against the same
			// translated rectangle that the retained text and mouse regions use.
			z.drop(markStart)
			output = draw(start)
			lines = strings.Split(strings.TrimSuffix(output, "\n"), "\n")
		}
		marks := append([]zone(nil), z.marks[markStart:]...)
		z.drop(markStart)
		for _, mark := range marks {
			if mark.y >= z.oy+top+start && mark.y < z.oy+top+start+f.rowRoom {
				mark.y -= start
				z.marks = append(z.marks, mark)
			}
		}
		return strings.Join(lines[start:start+f.rowRoom], "\n") + "\n"
	}
	return output
}

// viewList draws an open picker's options under it, in the value column: each
// with what it means beside it, the highlighted one marked. Each is a press.
func (f *form) viewList(st *styles, z *zones, top, labelW, inner int) string {
	row := f.rows[f.cursor]
	indent := strings.Repeat(" ", labelW+4)
	width := max(inner-len(indent)-2, 8)
	var b strings.Builder
	start, end := 0, len(row.Choices)
	if f.listRoom > 0 && end > f.listRoom {
		start = max(0, min(f.listAt-f.listRoom/2, end-f.listRoom))
		end = start + f.listRoom
	}
	f.choiceRow = top + f.listAt - start
	for j := start; j < end; j++ {
		c := row.Choices[j]
		mark, style := "  ", st.name
		if j == f.listAt {
			mark, style = st.key.Render("›")+" ", st.cursorName
		}
		text := style.Render(truncate(c.Label, width))
		if c.Hint != "" {
			if room := width - lipgloss.Width(c.Label) - 3; room > 8 {
				text += st.dimText.Render(" · " + truncate(c.Hint, room))
			}
		}
		z.markRow(hit{kind: hitFormChoice, idx: j}, top+j-start, inner+2*dialogPadLeft)
		b.WriteString(padANSI(indent+mark+text, inner))
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
		// under a binding to the project would state a fact the binding does not
		// carry. A locked row is the other way round — it exists to show what
		// the credential already is — so it draws its value, unless the value
		// is one nothing may draw.
		text := row.Why
		if row.Locked {
			switch {
			case row.Picker():
				text = row.Choices[row.at].Label
			case !row.Masked && strings.TrimSpace(row.input.Value()) != "":
				text = row.input.Value()
			}
		}
		return st.dimText.Render(truncate(text, valueW))
	}
	if row.Picker() {
		label := row.Choices[row.at].Label
		if len(row.Choices) == 1 {
			// Nothing to cycle through: it is a fact about the binding rather
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
	if row.Suffix != "" {
		unit = " " + row.Suffix
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
	hint := row.Hint
	// A picker's options each explain themselves, so the line follows the
	// option rather than the row: what "one harness" means is not what "every
	// discobox in this project" means.
	if row.Picker() && row.Choices[row.at].Hint != "" {
		hint = row.Choices[row.at].Hint
	}
	// An open list is read one option at a time: the line says the whole of
	// what the highlighted one means, which its row cuts short.
	if f.listing && row.Choices[f.listAt].Hint != "" {
		hint = row.Choices[f.listAt].Hint
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
