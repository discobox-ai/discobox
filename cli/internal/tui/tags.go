package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The tag filter is the header's third place, after the server and the folder:
// it narrows the list to the discoboxes carrying one tag. A discobox's tags are
// its own, in the meta file inside it, and the rows show the copy the server
// last heard (ADR 0136). The filter matches the tag as the row spells it —
// `wip`, or `ticket=ENG-12` — so what you pick is what you see after the name.
//
// It is only there once there is a tag to pick: a project nobody tags has no
// use for a control that can only say "all tags". It is not drawn over the
// harnesses and secrets screens, which list no discoboxes.

// allTags is the choice that is not a tag: every discobox, tagged or not.
const allTags = "all tags"

// tagged reports whether a discobox carries the tag the list is filtered to.
// Every discobox carries the empty one.
func (l *sandboxList) tagged(s Sandbox) bool {
	return l.tag == "" || slices.Contains(s.Tags, l.tag)
}

// tags are what the filter can narrow to: every tag the discoboxes inside the
// other two filters carry, in order, and the one it is on when none of them
// carries it any longer — the way the folder filter keeps its own, so the
// control does not vanish from under the choice it is showing.
func (l *sandboxList) tags() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range l.all {
		if !l.onServer(s) || !l.folder.holds(s, l.session) {
			continue
		}
		if !l.showArchived && s.State == StateArchived {
			continue
		}
		for _, tag := range s.Tags {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	slices.Sort(out)
	if l.tag != "" && !seen[l.tag] {
		out = append(out, l.tag)
	}
	return out
}

// tagChoices are what the dropdown offers: every discobox, then each tag. Nil
// when there is no tag to offer, and then the header draws no filter.
func (m *Model) tagChoices() []string {
	if m.onConfigScreen() {
		return nil
	}
	tags := m.list.tags()
	if len(tags) == 0 {
		return nil
	}
	return append([]string{""}, tags...)
}

// showsTagFilter reports whether the header draws the tag filter.
func (m *Model) showsTagFilter() bool { return len(m.tagChoices()) > 0 }

// tagLabel is how the current filter reads in the header: the tag as the rows
// draw it, so the eye matches the one to the other.
func (m *Model) tagLabel() string {
	if m.list.tag == "" {
		return allTags
	}
	return "#" + m.list.tag
}

// cycleTag steps to the next or previous choice.
func (m *Model) cycleTag(delta int) tea.Cmd {
	choices := m.tagChoices()
	if len(choices) < 2 {
		return status("no tags to show")
	}
	at := slices.Index(choices, m.list.tag)
	if at < 0 {
		at = 0
	}
	return m.selectTag(choices[(at+delta+len(choices))%len(choices)])
}

// selectTag applies a choice. The cursor goes back to the top for the reason
// it does on a folder: the rows under it are a different set now.
func (m *Model) selectTag(tag string) tea.Cmd {
	m.list.tag = tag
	m.list.resetCursor()
	m.layout()
	if tag == "" {
		return status("showing every discobox, tagged or not")
	}
	return status("showing discoboxes tagged %s", tag)
}

// updateTags handles the header's tag filter. It sits beside the folder, so it
// answers the keys the folder does: left and right change it, Enter opens the
// list, Down drops into the discoboxes and Up climbs to the server. Tab goes on
// round the ring from the folder, through here, to the server.
func (m *Model) updateTags(msg tea.KeyPressMsg) tea.Cmd {
	if !m.showsTagFilter() {
		// The last tag went while the keyboard was on the filter; the folder
		// beside it is where it was.
		m.focus = focusFolder
		return m.updateFolder(msg)
	}
	switch keyName(msg) {
	case "left", "h":
		return m.cycleTag(-1)
	case "right", "l":
		return m.cycleTag(1)
	case "enter", " ":
		m.dialog = m.tagDialog()
		return nil
	case "down", "j":
		if len(m.list.rows()) == 0 {
			m.backToPrompt()
			return nil
		}
		m.focus = focusList
		m.list.moveTo(0)
		return nil
	case "up", "k":
		if m.manyServers() {
			m.focus = focusServer
		}
		return nil
	case "tab":
		if m.manyServers() {
			m.focus = focusServer
			return nil
		}
		m.backToPrompt()
		return nil
	case "esc":
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

// tagDialog is the dropdown opened: every tag, with how many discoboxes carry
// it inside the other two filters.
func (m *Model) tagDialog() *dialog {
	choices := m.tagChoices()
	items := make([]action, 0, len(choices))
	for i, choice := range choices {
		n := itoa(i + 1)
		label := allTags
		if choice != "" {
			label = "#" + choice
		}
		items = append(items, action{
			key:     n,
			press:   n,
			label:   label,
			detail:  m.tagDetail(choice),
			enabled: true,
		})
	}
	menu := actionsDialog("Show discoboxes tagged", "", items, func(key string) tea.Cmd {
		for i, choice := range choices {
			if itoa(i+1) == key {
				return func() tea.Msg { return tagChosenMsg{tag: choice} }
			}
		}
		return nil
	})
	menu.cursor = max(slices.Index(choices, m.list.tag), 0)
	menu.keys = []hint{pressing("Enter shows the discoboxes with that tag", "enter"), pressing("Esc cancels", "esc")}
	return menu
}

// tagChosenMsg carries the dropdown's answer back to the live model, for the
// same reason folderChosenMsg does: the dialog closed over the model by value.
type tagChosenMsg struct{ tag string }

// tagDetail is how many discoboxes a choice lists, counted inside the server
// and folder the header names, which stay where they are when a tag is chosen.
func (m *Model) tagDetail(choice string) string {
	n := 0
	for _, s := range m.list.all {
		if !m.list.onServer(s) || !m.list.folder.holds(s, m.session) {
			continue
		}
		if !m.list.showArchived && s.State == StateArchived {
			continue
		}
		if choice == "" || slices.Contains(s.Tags, choice) {
			n++
		}
	}
	if choice == "" {
		return plural(n, "box", "boxes") + ", tagged or not"
	}
	if key, _, ok := strings.Cut(choice, "="); ok {
		return plural(n, "box", "boxes") + " · " + key + " set to this value"
	}
	return plural(n, "box", "boxes")
}

// viewTags draws the tag filter, the way viewFolder draws the folder.
func (m *Model) viewTags(hovered bool) string {
	return m.viewHeaderFilter(m.tagLabel(), m.focus == focusTags, hovered)
}
