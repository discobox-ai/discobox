package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// poolsLoadedMsg is the configured server's pools, for the run options' Pool
// row.
type poolsLoadedMsg struct {
	// server is the server the listing was read from; see harnessesLoadedMsg.
	server string
	pools  []console.Pool
	err    error
}

// loadPools reads the configured server's pools. It is read at startup, on a
// change of server, and whenever the run options are opened: pools change
// rarely, and the panel is where a stale listing would be seen.
func (m *Model) loadPools() tea.Cmd {
	server := m.configServer()
	return func() tea.Msg {
		pools, err := m.ds.Pools(m.ctx, server)
		return poolsLoadedMsg{server: server, pools: pools, err: err}
	}
}

// poolsLoaded takes a listing. A refused one leaves the row on the default
// without a word: a caller the server will not show its pools to creates on
// the default all the same, and the strip saying so on every start would be a
// complaint about something nobody asked for.
func (m *Model) poolsLoaded(msg poolsLoadedMsg) {
	if m.session.ServerName(msg.server) != m.configServer() || msg.err != nil {
		return
	}
	m.pools = msg.pools
	m.opts.setPools(msg.pools)
	m.layout()
}
