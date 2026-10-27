package tui

import (
	"context"
	"os/exec"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// CredentialUI asks on the active frontend and temporarily returns terminal
// ownership to Git when no credential helper answers.
type CredentialUI struct {
	Confirm func(context.Context, string) (bool, error)
	Run     func(context.Context, *exec.Cmd) error
}

type credentialQuestion struct {
	remote string
	answer chan bool
}
type credentialProcess struct {
	command *exec.Cmd
	answer  chan error
}

func (n narration) credentials() CredentialUI {
	return CredentialUI{
		Confirm: func(ctx context.Context, remote string) (bool, error) {
			answer := make(chan bool, 1)
			select {
			case n.ch <- narrationLine{question: &credentialQuestion{remote: remote, answer: answer}}:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			select {
			case yes := <-answer:
				return yes, nil
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
		question.answer <- false
		return m.nextNarration(msg.source)
	}
	answer := func(yes bool) tea.Cmd { question.answer <- yes; return nil }
	m.dialog = actionsDialog("Git credentials", ansi.Strip(question.remote)+" needs credentials to pull on the pool. Use your Git credentials?", []action{{key: "lend", label: "Use Git credentials for this pull", enabled: true}, {key: "push", label: "Fetch here and push", enabled: true}}, func(choice string) tea.Cmd { return answer(choice == "lend") })
	m.dialog.onCancel = func() tea.Cmd { return answer(false) }
	return m.nextNarration(msg.source)
}
