package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// An update's list flag replaces the list it names, so giving it nothing
// cannot also mean "empty it": an empty value is an entry, and leaving the
// flag out leaves the list as it is. --clear-<flag> is the one way to empty
// one, on every update that has such a flag (cli/DESIGN.md, "Command
// Naming"), and it cannot be given beside the flag it clears.

// clearFlagName is the flag that empties the list flag name.
func clearFlagName(name string) string { return "clear-" + name }

// addClearFlag gives the list flag name, already on cmd, its --clear-<name>.
// The two are refused together by value, not by cobra's mutually exclusive
// group, which counts `--clear-route=false` as given.
func addClearFlag(cmd *cobra.Command, name, usage string) {
	cmd.Flags().Bool(clearFlagName(name), false, usage)
	run := cmd.PreRunE
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if listCleared(cmd.Flags(), name) && cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s and --%s cannot be used together", clearFlagName(name), name)
		}
		if run != nil {
			return run(cmd, args)
		}
		return nil
	}
}

// listCleared reports whether --clear-<name> was given, and true, on a
// command that has it.
func listCleared(flags *pflag.FlagSet, name string) bool {
	cleared, err := flags.GetBool(clearFlagName(name))
	return err == nil && cleared
}
