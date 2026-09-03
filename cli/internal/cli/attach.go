package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// newAttachCommand implements `discobox attach`: open a discobox's own screen,
// which is the launcher's window on that one discobox and nothing else. It is
// the everyday shortcut for what pressing Enter on its row in `discobox tui`
// does, and --raw is what `discobox admin terminal attach primary
// --discobox-id ID` spells out in full.
func (a *App) newAttachCommand() *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "attach [DISCOBOX_ID]",
		Short: "Open a discobox: its terminal and what runs beside it",
		Long: `Open a discobox: its primary terminal, and everything running beside it.

Without DISCOBOX_ID the discobox is taken from the ones "discobox ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

The primary terminal is the discobox's default session: the configured harness,
or a plain shell when it has none. Attaching relaunches it with the harness's
relaunch command when it has stopped.

The window is the launcher's, opened on this discobox alone: the primary
terminal, the shells and services running beside it, its forwarded ports, and
the same keys that screen has in "discobox tui". The leader key then d — Ctrl-A
d by default, and Ctrl-A Ctrl-D works too — leaves it, which detaches and exits;
every session keeps running. Set DISCOBOX_LEADER to change the Ctrl-A when it
collides with what you run.

--raw attaches this terminal to the primary terminal directly, with no window
around it. Stdin is always attached, and a PTY is allocated the same way the
discobox's default terminal already runs one, so a pipe, a recording, or a
terminal you would rather keep as it is gets the stream and nothing else. Ctrl-A
d detaches there too. If an interrupt stops getting through — the discobox or
the server has gone quiet — Ctrl-C again says so, and one more quits. The
terminal keeps running, exactly as a detach leaves it. Without a terminal to
draw a window on, attach is raw whether or not the flag was given.`,
		Example: `  discobox attach
  discobox attach sbx_01hq
  discobox attach --raw sbx_01hq`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sandboxArg string
			if len(args) > 0 {
				sandboxArg = args[0]
			}
			projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
			if err != nil {
				return err
			}
			if !raw && canOpenWindow(cmd) {
				// The window is opened on the discobox as the server has it,
				// so its header names the box, its branch and its diff from
				// the first frame rather than filling in when the listing
				// arrives.
				res, err := client.GetSandbox(cmd.Context(), apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
				if err != nil {
					return err
				}
				sandbox, err := expectResponse[apimodel.Sandbox](res)
				if err != nil {
					return err
				}
				box := toTUISandbox(*sandbox)
				return a.runTUI(cmd, "", tui.WithAttach(box))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Attaching to the discobox's primary terminal (%s to detach)\n", a.detachHint())
			return a.attachSandboxTerminal(cmd.Context(), projectID, sandboxID, primaryExecID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "Attach this terminal straight to the discobox's primary terminal instead of opening the window on the discobox")
	return cmd
}
