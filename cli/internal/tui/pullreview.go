package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/review/ui"
)

// A pull request's files changed are discobox-review's window, embedded
// (github.com/discobox-ai/review/ui) over the pull request as GitHub has it
// (DataSource.PullSource): the same diff, folds, split view, syntax and file
// reader as the window a discobox's diff opens, with nothing on disk behind
// it. The source keeps no review and has no working tree, so the window is a
// reader: no comment, approval, checkout or editor is offered.
//
// It is measured between the two commits the tab last read (BaseSHA,
// HeadSHA), so what it shows is what a merge from here would merge. A push the
// tab reads makes the next f open a window over the new head, rather than
// moving the one on screen under the person reading it.

// reviewWindow is the files changed of one pull request at one head.
type reviewWindow struct {
	model ui.Model
	// head is the commit it was opened over.
	head string
	// width and height are what it was last told it has, so a frame that
	// changes neither does not lay it out again.
	width, height int
	// cancel ends its reads, when the window is replaced or its tab closed.
	cancel context.CancelFunc
}

// reviewMsg is a message the window's commands sent, back to the window that
// sent it: one from a window since replaced is dropped.
type reviewMsg struct {
	pane   int
	window *reviewWindow
	msg    tea.Msg
}

// fromReview tags a window's commands so that what they send comes back to it.
func fromReview(pane int, w *reviewWindow, cmd tea.Cmd) tea.Cmd {
	return tagCmd(cmd, func(msg tea.Msg) tea.Msg { return reviewMsg{pane: pane, window: w, msg: msg} })
}

// openReview turns the tab to its files changed, opening the window over the
// head it last read when there is none yet, or the one there is is of an
// older head.
func (m *Model) openReview(p *pane) tea.Cmd {
	v := p.issue
	pull := v.issue.Pull
	v.files = true
	if v.review != nil && v.review.head == pull.HeadSHA {
		return nil
	}
	v.closeReview()
	ctx, cancel := context.WithCancel(m.ctx)
	src, err := m.ds.PullSource(ctx, v.repository, v.number, *pull)
	if err != nil {
		cancel()
		v.files = false
		return m.report(true, "cannot show the files changed: %v", err)
	}
	w := &reviewWindow{
		model: ui.New(src, ui.Options{
			Embedded: true,
			// The source is two commits, which never change: only a push
			// moves the pull request, and that is a new window.
			Poll:     -1,
			Reserved: []string{m.leader(), pullFilesKey, issueReadKey},
			Theme:    reviewTheme(),
		}),
		head:   pull.HeadSHA,
		cancel: cancel,
	}
	v.review = w
	return fromReview(p.id, w, w.model.Init())
}

// closeReview ends the window, if there is one.
func (v *issuePane) closeReview() {
	if v.review != nil {
		v.review.cancel()
		v.review = nil
	}
}

// updateReview hands the window what its own commands sent, and turns the tab
// back to the conversation when it says it is closed.
func (m *Model) updateReview(msg reviewMsg) tea.Cmd {
	p := m.paneByID(msg.pane)
	if p == nil || p.issue == nil || p.issue.review != msg.window {
		return nil
	}
	return m.toReview(p, msg.msg)
}

// toReview is one message for the tab's window: a key, the pointer, or what
// one of its commands sent.
func (m *Model) toReview(p *pane, msg tea.Msg) tea.Cmd {
	w := p.issue.review
	if _, ok := msg.(ui.ClosedMsg); ok {
		p.issue.files = false
		return nil
	}
	next, cmd := w.model.Step(msg)
	w.model = next
	return fromReview(p.id, w, cmd)
}

// reviewKey is a key on the files changed, and reports whether the window took
// it. Two are the tab's, unless the window is taking text: f goes back to the
// conversation, as it came, and r reads the pull request again — the window
// opens over the new head if a push has moved it.
func (m *Model) reviewKey(p *pane, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !p.issue.review.model.Typing() {
		switch keyName(msg) {
		case pullFilesKey:
			p.issue.files = false
			return nil, true
		case issueReadKey:
			return nil, false
		}
	}
	return m.toReview(p, msg), true
}

// reviewView is the window drawn at width by height, from the tab's origin x,
// y on screen, which is what it reads the pointer against.
func (v *issuePane) reviewView(width, height, x, y int) []string {
	w := v.review
	if w.width != width || w.height != height {
		w.model = w.model.SetSize(width, height)
		w.width, w.height = width, height
	}
	w.model = w.model.SetOrigin(x, y)
	return fitRows(strings.Split(w.model.View().Content, "\n"), height)
}

// reviewTheme is the console's palette in the window's terms, so the files
// changed look like the rest of the window rather than like a program run in
// it. The colors the console has none of — a commit's, the history's, a hunk's
// header — are the window's own. A terminal without color gets none either
// way: the renderer reduces every frame to what the terminal can show.
func reviewTheme() ui.Theme {
	return ui.Theme{
		Accent:     lipgloss.Color(colGold),
		TitleText:  lipgloss.Color(colBlack),
		Muted:      lipgloss.Color(colGrey),
		Faint:      lipgloss.Color(colDim),
		Selection:  lipgloss.Color(colSelectedBG),
		Inactive:   lipgloss.Color(colInactive),
		Band:       lipgloss.Color(colHighlightBG),
		BandCursor: lipgloss.Color(colSelectedBG),
		Warn:       lipgloss.Color(colWarn),
		Error:      lipgloss.Color(colErr),
		OK:         lipgloss.Color(colGreen),
	}
}
