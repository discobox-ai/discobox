package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"

	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

const paletteKey = "k"

type openPaletteMsg struct{}

type palette struct {
	mouseIdentity string
	input         textinput.Model
	command       string
	box           *console.BoxRef
	selected      string
	offset        int
	archived      bool
	reason        string
	rows          []paletteRow
}

type paletteRow struct {
	identity string
	box      *console.Sandbox
	command  *consoleCommand
	why      string
	score    matchRank
}

func (m *Model) openPalette() tea.Cmd {
	if m.dialog != nil || m.optionsOpen || m.newOpen || m.focus == focusPrompt {
		return nil
	}
	m.recordConsoleUse("palette")
	ti := textinput.New()
	ti.Prompt = "› "
	ti.Placeholder = "Find a box or command"
	ti.CharLimit = 512
	inputStyles := ti.Styles()
	inputStyles.Cursor.Blink = false
	ti.SetStyles(inputStyles)
	ti.Focus()
	p := &palette{input: ti}
	m.palette = p
	m.disarmPaletteLeader()
	m.rebuildPalette(false)
	return nil
}

func (m *Model) disarmPaletteLeader() {
	m.leaderArmed = false
	if p := m.focusedPane(); p != nil {
		p.term.SetPrefixArmed(false)
	}
}

func (m *Model) closePalette() {
	m.palette = nil
	m.disarmPaletteLeader()
}

// matchRank carries the evidence used by the palette without changing the
// acceptance rules of the filters that also use fuzzyQuery.
type matchRank struct{ tier, gaps, start int }

func (a matchRank) compare(b matchRank) int {
	if n := cmp.Compare(a.tier, b.tier); n != 0 {
		return n
	}
	if n := cmp.Compare(a.gaps, b.gaps); n != 0 {
		return n
	}
	return cmp.Compare(a.start, b.start)
}
func paletteMatch(q fuzzyQuery, primary, detail []string) (matchRank, bool) {
	rank := matchRank{}
	for _, term := range q.terms {
		best := matchRank{tier: 99}
		for group, fields := range [][]string{primary, detail} {
			for _, field := range fields {
				hits := map[int]bool{}
				rr := []rune(field)
				if !q.matchTerm(rr, term, hits) {
					continue
				}
				lo, hi := len(rr), 0
				for i := range hits {
					lo = min(lo, i)
					hi = max(hi, i)
				}
				r := matchRank{tier: 3, gaps: hi - lo + 1 - len(term), start: lo}
				if r.gaps == 0 {
					r.tier = 2
					if lo == 0 {
						r.tier = 1
						if len(rr) == len(term) {
							r.tier = 0
						}
					}
				}
				if group == 1 {
					r.tier = 4
				}
				if r.compare(best) < 0 {
					best = r
				}
			}
		}
		if best.tier == 99 {
			return matchRank{}, false
		}
		rank.tier = max(rank.tier, best.tier)
		rank.gaps += best.gaps
		rank.start += best.start
	}
	if !q.empty() {
		var terms []string
		for _, t := range q.terms {
			terms = append(terms, string(t))
		}
		query := strings.Join(terms, " ")
		for _, field := range primary {
			same := field == query
			if !q.caseSensitive {
				same = strings.EqualFold(field, query)
			}
			if same {
				return matchRank{}, true
			}
		}
	}
	return rank, true
}

func (m *Model) boxRef(s console.Sandbox) console.BoxRef {
	return console.BoxRef{Server: m.session.ServerName(s.Server), ID: s.ID}
}
func (m *Model) paletteBox(ref console.BoxRef) *console.Sandbox {
	for _, s := range m.list.all {
		if m.boxRef(s) == ref {
			return &s
		}
	}
	return nil
}
func (m *Model) paletteFields(s console.Sandbox) ([]string, []string) {
	primary := []string{s.ID, strings.TrimPrefix(s.ID, "sbx_"), s.Name, s.ConfigName, s.GivenName}
	detail := []string{s.Description, s.Repository, s.RepositoryKey, s.Source, s.Branch, s.Git.Branch, s.Harness, s.HarnessName, s.Pool, s.PoolID, m.session.ServerName(s.Server), string(s.State)}
	for _, tag := range s.Tags {
		detail = append(detail, tag)
		detail = append(detail, strings.SplitN(tag, "=", 2)...)
	}
	return primary, detail
}

func (m *Model) rebuildPalette(reset bool) {
	p := m.palette
	if p == nil {
		return
	}
	old := p.selected
	if reset {
		p.selected = ""
		p.offset = 0
		p.reason = ""
	}
	q := parseFuzzy(p.input.Value())
	var rows []paletteRow
	if p.box == nil {
		for _, s := range m.list.all {
			if s.State == console.StateArchived && !p.archived {
				continue
			}
			primary, detail := m.paletteFields(s)
			score, ok := paletteMatch(q, primary, detail)
			if !ok {
				continue
			}
			why := ""
			if p.command != "" {
				for _, c := range m.consoleCommands(&s) {
					if c.id == p.command && !c.enabled {
						why = c.why
					}
				}
			}
			ref := m.boxRef(s)
			rows = append(rows, paletteRow{identity: "box:" + ref.Server + ":" + ref.ID, box: &s, score: score, why: why})
		}
	}
	if p.command == "" {
		var box *console.Sandbox
		if p.box != nil {
			box = m.paletteBox(*p.box)
		}
		for _, c := range m.consoleCommands(box) {
			if p.box != nil && !c.targeted {
				continue
			}
			score, ok := paletteMatch(q, append([]string{c.label}, c.aliases...), []string{c.detail})
			if !ok {
				continue
			}
			why := ""
			if !c.targeted && !c.enabled {
				why = c.why
			}
			if p.box != nil {
				if box == nil {
					why = "the box is no longer listed"
				} else if !c.enabled {
					why = c.why
				}
			}
			rows = append(rows, paletteRow{identity: "command:" + c.id, command: &c, score: score, why: why})
		}
	}
	eligible := slices.ContainsFunc(rows, func(r paletteRow) bool { return r.why == "" })
	if eligible {
		rows = slices.DeleteFunc(rows, func(r paletteRow) bool { return r.why != "" })
	}
	idQuery := strings.HasPrefix(p.input.Value(), "sbx_")
	if !q.empty() {
		for _, r := range rows {
			if r.box != nil && strings.Contains(strings.ToLower(r.box.ID), strings.ToLower(p.input.Value())) {
				idQuery = true
			}
		}
	}
	current := console.BoxRef{}
	if m.hasWorkspace() {
		current = m.boxRef(m.currentBox())
	} else if s := m.list.current(); s != nil {
		current = m.boxRef(*s)
	}
	slices.SortStableFunc(rows, func(a, b paletteRow) int {
		if q.empty() {
			priority := func(r paletteRow) int {
				if r.box != nil {
					ref := m.boxRef(*r.box)
					if ref == current {
						return 0
					}
					if m.paletteRecent[ref] > 0 {
						return 1
					}
					return 3
				}
				return 2
			}
			if n := cmp.Compare(priority(a), priority(b)); n != 0 {
				return n
			}
		} else {
			priority := func(r paletteRow) int {
				if idQuery {
					if r.box != nil {
						return 0
					}
					return 1
				}
				if r.command != nil && r.score.tier == 0 {
					return 0
				}
				return 1
			}
			if n := cmp.Compare(priority(a), priority(b)); n != 0 {
				return n
			}
			if n := a.score.compare(b.score); n != 0 {
				return n
			}
		}
		usage := func(r paletteRow) int {
			if r.box != nil {
				return m.paletteRecent[m.boxRef(*r.box)]
			}
			return m.paletteUsage[r.command.id]
		}
		if n := cmp.Compare(usage(b), usage(a)); n != 0 {
			return n
		}
		if q.empty() {
			return 0
		}
		label := func(r paletteRow) string {
			if r.box != nil {
				return displayName(*r.box)
			}
			return r.command.label
		}
		if n := strings.Compare(label(a), label(b)); n != 0 {
			return n
		}
		return strings.Compare(a.identity, b.identity)
	})
	p.rows = rows
	if !reset && old != "" {
		p.selected = old
		return
	}
	if len(rows) > 0 {
		p.selected = rows[0].identity
	}
}

func (m *Model) paletteMove(delta int) {
	p := m.palette
	m.rebuildPalette(false)
	at := slices.IndexFunc(p.rows, func(r paletteRow) bool { return r.identity == p.selected })
	if len(p.rows) > 0 {
		at = max(0, min(len(p.rows)-1, at+delta))
		p.selected = p.rows[at].identity
		p.reason = ""
	}
}
func (m *Model) paletteChip(c *consoleCommand, box *console.Sandbox) {
	p := m.palette
	if c != nil {
		p.command = c.id
		if c.id == "purge" || c.id == "unarchive" {
			p.archived = true
		}
	}
	if box != nil {
		ref := m.boxRef(*box)
		p.box = &ref
	}
	p.input.SetValue("")
	m.rebuildPalette(true)
}
func (m *Model) paletteChoose(complete bool) tea.Cmd {
	p := m.palette
	m.rebuildPalette(false)
	at := slices.IndexFunc(p.rows, func(r paletteRow) bool { return r.identity == p.selected })
	if at < 0 {
		p.reason = "the selected result is no longer available"
		return nil
	}
	r := p.rows[at]
	if r.why != "" {
		p.reason = r.why
		return nil
	}
	if p.command == "" && p.box == nil {
		if r.box != nil {
			if complete {
				m.paletteChip(nil, r.box)
				return nil
			}
			box := *r.box
			m.closePalette()
			return m.paletteNavigate(box)
		}
		if r.command.targeted {
			m.paletteChip(r.command, nil)
			return nil
		}
		if complete {
			p.input.SetValue(r.command.label)
			p.input.CursorEnd()
			m.rebuildPalette(true)
			return nil
		}
	}
	var box console.Sandbox
	var c *consoleCommand
	if r.box != nil {
		box = *r.box
		for _, cc := range m.consoleCommands(&box) {
			if cc.id == p.command {
				c = &cc
				break
			}
		}
	} else {
		c = r.command
		if p.box != nil {
			s := m.paletteBox(*p.box)
			if s == nil {
				p.reason = "the box is no longer listed"
				return nil
			}
			box = *s
		}
	}
	if c == nil {
		return nil
	}
	m.closePalette()
	return m.executeConsoleCommand(*c, box)
}
func (m *Model) paletteNavigate(box console.Sandbox) tea.Cmd {
	if m.oneShot() {
		return m.focusAddressed(box.Server, box.ID, box.ID)
	}
	m.revealPaletteBox(box)
	return m.landOn(box)
}
func (m *Model) paletteCompleteAlias() {
	p := m.palette
	if p.command != "" || p.box != nil {
		return
	}
	first, rest, ok := strings.Cut(p.input.Value(), " ")
	if !ok || strings.HasPrefix(first, "sbx_") {
		return
	}
	for _, c := range m.consoleCommands(nil) {
		if !c.targeted {
			continue
		}
		for _, a := range c.aliases {
			if !strings.Contains(a, " ") && strings.EqualFold(first, a) {
				m.paletteChip(&c, nil)
				p.input.SetValue(rest)
				p.input.CursorEnd()
				m.rebuildPalette(true)
				return
			}
		}
	}
}
func (m *Model) paletteKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.palette
	switch keyName(msg) {
	case "esc":
		if p.command != "" || p.box != nil {
			p.command = ""
			p.box = nil
		} else if p.input.Value() != "" {
			p.input.SetValue("")
		} else {
			m.closePalette()
			return nil
		}
		m.rebuildPalette(true)
	case "down", "ctrl+n":
		m.paletteMove(1)
	case "up", "ctrl+p", "shift+tab":
		m.paletteMove(-1)
	case "pgdown":
		m.paletteMove(max(1, min(12, m.height-10)))
	case "pgup":
		m.paletteMove(-max(1, min(12, m.height-10)))
	case "tab":
		if p.command != "" || p.box != nil {
			m.paletteMove(1)
		} else {
			return m.paletteChoose(true)
		}
	case "enter":
		return m.paletteChoose(false)
	case "alt+a":
		p.archived = !p.archived
		m.rebuildPalette(true)
	default:
		before := p.input.Value()
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(msg)
		if before != p.input.Value() {
			m.paletteCompleteAlias()
			m.rebuildPalette(true)
		}
		return cmd
	}
	return nil
}

func (m *Model) palettePaste(msg tea.PasteMsg) tea.Cmd {
	p := m.palette
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	m.paletteCompleteAlias()
	m.rebuildPalette(true)
	return cmd
}

// viewPalette builds the background first, then discards its hit map: a click
// behind the card only dismisses it. The trusted status row is covered too.
func (m *Model) viewPalette(background string) string {
	m.rebuildPalette(false)
	p := m.palette
	w, h := max(1, m.screenWidth), max(1, m.height)
	cw := max(1, min(w-4, 110))
	inner := max(1, cw-4)
	page := max(1, min(12, h-10))
	at := slices.IndexFunc(p.rows, func(r paletteRow) bool { return r.identity == p.selected })
	if at >= 0 {
		p.offset = max(0, min(p.offset, at))
		if at >= p.offset+page {
			p.offset = at - page + 1
		}
	}
	p.offset = min(p.offset, max(0, len(p.rows)-page))
	lines := []string{"Command palette"}
	chip := ""
	if p.command != "" {
		chip = p.command + " ▸ ×"
	}
	if p.box != nil {
		chip = p.box.ID + " ▸ ×"
	}
	if chip != "" {
		lines = append(lines, ansi.Truncate(chip, inner, "…"))
	}
	p.input.SetWidth(max(1, inner-ansi.StringWidth(p.input.Prompt)-1))
	lines = append(lines, p.input.View())
	scope := "All servers · all folders · Alt-A include archived"
	if p.archived {
		scope = "All servers · all folders · includes archived"
	}
	scope += fmt.Sprintf(" · %d results", len(p.rows))
	lines = append(lines, ansi.Truncate(scope, inner, "…"))
	top := len(lines)
	for i := p.offset; i < min(len(p.rows), p.offset+page); i++ {
		r := p.rows[i]
		var line string
		if r.box != nil {
			s := r.box
			statusTags := []string{}
			for _, tag := range s.RowTags() {
				if console.IsStatusTag(tag) {
					statusTags = append(statusTags, tag)
				}
			}
			line = strings.Join(statusTags, " ") + " " + fmt.Sprintf("□ %s  %s  %s  %s  %s", displayName(*s), shortID(s.ID), s.State, s.RepositoryKey, m.session.ServerName(s.Server))
		} else {
			c := r.command
			line = "› " + c.label + "  " + c.detail
		}
		if r.why != "" {
			line += " — " + r.why
		}
		line = strconv.QuoteToGraphic(line)
		line = line[1 : len(line)-1]
		room := inner
		if r.box != nil && p.command == "" && p.box == nil {
			room = max(1, inner-11)
		}
		if r.command != nil {
			binding := "[" + m.consoleCommandBinding(*r.command) + "]"
			binding = ansi.Truncate(binding, max(1, room-3), "…")
			textRoom := max(1, room-2-ansi.StringWidth(binding)-1)
			line = padANSI(ansi.Truncate(line, textRoom, "…"), textRoom) + " " + binding
		}
		marker := "  "
		if r.identity == p.selected {
			marker = "❯ "
		}
		line = marker + padANSI(ansi.Truncate(line, max(1, room-2), "…"), max(1, room-2))
		if r.box != nil && p.command == "" && p.box == nil {
			line += " [commands]"
		}
		if hits, ok := parseFuzzy(p.input.Value()).match(ansi.Strip(line)); ok {
			line = litRunes(ansi.Strip(line), hits, 0, m.st.dimText, m.st.emphasis)
		}
		if r.identity == p.selected {
			line = highlight(m.st, line, colHighlightBG)
		} else if r.why != "" {
			line = m.st.dimText.Render(line)
		}
		lines = append(lines, line)
	}
	if len(p.rows) == 0 {
		lines = append(lines, ansi.Truncate("No applicable matches for "+fmt.Sprintf("%q", p.input.Value()), inner, "…"))
	}
	if len(m.list.waiting) > 0 || len(m.list.unreachable) > 0 {
		lines = append(lines, ansi.Truncate("Partial results: "+strings.Join(append(slices.Clone(m.list.waiting), m.list.unreachable...), ", "), inner, "…"))
	}
	var stale []string
	for name := range m.list.listed {
		if age := m.list.listedAgo(name); age != "" {
			stale = append(stale, m.session.ServerName(name)+" "+age)
		}
	}
	slices.Sort(stale)
	if len(stale) > 0 {
		lines = append(lines, ansi.Truncate(strings.Join(stale, " · "), inner, "…"))
	}
	if p.reason != "" {
		lines = append(lines, ansi.Truncate(p.reason, inner, "…"))
	}
	footer := []hint{keyed("↑", "up", ""), keyed("↓", "down", ""), keyed("Tab", "tab", "compose"), keyed("Enter", "enter", "choose"), keyed("Esc", "esc", "back")}
	footer = fitHints(footer, " · ", inner)
	localZones := zones{}
	lines = append(lines, viewHints(m.st, &localZones, footer, 0, " · "))
	card := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(max(1, cw)).Render(strings.Join(lines, "\n"))
	x, y := max(0, (w-lipgloss.Width(card))/2), max(0, (h-lipgloss.Height(card))/2)
	m.zones.reset()
	m.zones.mark(hit{kind: hitPaletteCard}, x, y, lipgloss.Width(card), lipgloss.Height(card))
	if chip != "" {
		m.zones.mark(hit{kind: hitPaletteChip}, x+2, y+2, inner, 1)
	}
	for _, z := range localZones.marks {
		m.zones.mark(z.what, x+2+z.x, y+len(lines), z.width, z.height)
	}
	inputY := y + 2
	if chip != "" {
		inputY++
	}
	m.zones.mark(hit{kind: hitPaletteInput}, x+2, inputY, inner, 1)
	scopeY := y + 1 + top - 1
	m.zones.mark(keyHit("alt+a"), x+2, scopeY, inner, 1)
	for i := p.offset; i < min(len(p.rows), p.offset+page); i++ {
		r := p.rows[i]
		m.zones.mark(hit{kind: hitPaletteRow, idx: i, url: r.identity}, x+2, y+1+top+i-p.offset, inner, 1)
		if r.box != nil && p.command == "" && p.box == nil {
			m.zones.mark(hit{kind: hitPaletteCommands, url: r.identity}, x+2+max(1, inner-11), y+1+top+i-p.offset, min(11, inner), 1)
		}
	}
	bg := strings.Split(ansi.Strip(background), "\n")
	fg := strings.Split(card, "\n")
	var out []string
	for row := 0; row < h; row++ {
		if row == h-1 && m.bare() {
			m.zones.mark(keyHit("esc"), 0, h-1, w, 1)
			out = append(out, pad("discobox · Command palette · Esc back", w))
			continue
		}
		base := ""
		if row < len(bg) {
			base = bg[row]
		}
		base = ansi.Truncate(base, w, "")
		base += strings.Repeat(" ", max(0, w-ansi.StringWidth(base)))
		if row >= y && row < y+len(fg) {
			f := fg[row-y]
			left := ansi.Truncate(base, x, "")
			right := ansi.Cut(base, x+lipgloss.Width(f), w)
			out = append(out, m.st.greyedOut.Render(left)+f+m.st.greyedOut.Render(right))
		} else {
			out = append(out, m.st.greyedOut.Render(base))
		}
	}
	return strings.Join(out, "\n")
}

func (m *Model) paletteMouse(msg tea.MouseMsg) tea.Cmd {
	p := m.palette
	switch ev := msg.(type) {
	case tea.MouseWheelMsg:
		if ev.Button == tea.MouseWheelUp {
			m.paletteMove(-1)
		} else {
			m.paletteMove(1)
		}
	case tea.MouseClickMsg:
		if ev.Button != tea.MouseLeft {
			return nil
		}
		where, ok := m.zones.at(ev.X, ev.Y)
		if !ok {
			m.closePalette()
			return nil
		}
		switch where.what.kind {
		case hitPaletteChip:
			p.command = ""
			p.box = nil
			m.rebuildPalette(true)
		case hitPaletteCommands:
			p.selected = where.what.url
			return m.paletteChoose(true)
		case hitPaletteInput:
			p.input.SetCursor(max(0, ev.X-where.x-2))
		case hitPaletteRow:
			if where.what.idx >= len(p.rows) {
				return nil
			}
			same := p.mouseIdentity == where.what.url
			p.mouseIdentity = where.what.url
			p.selected = where.what.url
			if m.countClick(ev.X, ev.Y) >= 2 && same {
				return m.paletteChoose(false)
			}
		case hitKey:
			for _, key := range where.what.keys {
				if cmd := m.paletteKey(keyPress(key)); cmd != nil {
					return cmd
				}
				if m.palette == nil {
					return nil
				}
			}
		}
	}
	return nil
}

// paletteReveal is a navigation-only view. It never becomes a saved filter.
type paletteReveal struct {
	target   console.BoxRef
	server   string
	folder   console.Folder
	tags     []string
	archived bool
}

func (m *Model) revealPaletteBox(box console.Sandbox) {

	m.restorePaletteView()
	visible := slices.ContainsFunc(m.list.rows(), func(s console.Sandbox) bool { return m.boxRef(s) == m.boxRef(box) })
	if visible {
		return
	}
	m.paletteReveal = &paletteReveal{target: m.boxRef(box), server: m.list.server, folder: m.list.folder, tags: slices.Clone(m.list.tags), archived: m.list.showArchived}
	m.list.server = ""
	m.list.folder = everyFolder
	m.list.tags = nil
	m.list.showArchived = box.State == console.StateArchived

	m.status = "Showing the palette result outside your filters; the filters return when you leave"
	m.list.clamp()
}
func (m *Model) restorePaletteView() {
	r := m.paletteReveal
	if r == nil {
		return
	}

	m.paletteReveal = nil
	m.list.server, m.list.folder, m.list.tags, m.list.showArchived = r.server, r.folder, r.tags, r.archived
	m.list.clamp()
}

func (m *Model) recordPaletteVisit(box console.Sandbox) {
	if m.paletteRecent == nil {
		m.paletteRecent = map[console.BoxRef]int{}
	}
	m.paletteClock++
	m.paletteRecent[m.boxRef(box)] = m.paletteClock
	if len(m.paletteRecent) > 100 {
		var oldest console.BoxRef
		v := m.paletteClock
		for k, n := range m.paletteRecent {
			if n < v {
				oldest = k
				v = n
			}
		}
		delete(m.paletteRecent, oldest)
	}
}
