package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The report at the end of the status line saying the server is still setting
// itself up.
//
// The work is one-time — staging the images a discobox runs — and it used to be
// waited on before the window opened, which meant several minutes of a status
// line before anything appeared. Nothing about the window needs it finished:
// the launcher lists, the composer takes input, and only actually running a
// discobox wants those images.
//
// So the window opens at once and this reports on the row every screen already
// keeps for saying what is going on. It is pinned to the right end of that row
// rather than taking the line the way a message does: the left of the row
// belongs to whatever the user just did — the busy line, a result, the keys —
// and this belongs to something they did not do and cannot act on, so the two
// have to be able to say their piece at the same time. See Model.viewStatus and
// viewPaneWindow, which pin it, and statusLine, which draws the left.
//
// A row of its own, under the border, is what this used to be. It cost a row
// the window then had to be told about — every screen that fills the terminal,
// the pane geometry, the chrome selection's grid — and being outside all of
// them is what let it scroll the alternate screen and hand its own presses to
// the border above it. On the status row it is inside the frame like everything
// else, and none of that is a question that can be got wrong.
//
// It removes itself when the work finishes, which is the whole shape of it — a
// report that exists only while there is something to say.

// initializationMsg carries one update. done is true when the channel closes,
// which is how "finished" arrives.
type initializationMsg struct {
	line string
	done bool
}

// WithInitialization reports on the status line while the server finishes
// setting itself up, headed by title and fed by updates. The report disappears
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
		// Nothing left to say, so nothing is said.
		m.initUpdates, m.initLine, m.initTitle = nil, "", ""
		return nil
	}
	m.initLine = msg.line
	return m.awaitInitialization()
}

// viewInitialization is the report itself, or empty when there is nothing to
// say. The caller pins it to the right end of its status row.
//
// It is not cut to fit here. The row it goes on is what knows how much room
// there is, and spreadPin cuts the keys back to make room for this rather than
// the other way round — a wait the user did not ask for is the one thing on
// that row nothing else on screen accounts for, so it is the last to give way.
func (m *Model) viewInitialization() string {
	line := strings.TrimSpace(m.initLine)
	if line == "" {
		return ""
	}
	if title := strings.TrimSpace(m.initTitle); title != "" {
		line = title + ": " + line
	}
	return m.st.initializing.Render(line)
}
