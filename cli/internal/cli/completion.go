package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (a *App) newCompletionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion script",
		// Named for the binary this is, not for the one it usually is: the
		// latest channel installs the same CLI as `discobox-dev`, and a script
		// telling somebody to write `_discobox` would have them overwrite the
		// completions of the stable install sitting beside it (ADR 0105).
		Long: fmt.Sprintf(`Generate shell completion script.

Examples:
  source <(%[1]s completion bash)
  %[1]s completion zsh > "${fpath[1]}/_%[1]s"
  %[1]s completion fish > ~/.config/fish/completions/%[1]s.fish`, commandName()),
		Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{
			"bash",
			"zsh",
			"fish",
			"powershell",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletion(out)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
	return cmd
}
