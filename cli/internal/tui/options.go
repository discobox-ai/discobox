package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// optionSet is the run options panel: the core's rows (console.RunOptions:
// every flag `discobox run` takes, what each offers and the request they add up
// to) and this window's cursor over them.
//
// The panel is a picker, not a form: left and right change a value in place,
// so the common case — swap the harness, turn dirty-carry off, cut from the
// repository next door — never opens a text field. Only the repeated flags
// (env, secrets) and a source the listing has never seen do.
type optionSet struct {
	*console.RunOptions
	cursor int
}

func newOptions(session console.Session) *optionSet {
	return &optionSet{RunOptions: console.NewRunOptions(session)}
}

// setPools takes the server's pools, and steps the cursor off the Pool row
// when there is no longer a pool to choose on it.
func (o *optionSet) setPools(pools []console.Pool) {
	o.SetPools(pools)
	if o.cursor == console.OptPool && !o.Shown(console.OptPool) {
		o.move(-1)
	}
}

// clearPools puts the Pool row back on the default, for a change of server.
func (o *optionSet) clearPools() {
	o.ClearPools()
	if o.cursor == console.OptPool && !o.Shown(console.OptPool) {
		o.move(-1)
	}
}

// optionsKey opens the run options panel and closes it. It is a window key
// rather than each screen's own — see updateKey — so the panel is one press
// away from wherever the window is, and one press from being put away again.
var optionsKey = bareKey("options")

// hint is what a row says under the rows: the core's words, and for the
// harness row this window's way to the harnesses screen, for the Source row
// the one thing about it that left and right cannot do.
func (o *optionSet) hint(opt *console.RunOption) string {
	if opt == o.Opts[console.OptSource] {
		return opt.Hint + " · Enter opens the whole list, and a path of your own"
	}
	if opt != o.Opts[console.OptHarness] {
		return opt.Hint
	}
	if !o.HarnessesListed() {
		return opt.Hint + " · " + HarnessesKeyName + " enables, disables and picks the default harness"
	}
	return opt.Hint + " · " + HarnessesKeyName + " manages them"
}

// sourceChosenMsg carries the dropdown's answer back to the live model, for the
// same reason filterChosenMsg does: the dialog closed over the model by value.
// enter is the row that is not an answer but a request for the input field.
type sourceChosenMsg struct {
	source string
	enter  bool
	// typed says the answer came from the input field rather than off the
	// list, so it names a place nothing has vouched for and is checked before
	// it is taken. See Model.resolveSource.
	typed bool
	// card is the create card the answer came off, so answering it carries
	// that create on rather than only setting the row. Nil is the Source row's
	// own list. See Model.askHowToCreate.
	card *createCard
}

// sourceDialog is the Source row opened out: every source the project has been
// cut from, "no source", and the way to name one the listing has never seen.
// The create card's list is the same choices (createCard.view): the answer
// that settles the row is the answer that settles a create, and offering a
// different set depending on which of the two asked would be two controls
// wearing one name.
func (o *optionSet) sourceDialog() *dialog {
	opt := o.Opts[console.OptSource]
	items := make([]action, 0, len(opt.Choices)+1)
	for i, choice := range opt.Choices {
		n := itoa(i + 1)
		items = append(items, action{
			// The key is the row's index, so the first nine choices can be
			// picked by number as well as by moving to them.
			key:     n,
			press:   n,
			label:   choice,
			detail:  o.SourceDetail(opt.Values[i]),
			enabled: true,
		})
	}
	items = append(items, action{key: "e", press: "e", label: console.EnterSourceChoice, enabled: true})
	menu := actionsDialog("Cut the discobox from", "", items, func(key string) tea.Cmd {
		if key == "e" {
			return func() tea.Msg { return sourceChosenMsg{enter: true} }
		}
		for i, value := range opt.Values {
			if itoa(i+1) == key {
				return func() tea.Msg { return sourceChosenMsg{source: value} }
			}
		}
		return nil
	})
	menu.cursor = opt.Index
	menu.keys = []hint{pressing("Enter cuts the next discobox from that", "enter"), pressing("Esc cancels", "esc")}
	return menu
}

func (o *optionSet) current() *console.RunOption { return o.Opts[o.cursor] }

func (o *optionSet) move(delta int) {
	o.cursor = (o.cursor + delta + len(o.Opts)) % len(o.Opts)
	step := sign(delta)
	if step == 0 {
		step = -1
	}
	for !o.Shown(o.cursor) {
		o.cursor = (o.cursor + step + len(o.Opts)) % len(o.Opts)
	}
}

// moveTo puts the cursor on one row, which is what a press on it means. It
// clamps rather than wrapping: a pointer names a row that is there. A row that
// is not drawn is stepped past in the direction the cursor was going.
func (o *optionSet) moveTo(i int) {
	i = min(max(i, 0), len(o.Opts)-1)
	if !o.Shown(i) {
		if i > o.cursor && i+1 < len(o.Opts) {
			i++
		} else {
			i--
		}
	}
	o.cursor = i
}

// chips is the mode line under the composer: the core's summary
// (RunOptions.Summary), lit while the composer has focus.
func (o *optionSet) chips(st *styles) string {
	return o.renderChips(st, true)
}

// mutedChips is the same resolved run summary after focus has left the
// composer. Every glyph uses the muted style so the line recedes as one unit.
func (o *optionSet) mutedChips(st *styles) string {
	return o.renderChips(st, false)
}

// renderChips draws the summary. The resolved project default stays muted; an
// explicit answer is gold while the composer has focus, and so is the marker
// once the harness is one.
func (o *optionSet) renderChips(st *styles, focused bool) string {
	summary := o.Summary()
	// Nothing chosen, nothing to say. The marker introduces the answers given,
	// so on its own it introduces nothing and is one more thing on screen that
	// never changes.
	if len(summary) == 0 {
		return ""
	}
	parts := make([]string, 0, len(summary))
	for _, chip := range summary {
		style := st.chip
		if focused && chip.Chosen {
			style = st.chipOn
		}
		parts = append(parts, style.Render(chip.Text))
	}
	marker, separator := st.chip, st.chip
	if focused && o.Opts[console.OptHarness].Changed() {
		marker = st.chipOn
	}
	return marker.Render("⏵⏵ ") + strings.Join(parts, separator.Render(" · "))
}

func (o *optionSet) view(st *styles, z *zones, width int, prompt string) string {
	// The same box the dialogs get: this panel stands in place of the window
	// exactly as they do, and two modal surfaces at two sizes read as two
	// different kinds of thing.
	boxWidth := dialogWidth(width)
	inner := max(boxWidth-dialogChromeWidth, 20)

	var b strings.Builder
	b.WriteString(st.dialogTitle.Render("New Discobox"))
	b.WriteString("\n")
	// The keys, as a key line like every other in the window: the panel is one
	// of the modal surfaces, and a surface where the offers were text and
	// nowhere else they were buttons would be the odd one out.
	z.push(dialogPadLeft, strings.Count(b.String(), "\n")+dialogPadTop)
	b.WriteString(viewHints(st, z, fitHints([]hint{
		says("← → change"),
		pressing("Enter opens the row", "enter"),
		pressing("Esc back to the prompt", "esc"),
	}, hintSep, inner), 0, hintSep))
	z.pop()
	b.WriteString("\n\n")

	// Where the first row lands inside the card: the title, the key line under
	// it, and the blank between them and the rows. Counted off the builder
	// rather than written out, so a line added above the rows moves the marks
	// with it.
	rowsTop := strings.Count(b.String(), "\n") + dialogPadTop

	y := rowsTop
	for i, opt := range o.Opts {
		if !o.Shown(i) {
			continue
		}
		b.WriteString(viewRow(opt, st, z, optionRow{
			value: opt.Display(), y: y, inner: inner, cursor: i == o.cursor,
			row:   hit{kind: hitOptionRow, idx: i},
			cycle: hit{kind: hitOptionCycle, idx: i},
		}))
		b.WriteString("\n")
		y++
	}

	b.WriteString("\n")
	b.WriteString(st.dimText.Render(truncate(o.hint(o.current()), inner)))
	b.WriteString("\n\n")
	b.WriteString(st.dimText.Render("would run"))
	b.WriteString("\n")
	for _, l := range wrap(o.Command(prompt), inner) {
		b.WriteString(st.command.Render(l))
		b.WriteString("\n")
	}

	return st.dialog.Width(boxWidth).Render(b.String())
}

// optionLabelW is the label column of an option row, in the panel and on the
// create card both.
const optionLabelW = 21

// optionRow is where one option row is drawn and what a press on it is: the
// panel and the create card draw the same rows, each with marks of its own.
type optionRow struct {
	// value is what the row reads as, which is the option's own display
	// except where the card says the same thing in other words.
	value string
	y     int
	inner int
	// cursor is whether the keyboard is on this row.
	cursor bool
	// row is the press on the row, and cycle the one on its arrows, which is
	// marked once for each direction.
	row, cycle hit
}

// viewRow draws one option row and marks it.
func viewRow(o *console.RunOption, st *styles, z *zones, at optionRow) string {
	// The row, and then the arrows over the top of it: they are drawn on
	// the cursor row alone, and each is a press that changes the value the
	// way the arrow key does.
	z.markRow(at.row, at.y, at.inner+2*dialogPadLeft)

	hovered := z.hovering(0, at.y, at.inner, 1)

	bar := " "
	label := st.dimText.Render(pad(o.Label, optionLabelW))
	switch {
	case at.cursor:
		bar = st.key.Render("❯")
		label = st.cursorName.Render(pad(o.Label, optionLabelW))
	case hovered:
		// Under the pointer: the label says so, and the chevron stays the
		// cursor's, exactly as it does on a menu's rows.
		label = st.hover.Render(pad(o.Label, optionLabelW))
	}
	value := at.value
	valueStyle := st.dimText
	switch {
	case o.Changed():
		valueStyle = st.chipOn
	case at.cursor:
		valueStyle = st.name
	}
	// A row whose value can be stepped keeps the two columns its arrows
	// need, whether or not they are lit: the arrows are the one affordance
	// the panel depends on, and a value that jumped two cells sideways as
	// the pointer crossed it would be a panel that fidgets. They light on
	// the cursor row, which is what says left and right will work, and on
	// the row under the pointer, which is what says a press will — without
	// that, changing a value with the mouse is click the row, watch
	// nothing happen, then click the arrow that has appeared.
	if o.Kind != console.OptionChoice && o.Kind != console.OptionToggle {
		value = valueStyle.Render(truncate(value, max(at.inner-optionLabelW-3, 6)))
		return padANSI(bar+" "+label+" "+value, at.inner)
	}
	left, right := st.dimText.Render("  "), st.dimText.Render("  ")
	if at.cursor || hovered {
		left, right = st.key.Render("‹ "), st.key.Render(" ›")
	}
	value = left + valueStyle.Render(truncate(value, max(at.inner-optionLabelW-7, 6))) + right
	// Both arrow columns are marked on every stepping row, lit or not: the
	// press that lights them is the same press that would use them, and a
	// target that only exists after the pointer has already stopped is one
	// the first click misses.
	valueX := dialogPadLeft + optionLabelW + 3
	back, forward := at.cycle, at.cycle
	back.delta, forward.delta = -1, 1
	z.mark(back, valueX, at.y, 2, 1)
	z.mark(forward, valueX+lipgloss.Width(value)-2, at.y, 2, 1)
	return padANSI(bar+" "+label+" "+value, at.inner)
}
