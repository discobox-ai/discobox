package cli

import (
	"fmt"
	"io"

	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/spf13/cobra"
)

// readRunPrompt consumes the explicit "-" prompt before any sandbox is made.
// The composer draws on stderr so a detached run can still pipe its output.
func readRunPrompt(cmd *cobra.Command) (string, error) {
	if isTerminalStream(cmd.InOrStdin()) {
		if !isTerminalStream(cmd.ErrOrStderr()) {
			return "", fmt.Errorf("writing a prompt interactively requires a terminal on stderr; pipe the prompt into discobox run SOURCE - instead")
		}
		return tui.ComposePrompt(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr())
	}
	text, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("read prompt from stdin: %w", err)
	}
	return string(text), nil
}
