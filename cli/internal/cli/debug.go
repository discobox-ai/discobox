package cli

import "github.com/spf13/cobra"

func (a *App) newDebugCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "debug",
		Short:  "Inspect and manage low-level resources",
		Hidden: true,
	}
	cmd.AddCommand(a.newSandboxCommand())
	cmd.AddCommand(a.newSandboxTerminalsCommand())
	cmd.AddCommand(a.newSandboxExecCommand())
	cmd.AddCommand(a.newProviderCommand())
	cmd.AddCommand(a.newWorkerCommand())
	cmd.AddCommand(a.newJobCommand())
	cmd.AddCommand(a.newHarnessCommand())
	cmd.AddCommand(a.newHooksCommand())
	return cmd
}
