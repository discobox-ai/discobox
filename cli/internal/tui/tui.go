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
	model := New(ctx, ds, options...)
	program := tea.NewProgram(model, tea.WithContext(ctx))
	if _, err := program.Run(); err != nil {
		return err
	}
	// A window opened as an attach ends with whatever ended the attach, so the
	// command that opened it fails the way a plain attach fails. Everything
	// else ends with nothing to say. See Model.exit.
	return model.exitErr
}
