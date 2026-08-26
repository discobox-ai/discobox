package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// A line below the window saying the server is still setting itself up.
//
// The work is one-time — staging the images a discobox runs — and it used to be
// waited on before the window opened, which meant several minutes of a status
// line before anything appeared. Nothing about the window needs it finished:
// the launcher lists, the composer takes input, and only actually running a
// discobox wants those images.
//
// So the window opens at once and this reports underneath it, outside the
// border, where it is plainly not part of the application. Deliberately not the
// busy line inside the frame: that one belongs to whatever the user just did,
// and this belongs to something they did not do and cannot act on.
//
// It removes itself when the work finishes, which is the whole shape of it — a
// line that exists only while there is something to say.

// initializationMsg carries one update. done is true when the channel closes,
// which is how "finished" arrives.
type initializationMsg struct {
	line string
	done bool
}

// WithInitialization shows a line under the window while the server finishes
// setting itself up, headed by title and fed by updates. The line disappears
// when updates is closed.
func WithInitialization(title string, updates <-chan string) Option {
	return func(m *Model) {
		m.initTitle = title
		m.initUpdates = updates
	}
}

// awaitInitialization blocks in a command until the next update arrives, which
// is what makes this a subscription rather than a poll: nothing redraws between
// updates, and a closed channel ends it.
func (m *Model) awaitInitialization() tea.Cmd {
	updates := m.initUpdates
	if updates == nil {
		return nil
	}
	return func() tea.Msg {
		line, ok := <-updates
		if !ok {
			return initializationMsg{done: true}
		}
		return initializationMsg{line: line}
	}
}

// applyInitialization records an update and asks for the next one.
func (m *Model) applyInitialization(msg initializationMsg) tea.Cmd {
	if msg.done {
		// Nothing left to say, so nothing is said: the line and the row it
		// occupied both go.
		m.initUpdates, m.initLine, m.initTitle = nil, "", ""
		return nil
	}
	m.initLine = msg.line
	return m.awaitInitialization()
}

// viewInitialization is the line itself, or empty when there is nothing to
// report. The caller renders it outside the window's border.
func (m *Model) viewInitialization() string {
	line := strings.TrimSpace(m.initLine)
	if line == "" {
		return ""
	}
	if title := strings.TrimSpace(m.initTitle); title != "" {
		line = title + ": " + line
	}
	// Truncated rather than wrapped: a second row would move the window every
	// time the text changed length, and this is the one thing on screen that
	// must not make anything else jump.
	if m.width > 0 {
		line = lipgloss.NewStyle().MaxWidth(m.width).Render(line)
	}
	return m.st.initializing.Render(line)
}
