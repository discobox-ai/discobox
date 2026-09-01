package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The folder filter is the path in the header, made selectable: it says where
// the sandboxes on screen came from, and changing it changes which ones are on
// screen. It replaces both the folder column — every row on screen shares the
// value, so a column repeating it says nothing — and the key that used to
// toggle "only the ones started here", which was the same filter with one of
// its choices missing.

// allFolders is the choice that is not a path: every sandbox in the project,
// wherever it was started. It is last in the list rather than first, because
// the folder you are standing in is the one you almost always want.
const allFolders = "all folders"

// folderChoices are what the dropdown offers: the folders something was started
// from, then the choice to drop the filter entirely.
func (m *Model) folderChoices() []string {
	return append(m.list.folders(), allFolders)
}

// folderLabel is how the current filter reads in the header.
func (m *Model) folderLabel() string {
	if m.list.folder == "" {
		return allFolders
	}
	// The branch belongs to the directory the window is running in, and says
	// what a new sandbox would be cut from. It means nothing next to a folder
	// somewhere else, so it is only shown against the one it describes.
	if m.list.folder == m.session.Directory && m.session.Branch != "" {
		return m.session.Directory + " @ " + m.session.Branch
	}
	return m.list.folder
}

// cycleFolder steps to the next or previous choice.
func (m *Model) cycleFolder(delta int) tea.Cmd {
	choices := m.folderChoices()
	if len(choices) < 2 {
		return status("no other folders to show")
	}
	at := m.folderIndex(choices)
	next := choices[(at+delta+len(choices))%len(choices)]
	return m.selectFolder(next)
}

// folderIndex is where the current filter sits among the choices.
func (m *Model) folderIndex(choices []string) int {
	want := m.list.folder
	if want == "" {
		want = allFolders
	}
	for i, choice := range choices {
		if choice == want {
			return i
		}
	}
	return 0
}

// selectFolder applies a choice from the dropdown.
//
// The cursor goes back to the top: the rows underneath it are a different set
// of sandboxes now, and leaving the cursor on row four of a list that has been
// replaced points it at something nobody chose.
func (m *Model) selectFolder(choice string) tea.Cmd {
	if choice == allFolders {
		m.list.folder = ""
	} else {
		m.list.folder = choice
	}
	// Where the window is listing from is where it creates from.
	m.opts.setFolder(m.list.folder)
	m.list.resetCursor()
	m.layout()
	return status("showing %s", m.folderLabel())
}

// updateFolder handles the header's dropdown. Left and right change it in
// place, which is the common case — there are usually two or three folders —
// and Enter opens the whole list when there are more than that.
func (m *Model) updateFolder(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "left", "h":
		return m.cycleFolder(-1)
	case "right", "l":
		return m.cycleFolder(1)
	case "enter", " ":
		m.dialog = m.folderDialog()
		return nil
	case "down", "j":
		// Down moves into the list, the way it does everywhere else in the
		// window: up and down cross between panes, and opening the dropdown is
		// what Enter is for.
		if len(m.list.rows()) == 0 {
			// An empty list is nothing to move through, so Down carries on to
			// the prompt — which is where Down always ends up, and what the
			// empty list itself says to do. Stopping here would make an empty
			// folder a dead end in the one direction with somewhere to go.
			m.backToPrompt()
			return nil
		}
		m.focus = focusList
		m.list.moveTo(0)
		return nil
	case "esc", "tab":
		m.backToPrompt()
		return nil
	case "up", "k":
		// The filter is the top of the window. Up has nowhere left to go, and
		// jumping to the prompt from here would be moving down the screen.
		return nil
	case "shift+tab", "ctrl+o":
		m.optionsOpen = true
		return nil
	case "f1", "?":
		m.dialog = textDialog("Keys", m.helpText())
		return nil
	}
	return nil
}

// folderDialog is the dropdown opened: every folder in full, with the sandboxes
// each holds beside it. The whole path, because two checkouts of the same
// repository differ by one segment somewhere in the middle, and the count,
// because that is what the choice is usually made on.
func (m *Model) folderDialog() *dialog {
	choices := m.folderChoices()
	items := make([]action, 0, len(choices))
	for i, choice := range choices {
		n := itoa(i + 1)
		items = append(items, action{
			// The key is the row's index, so the first nine choices can be
			// picked by number as well as by moving to them.
			key:     n,
			press:   n,
			label:   choice,
			detail:  m.folderDetail(choice),
			enabled: true,
		})
	}
	menu := actionsDialog("Show discoboxes from", "", items, func(key string) tea.Cmd {
		for i, choice := range choices {
			if itoa(i+1) == key {
				return func() tea.Msg { return folderChosenMsg{folder: choice} }
			}
		}
		return nil
	})
	menu.cursor = m.folderIndex(choices)
	menu.footer = "Enter shows that folder's discoboxes · Esc cancels"
	return menu
}

// folderChosenMsg carries the dropdown's answer back to the live model, for the
// same reason every other dialog does: it closed over the model by value.
type folderChosenMsg struct{ folder string }

// folderDetail is what each choice is worth knowing: how many sandboxes it
// holds, and whether it is the one this window is running in.
func (m *Model) folderDetail(choice string) string {
	if choice == allFolders {
		return plural(len(m.list.all), "box", "boxes") + " in the project"
	}
	n := 0
	for _, s := range m.list.all {
		if s.Folder == choice {
			n++
		}
	}
	detail := plural(n, "box", "boxes")
	if choice == m.session.Directory {
		detail += " · where this window is running"
	}
	return detail
}
