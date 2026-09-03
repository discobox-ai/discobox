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

// screenClearedMsg is the backstop under the acknowledgements below: a
// terminal that never answers is not one the window waits on forever.
type screenClearedMsg struct{}

// clearAcks is how many cursor-position answers the window waits for before it
// takes the screen.
//
// The renderer does not draw the frames the window returns. It keeps the latest
// one and a ticker goroutine writes whichever is current when it fires, so a
// frame the window holds only briefly is a frame that may never be written at
// all — and on a terminal whose writes are slow, which is where this was
// reported, briefly is exactly what it was. What the window needs is not a
// pause but a fact: that the erasing frame has gone out. Asking the terminal
// where its cursor is buys that fact, because the request is written from the
// same ticker, immediately before the frame is.
//
// Two answers, not one. The first says a tick ran and wrote the request; the
// frame goes out in that same tick, just after it, so the answer can overtake
// it. The second request is only sent once the first is answered, so it cannot
// be written before that tick finished — and a tick that finished is a frame
// that was written.
const clearAcks = 2

// clearTimeout gives up on the answers. Every terminal worth the name reports
// its cursor, so this is for the ones that are not terminals at all — a pipe,
// a test harness — where nothing was printed on a screen anybody is looking at.
// Missing the window costs the prompt left behind, which is where this started,
// and nothing worse.
const clearTimeout = 500 * time.Millisecond

// clearPrinted holds the window on one empty inline frame the first time it
// takes the whole terminal, so the opening prompt comes off the screen it was
// printed on before the window moves to the other one.
//
// The prompt is printed inline, under the command that started it, and
// everything else the window draws is on the alternate screen. Switching
// screens does not take the printed rows along: they stay on the primary
// screen, behind the window, and whatever the window later drops back onto it
// lands in the middle of them — a harness setup run through tea.Exec prints
// straight over the old prompt, or the shell prompt you get back on the way
// out, which is what the leftovers are usually seen behind.
//
// An empty inline frame is how the renderer is asked to erase the rows it
// printed, and it is the only thing that knows where they are — hence a frame
// of nothing rather than an escape sequence of our own. The window then holds
// still until the terminal says that frame has been written. See clearAcks.
func (m *Model) clearPrinted(cmd tea.Cmd) tea.Cmd {
	if m.clearing > 0 || !m.printed || !m.takesScreen() {
		return cmd
	}
	m.clearing = clearAcks
	return tea.Batch(cmd, askCursorPosition, tea.Tick(clearTimeout, func(time.Time) tea.Msg { return screenClearedMsg{} }))
}

// askCursorPosition asks the terminal to report where its cursor is. The answer
// comes back as a tea.CursorPositionMsg; what it says does not matter, only that
// it came. See clearAcks.
func askCursorPosition() tea.Msg { return tea.RequestCursorPosition() }

// screenCleared takes the window off the erasing frame once the terminal has
// acknowledged it, and is what every answer while the window is clearing goes
// to. See clearAcks.
func (m *Model) screenCleared() tea.Cmd {
	if m.clearing > 0 {
		m.clearing--
	}
	if m.clearing > 0 {
		return askCursorPosition
	}
	m.printed = false
	return nil
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
	if m.compactShowsLogo() {
		return max(m.inner()-m.logo.column(), 20)
	}
	return m.inner()
}

// compactChrome is what the opening frame costs before its body: the box's two
// edges, the header and the blank under it, and the blank and the status line
// under that.
const compactChrome = 6

// composerChrome is what the composer costs around the field: its label, the
// rule under that, the rule below the field, and the mode line.
const composerChrome = 4

// compactRows is how tall the opening frame can get around a body of the given
// height: its chrome, and the line under the frame whenever there is one to
// report. The tallest it can get rather than the height it is at now, because
// it grows on its own — the composer as you type, the line when the server has
// something to say — and a frame that grows past the screen it is printed on
// cannot take back what that costs.
func (m *Model) compactRows(body int) int {
	rows := compactChrome + body
	if m.initLine != "" || m.initUpdates != nil {
		rows++
	}
	return rows
}

// fitsInline reports whether a frame of that many rows fits on the screen it is
// printed on.
//
// A frame taller than the screen does not simply overflow: printing it scrolls
// the terminal, and the rows that go off the top go into its scrollback, where
// no escape sequence of ours can reach them — the erasing frame cleans the
// screen and they stay above it, which is where a leftover row of the opening
// prompt comes from.
func (m *Model) fitsInline(rows int) bool {
	return rows <= m.height
}

// compactShowsLogo reports whether the opening frame has room for the mark.
//
// Beyond the width every screen wants it at (showLogo), this frame is printed
// on the screen the window was started from and has to fit there. The mark is
// twelve rows and the tallest thing on the frame, so a short terminal loses the
// decoration first — exactly as a narrow one does, and ahead of losing the
// small window altogether.
func (m *Model) compactShowsLogo() bool {
	return m.showLogo() && m.fitsInline(m.compactRows(m.logo.height()))
}

// compactFits reports whether the opening frame fits the screen at all. Below
// some size it does not, and layout opens the window out instead: a window that
// takes the screen prints nothing on the one behind it.
func (m *Model) compactFits() bool {
	body := composerChrome + promptMaxRows
	if m.compactShowsLogo() {
		body = max(body, m.logo.height())
	}
	return m.fitsInline(m.compactRows(body))
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
	if m.compactShowsLogo() {
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
	// ↑ is only offered while the prompt is empty: once there is text in it,
	// ↑ walks the text and Tab is the way out.
	banner := "Tab or ↑ for the discoboxes you already have"
	if m.prompt.Value() != "" {
		banner = "Tab for the discoboxes you already have"
	}
	return m.box(banner, rows)
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
