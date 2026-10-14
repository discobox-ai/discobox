package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The header's filters — server, folder, tag — are kept per folder, the way
// the draft is: a window opens on every server, every folder and every tag in
// a directory it has never been narrowed in, and on whatever it was left on in
// one it has. The folder is the session's own directory, so narrowing the list
// in one checkout says nothing about the next.
//
// Where they are kept is the DataSource's business (SaveView). The window's
// part is when, which is when the draft is written: on the listing's clock
// while it is open, and on the way out.

// restoreView puts the header back on what the last window in this folder left
// it on. A server that is no longer registered is every server, since a filter
// naming nothing on the list would list nothing.
//
// With nothing saved, the default is everything, narrowed by what the command
// line named: --server is the primary, and -C is the window's own folder. That
// narrowing is where the window opens, not a choice made in it, so it is what
// the store is taken to have — it is saved only once the filters move off it.
func (m *Model) restoreView(view ListView) {
	if view.IsZero() {
		if m.session.SourceChosen {
			own := m.session.folder()
			view.FolderKey, view.FolderLabel, view.FolderSource, view.FolderLocal = own.key, own.label, own.source, own.local
		}
		if m.session.ServerChosen && len(m.session.Servers) > 0 {
			view.Server = m.session.Servers[0]
		}
	}
	m.savedView = view
	// The window's own folder is the session's, not the copy saved: its label
	// carries the branch checked out now, and its source the ref -C names now,
	// neither of which is part of the key the view was saved under.
	switch view.FolderKey {
	case "":
		m.list.folder = everyFolder
	case m.session.OriginKey, m.session.folder().key:
		// Its origin key is what a view saved before the window's folder was
		// its repository names it by.
		m.list.folder = m.session.folder()
	default:
		m.list.folder = folder{key: view.FolderKey, label: view.FolderLabel, source: view.FolderSource, local: view.FolderLocal}
	}
	m.list.server = ""
	if view.Server != "" && m.manyServers() && m.knownServer(view.Server) {
		m.list.server = view.Server
	}
	m.list.tags = view.Tags
}

// knownServer reports whether name is among the servers the window lists.
func (m *Model) knownServer(name string) bool {
	for _, s := range m.session.Servers {
		if s == name {
			return true
		}
	}
	return false
}

// currentView is the header's filters as they stand. Every folder is no
// folder at all, so its label is not carried: the zero view is the default.
func (m *Model) currentView() ListView {
	view := ListView{Server: m.list.server, Tags: m.list.tags}
	if f := m.list.folder; f.key != "" {
		view.FolderKey, view.FolderLabel, view.FolderSource, view.FolderLocal = f.key, f.label, f.source, f.local
	}
	return view
}

// saveView writes the filters if they have moved since the store last had
// them, and returns nil when they have not, so an idle window writes nothing.
func (m *Model) saveView() tea.Cmd {
	folder, view, ok := m.viewToSave()
	if !ok {
		return nil
	}
	ds, ctx := m.ds, m.ctx
	return func() tea.Msg {
		if err := ds.SaveView(ctx, folder, view); err != nil {
			return statusMsg{text: "cannot save the filters: " + err.Error(), err: true}
		}
		return nil
	}
}

// saveViewNow writes them on the way out, from the update loop, for the reason
// saveDraftNow does: a command batched with tea.Quit races the shutdown.
func (m *Model) saveViewNow() {
	folder, view, ok := m.viewToSave()
	if !ok {
		return
	}
	_ = m.ds.SaveView(m.ctx, folder, view)
}

// viewToSave is what a write would carry, and whether there is one to make. It
// is marked saved before it lands, as a draft is, so a failing write is
// reported once rather than on every tick.
func (m *Model) viewToSave() (folder string, view ListView, ok bool) {
	// A `discobox new` or `discobox attach` window never shows the list, so
	// nothing it holds is a choice somebody made; and before the session lands
	// the window has neither a folder to key by nor the view it opened on.
	if m.oneShot() || m.session.Directory == "" {
		return "", ListView{}, false
	}
	view = m.currentView()
	if view.Equal(m.savedView) {
		return "", ListView{}, false
	}
	m.savedView = view
	return m.session.Directory, view, true
}
