package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The window asks its terminal things — Bubble Tea's own DECRQM 2026/2027 at
// startup and its kitty keyboard query with each first frame, and the OSC
// 7501 probe and its DA1 (programstatus.go) — and the terminal answers on the
// window's input. A window that ends before the answers arrive leaves them
// on the terminal, where the shell it returns to reads them as typed input
// (`^[[?2027;3$y^[[?61;…c^[[?1u` at the prompt): a refused attach ends that
// fast, and a terminal behind WSL answers that slowly.
//
// So the window does not end until its terminal has answered. A terminal
// answers in order, so the answer to a DA1 — which every terminal answers —
// comes after the answer to everything asked before it: the window ends once
// a DA1 asked after its last query is answered (settleExit), or once
// exitSettleLimit has passed, for a terminal that never answers.

// exitSettleLimit bounds the wait for the terminal's answers on the way out.
// The wait is two round trips, so the limit is well over two of a slow link's
// (ssh across the world, WSL): a limit that lapsed with answers still in
// flight would leave them for the shell. A terminal that answers in time ends
// the window as soon as it has, so only one that answers no DA1, which is next
// to none, waits it out.
const exitSettleLimit = 2 * time.Second

// exitSettleRounds is how many DA1s the window asks on the way out, each once
// the last is answered. One is not enough: Bubble Tea writes what the window
// asks ahead of the frame on the same beat, so a first frame drawn on the beat
// that carries the first DA1 asks its keyboard query after it. A second, asked
// once the first is answered, goes out on a later beat than that frame.
const exitSettleRounds = 2

// exitSettledMsg is exitSettleLimit passing on the way out.
type exitSettledMsg struct{}

// attributesQuery asks the terminal for its primary device attributes,
// counted so the way out knows whether any answer is still owed.
func (m *Model) attributesQuery() tea.Cmd {
	m.attributesOwed++
	return tea.Raw(ansi.RequestPrimaryDeviceAttributes)
}

// beginExit starts the way out: the window is closed from here (m.quit), and
// the program ends once the terminal has answered, or the limit passes. The
// first DA1 goes out at once, whatever is still owed: its answer comes after
// those anyway.
func (m *Model) beginExit() tea.Cmd {
	m.exitRounds = 1
	return tea.Batch(m.attributesQuery(), tick(exitSettleLimit, func(time.Time) tea.Msg { return exitSettledMsg{} }))
}

// settleExit is the next step out once no DA1 is owed: another DA1 while
// rounds remain, and the end of the program after the last is answered.
func (m *Model) settleExit() tea.Cmd {
	if m.attributesOwed > 0 {
		return nil
	}
	if m.exitRounds == exitSettleRounds {
		return tea.Quit
	}
	m.exitRounds++
	return m.attributesQuery()
}

// updateExiting is the window once it is on its way out: it only waits for
// its terminal's answers, which are read here so they never reach the shell,
// and for its limit. Everything else arrives at a window that has closed.
func (m *Model) updateExiting(msg tea.Msg) tea.Cmd {
	switch msg.(type) {
	case exitSettledMsg:
		return tea.Quit
	}
	if isProbeSettled(msg) {
		return m.settleExit()
	}
	return nil
}
