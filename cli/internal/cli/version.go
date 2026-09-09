package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCommand is `discobox version`, which prints what `discobox
// --version` prints.
//
// Anything driving a CLI without reading its help reaches for both spellings,
// and one of them answering "unknown command" is a worse trade than a command
// nobody has to be told about. It is hidden rather than listed: --version is
// the spelling the help documents, and a command list is for the things you
// cannot guess.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "version",
		Short:  "Print the Discobox version",
		Hidden: true,
		Args:   cobra.NoArgs,
		// The root's hook resolves the leader key and starts the parent watch,
		// and a version needs neither. What somebody diagnosing a broken
		// environment asks first is what they are running, so an environment
		// this cannot parse — a leader key it does not know, an output format
		// it does not have — must not be what stops them hearing it. cobra runs
		// the closest hook only, so this empty one is how the root's is skipped;
		// cobra answers --version earlier still, before any hook at all.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printVersion(cmd)
		},
	}
}

// printVersion writes what `discobox --version` writes.
//
// Cobra prints the version flag through its defaultVersionFunc, a plain Fprintf
// it keeps in step with its default template by hand rather than by executing
// that template; this is the same line. It matches for as long as the root
// leaves the version template alone, which it does — a SetVersionTemplate would
// have to be reflected here.
func printVersion(cmd *cobra.Command) error {
	root := cmd.Root()
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s version %s\n", root.DisplayName(), root.Version)
	return err
}
