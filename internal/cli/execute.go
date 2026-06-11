package cli

import "context"

// Execute runs the Disco2 CLI root command.
func Execute(ctx context.Context) error {
	return NewRootCommand().ExecuteContext(ctx)
}
