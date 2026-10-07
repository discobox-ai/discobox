package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// runActionMany applies one action across several resources,
// reporting each independently so one failure does not hide the rest.
//
// verb names the action ("archive") for a failure, and done is what actually
// happened ("archived") for a success. Neither is always "delete": deleting a
// sandbox archives it (ADR 0022 §2), and telling the user their sandbox was
// deleted when its data is still there and restorable would be a lie in the
// direction that matters. Both are spelled out because English does not derive
// one from the other ("stop", "stopped").
func runActionMany(cmd *cobra.Command, args []string, resourceName, verb, done string, actOne func(string) (string, error)) error {
	failures := 0
	for _, arg := range args {
		actedID, err := actOne(arg)
		if err != nil {
			failures++
			fmt.Fprintf(cmd.ErrOrStderr(), "failed to %s %s %q: %v\n", verb, resourceName, arg, err)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", actedID, done)
	}
	if failures > 0 {
		return fmt.Errorf("failed to %s %d %s", verb, failures, pluralize(resourceName, failures))
	}
	return nil
}

func pluralize(value string, count int) string {
	if count == 1 {
		return value
	}
	if strings.HasSuffix(value, "x") {
		return value + "es"
	}
	return value + "s"
}
