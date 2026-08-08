package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// The window opens small: the mark, a prompt beside it, and nothing else. It is
// inline, so it sits under your last command and takes only the rows it needs.
//
// That is the common case answered at its own size — you came here to start
// something, and a screenful of sandboxes you did not ask to see is a screenful
// you have to look past to type. Reaching for the sandboxes is what says you
// want the rest of it, and the window opens out to full screen and stays there:
// having asked for the list once, flipping back and forth around it would be
// the window arguing with you.

// expand takes the window from the opening prompt to the full launcher. It is
// one way: see above.
func (m *Model) expand() {
	if m.expanded {
		return
	}
	m.expanded = true
	m.layout()
}

// compactLayout sizes the opening window: the composer beside the mark, as wide
// as what is left of the terminal.
func (m *Model) compactLayout() {
	width := m.compactPromptWidth()
	m.prompt.SetWidth(max(width-2, 10))
	m.prompt.SetHeight(min(max(m.prompt.LineCount(), 1), 8))
}

// compactPromptWidth is what the composer gets: the box's inside, less the mark
// when there is room for one.
func (m *Model) compactPromptWidth() int {
	if m.showLogo() {
		return max(m.inner()-m.logo.column(), 20)
	}
	return m.inner()
}

// viewCompact draws the opening window: a header, the mark with the composer
// beside it, and the keys underneath.
//
// The composer is centered against the mark rather than hung from its top,
// because the mark is the taller of the two and a prompt pinned to its shoulder
// reads as a caption on it rather than as the thing you are meant to type in.
func (m *Model) viewCompact() string {
	promptW := m.compactPromptWidth()

	composer := strings.Split(m.viewComposer(promptW), "\n")
	body := composer
	if m.showLogo() {
		mark := m.logo.viewCentered(max(m.logo.height(), len(composer)))
		body = strings.Split(
			lipgloss.JoinHorizontal(lipgloss.Top, mark, centerVertical(composer, m.logo.height(), promptW)),
			"\n")
	}

	rows := []string{m.viewHeader(m.inner()), ""}
	rows = append(rows, body...)
	rows = append(rows, "", m.viewStatus())

	// Nothing on this frame suggests there is anything behind it, so it says
	// so — in the very top line, which is the one place a centered word cannot
	// be squeezed out by what is beside it, and which costs no row of its own.
	return m.box("Tab or ↑ for the discoboxes you already have", rows)
}

// centerVertical pads a block to height rows with the block in the middle, so
// it sits against the middle of whatever it is being joined to.
func centerVertical(rows []string, height, width int) string {
	if len(rows) >= height {
		return strings.Join(rows, "\n")
	}
	blank := strings.Repeat(" ", max(width, 0))
	top := (height - len(rows)) / 2
	out := make([]string, 0, height)
	for range top {
		out = append(out, blank)
	}
	out = append(out, rows...)
	for len(out) < height {
		out = append(out, blank)
	}
	return strings.Join(out, "\n")
}
