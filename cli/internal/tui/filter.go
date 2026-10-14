package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The header's filter is one control: a line saying what the list is narrowed
// to, and one card that changes all of it. The server, the folder and the tags
// are one filter (sandboxList.nest), so they are chosen on one card rather
// than three dropdowns: each dropdown answered one question with the other two
// held still, where the card shows every choice at once, counted against
// the others as they are marked, and applies them together.

// filterPicker is the card's state: a copy of the list holding the choices
// marked so far. The copy is what the counts on the card are taken through,
// so every count says what that choice would list alongside the others
// marked, and what the folders and tags offered are, since those depend on
// the server and folder marked above them.
type filterPicker struct {
	list sandboxList
	// config is a card opened over the harnesses or secrets screens, which
	// are one server's (ADR 0131 §2): it offers the servers alone, and not
	// every server at once, which nothing can be added to.
	config bool
	cursor int
	// offset is the first line of the card's rows on screen, kept by the
	// draw: only it knows how many lines the window has room for.
	offset int
}

// filterRow is one choice on the card: the group it is in, what it reads as,
// what it is worth knowing, whether it is marked, and how marking it narrows
// the list. A row in a group that takes several marks at once — the tags —
// is several, and says how Space lets go of it unless, like `all tags`,
// marking another is the only way off it.
type filterRow struct {
	group   string
	label   string
	detail  string
	marked  bool
	several bool
	mark    func(l *sandboxList)
	unmark  func(l *sandboxList)
}

// filterDialog is the card, opened on the filter as it stands with the cursor
// on the first choice marked.
func (m *Model) filterDialog() *dialog {
	p := &filterPicker{list: *m.list, config: m.onConfigScreen()}
	for i, row := range p.rows() {
		if row.marked {
			p.cursor = i
			break
		}
	}
	title := "Show discoboxes"
	switch {
	case m.harnessesOpen:
		title = "Show harnesses on"
	case m.secretsOpen:
		title = "Show secrets on"
	case m.skillsOpen:
		title = "Show skills on"
	}
	return &dialog{kind: dlgFilter, title: title, filter: p}
}

// rows are the card's choices, group by group: the servers when there is more
// than one, the folders on the server marked, and the tags inside both once
// anything there is tagged. The servers and folders are one choice each; the
// tags are any number, with `all tags` the way to none.
func (p *filterPicker) rows() []filterRow {
	l := &p.list
	var rows []filterRow
	if servers := l.session.Servers; len(servers) > 1 {
		choices := servers
		if !p.config {
			choices = append([]string{""}, servers...)
		}
		for _, name := range choices {
			label := name
			if name == "" {
				label = allServers
			}
			rows = append(rows, filterRow{
				group: "Server", label: label, detail: p.serverDetail(name),
				marked: p.shows(name),
				mark:   func(l *sandboxList) { l.server = name },
			})
		}
	}
	if p.config {
		return rows
	}
	for _, f := range append(l.folders(), everyFolder) {
		rows = append(rows, filterRow{
			group: "Folder", label: f.label, detail: p.folderDetail(f),
			marked: l.folder.key == f.key,
			mark:   func(l *sandboxList) { l.folder = f },
		})
	}
	if tags := l.tagChoices(); len(tags) > 0 {
		rows = append(rows, filterRow{
			group: "Tag", label: allTags, detail: p.tagDetail(""),
			marked: len(l.tags) == 0, several: true,
			mark: func(l *sandboxList) { l.tags = nil },
		})
		for _, tag := range tags {
			rows = append(rows, filterRow{
				group: "Tag", label: tagLabel(tag), detail: p.tagDetail(tag),
				marked: slices.Contains(l.tags, tag), several: true,
				mark:   func(l *sandboxList) { l.addTag(tag) },
				unmark: func(l *sandboxList) { l.toggleTag(tag) },
			})
		}
	}
	return rows
}

// shows reports whether a server is the one marked. Over the harnesses and
// secrets screens every server at once shows the primary's, so the primary is
// marked there — without the list being moved onto it, which an Enter that
// changed nothing would otherwise do behind the screen.
func (p *filterPicker) shows(name string) bool {
	if p.config && p.list.server == "" {
		return name == p.list.session.Servers[0]
	}
	return p.list.server == name
}

// count is how many discoboxes the list would show with one more choice
// marked, counted the way the list counts its rows.
func (p *filterPicker) count(mark func(l *sandboxList)) int {
	l := p.list
	mark(&l)
	return l.count()
}

// serverDetail is what a server is worth knowing beside its count: which one
// is the primary — where a create goes when the filter names none — and which
// did not answer.
func (p *filterPicker) serverDetail(name string) string {
	if slices.Contains(p.list.unreachable, name) {
		return "not answering"
	}
	detail := plural(p.count(func(l *sandboxList) { l.server = name }), "box", "boxes")
	switch name {
	case "":
		detail += " · created on " + p.list.session.Servers[0]
	case p.list.session.Servers[0]:
		detail += " · the primary"
	}
	return detail
}

// folderDetail is a folder's count, and whether it is the one this window is
// running in.
func (p *filterPicker) folderDetail(f folder) string {
	detail := plural(p.count(func(l *sandboxList) { l.folder = f }), "box", "boxes")
	if f.key != "" && f.key == p.list.session.folder().key {
		detail += " · where this window is running"
	}
	return detail
}

// tagDetail is a tag's count alongside the tags marked, and for a key=value
// tag what the value is of. The empty tag is `all tags`, counted with none.
func (p *filterPicker) tagDetail(tag string) string {
	mark := func(l *sandboxList) { l.addTag(tag) }
	if tag == "" {
		mark = func(l *sandboxList) { l.tags = nil }
	}
	detail := plural(p.count(mark), "box", "boxes")
	if key, _, ok := strings.Cut(tag, "="); ok {
		detail += " · " + key + " set to this value"
	}
	return detail
}

// move steps the cursor, stopping at either end.
func (p *filterPicker) move(delta int) {
	p.cursor = min(max(p.cursor+delta, 0), len(p.rows())-1)
}

// toggle is Space or a press on the choice at i: in a group of several marks
// it lets go of a marked choice, and otherwise it marks it as pick does.
func (p *filterPicker) toggle(i int) {
	rows := p.rows()
	if i < 0 || i >= len(rows) {
		return
	}
	if rows[i].marked && rows[i].unmark != nil {
		p.cursor = i
		rows[i].unmark(&p.list)
		p.cursor = min(p.cursor, len(p.rows())-1)
		return
	}
	p.pick(i)
}

// pick marks the choice at i, in place of whatever its group had marked — or,
// among the tags, alongside it. The groups below it are drawn again from the
// new choice, so the cursor is held to the rows there are.
func (p *filterPicker) pick(i int) {
	rows := p.rows()
	if i < 0 || i >= len(rows) {
		return
	}
	p.cursor = i
	// A choice already marked stays as it is: over the config screens the
	// primary is marked while the list shows every server, and marking it
	// again would narrow the list to it behind the screen.
	if rows[i].marked {
		return
	}
	rows[i].mark(&p.list)
	p.cursor = min(p.cursor, len(p.rows())-1)
}

// chosen is the card's answer: the three filters as marked.
func (p *filterPicker) chosen() tea.Cmd {
	msg := filterChosenMsg{server: p.list.server, folder: p.list.folder, tags: p.list.tags}
	return func() tea.Msg { return msg }
}

// filterChosenMsg carries the card's answer back to the live model, for the
// same reason every other dialog does: it closed over the model by value.
type filterChosenMsg struct {
	server string
	folder folder
	tags   []string
}

// update answers a key on the card. Space marks the choice under the cursor —
// or, on a tag already marked, unmarks it — and Enter marks it too and applies
// everything marked: ↓ Enter changes one filter the way a list anywhere else
// in the window takes its highlighted row, and Space first changes several in
// one trip. Enter only ever marks, so a tag it is on is kept rather than
// dropped by the key that applies the card.
func (p *filterPicker) update(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch keyName(msg) {
	case "up", "k":
		p.move(-1)
	case "down", "j":
		p.move(1)
	case "home", "g":
		p.cursor = 0
	case "end", "G":
		p.move(len(p.rows()))
	case " ":
		p.toggle(p.cursor)
	case "enter":
		p.pick(p.cursor)
		return p.chosen(), true
	case "q":
		return nil, true
	}
	return nil, false
}

// view draws the groups, each under its own rule, with the mark beside each
// choice and the cursor's chevron beside the row Space would mark — in a
// window of at most height lines that keeps the cursor on screen. Every
// server, folder and tag on one card is taller than a short terminal, and a
// frame taller than the terminal scrolls it.
func (p *filterPicker) view(st *styles, z *zones, top, inner, height int) string {
	rows := p.rows()
	labelW := 14
	for _, row := range rows {
		labelW = max(labelW, lipgloss.Width(row.label))
	}
	labelW = min(labelW, max(inner-30, 14))

	// The card as lines first, each knowing the row it draws, so the window
	// can be cut from it without re-walking the groups.
	type cardLine struct {
		group string
		row   int
	}
	var lines []cardLine
	at := 0
	for i, row := range rows {
		if i == 0 || row.group != rows[i-1].group {
			if i > 0 {
				lines = append(lines, cardLine{row: -1})
			}
			lines = append(lines, cardLine{group: row.group, row: -1})
		}
		if i == p.cursor {
			at = len(lines)
		}
		lines = append(lines, cardLine{row: i})
	}

	// One line goes to saying how much is out of the window, when anything is.
	room := max(height, 3)
	if len(lines) > room {
		room--
	}
	// The cursor stays in the window, and so does its group's rule while the
	// cursor is on the group's first row: a choice with no group over it does
	// not say what it chooses.
	p.offset = min(p.offset, max(at-1, 0))
	p.offset = max(p.offset, at-room+1)
	p.offset = min(max(p.offset, 0), max(len(lines)-room, 0))
	end := min(p.offset+room, len(lines))

	var b strings.Builder
	for n, ln := range lines[p.offset:end] {
		line := top + n
		switch {
		case ln.group != "":
			b.WriteString(sectionRule(st, ln.group, inner))
		case ln.row < 0:
		default:
			i, row := ln.row, rows[ln.row]
			z.markRow(hit{kind: hitFilterRow, idx: i}, line, inner+2*dialogPadLeft)
			bar, label := " ", st.dimText
			switch {
			case i == p.cursor:
				bar, label = st.key.Render("❯"), st.cursorName
			case row.marked:
				label = lipgloss.NewStyle()
			case z.hovering(0, line, inner+2*dialogPadLeft, 1):
				label = st.hover
			}
			// A group of several marks wears boxes rather than dots, so the
			// card says which groups Space adds to and which it moves in.
			off, on := "○", "●"
			if row.several {
				off, on = "□", "■"
			}
			dot := st.dimText.Render(off)
			if row.marked {
				dot = st.key.Render(on)
			}
			room := max(inner-labelW-8, 8)
			b.WriteString(padANSI(bar+" "+dot+" "+label.Render(pad(truncate(row.label, labelW), labelW))+"  "+st.dimText.Render(truncate(row.detail, room)), inner))
		}
		b.WriteString("\n")
	}
	if above, below := p.offset, len(lines)-end; above > 0 || below > 0 {
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
	return b.String()
}

// filterLabel is how the filter reads in the header: what it narrows the list
// to, and nothing about what it leaves open — the list's own sections name
// those. Over the harnesses and secrets screens it is the server they show,
// and nothing with one server to show.
func (m *Model) filterLabel() string {
	if m.onConfigScreen() {
		if !m.manyServers() {
			return ""
		}
		return "server " + m.configServer()
	}
	var parts []string
	if m.list.server != "" && m.manyServers() {
		parts = append(parts, "server "+m.list.server)
	}
	if m.list.folder.key != "" {
		parts = append(parts, m.list.folder.label)
	}
	if len(m.list.tags) > 0 {
		parts = append(parts, tagsLabel(m.list.tags))
	}
	if len(parts) == 0 {
		return "all discoboxes"
	}
	return strings.Join(parts, " · ")
}

// viewFilter draws the header's filter: the line saying what the list is
// narrowed to, with a caret after it that says it opens.
func (m *Model) viewFilter(hovered bool) string {
	label := m.filterLabel()
	if label == "" {
		return ""
	}
	if m.focus == focusFilter && !m.onConfigScreen() {
		// The keyboard is already on it and it wears its own marks; the
		// pointer resting there has nothing left to say.
		return m.st.cursorName.Render(label) + m.st.key.Render(" ▾")
	}
	style := m.st.headerLabel
	if hovered {
		style = m.st.hover
	}
	return style.Render(label + " ▾")
}

// applyFilter narrows the list to what the card marked.
//
// The cursor goes back to the top: the rows underneath it are a different set
// of discoboxes now, and leaving it on row four of a list that has been
// replaced points it at something nobody chose.
func (m *Model) applyFilter(server string, f folder, tags []string) tea.Cmd {
	serverMoved, folderMoved := server != m.list.server, f.key != m.list.folder.key
	if !serverMoved && !folderMoved && slices.Equal(tags, m.list.tags) {
		return nil
	}
	was := m.configServer()
	m.list.server, m.list.folder, m.list.tags = server, f, tags
	// Where the window is listing from is where it creates from. Every server
	// at once is no answer to which server, so a create from there falls back
	// to the primary, which is where `discobox new` puts it with no --server
	// at all (ADR 0116 §5).
	if folderMoved {
		m.opts.setFolder(f.source)
	}
	if serverMoved {
		m.opts.setServer(server)
	}
	m.list.resetCursor()
	m.layout()
	reload := m.configServerChanged(was)
	switch {
	case !serverMoved:
		return tea.Batch(reload, status("showing %s", m.filterLabel()))
	case m.harnessesOpen:
		return tea.Batch(reload, status("harnesses on %s · new discoboxes go there", server))
	case m.secretsOpen:
		return tea.Batch(reload, status("secrets on %s · new discoboxes go there", server))
	case m.skillsOpen:
		return tea.Batch(reload, status("skills on %s · new discoboxes go there", server))
	case server == "":
		return tea.Batch(reload, status("showing %s · new discoboxes go to %s", m.filterLabel(), m.primaryServer()))
	}
	return tea.Batch(reload, status("showing %s · new discoboxes go to %s", m.filterLabel(), server))
}

// updateFilter handles the header's filter while the keyboard is on it. It is
// the top of the ladder focus climbs — prompt, discoboxes, filter — so Up stays
// put, Down steps back into the list, and Tab goes round to the prompt.
func (m *Model) updateFilter(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "enter", " ":
		m.dialog = m.filterDialog()
		return nil
	case "down", "j":
		// Down moves into the list, the way it does everywhere else in the
		// window: up and down cross between panes, and opening the card is
		// what Enter is for.
		if len(m.list.rows()) == 0 {
			// An empty list is nothing to move through, so Down carries on to
			// the prompt — which is where Down always ends up, and what the
			// empty list itself says to do.
			m.backToPrompt()
			return nil
		}
		m.focus = focusList
		m.list.moveTo(0)
		return nil
	case "esc", "tab":
		m.backToPrompt()
		return nil
	case "shift+tab":
		m.optionsOpen = true
		return nil
	case "f1", "?":
		m.dialog = m.helpDialog()
		return nil
	}
	return nil
}
