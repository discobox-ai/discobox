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
default terminal already runs one. The leader key then d — Ctrl-A d by default,
and Ctrl-A Ctrl-D works too — detaches without ending the session. Set
DISCOBOX_LEADER to change the Ctrl-A when it collides with what you run.`,
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
			fmt.Fprintf(cmd.ErrOrStderr(), "Attaching to the sandbox's primary terminal (%s to detach)\n", a.detachHint())
			return a.attachSandboxTerminal(cmd.Context(), projectID, sandboxID, primaryExecID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	return cmd
}
