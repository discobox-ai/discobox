package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newAttachCommand implements `disco attach`: attach this terminal directly to
// a sandbox's primary terminal and nothing else. It is the everyday shortcut
// for what `disco box terminal attach primary --sandbox-id ID` spells out in
// full.
func (a *App) newAttachCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach [SANDBOX_ID]",
		Short: "Attach to a sandbox's primary terminal",
		Long: `Attach this terminal to a sandbox's primary terminal.

Without SANDBOX_ID the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

The primary terminal is the sandbox's default session: the configured harness,
or a plain shell when it has none. Attaching relaunches it with the harness's
relaunch command when it has stopped.

Stdin is always attached, and a PTY is allocated the same way the sandbox's
default terminal already runs one. Ctrl-P Ctrl-Q detaches without ending the
session.`,
		Example: `  disco attach
  disco attach sbx_01hq`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sandboxArg string
			if len(args) > 0 {
				sandboxArg = args[0]
			}
			projectID, sandboxID, _, err := a.selectSandbox(cmd, sandboxArg)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Attaching to the sandbox's primary terminal (Ctrl-P Ctrl-Q to detach)")
			return a.attachSandboxTerminal(cmd.Context(), projectID, sandboxID, primaryExecID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	return cmd
}
