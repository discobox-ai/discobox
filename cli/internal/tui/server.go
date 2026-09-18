package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The server filter is the header's other place, beside the folder: it says
// which server's discoboxes are on screen, and — because the server you are
// looking at is the server you are about to create on — which server the
// prompt runs on. One control, not two that happen to agree, the same way the
// folder filter is both the list's filter and the source a create cuts from.
//
// It opens on every server, which is the listing ADR 0116 §4 describes: seeing
// several machines at once is the point of registering them, and a window that
// hid the others until asked would be the one-server-at-a-time client that ADR
// rejected. Narrowing to one is what the filter is for — a long list cut down
// to the machine being worked on, and a create that follows the eye.
//
// It is only there when there is more than one server to choose (ADR 0116 §5),
// which is the same condition the list groups under and the run options offer
// their Server row under: naming the one server there is says nothing.

// allServers is the choice that is not a server: every discobox the window can
// reach, in its sections. It leads the list, where "all folders" trails it, for
// the one reason either order has — the choice the window opens on is the one
// to lead with, and this is where the window opens.
const allServers = "all servers"

// serverChoices are what the dropdown offers: every server at once, which is
// the empty name, then every server the window lists, the primary first. Nil
// when there is only the primary, and then the header draws no filter at all.
//
// Over the harnesses and secrets screens every server at once is not offered:
// what they show is one server's, and a harness enabled or a secret added has
// to go to one (ADR 0131 §2).
func (m *Model) serverChoices() []string {
	if !m.manyServers() {
		return nil
	}
	if m.onConfigScreen() {
		return append([]string(nil), m.session.Servers...)
	}
	return append([]string{""}, m.session.Servers...)
}

// onConfigScreen reports whether the harnesses or the secrets screen is the
// one the header is drawn over.
func (m *Model) onConfigScreen() bool { return m.harnessesOpen || m.secretsOpen }

// configServer is the server the harnesses and secrets screens show and change,
// and the one whose harnesses a create is checked against (ADR 0131 §2): the
// server the header names, or the primary when it names every server. It is
// always the server the next create goes to, which is why one list of
// harnesses serves the screen, the run options and the questions a run asks.
// Empty with one server, where the data source needs no name.
func (m *Model) configServer() string { return m.serverName(m.list.server) }

// serverName is a server's name with the empty one — the primary, however it
// was written before the session named it — spelled out, so two names for the
// primary compare equal. A load that went before the session landed carries
// the empty name, and the model it lands in may since have learned the other.
func (m *Model) serverName(name string) string {
	if name == "" && len(m.session.Servers) > 0 {
		return m.session.Servers[0]
	}
	return name
}

// manyServers reports whether there is a server to choose, which is what puts
// the filter in the header.
func (m *Model) manyServers() bool { return len(m.session.Servers) > 1 }

// serverLabel is how the current filter reads in the header. A server wears
// the word the list's own section headers give it: a bare hostname beside a
// path is one more name on the row, and does not say what it names.
func (m *Model) serverLabel() string {
	if m.onConfigScreen() {
		return "server " + m.configServer()
	}
	if m.list.server == "" {
		return allServers
	}
	return "server " + m.list.server
}

// cycleServer steps to the next or previous choice.
func (m *Model) cycleServer(delta int) tea.Cmd {
	choices := m.serverChoices()
	if len(choices) < 2 {
		return status("no other servers to show")
	}
	at := m.serverIndex(choices)
	return m.selectServer(choices[(at+delta+len(choices))%len(choices)])
}

// serverIndex is where the current filter sits among the choices. Over the
// harnesses and secrets screens that is the server they show, which is the
// primary while the list shows every server.
func (m *Model) serverIndex(choices []string) int {
	current := m.list.server
	if m.onConfigScreen() {
		current = m.configServer()
	}
	for i, choice := range choices {
		if choice == current {
			return i
		}
	}
	return 0
}

// selectServer applies a choice from the dropdown.
//
// The cursor goes back to the top for the same reason it does on a folder: the
// rows under it are a different set of discoboxes now, and leaving it on row
// four of a list that has been replaced points it at something nobody chose.
func (m *Model) selectServer(choice string) tea.Cmd {
	was := m.configServer()
	m.list.server = choice
	// Where the window is listing from is where it creates. Every server at
	// once is no answer to that, so the create falls back to the primary,
	// which is where `discobox new` puts it with no --server at all
	// (ADR 0116 §5).
	m.opts.setServer(choice)
	m.list.resetCursor()
	m.layout()
	reload := m.configServerChanged(was)
	switch {
	case m.harnessesOpen:
		return tea.Batch(reload, status("harnesses on %s · new discoboxes go there", choice))
	case m.secretsOpen:
		return tea.Batch(reload, status("secrets on %s · new discoboxes go there", choice))
	case choice == "":
		return tea.Batch(reload, status("showing every server · new discoboxes go to %s", m.primaryServer()))
	}
	return tea.Batch(reload, status("showing %s · new discoboxes go there", choice))
}

// configServerChanged re-reads what is kept for the server the harnesses and
// secrets screens show, when a change of filter has moved it off was. The
// rows another server listed are dropped rather than left under the new
// server's name while its own are read: a secret listed under "server beta"
// that is alpha's is the one mistake this screen exists to prevent.
//
// The harnesses are read whichever screen is up, because they are also what
// a create is checked against and what the run options offer; the secrets
// only while their screen is, which is the only place they are drawn.
func (m *Model) configServerChanged(was string) tea.Cmd {
	if m.configServer() == was {
		return nil
	}
	m.harnesses.clear()
	m.secrets.clear()
	m.syncRequestRows()
	cmds := []tea.Cmd{m.loadHarnesses()}
	if m.secretsOpen {
		cmds = append(cmds, m.loadSecrets())
	}
	return tea.Batch(cmds...)
}

// primaryServer is the server a create goes to when the filter names none,
// which is what the window opens on. It is the session's first, which is the
// primary (ADR 0116 §3).
func (m *Model) primaryServer() string {
	if len(m.session.Servers) == 0 {
		return "this server"
	}
	return m.session.Servers[0]
}

// followServer points the list at the server the run options' Server row has
// moved to, so what is listed is where the next Enter will create. The header
// and the row are one control in both directions, the way the header's folder
// and the panel's source are (followSource).
func (m *Model) followServer() tea.Cmd {
	row := m.opts.server()
	if row == nil {
		return nil
	}
	// The row has no "every server" choice — a create goes to one server — so
	// following it always lands the list on one, wherever it was before.
	name := row.selected()
	var reload tea.Cmd
	if name != m.list.server {
		was := m.configServer()
		m.list.server = name
		m.list.resetCursor()
		m.layout()
		reload = m.configServerChanged(was)
	}
	return tea.Batch(reload, status("creating on %s · showing its discoboxes", name))
}

// serverStep is which way an arrow moves the server: the harnesses and secrets
// screens change it with the arrows the control itself answers to.
func serverStep(msg tea.KeyPressMsg) int {
	if keyName(msg) == "left" {
		return -1
	}
	return 1
}

// updateServer handles the header's server dropdown. Left and right change it
// in place and Enter opens the whole list, as on the folder filter. It is the
// top of the ladder focus climbs — prompt, discoboxes, folder, server — so Up
// stays put, Down steps back to the folder, and Tab goes round to the prompt.
func (m *Model) updateServer(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "left", "h":
		return m.cycleServer(-1)
	case "right", "l":
		return m.cycleServer(1)
	case "enter", " ":
		m.dialog = m.serverDialog()
		return nil
	case "down", "j":
		// Down is Up undone, one rung at a time: back to the folder, which is
		// where Up came from.
		m.focus = focusFolder
		return nil
	case "esc", "tab":
		m.backToPrompt()
		return nil
	case "up", "k":
		// The top of the ladder. Up has nowhere left to go, and jumping to the
		// prompt from here would be moving down the screen.
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

// serverDialog is the dropdown opened: every server in full, with what each
// one is worth knowing beside it — how many discoboxes it is holding, which of
// them is the primary, and which of them did not answer.
func (m *Model) serverDialog() *dialog {
	choices := m.serverChoices()
	items := make([]action, 0, len(choices))
	for i, choice := range choices {
		n := itoa(i + 1)
		label := choice
		if choice == "" {
			label = allServers
		}
		items = append(items, action{
			// The key is the row's index, so the first nine choices can be
			// picked by number as well as by moving to them.
			key:     n,
			press:   n,
			label:   label,
			detail:  m.serverDetail(choice),
			enabled: true,
		})
	}
	title, keys := "Show discoboxes on", "Enter shows that server's discoboxes"
	switch {
	case m.harnessesOpen:
		title, keys = "Show harnesses on", "Enter shows that server's harnesses"
	case m.secretsOpen:
		title, keys = "Show secrets on", "Enter shows that server's secrets"
	}
	menu := actionsDialog(title, "The window creates on the server it is showing.", items, func(key string) tea.Cmd {
		for i, choice := range choices {
			if itoa(i+1) == key {
				return func() tea.Msg { return serverChosenMsg{server: choice} }
			}
		}
		return nil
	})
	menu.cursor = m.serverIndex(choices)
	menu.keys = []hint{pressing(keys, "enter"), pressing("Esc cancels", "esc")}
	return menu
}

// serverChosenMsg carries the dropdown's answer back to the live model, for the
// same reason folderChosenMsg does: the dialog closed over the model by value.
type serverChosenMsg struct{ server string }

// serverDetail is what each choice is worth knowing: how many discoboxes it is
// holding, and whether it is the primary — the one a create goes to when the
// filter names none — or a server this listing never heard back from.
//
// The count is taken in the folder the header names, which stays where it is
// when a server is chosen, for the reason folderDetail's is taken on the server.
func (m *Model) serverDetail(choice string) string {
	n := 0
	for _, s := range m.list.all {
		if (choice == "" || s.Server == choice) && m.list.folder.holds(s, m.session) {
			n++
		}
	}
	if choice == "" {
		return plural(n, "box", "boxes") + " · created on " + m.primaryServer()
	}
	for _, name := range m.list.unreachable {
		if name == choice {
			return "not answering"
		}
	}
	detail := plural(n, "box", "boxes")
	if choice == m.primaryServer() {
		detail += " · the primary, where this window is pointed"
	}
	return detail
}
