package cli

import "github.com/spf13/cobra"

func (a *App) newAdminCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Manage advanced Discobox configuration",
	}
	cmd.AddCommand(a.newProjectCommand())
	cmd.AddCommand(a.newSandboxCommand())
	cmd.AddCommand(a.newSandboxTerminalsCommand())
	cmd.AddCommand(a.newSandboxExecCommand())
	cmd.AddCommand(a.newProviderCommand())
	cmd.AddCommand(a.newPoolCommand())
	cmd.AddCommand(a.newJobCommand())
	cmd.AddCommand(a.newHarnessCommand())
	cmd.AddCommand(a.newHooksCommand())
	cmd.AddCommand(a.newServerCommand())
	cmd.AddCommand(a.newSSHKeyCommand())
	cmd.AddCommand(a.newSSHConfigCommand())
	cmd.AddCommand(a.newSSHProxyCommand())
	cmd.AddCommand(a.newIrohIDCommand())
	return cmd
}
