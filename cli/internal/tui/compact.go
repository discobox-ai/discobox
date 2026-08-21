package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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

// screenClearedMsg says the empty frame below has had its moment.
type screenClearedMsg struct{}

// clearPause is how long that frame is held. The renderer flushes on its own
// clock — 60 frames a second — rather than on ours, so the frame has to be the
// current one across one of its ticks; a couple of them is a pause nobody sees
// on the way to a screen. Missing the window costs the prompt left behind, which
// is where this started, and nothing worse.
const clearPause = 40 * time.Millisecond

// clearPrinted holds the window on one empty inline frame the first time it
// takes the whole terminal, so the opening prompt comes off the screen it was
// printed on before the window moves to the other one.
//
// The prompt is printed inline, under the command that started it, and
// everything else the window draws is on the alternate screen. Switching
// screens does not take the printed rows along: they stay on the primary
// screen, behind the window, and whatever the window later drops back onto it
// lands in the middle of them — a harness setup run through tea.Exec prints
// straight over the old prompt, which reads as a screen drawn twice.
//
// An empty inline frame is how the renderer is asked to erase the rows it
// printed, and it is the only thing that knows where they are — hence a frame
// of nothing rather than an escape sequence of our own.
func (m *Model) clearPrinted(cmd tea.Cmd) tea.Cmd {
	if m.clearing || !m.printed || !m.takesScreen() {
		return cmd
	}
	m.clearing = true
	return tea.Batch(cmd, tea.Tick(clearPause, func(time.Time) tea.Msg { return screenClearedMsg{} }))
}

// compactLayout sizes the opening window: the composer beside the mark, as wide
// as what is left of the terminal. Its height follows the text on its own, up
// to promptMaxRows.
func (m *Model) compactLayout() {
	m.prompt.SetWidth(max(m.compactPromptWidth()-2, 10))
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
