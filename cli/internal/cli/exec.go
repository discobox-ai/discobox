package cli

import (
	"github.com/spf13/cobra"
)

// newExecCommand runs one command inside a sandbox and gives the caller its
// stdio, signals, and exit status — the everyday counterpart to the raw,
// fully configurable `disco box exec create`.
func (a *App) newExecCommand() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:   "exec [flags] [--] COMMAND [ARG...]",
		Short: "Run a command in a sandbox",
		Long: `Run a command in a sandbox and stream it to this terminal.

Without --sandbox-id the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Stdin is always attached, and a PTY is allocated only when this terminal is one,
so piping and redirecting behave like a local command. Signals are forwarded to
the remote process, and exec exits with its exit code.`,
		Example: `  disco exec go test ./...
  disco exec --sandbox-id sbx_01hq bash
  disco exec -- ls -la`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, _, err := a.selectSandbox(cmd, sandboxID)
			if err != nil {
				return err
			}
			// A PTY is only correct when every stream this exec touches is one:
			// allocating one for a pipe would echo input back and dress output in
			// escape sequences the consumer never asked for.
			tty := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout()) && isTerminalStream(cmd.ErrOrStderr())
			body, err := createSandboxExecBody(sandboxExecCreateOptions{interactive: true, tty: tty}, args)
			if err != nil {
				return err
			}
			exec, err := a.createSandboxExec(cmd.Context(), projectID, resolvedSandboxID, body)
			if err != nil {
				return err
			}
			if err := a.attachSandboxExec(cmd.Context(), projectID, resolvedSandboxID, exec.ID, true, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			return a.returnSandboxExecStatus(cmd.Context(), projectID, resolvedSandboxID, exec.ID)
		},
	}
	// Stop parsing flags at the first positional argument so the command's own
	// flags belong to it: `disco exec sh -c ...` passes -c through instead of
	// failing on an unknown shorthand.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&sandboxID, "sandbox-id", "", "Sandbox to run in; when omitted, the sandbox started from this directory, or a prompt to pick one")
	_ = cmd.RegisterFlagCompletionFunc("sandbox-id", a.completeSandboxes)
	return cmd
}
