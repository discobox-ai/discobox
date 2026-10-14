package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"
)

// The server is the widest of the header filter's three (filter.go): it says
// which server's discoboxes are on screen, and — because the server you are
// looking at is the server you are about to create on — which server the
// prompt runs on. One control, not two that happen to agree, the same way the
// folder is both the list's filter and the source a create cuts from.
//
// It opens on every server, which is the listing ADR 0116 §4 describes: seeing
// several machines at once is the point of registering them, and a window that
// hid the others until asked would be the one-server-at-a-time client that ADR
// rejected. Narrowing to one is what the filter is for — a long list cut down
// to the machine being worked on, and a create that follows the eye.
//
// It is only offered when there is more than one server to choose (ADR 0116
// §5), which is the same condition the list groups under and the run options
// offer their Server row under: naming the one server there is says nothing.

// allServers is the choice that is not a server: every discobox the window can
// reach, in its sections. It leads the servers, where "all folders" trails the
// folders, for the one reason either order has — the choice the window opens
// on is the one to lead with, and this is where the window opens.
const allServers = "all servers"

// serverChoices are what ← → step through on the harnesses and secrets
// screens: every server the window lists, the primary first. Every server at
// once is not among them: what those screens show is one server's, and a
// harness enabled or a secret added has to go to one (ADR 0131 §2). Nil when
// there is only the primary.
func (m *Model) serverChoices() []string {
	if !m.manyServers() {
		return nil
	}
	return append([]string(nil), m.session.Servers...)
}

// onConfigScreen reports whether the harnesses, the secrets or the skills
// screen is the one the header is drawn over.
func (m *Model) onConfigScreen() bool { return m.harnessesOpen || m.secretsOpen || m.skillsOpen }

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
// the servers on the filter's card.
func (m *Model) manyServers() bool { return len(m.session.Servers) > 1 }

// cycleServer steps to the next or previous choice, which is what ← → do on
// the harnesses and secrets screens.
func (m *Model) cycleServer(delta int) tea.Cmd {
	choices := m.serverChoices()
	if len(choices) < 2 {
		return status("no other servers to show")
	}
	at := m.serverIndex(choices)
	return m.applyFilter(choices[(at+delta+len(choices))%len(choices)], m.list.folder, m.list.tags)
}

// serverIndex is where the server those screens show sits among the choices.
func (m *Model) serverIndex(choices []string) int {
	return max(slices.Index(choices, m.configServer()), 0)
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
	m.rules.clear()
	m.leaveEmptyRules()
	m.skills.clear()
	m.syncRequestRows()
	cmds := []tea.Cmd{m.loadHarnesses()}
	if m.secretsOpen {
		cmds = append(cmds, m.loadSecrets())
	}
	if m.skillsOpen {
		cmds = append(cmds, m.loadSkills())
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
