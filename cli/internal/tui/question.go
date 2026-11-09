package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// questionDialog draws a question the core describes (console.Question): a
// confirm for a yes or no, a menu of its choices otherwise, each choice's hint
// what choosing it means. answer takes the chosen key. A yes or no left any
// way but yes — n, Esc, Enter where Enter means no — is answered no; leaving a
// choice is the dialog's onCancel, which the caller sets.
func questionDialog(q console.Question, answer func(key string) tea.Cmd) *dialog {
	title, body := ansi.Strip(q.Title), ansi.Strip(q.Body)
	if len(q.Choices) == 0 {
		d := confirmDialog(title, body, func(string) tea.Cmd { return answer(console.AnswerYes) })
		d.defaultNo = q.DefaultNo
		d.onCancel = func() tea.Cmd { return answer(console.AnswerNo) }
		return d
	}
	items := make([]action, len(q.Choices))
	hints := make([]string, len(q.Choices))
	for i, choice := range q.Choices {
		items[i] = action{key: choice.Key, label: choice.Label, enabled: true}
		hints[i] = choice.Hint
	}
	d := actionsDialog(title, body, items, answer)
	d.choiceDescriptions = hints
	d.wrapDetails = true
	return d
}

// ask puts a running command's question to the person. One from an
// operation the window has moved on from is declined unasked: nobody is
// waiting on its answer here any more.
func (m *Model) ask(feed narration, asking *console.Asking) tea.Cmd {
	if feed.gen != m.busyGen {
		asking.Decline(context.Canceled)
		return nil
	}
	previous := m.dialog
	m.dialog = questionDialog(asking.Question, func(key string) tea.Cmd { asking.Answer(key); return nil })
	if len(asking.Choices) > 0 {
		// Left unchosen, a pick of one is no answer: the command is told it
		// was dismissed. Every way the card closes comes here.
		m.dialog.onCancel = func() tea.Cmd { asking.Decline(context.Canceled); return nil }
	}
	m.dialog.waitedOn = true
	// The question comes up over whatever the window was showing — the wait
	// on this create, in a window that is one command's own — and answering
	// it puts that back rather than uncovering the list.
	m.dialog.back = previous
	return nil
}
