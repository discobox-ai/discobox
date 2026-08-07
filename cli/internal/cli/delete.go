package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// runActionMany applies one destructive action across several resources,
// reporting each independently so one failure does not hide the rest.
//
// verb is what actually happened, in the past tense, and it is not always
// "deleted": deleting a sandbox archives it (ADR 0022 §2), and telling the user
// their sandbox was deleted when its data is still there and restorable would
// be a lie in the direction that matters.
func runActionMany(cmd *cobra.Command, args []string, resourceName, verb string, actOne func(string) (string, error)) error {
	failures := 0
	for _, arg := range args {
		actedID, err := actOne(arg)
		if err != nil {
			failures++
			fmt.Fprintf(cmd.ErrOrStderr(), "failed to %s %s %q: %v\n", presentTense(verb), resourceName, arg, err)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", actedID, verb)
	}
	if failures > 0 {
		return fmt.Errorf("failed to %s %d %s", presentTense(verb), failures, pluralize(resourceName, failures))
	}
	return nil
}

// presentTense turns the past-tense verb the caller reports success with into
// the one an error message needs ("archived" -> "archive").
func presentTense(verb string) string {
	return strings.TrimSuffix(verb, "d")
}

func pluralize(value string, count int) string {
	if count == 1 {
		return value
	}
	return value + "s"
}
