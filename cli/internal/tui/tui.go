package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// Run starts the launcher against the given data source and blocks until the
// user quits or the context is canceled.
//
// The alternate screen is asked for by the view rather than by a program option,
// because Bubble Tea v2 decides it per frame.
func Run(ctx context.Context, ds DataSource, options ...Option) error {
	program := tea.NewProgram(New(ctx, ds, options...), tea.WithContext(ctx))
	_, err := program.Run()
	return err
}
