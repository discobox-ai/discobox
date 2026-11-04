package cli

import (
	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// newConfigureCommand opens the launcher on its harnesses screen. There is no
// setup a harness has to be put through first (ADR 26-11-02-152): this is
// where its default, its saved files and its reset live.
//
// It is the launcher's screen rather than a small inline menu of its own —
// same window, same list, same keys — because managing the harnesses and
// running something on one are the same job seen from two ends, and two
// programs with two ideas of what a harness list looks like is one too many.
func (a *App) newConfigureCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "configure",
		Aliases: []string{"config"},
		Short:   "Set the default harness and manage what is saved to each",
		Long: `Open the console on its harnesses screen.

Move with up/down, then make the highlighted harness the project default (s),
read its whole configuration (v), edit one of its files in your editor (f), or
reset it to its bundle's defaults (r), which forgets the settings and logins
saved to it from discoboxes.

Every harness can run without any setup here: sign in and change settings
inside a discobox the way you would on your own machine, then save the change
from the console, which makes it the default for every discobox on that harness.

It is the console's ` + tui.HarnessesKeyName + ` screen: Esc leaves it for the prompt every
discobox starts from, and Ctrl-C quits.`,
		Example: `  discobox configure
  discobox config`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// No --leader here: this command is about the harnesses, and the
			// environment's leader is already resolved on the App for the
			// panes the screen is one Esc away from.
			return a.runConsole(cmd, "", tui.WithHarnesses())
		},
	}
}
