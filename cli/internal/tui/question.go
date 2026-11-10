package tui

import (
	"context"
	"strconv"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// questionDialog draws a question the core describes (console.Question): a
// confirm for a yes or no, a menu of its choices otherwise, under its subject.
// answer takes the chosen key. A yes or no left any way but yes — n, Esc,
// Enter where Enter means no — is answered no; leaving a choice is the
// dialog's onCancel, which the caller sets.
//
// A pick of one between a yes and a no in words of their own (each choice's
// Means: the create's questions about the working tree) is its two rows with
// what each means beside it, y and n pressing them. Any other pick has each
// choice's hint under the menu while it is chosen, and its first nine
// choices are pressed by their place, 1 to 9.
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
	yesNo := true
	for i, choice := range q.Choices {
		items[i] = action{key: choice.Key, label: choice.Label, enabled: true}
		if i < 9 {
			items[i].press = strconv.Itoa(i + 1)
		}
		hints[i] = choice.Hint
		yesNo = yesNo && choice.Means != ""
	}
	if yesNo {
		for i, choice := range q.Choices {
			items[i].press = map[string]string{console.AnswerYes: "y", console.AnswerNo: "n"}[choice.Means]
			items[i].detail = choice.Hint
		}
		d := actionsDialog(title, body, items, answer)
		d.subject = q.Subject
		return d
	}
	d := actionsDialog(title, body, items, answer)
	d.subject = q.Subject
	d.choiceDescriptions = hints
	d.wrapDetails = true
	return d
}

// ask puts a running command's question to the person. One from an
// operation the window has moved on from is declined unasked: nobody is
// waiting on its answer here any more. A create's own questions — the ones
// asked before it is sent (console.Create) — never are: the window has not
// moved on from a create while it waits (createWatch), whatever has taken the
// busy line since, and declining one of them answers it. What the create asks
// once it is sending (Git's credential consent, delivering the source) is
// declined like any stale feed's: it is consent, not a create's question.
func (m *Model) ask(feed narration, asking *console.Asking) tea.Cmd {
	createAsks := feed.create != nil && !feed.create.sending
	if feed.gen != m.busyGen && !createAsks {
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
	if asking.Measure != "" {
		return m.measureFor(m.dialog, asking.Measure)
	}
	return nil
}

// measureFor counts a directory a question turns on (Question.Measure) while
// the question is up, saying the count on it as it climbs (directorySized).
// It is counted while the question is up rather than before it: a home
// directory takes long enough to walk that waiting for the number would be
// the thing the question exists to avoid.
func (m *Model) measureFor(d *dialog, dir string) tea.Cmd {
	m.endDirectorySize()
	total, stop := m.ds.MeasureDirectory(m.ctx, dir)
	m.copySize, m.copyStop, m.copyDialog = total, stop, d
	d.emphasis = total().Said()
	return m.pollDirectorySize()
}
