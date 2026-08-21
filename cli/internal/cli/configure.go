package cli

import (
	"github.com/spf13/cobra"

	"github.com/obot-platform/discobox/cli/internal/tui"
)

// newConfigureCommand opens the launcher on its harnesses screen.
//
// It used to be a small inline menu of its own. It is the launcher's screen now
// — same window, same list, same keys — because managing the harnesses and
// running something on one are the same job seen from two ends, and two
// programs with two ideas of what a harness list looks like is one too many.
func (a *App) newConfigureCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "configure",
		Aliases: []string{"config", "conf", "c", "init"},
		Short:   "Enable, disable, and set the default harness",
		Long: `Open the launcher on its harnesses screen.

Move with up/down, then enable or reconfigure the highlighted harness (e), disable
it (d), make it the project default (s), read its whole configuration (v), or
edit one of its files in your editor (f). Enabling hands the terminal to the
harness's own setup and comes back when it exits.

It is the launcher's ` + tui.HarnessesKeyName + ` screen: Esc leaves it for the prompt every
discobox starts from, and Ctrl-C quits.`,
		Example: `  discobox configure
  discobox config`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// No --leader here: this command is about the harnesses, and the
			// environment's leader is already resolved on the App for the
			// panes the screen is one Esc away from.
			return a.runTUI(cmd, "", tui.WithHarnesses())
		},
	}
}
