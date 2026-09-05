package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// versionRequested reports whether this invocation is the bare word `discobox
// version` rather than a run prompted with it.
//
// The bare command takes any word as a prompt (see newRootCommand), so without
// this the word "version" is a discobox prompted "version": a sandbox and a
// whole agent, for a question that has a one-line answer. Anything driving the
// CLI without reading its help reaches for both spellings, and one of them
// costing a run is not a trade worth keeping.
//
// The carve-out is the smallest one that answers that: the single word, alone,
// with nothing a run takes. `discobox version bump the go modules` and
// `discobox version -d` are still the prompts they always were, so what ADR
// 0089 bought stays bought — only the one-word prompt "version" is gone, and no
// spelling of this could have kept it.
func versionRequested(flags *pflag.FlagSet, args []string) bool {
	return len(args) == 1 && args[0] == "version" && !runRequested(flags, nil)
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
