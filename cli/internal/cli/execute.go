package cli

import "context"

// Execute runs the Discobox CLI root command.
func Execute(ctx context.Context) error {
	return NewRootCommand().ExecuteContext(ctx)
}
