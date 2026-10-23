package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The create card is what Enter in the prompt opens: where this discobox is
// cut from, and under that the few settings worth a second look before it is
// made — the harness, the server when there is more than one, and what is
// brought in beside the source.
//
// The list is the answer and the rows are the settings. The cursor opens in
// the list and Enter there creates, so a create that wants nothing changed is
// Enter twice. The settings open folded away, as one line saying what they are
// set to: most creates choose a source and nothing else, and a card that
// spells out five rows nobody changes is five rows read past every time. Tab
// unfolds them and is the only way down into them, which keeps ↑↓ on the one
// thing most creates choose between.
//
// The rows are the run options' own (optionSet), not copies: what is changed
// here is what the panel and the strip under the composer say afterwards, and
// a harness listing that lands while the card is up redraws its row. The
// source is the one thing that is not — the answer is where this discobox is
// cut from, and the Source row and the header stay where they were. See
// Model.askHowToCreate.
type createCard struct {
	opts *optionSet
	// prompt is what the composer held when the card opened, for the command
	// the card previews. Nothing can type into the composer under a dialog.
	prompt string
	// base is the request `discobox run` opened the window on, when the card
	// is asking where that run is cut from (WithRun). What the card has no row
	// for — the prompt's own tokens, --grant, --pool, --declared-sources — is
	// taken from it as given. Nil for a card the composer opened.
	base *RunRequest
	// cursor is the row of the list Enter creates from: a source, or the last
	// row, which is the way to name one.
	cursor int
	// row is the setting the keyboard is on, as an index into rows, or -1
	// while it is in the list.
	row int
	// offset is the first row of the list on screen, kept by the draw: only
	// it knows how many the window has room for.
	offset int
	// expanded is whether the settings have been unfolded. Once they have
	// they stay so for the card, the list included: folding them again under
	// a Tab back to the list would hide the row just changed.
	expanded bool
}

// createServerMsg says the card moved the Server row, which the header follows
// the way it follows the panel's (Model.followServer).
type createServerMsg struct{}

// createItemMsg asks for the field a repeated row's next value is typed into.
type createItemMsg struct {
	opt  *option
	card *createCard
}

// newCreateCard opens the card on where a create would be cut from with
// nothing said: the Source row's choice, which is the last card's answer until
// the header or the row is moved (optionSet.cutFrom).
func (o *optionSet) newCreateCard(prompt string, base *RunRequest) *createCard {
	c := &createCard{opts: o, prompt: prompt, base: base, row: -1}
	source := o.opts[optSource]
	c.cursor = source.idx
	if base != nil {
		// `discobox run` with no SOURCE was the directory it ran in before it
		// asked, and that stays the first answer whatever the last window was
		// left cutting from.
		if i := slices.Index(source.values, o.session.Directory); i >= 0 {
			c.cursor = i
		}
	}
	return c
}

// rows are the settings under the list, as indexes into the options: the
// harness first, since it is the one most often changed, then the server where
// there is one to choose, then what is brought in beside the source.
func (c *createCard) rows() []int {
	rows := []int{optHarness}
	if c.opts.server() != nil {
		rows = append(rows, optServer)
	}
	return append(rows, optProjectSkills, optUserSkills, optSkills, optInclude)
}

// current is the setting the keyboard is on, or nil while it is in the list.
func (c *createCard) current() *option {
	rows := c.rows()
	if c.row < 0 || c.row >= len(rows) {
		return nil
	}
	return c.opts.opts[rows[c.row]]
}

// sources is how many rows of the list are sources; the row after them is the
// way to name one the listing has never seen.
func (c *createCard) sources() int { return len(c.opts.opts[optSource].values) }

// source is the source under the cursor, and whether the cursor is on one
// rather than on the row that asks for it.
func (c *createCard) source() (string, bool) {
	values := c.opts.opts[optSource].values
	if c.cursor < 0 || c.cursor >= len(values) {
		return "", false
	}
	return values[c.cursor], true
}

// moveSource steps the list's cursor, stopping at either end.
func (c *createCard) moveSource(delta int) {
	c.cursor = min(max(c.cursor+delta, 0), c.sources())
}

// pressSource is a press on a row of the list: the cursor goes to it, and the
// keyboard with it.
func (c *createCard) pressSource(i int) {
	c.row = -1
	c.cursor = min(max(i, 0), c.sources())
}

// expand unfolds the settings, which is what Tab into them and a press on the
// folded line both mean.
func (c *createCard) expand() { c.expanded = true }

// pressRow is a press on a setting, by its option index.
func (c *createCard) pressRow(opt int) {
	if i := slices.Index(c.rows(), opt); i >= 0 {
		c.row = i
	}
}

// tab moves between the list and the settings, and goes round: the list, each
// setting in turn, the list again. The first Tab unfolds them.
func (c *createCard) tab(delta int) {
	c.expand()
	stops := len(c.rows()) + 1
	c.row = (c.row+1+delta+stops)%stops - 1
}

// create is the card's answer: the source under the cursor, or a request for
// the field one is typed into.
func (c *createCard) create() tea.Cmd {
	source, ok := c.source()
	if !ok {
		return func() tea.Msg { return sourceChosenMsg{enter: true, card: c} }
	}
	return func() tea.Msg { return sourceChosenMsg{source: source, card: c} }
}

// cycle steps the setting under the cursor.
func (c *createCard) cycle(delta int) tea.Cmd {
	opt := c.current()
	if opt == nil {
		return nil
	}
	opt.cycle(delta)
	if opt == c.opts.server() {
		return func() tea.Msg { return createServerMsg{} }
	}
	return nil
}

// update answers a key on the card, and reports whether it closed it.
//
// Enter creates from wherever the keyboard is, except on a repeated row, where
// it is the only key that can ask for another value. Tab is the way out of
// that row as it is the way into it.
func (c *createCard) update(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	key := keyName(msg)
	switch key {
	case "tab":
		c.tab(1)
		return nil, false
	case "shift+tab":
		c.tab(-1)
		return nil, false
	}

	opt := c.current()
	if opt == nil {
		switch key {
		case "up", "k":
			c.moveSource(-1)
		case "down", "j":
			c.moveSource(1)
		case "home", "g":
			c.cursor = 0
		case "end", "G":
			c.cursor = c.sources()
		case "e":
			c.cursor = c.sources()
			return c.create(), true
		case "enter":
			return c.create(), true
		default:
			// The first nine sources answer to their numbers, as they do on
			// the Source row's own list.
			for i := range min(c.sources(), 9) {
				if key == itoa(i+1) {
					c.cursor = i
					return c.create(), true
				}
			}
		}
		return nil, false
	}

	switch key {
	case "up", "k":
		// Up off the first setting is back in the list; nothing but Tab goes
		// the other way.
		c.row--
	case "down", "j":
		c.row = min(c.row+1, len(c.rows())-1)
	case "left", "h":
		return c.cycle(-1), false
	case "right", "l", " ":
		return c.cycle(1), false
	case "backspace", "-":
		if opt.kind == optMulti && len(opt.items) > 0 {
			opt.items = opt.items[:len(opt.items)-1]
		}
	case "enter":
		if opt.kind == optMulti {
			return func() tea.Msg { return createItemMsg{opt: opt, card: c} }, false
		}
		return c.create(), true
	}
	return nil, false
}

// hints is the card's key line: what the keys do where the keyboard is.
func (c *createCard) hints() []hint {
	opt := c.current()
	switch {
	case opt == nil:
		options := "Tab options"
		if !c.expanded {
			options = "Tab shows options"
		}
		return []hint{
			says("↑↓ moves"),
			pressing("Enter creates", "enter"),
			pressing(options, "tab"),
			pressing("Esc cancels", "esc"),
		}
	case opt.kind == optMulti:
		line := []hint{pressing("Enter adds one", "enter")}
		if len(opt.items) > 0 {
			line = append(line, pressing("⌫ removes the last", "backspace"))
		}
		return append(line, pressing("Tab next", "tab"), pressing("Esc cancels", "esc"))
	}
	return []hint{
		says("←→ changes"),
		pressing("Enter creates", "enter"),
		pressing("Tab next", "tab"),
		pressing("Esc cancels", "esc"),
	}
}

// display is how a setting reads on the card, which is the option's own words
// except for a harness nobody chose: "auto", and what that is where the
// project says. The panel names the default outright; the card is read at the
// moment of creating, where what matters is that nobody has to pick.
func (c *createCard) display(opt *option) string {
	if opt != c.opts.opts[optHarness] || opt.idx != 0 || len(opt.choices) == 0 {
		return opt.display()
	}
	if opt.choices[0] == unsetHarness {
		return "auto"
	}
	return "auto (" + opt.choices[0] + ")"
}

// hint is the line under the settings: what the row the keyboard is on sets,
// or, in the list, what the source under the cursor is.
func (c *createCard) hint() string {
	if opt := c.current(); opt != nil {
		return opt.hint
	}
	if _, ok := c.source(); !ok {
		return "SOURCE · a path, URL or owner/repo, with @REF and //PATH, the listing has never seen"
	}
	return "SOURCE · where the discobox is cut from"
}

// request is the create the card would make from the source under its cursor.
// The row that asks for a source has none yet, so it reads as the folder the
// field would offer.
func (c *createCard) request() RunRequest {
	source, ok := c.source()
	if !ok {
		source = c.opts.sourceDir()
	}
	return c.requestFrom(source)
}

// requestFrom is the create the card makes from one source: the run options
// as they stand, and from `discobox run`'s own request what the card has no
// row for.
func (c *createCard) requestFrom(source string) RunRequest {
	req := c.opts.request(c.prompt, source)
	if c.base != nil {
		req.Prompt = append([]string(nil), c.base.Prompt...)
		req.Grant, req.GrantStatement, req.GrantDefine, req.Pool = c.base.Grant, c.base.GrantStatement, c.base.GrantDefine, c.base.Pool
		req.SkipDeclaredSources = c.base.SkipDeclaredSources
	}
	return req
}

// view draws the list, the settings under it, and the command they add up to,
// in at most height lines. The list is what gives way on a short terminal: it
// is the part that can be any length, and it scrolls under its cursor.
//
// Folded, the settings are their rule and the strip under the composer, which
// says what they are set to — the harness always, and anything else that was
// chosen — and the command preview waits with them: what a create would run is
// a question about the settings, and asked before they are on screen.
func (c *createCard) view(st *styles, z *zones, top, inner, height int) string {
	source := c.opts.opts[optSource]
	rows := c.rows()
	command := wrap(c.opts.commandFor(c.request()), inner)
	summary := c.opts.mutedChips(st)

	// Everything but the list: its rule, the blank and the rule over the
	// settings, the settings, and the blank, the hint, the blank and the
	// command under them. The command is the first thing to go.
	fixed := 1 + 2 + len(rows) + 2
	preview := 2 + len(command)
	if !c.expanded {
		fixed, preview = 1+2, 0
		if summary != "" {
			fixed++
		}
	}
	if height-fixed-preview < min(c.sources()+1, 3) {
		preview = 0
	}
	room := max(height-fixed-preview, 3)

	type listRow struct{ key, label, detail string }
	list := make([]listRow, 0, c.sources()+1)
	for i, choice := range source.choices {
		key := " "
		if i < 9 {
			key = itoa(i + 1)
		}
		list = append(list, listRow{key: key, label: choice, detail: c.opts.sourceDetail(source.values[i])})
	}
	list = append(list, listRow{key: "e", label: enterSourceChoice})

	// One line goes to saying how much of the list is out of the window.
	if len(list) > room {
		room--
	}
	c.offset = min(c.offset, c.cursor)
	c.offset = max(c.offset, c.cursor-room+1)
	c.offset = min(max(c.offset, 0), max(len(list)-room, 0))
	end := min(c.offset+room, len(list))

	labelW := 14
	for _, row := range list {
		labelW = max(labelW, lipgloss.Width(row.label))
	}
	labelW = min(labelW, max(inner-28, 14))

	var b strings.Builder
	line := func() int { return top + strings.Count(b.String(), "\n") }

	b.WriteString(sectionRule(st, "Cut from", inner))
	b.WriteString("\n")
	for i := c.offset; i < end; i++ {
		row := list[i]
		y := line()
		z.markRow(hit{kind: hitCreateSource, idx: i}, y, inner+2*dialogPadLeft)
		bar, label := " ", st.dimText
		switch {
		case i == c.cursor && c.row < 0:
			bar, label = st.key.Render("❯"), st.cursorName
		case i == c.cursor:
			// The keyboard is down in the settings, and this is still what
			// Enter there creates from.
			bar, label = st.dimText.Render("❯"), st.name
		case z.hovering(0, y, inner+2*dialogPadLeft, 1):
			label = st.hover
		}
		detail := max(inner-labelW-8, 8)
		b.WriteString(padANSI(bar+" "+st.key.Render(row.key)+"  "+
			label.Render(pad(truncate(row.label, labelW), labelW))+"  "+
			st.dimText.Render(truncate(row.detail, detail)), inner))
		b.WriteString("\n")
	}
	if above, below := c.offset, len(list)-end; above > 0 || below > 0 {
		var more []string
		if above > 0 {
			more = append(more, "↑ "+itoa(above)+" more")
		}
		if below > 0 {
			more = append(more, "↓ "+itoa(below)+" more")
		}
		b.WriteString(st.dimText.Render("  " + strings.Join(more, " · ")))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if !c.expanded {
		// The folded line is a press that unfolds it, rule and summary both.
		height := 1
		if summary != "" {
			height = 2
		}
		y := line()
		z.mark(hit{kind: hitCreateExpand}, 0, y, inner+2*dialogPadLeft, height)
		rule := sectionRule(st, "Options · Tab to show", inner)
		if z.hovering(0, y, inner+2*dialogPadLeft, height) {
			rule = st.hover.Render(ansi.Strip(rule))
		}
		b.WriteString(rule)
		b.WriteString("\n")
		if summary != "" {
			b.WriteString("  ")
			b.WriteString(truncate(summary, inner-2))
			b.WriteString("\n")
		}
		return b.String()
	}
	b.WriteString(sectionRule(st, "Options · Tab", inner))
	b.WriteString("\n")
	for i, idx := range rows {
		opt := c.opts.opts[idx]
		b.WriteString(opt.viewRow(st, z, optionRow{
			value: c.display(opt), y: line(), inner: inner, cursor: i == c.row,
			row:   hit{kind: hitCreateRow, idx: idx},
			cycle: hit{kind: hitCreateCycle, idx: idx},
		}))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(st.dimText.Render(truncate(c.hint(), inner)))
	b.WriteString("\n")
	if preview > 0 {
		b.WriteString("\n")
		b.WriteString(st.dimText.Render("would run"))
		for _, l := range command {
			b.WriteString("\n")
			b.WriteString(st.command.Render(l))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// createDialog is the card as the dialog the window draws. A card that is
// `discobox run`'s question (base) is the window's whole purpose, so turning it
// down is the command ending with nothing made, however often the card has
// been rebuilt on the way back from a field it opened.
func (m *Model) createDialog(card *createCard) *dialog {
	d := &dialog{kind: dlgCreate, title: "New discobox", create: card}
	if card.base != nil {
		d.onCancel = func() tea.Cmd { return m.exit(errNothingCreated) }
	}
	return d
}
