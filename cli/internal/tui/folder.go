package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The folder filter is the place in the header, made selectable: it says where
// the sandboxes on screen came from, and changing it changes which ones are on
// screen. It stands in for both a folder column — every row on screen shares
// the value, so a column repeating it says nothing — and a key toggling "only
// the ones started here", which would be the same filter with one of its
// choices missing.

// A folder is an origin key (ADR 0111): a machine, and where the discoboxes in
// it had their source from — a directory or a repository URL — or the machine
// alone for the ones with none. It is matched by key and named by its source,
// so one path on two machines is two folders, and a URL is a folder the way a
// directory is.

// folder is one of the header's choices: the origin key the discoboxes in it
// are filed under, what it reads as, the source a create from it cuts from,
// and whether it is this machine's — in which case this machine's discoboxes
// with no source are in it too, the way `discobox ls` lists them beside the
// directory's own.
type folder struct {
	key    string
	label  string
	source string
	local  bool
}

// allFolders is the choice that is not a place: every sandbox in the project,
// wherever it was started. It is last in the list rather than first, because
// the folder you are standing in is the one you almost always want.
const allFolders = "all folders"

// everyFolder is allFolders as a choice: no key, so nothing is filtered out.
var everyFolder = folder{label: allFolders}

// holds reports whether a discobox is in the folder: filed under its key, or —
// in a folder of this machine's — under this machine's own, where its
// discoboxes with no source are. Every discobox is in the folder with no key.
func (f folder) holds(s Sandbox, session Session) bool {
	switch {
	case f.key == "":
		return true
	case s.OriginKey == f.key:
		return true
	default:
		return f.local && session.HostKey != "" && s.OriginKey == session.HostKey
	}
}

// folder is the window's own folder: where a discobox cut from its own source
// is filed, named the way the header spells that source — which is the one
// place a branch is shown, since it belongs to the directory the window is
// running in and means nothing next to a folder somewhere else. It is this
// machine's, so its discoboxes with no source are in it.
func (s Session) folder() folder {
	return folder{key: s.OriginKey, label: s.sourceLabel(), source: s.source(), local: true}
}

// folderChoices are what the dropdown offers: the folders something was started
// from, then the choice to drop the filter entirely.
func (m *Model) folderChoices() []folder {
	return append(m.list.folders(), everyFolder)
}

// folderLabel is how the current filter reads in the header.
func (m *Model) folderLabel() string {
	if m.list.folder.key == "" {
		return allFolders
	}
	return m.list.folder.label
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
func (m *Model) folderIndex(choices []folder) int {
	for i, choice := range choices {
		if choice.key == m.list.folder.key {
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
func (m *Model) selectFolder(choice folder) tea.Cmd {
	m.list.folder = choice
	// Where the window is listing from is where it creates from.
	m.opts.setFolder(choice.source)
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
	case "shift+tab":
		m.optionsOpen = true
		return nil
	case "f1", "?":
		m.dialog = m.helpDialog()
		return nil
	}
	return nil
}

// folderDialog is the dropdown opened: every folder in full, with the sandboxes
// each holds beside it. The whole path or URL, because two checkouts of the
// same repository differ by one segment somewhere in the middle, and the
// count, because that is what the choice is usually made on.
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
			label:   choice.label,
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
	menu.keys = []hint{pressing("Enter shows that folder's discoboxes", "enter"), pressing("Esc cancels", "esc")}
	return menu
}

// folderChosenMsg carries the dropdown's answer back to the live model, for the
// same reason every other dialog does: it closed over the model by value.
type folderChosenMsg struct{ folder folder }

// folderDetail is what each choice is worth knowing: how many sandboxes it
// holds, and whether it is the one this window is running in.
func (m *Model) folderDetail(choice folder) string {
	if choice.key == "" {
		return plural(len(m.list.all), "box", "boxes") + " in the project"
	}
	n := 0
	for _, s := range m.list.all {
		if choice.holds(s, m.session) {
			n++
		}
	}
	detail := plural(n, "box", "boxes")
	if choice.key == m.session.OriginKey {
		detail += " · where this window is running"
	}
	return detail
}
