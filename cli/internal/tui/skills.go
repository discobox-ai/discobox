package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The skills screen: the skills every new discobox gets, wherever each is
// kept (ADR 26-10-14-894 §3). A Git repository is the project's, on its
// server, and every discobox of the project is created with it, whoever
// creates it. A directory is this machine's, in client.yaml's new.skills, and
// goes into every discobox created from here.
//
// It is the secrets screen's shape: a list on a function key, one letter per
// action, and every action the call `discobox skills` makes. What a value is
// decides where it goes, read the way --skills reads it, so there is one thing
// to type rather than a choice of list to put it in.

// skillsKey opens the screen, on the function key after the secrets.
const skillsKey = "f5"

// SkillsKeyName is how that key is spelled to the user.
const SkillsKeyName = "F5"

// skillsList is the screen's list: the project's skills sources in the order
// a create adds them, then client.yaml's, and where the cursor is.
type skillsList struct {
	all []SkillsEntry
	// file is the client.yaml the directories were read from, or where one
	// would be written; empty when none is read.
	file   string
	cursor int
	offset int
	loaded bool

	width, height int
	now           func() time.Time
	drawn         drawn
}

func newSkillsList() *skillsList { return &skillsList{now: time.Now} }

// setAll takes a refreshed listing, keeping the cursor on the entry it was on.
func (l *skillsList) setAll(listing SkillsListing) {
	l.loaded = true
	var on SkillsEntry
	if entry := l.current(); entry != nil {
		on = *entry
	}
	l.all, l.file = listing.Entries, listing.ClientFile
	for i, entry := range l.all {
		if entry == on {
			l.cursor = i
			break
		}
	}
	l.clamp()
}

func (l *skillsList) clear() {
	l.all, l.file, l.loaded, l.cursor, l.offset = nil, "", false, 0, 0
}

func (l *skillsList) current() *SkillsEntry {
	if l.cursor < 0 || l.cursor >= len(l.all) {
		return nil
	}
	return &l.all[l.cursor]
}

func (l *skillsList) move(delta int) {
	l.cursor += delta
	l.clamp()
}

func (l *skillsList) moveTo(i int) {
	l.cursor = i
	l.clamp()
}

func (l *skillsList) clamp() {
	l.cursor = min(l.cursor, len(l.all)-1)
	l.cursor = max(l.cursor, 0)
	if l.height <= 0 {
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+l.height {
		l.offset = l.cursor - l.height + 1
	}
	l.offset = max(l.offset, 0)
}

func (l *skillsList) counts() (project, client int) {
	for _, entry := range l.all {
		if entry.Project {
			project++
		} else {
			client++
		}
	}
	return project, client
}

func (l *skillsList) view(st *styles, z *zones) string {
	project, client := l.counts()
	right := itoa(project) + " project  ·  " + itoa(client) + " client.yaml"
	blank := strings.Repeat(" ", max(l.width, 0))
	out := []string{renderTitle(st.titleList, "Skills", right, l.width)}

	body := make([]string, 0, l.height)
	if len(l.all) == 0 && l.loaded {
		body = append(body, st.dimText.Render(pad("  no skills for new discoboxes — n adds a Git URL[@REF][//PATH] to the project, or a directory to client.yaml", l.width)))
	}
	l.drawn = drawn{top: len(out), first: l.offset}
	for i := l.offset; i < len(l.all) && len(body) < l.height; i++ {
		body = append(body, l.row(st, l.all[i], i))
		l.drawn.count++
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	block := append(append(out, body...), blank)
	z.markList(hitSkillsRow, l.drawn, l.width, len(block))
	return lipgloss.JoinVertical(lipgloss.Left, block...)
}

// row draws one entry: where it is kept, then what it names. The age is the
// project's alone, since client.yaml keeps no dates; it is the first thing a
// narrow terminal gives up.
func (l *skillsList) row(st *styles, entry SkillsEntry, i int) string {
	atCursor := i == l.cursor
	marker := "  "
	if atCursor {
		marker = st.key.Render("❯") + " "
	}
	where := st.dimText.Render(pad("client.yaml", 13))
	if entry.Project {
		where = st.chipOn.Render(pad("project", 13))
	}
	head := marker + where
	tail := ""
	if entry.Project && l.width-lipgloss.Width(head) > 40 {
		tail = st.dimText.Render(pad(ago(entry.Added, l.now()), 8))
	}
	valueW := max(l.width-lipgloss.Width(head)-lipgloss.Width(tail), 4)
	style := st.name
	if atCursor {
		style = st.cursorName
	}
	line := padANSI(head+padANSI(style.Render(truncateMiddle(entry.Value, valueW)), valueW)+tail, l.width)
	if atCursor {
		return highlight(st, line, colHighlightBG)
	}
	return line
}

// ---------------------------------------------------------------------------
// the screen

type skillsLoadedMsg struct {
	// server is the server the listing was read from; see harnessesLoadedMsg.
	server  string
	listing SkillsListing
	err     error
}

// skillsActionMsg carries a completed add or remove back into the update
// loop, so the listing is re-read from one place.
type skillsActionMsg struct {
	did string
	err error
}

func (m *Model) loadSkills() tea.Cmd {
	server := m.configServer()
	return func() tea.Msg {
		listing, err := m.ds.Skills(m.ctx, server)
		return skillsLoadedMsg{server: server, listing: listing, err: err}
	}
}

func (m *Model) openSkills() tea.Cmd {
	m.skillsOpen = true
	m.optionsOpen = false
	m.harnessesOpen = false
	m.secretsOpen = false
	m.layout()
	return m.loadSkills()
}

func (m *Model) closeSkills() {
	m.skillsOpen = false
	m.layout()
}

func (m *Model) skillsLoaded(msg skillsLoadedMsg) tea.Cmd {
	if m.serverName(msg.server) != m.configServer() {
		// Read for a server the header has since moved off; see
		// harnessesLoaded.
		return nil
	}
	if msg.err != nil {
		return m.report(true, "cannot read the skills: %v", msg.err)
	}
	m.skills.setAll(msg.listing)
	m.layout()
	return nil
}

// updateSkills handles the screen. A letter is a command here, as on the
// secrets screen.
func (m *Model) updateSkills(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "esc", "q", "tab":
		m.closeSkills()
	case "down", "j":
		m.skills.move(1)
	case "up", "k":
		m.skills.move(-1)
	case "pgdown":
		m.skills.move(m.skills.height)
	case "pgup":
		m.skills.move(-m.skills.height)
	case "home", "g":
		m.skills.moveTo(0)
	case "end", "G":
		m.skills.moveTo(len(m.skills.all) - 1)
	case "?":
		m.dialog = m.helpDialog()
	case "r":
		return tea.Batch(m.loadSkills(), status("refreshing"))
	case "left", "right":
		return m.cycleServer(serverStep(msg))
	case "n", "a":
		return m.addSkillsForm()
	case "d", "delete":
		return m.confirmRemoveSkills()
	}
	return nil
}

// addSkillsForm asks for one value and files it by what it is, as `discobox
// skills add` does.
func (m *Model) addSkillsForm() tea.Cmd {
	server := m.configServer()
	file := m.skills.file
	if file == "" {
		file = "client.yaml"
	}
	body := "A Git URL[@REF][//PATH] goes to the project: every discobox of it is created\n" +
		"with the skills in PATH linked from a checkout, whoever creates it.\n" +
		"A directory goes to " + file + ", for discoboxes created from here."
	m.dialog = inputDialog("Add skills", body, "https://github.com/acme/tools//agents/skills", "", func(value string) tea.Cmd {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		m.dialog = statusDialog("Skills", "adding "+value+"…")
		return func() tea.Msg {
			entry, err := m.ds.AddSkills(m.ctx, server, value)
			if err != nil {
				return skillsActionMsg{err: err}
			}
			return skillsActionMsg{did: "added " + entry.Value + " to " + entry.where()}
		}
	})
	return nil
}

func (m *Model) confirmRemoveSkills() tea.Cmd {
	entry := m.skills.current()
	if entry == nil {
		return nil
	}
	removed := *entry
	goes := section{label: "what goes", fields: []field{
		{label: "skills", value: removed.Value, tone: toneAccent},
		{label: "from", value: removed.where()},
	}}
	goes.lines = []line{{text: "discoboxes already created keep theirs; only new ones go without"}}
	server := m.configServer()
	d := confirmDialog("Remove skills", "", func(string) tea.Cmd {
		m.dialog = statusDialog("Skills", "removing "+removed.Value+"…")
		return func() tea.Msg {
			err := m.ds.RemoveSkills(m.ctx, server, removed)
			return skillsActionMsg{did: "removed " + removed.Value + " from " + removed.where(), err: err}
		}
	})
	d.sections = []section{goes}
	d.answerLabel = "remove " + removed.Value + "?"
	d.defaultNo = true
	m.dialog = d
	return nil
}

// where is where an entry is kept, as the screen says it.
func (e SkillsEntry) where() string {
	if e.Project {
		return "the project"
	}
	return "client.yaml"
}

// skillsActed closes the dialog, says what happened, and re-reads.
func (m *Model) skillsActed(msg skillsActionMsg) tea.Cmd {
	if msg.err != nil {
		m.dialog = errorDialog("Skills", fmt.Sprintf("%v", msg.err))
		return m.loadSkills()
	}
	m.dialog = nil
	return tea.Batch(m.loadSkills(), m.report(false, "%s", msg.did))
}

func (m *Model) pressSkillsRow(idx, _ int) tea.Cmd {
	if idx < 0 || idx >= len(m.skills.all) {
		return nil
	}
	m.skills.moveTo(idx)
	return nil
}

// ---------------------------------------------------------------------------
// view

// skillsChrome is what the screen costs in rows before an entry is drawn, as
// harnessesChrome counts it.
const skillsChrome = harnessesChrome

func (m *Model) viewSkills() string {
	m.zones.push(bodyLeft+m.logoColumn(), headerTop+2)
	body := m.skills.view(m.st, &m.zones)
	m.zones.pop()
	if m.showLogo() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(lipgloss.Height(body)), body)
	}

	m.zones.push(bodyLeft, headerTop)
	rows := []string{m.viewHeader(m.inner()), ""}
	m.zones.pop()
	rows = append(rows, strings.Split(body, "\n")...)

	m.zones.push(bodyLeft, headerTop+len(rows))
	rows = append(rows, strings.Split(m.viewStatus(), "\n")...)
	m.zones.pop()
	return m.box(rows)
}

func (m *Model) skillsHints() []hint {
	hints := []hint{says("↑↓ move"), keyed("n", "n", "add")}
	if m.skills.current() != nil {
		hints = append(hints, keyed("d", "d", "remove"))
	}
	if m.manyServers() {
		hints = append(hints, says("←→ server"))
	}
	return append(hints, keyed("esc", "esc", "back"))
}
