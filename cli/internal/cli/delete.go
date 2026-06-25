package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func runDeleteMany(cmd *cobra.Command, args []string, resourceName string, deleteOne func(string) (string, error)) error {
	failures := 0
	for _, arg := range args {
		deletedID, err := deleteOne(arg)
		if err != nil {
			failures++
			fmt.Fprintf(cmd.ErrOrStderr(), "failed to delete %s %q: %v\n", resourceName, arg, err)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", deletedID)
	}
	if failures > 0 {
		return fmt.Errorf("failed to delete %d %s", failures, pluralize(resourceName, failures))
	}
	return nil
}

func pluralize(value string, count int) string {
	if count == 1 {
		return value
	}
	return value + "s"
}
