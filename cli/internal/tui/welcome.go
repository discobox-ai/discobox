package tui

import (
	"fmt"
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

// The screen is short on purpose: this is something between someone and the
// thing they came to do, so it earns its place by being read, not by being
// complete. What it has to get across is the shape of the loop — out of the
// source directory, into a sandbox, back as a commit — so it is three numbered
// steps, each with the command that performs it.
//
// The commands are the point. Someone who reads only the three green lines has
// the whole of using this; the prose beside them only says why each one is run.
// They are the TUI flow rather than `discobox run`, which does the same work in
// one line and teaches none of the shape.
const welcomeTitle = "Welcome to Discobox"

const welcomeIntro = "Discobox runs coding agents in isolated sandboxes on this machine."

// welcomeSteps is the loop, in the order it happens. The middle one has no
// discobox command in it because that step is not ours: it is the agent, or
// you, working until there is something to take out.
var welcomeSteps = []struct{ text, command string }{
	{"A new box, with your source copied into it", "cd ~/src/my-project && discobox"},
	{"Work with the agent until the change is a commit", `git commit -am "..."`},
	{"Take that commit back out to your source", "discobox apply"},
}

// welcomeKeys is the window's own three keys, after the three steps: what the
// screen behind this one expects.
const welcomeKeys = "Enter starts one, Tab moves to the ones you have, F1 lists every key."

// welcomeFooter is the one instruction, and the only key the screen takes.
const welcomeFooter = "Press Enter to continue"

// welcomeIndent is where a step's words start, past its number, so a wrapped
// line and the command under it line up with the text rather than the digit.
const welcomeIndent = "   "

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

// viewWelcome draws the introduction: the mark, what this is, the three steps,
// and the key that leaves. It wears the dialog's box because it is the same
// kind of thing — one card, centered, with the window it stands in front of not
// drawn at all.
func (m *Model) viewWelcome() string {
	// Narrower than a dialog, which sizes itself to the window because what it
	// holds is rows and columns. This is prose, and prose set across a hundred
	// columns of a wide terminal is prose nobody finishes a line of.
	boxWidth := min(dialogWidth(m.width), welcomeMaxWidth)
	inner := max(boxWidth-dialogChromeWidth, 16)

	rows := []string{m.st.dialogTitle.Render(truncate(welcomeTitle, inner)), ""}
	rows = append(rows, welcomeLines(welcomeIntro, "", inner)...)
	for i, step := range welcomeSteps {
		rows = append(rows, "")
		rows = append(rows, welcomeLines(step.text, fmt.Sprintf("%d. ", i+1), inner)...)
		// The command carries the one color the window uses for commands, and
		// bold as well so a terminal with no color still shows which of the two
		// lines is the one to type.
		rows = append(rows, m.st.command.Bold(true).Render(truncate(welcomeIndent+step.command, inner)))
	}
	rows = append(rows, "")
	rows = append(rows, welcomeLines(welcomeKeys, "", inner)...)
	// The one instruction is the one control: a press on it is the Enter it
	// asks for. The prose above stays selectable rather than being one big
	// button.
	rows = append(rows, "", m.st.key.Render(welcomeFooter))

	// The mark goes on top, centered over the card, when there are rows to
	// spare for it: here it is the thing being introduced rather than the thing
	// beside the list. On a short terminal the steps are what the screen is
	// for, so the mark is what gives way — the same trade the list makes at
	// minWidthForLogo, in the other dimension.
	if logo := m.logo.centeredRows(inner); len(logo) > 0 &&
		len(logo)+1+len(rows)+dialogChromeHeight <= m.height {
		rows = append(append(logo, ""), rows...)
	}

	m.zones.mark(keyHit("enter"), dialogPadLeft, len(rows)-1+dialogPadTop, lipgloss.Width(welcomeFooter), 1)
	return m.st.dialog.Width(boxWidth).Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// welcomeLines wraps one block of the screen under its own first line: a
// step's number sits in the margin, and its wrapped lines and the command below
// it start where its words do. A block with no prefix — the two lines of prose
// — hangs from the left edge like the paragraph it is. Wrapping each block on
// its own is what keeps the steps one to a row and the blank lines between them
// intact; wrapping the screen at once would run them into one paragraph.
func welcomeLines(text, prefix string, inner int) []string {
	hang := strings.Repeat(" ", lipgloss.Width(prefix))
	var rows []string
	for i, line := range wrap(text, inner-len(hang)) {
		lead := hang
		if i == 0 {
			lead = prefix
		}
		rows = append(rows, truncate(lead+line, inner))
	}
	return rows
}
