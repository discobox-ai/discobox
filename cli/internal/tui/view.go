package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The header's filters — server, folder, tag — are kept the way the draft is,
// one set on this machine whatever directory a window is opened in (ADR
// 26-10-20-596): a window opens on whatever the last one was left on, and on
// every server, every folder and every tag when nothing has been narrowed.
//
// Where they are kept is the DataSource's business (SaveView). The window's
// part is when, which is when the draft is written: on the listing's clock
// while it is open, and on the way out.

// restoreView puts the header back on what the last window left it on
// (console.Session.Restore). A server that is no longer registered is every
// server, since a filter naming nothing on the list would list nothing, and
// --server outranks what was saved. Whether the archived are listed is not
// saved, and stays as it is.
func (m *Model) restoreView(view console.ListView) {
	archived := m.list.Filter.Archived
	m.savedView, m.list.Filter = m.session.Restore(view)
	m.list.Filter.Archived = archived
}

// currentView is the header's filters as they stand (console.Filter.View).
func (m *Model) currentView() console.ListView {
	return m.list.Filter.View()
}

// saveView writes the filters if they have moved since the store last had
// them, and returns nil when they have not, so an idle window writes nothing.
func (m *Model) saveView() tea.Cmd {
	view, ok := m.viewToSave()
	if !ok {
		return nil
	}
	ds, ctx := m.ds, m.ctx
	return func() tea.Msg {
		if err := ds.SaveView(ctx, view); err != nil {
			return statusMsg{text: "cannot save the filters: " + err.Error(), err: true}
		}
		return nil
	}
}

// saveViewNow writes them on the way out, from the update loop, for the reason
// saveDraftNow does: a command batched with tea.Quit races the shutdown.
func (m *Model) saveViewNow() {
	view, ok := m.viewToSave()
	if !ok {
		return
	}
	_ = m.ds.SaveView(m.ctx, view)
}

// viewToSave is what a write would carry, and whether there is one to make. It
// is marked saved before it lands, as a draft is, so a failing write is
// reported once rather than on every tick.
func (m *Model) viewToSave() (view console.ListView, ok bool) {
	// A `discobox run` or `discobox attach` window never shows the list, so
	// nothing it holds is a choice somebody made; and before the session lands
	// the window does not have the view it opened on.
	if m.paletteReveal != nil || m.oneShot() || m.session.Directory == "" {
		return console.ListView{}, false
	}
	view = m.currentView()
	if view.Equal(m.savedView) {
		return console.ListView{}, false
	}
	m.savedView = view
	return view, true
}
