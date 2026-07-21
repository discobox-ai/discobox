package cli

import "github.com/spf13/cobra"

func (a *App) newBoxCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "box",
		Short: "Manage advanced Discobox configuration",
	}
	cmd.AddCommand(a.newSandboxCommand())
	cmd.AddCommand(a.newSandboxTerminalsCommand())
	cmd.AddCommand(a.newSandboxExecCommand())
	cmd.AddCommand(a.newProviderCommand())
	cmd.AddCommand(a.newPoolCommand())
	cmd.AddCommand(a.newJobCommand())
	cmd.AddCommand(a.newHarnessCommand())
	cmd.AddCommand(a.newHooksCommand())
	cmd.AddCommand(a.newServerCommand())
	return cmd
}
