package tui

import (
	"context"
	"os/exec"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

// CredentialUI asks on the active frontend and temporarily returns terminal
// ownership to Git when no credential helper answers.
type CredentialUI struct {
	Report  func(string)
	Confirm func(context.Context, string) (bool, error)
	Run     func(context.Context, *exec.Cmd) error
}

type credentialProcess struct {
	command *exec.Cmd
	answer  chan error
}

func (n narration) credentials() CredentialUI {
	return CredentialUI{
		Report: n.report,
		Confirm: func(ctx context.Context, remote string) (bool, error) {
			return n.confirm(ctx, gitCredentialQuestion(remote))
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

func gitCredentialQuestion(remote string) narrationQuestion {
	return narrationQuestion{
		title: "Git credentials",
		body:  ansi.Strip(remote) + " needs credentials to pull on the pool.",
		choices: []action{
			{key: "lend", label: sandboxcreate.GitLendLabel, enabled: true},
			{key: "push", label: sandboxcreate.GitLocalLabel, enabled: true},
		},
		descriptions: []string{sandboxcreate.GitLendExplanation, sandboxcreate.GitLocalExplanation},
		yes:          "lend",
		cancel:       context.Canceled,
	}
}
