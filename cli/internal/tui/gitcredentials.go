package tui

import (
	"context"
	"os/exec"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

type credentialQuestion struct {
	remote string
	answer chan credentialAnswer
}
type credentialAnswer struct {
	yes bool
	err error
}
type credentialProcess struct {
	command *exec.Cmd
	answer  chan error
}

func (n narration) credentials() console.CredentialUI {
	return console.CredentialUI{
		Report: n.report,
		Confirm: func(ctx context.Context, remote string) (bool, error) {
			answer := make(chan credentialAnswer, 1)
			select {
			case n.ch <- narrationLine{question: &credentialQuestion{remote: remote, answer: answer}}:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			select {
			case result := <-answer:
				return result.yes, result.err
			case <-ctx.Done():
				return false, ctx.Err()
			}
		},
		Run: func(ctx context.Context, command *exec.Cmd) error {
			answer := make(chan error, 1)
			select {
			case n.ch <- narrationLine{process: &credentialProcess{command: command, answer: answer}}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case err := <-answer:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

func (m *Model) credentialQuestion(msg narrationMsg) tea.Cmd {
	question := msg.question
	if msg.source.gen != m.busyGen {
		question.answer <- credentialAnswer{err: context.Canceled}
		return m.nextNarration(msg.source)
	}
	answer := func(yes bool) tea.Cmd { question.answer <- credentialAnswer{yes: yes}; return nil }
	previous := m.dialog
	m.dialog = actionsDialog("Git credentials", ansi.Strip(question.remote)+" needs credentials to pull on the pool.", []action{{key: "lend", label: sandboxcreate.GitLendLabel, enabled: true}, {key: "push", label: sandboxcreate.GitLocalLabel, enabled: true}}, func(choice string) tea.Cmd { return answer(choice == "lend") })
	m.dialog.onCancel = func() tea.Cmd { question.answer <- credentialAnswer{err: context.Canceled}; return nil }
	m.dialog.choiceDescriptions = []string{sandboxcreate.GitLendExplanation, sandboxcreate.GitLocalExplanation}
	m.dialog.wrapDetails = true
	// The question comes up over whatever the window was showing — the wait
	// on this create, in a window that is one command's own — and answering
	// it puts that back rather than uncovering the list.
	m.dialog.back = previous
	return m.nextNarration(msg.source)
}
