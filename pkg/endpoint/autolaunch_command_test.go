package endpoint

import "context"

// staticCommand is the LaunchOptions.Command of a program already known.
//
// A test helper rather than exported API: every caller in the repository
// resolves its command when a launch is actually needed (that is what the func
// is for), so a package-level constructor for the already-known case would be
// public surface on the contracts module added for no reason but shorter test
// call sites.
func staticCommand(path string, args ...string) func(context.Context) (Command, error) {
	return func(context.Context) (Command, error) {
		return Command{Path: path, Args: args}, nil
	}
}
