package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// actionResult is one argument's entry in what runActionMany writes under
// -o json: the argument as given, and either the ID it was acted on as and
// what happened to it, or why it failed.
type actionResult struct {
	Argument string `json:"argument"`
	ID       string `json:"id,omitempty"`
	Result   string `json:"result,omitempty"`
	Error    string `json:"error,omitempty"`
}

// actionManyJSONHelp is how a command's help says what runActionMany writes
// under -o json.
const actionManyJSONHelp = `With -o json it writes one object, {"results": [...]}, with an entry per
argument in the order given: the "argument", and either the "id" it was acted
on as and its "result", or its "error". A failure still makes the command fail.`

// runActionMany applies one action across several resources,
// reporting each independently so one failure does not hide the rest.
//
// verb names the action ("archive") for a failure, and done is what actually
// happened ("archived") for a success. Neither is always "delete": deleting a
// sandbox archives it (ADR 0022 §2), and telling the user their sandbox was
// deleted when its data is still there and restorable would be a lie in the
// direction that matters. Both are spelled out because English does not derive
// one from the other ("stop", "stopped").
//
// Under -o json stdout is one object, {"results": [...]}, an entry per
// argument in the order given, failures among them, written once every
// argument has run. A failure is still said on stderr and still fails the
// command, so a script that checks only the exit status reads it the same way
// under either format.
func (a *App) runActionMany(cmd *cobra.Command, args []string, resourceName, verb, done string, actOne func(string) (string, error)) error {
	asJSON := a.output == "json"
	results := make([]actionResult, 0, len(args))
	failures := 0
	for _, arg := range args {
		actedID, err := actOne(arg)
		if err != nil {
			failures++
			fmt.Fprintf(cmd.ErrOrStderr(), "failed to %s %s %q: %v\n", verb, resourceName, arg, err)
			results = append(results, actionResult{Argument: arg, Error: err.Error()})
			continue
		}
		if asJSON {
			results = append(results, actionResult{Argument: arg, ID: actedID, Result: done})
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", actedID, done)
	}
	if asJSON {
		// An error can carry what a server or a discobox wrote, so the JSON
		// is escaped for the terminal it is as likely to be read on.
		if err := writeTerminalSafeJSON(cmd.OutOrStdout(), map[string]any{"results": results}); err != nil {
			return err
		}
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
