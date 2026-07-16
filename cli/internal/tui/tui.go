package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// Run starts the TUI against the given data source and blocks until the user
// quits or the context is canceled. The alternate screen is entered by the
// root view rather than a program option (Bubble Tea v2 controls it per-frame).
func Run(ctx context.Context, ds DataSource) error {
	program := tea.NewProgram(New(ctx, ds), tea.WithContext(ctx))
	_, err := program.Run()
	return err
}
