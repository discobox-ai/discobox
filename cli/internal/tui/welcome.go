package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The welcome is the first thing anyone sees, and the only screen in the window
// that is shown once rather than opened. It says what a discobox is, because
// every other screen assumes you already know: the prompt asks for work without
// saying where the work will run, and the list is a list of things whose name
// is the thing being explained.
//
// Once is per project, and the project is what remembers — see
// model.Project.Welcomed. The alternative was a file beside the CLI's other
// local state, which would have re-welcomed the same person on their second
// machine and never welcomed the second person on a project they were added to.

// WithWelcome opens the window on the introduction, ahead of whatever screen it
// was going to open on. Enter takes it down and reveals that screen; nothing
// else about the window changes, so a `discobox configure` that welcomes first
// still lands on the harnesses.
func WithWelcome() Option {
	return func(m *Model) { m.welcoming = true }
}

// welcomeTitle and welcomeBody are the whole of it. It is short on purpose:
// this is a screen between someone and the thing they came to do, so it earns
// its place by being read, not by being complete.
const welcomeTitle = "Welcome to Discobox"

const welcomeBody = `Discobox runs coding agents somewhere other than your machine.

Every task gets a discobox of its own: a container holding a copy of the folder you launched from, with an agent already working in it. Several run at once without treading on each other, and none of them touches your own checkout — you read what a discobox did, and apply it when you want it.

Type what you want done and press Enter to start one. Tab moves to the discoboxes you already have, and F1 lists every key.`

// welcomeFooter is the one instruction, and the only key the screen takes.
const welcomeFooter = "Press Enter to continue"

// welcomeMaxWidth is the widest the card is set, border included — about
// seventy columns of text, which is a comfortable measure to read.
const welcomeMaxWidth = 78

// updateWelcome handles the introduction's single key. Enter dismisses it and
// records that it has been shown; anything else is ignored rather than passed
// through, so a key aimed at the screen behind cannot arrive before that screen
// is visible. Ctrl-C is handled before this and still quits.
func (m *Model) updateWelcome(msg tea.KeyPressMsg) tea.Cmd {
	if keyName(msg) != "enter" {
		return nil
	}
	m.welcoming = false
	m.layout()
	return m.markWelcomed()
}

// markWelcomed tells the server the introduction has been shown. It runs behind
// the screen it dismisses: the user has read the welcome whether or not the
// write lands, and holding the window on a round trip to say so would be the
// screen outstaying the key that closed it.
//
// A failure is reported and nothing more. The cost of losing it is one repeated
// welcome, which is not worth a retry loop or a dialog.
func (m *Model) markWelcomed() tea.Cmd {
	return func() tea.Msg {
		if err := m.ds.MarkWelcomed(m.ctx); err != nil {
			return statusMsg{text: "could not save that you have seen the welcome: " + err.Error(), err: true}
		}
		return nil
	}
}

// viewWelcome draws the introduction: the mark, what this is, and the key that
// leaves. It wears the dialog's box because it is the same kind of thing — one
// card, centered, with the window it stands in front of not drawn at all.
func (m *Model) viewWelcome() string {
	// Narrower than a dialog, which sizes itself to the window because what it
	// holds is rows and columns. This is prose, and prose set across a hundred
	// columns of a wide terminal is prose nobody finishes a line of.
	boxWidth := min(dialogWidth(m.width), welcomeMaxWidth)
	inner := max(boxWidth-dialogChromeWidth, 16)

	var rows []string
	// The mark, when the box is wide enough to hold it whole. It is the one
	// place in the window the mark is centered over text rather than beside it:
	// here it is the thing being introduced.
	if m.logo.height() > 0 && m.logo.column() <= inner {
		rows = append(rows, m.logo.view(m.logo.height()), "")
	}
	rows = append(rows, m.st.dialogTitle.Render(truncate(welcomeTitle, inner)), "")

	// Paragraphs wrap independently and keep the blank line between them; a
	// single wrap over the whole text would fold them into one block.
	for i, paragraph := range strings.Split(welcomeBody, "\n\n") {
		if i > 0 {
			rows = append(rows, "")
		}
		for _, line := range wrap(paragraph, inner) {
			rows = append(rows, truncate(line, inner))
		}
	}
	rows = append(rows, "", m.st.key.Render(welcomeFooter))

	return m.st.dialog.Width(boxWidth).Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}
