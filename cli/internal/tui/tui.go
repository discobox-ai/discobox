package tui

import (
	"context"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/discobox-ai/discobox/cli/internal/clipwatch"
)

// Run starts the launcher against the given data source and blocks until the
// user quits or the context is canceled.
//
// The alternate screen is asked for by the view rather than by a program option,
// because Bubble Tea v2 decides it per frame.
//
// A window opened on one run (WithRun) also returns the discobox that run made,
// and nil if the window was left before the create came back: the alternate
// screen takes everything the window showed with it, so the command that opened
// it is what is left to say which discobox it was.
func Run(ctx context.Context, ds DataSource, options ...Option) (*Sandbox, error) {
	model := New(ctx, ds, options...)
	// For as long as the window is open: see Model.clipWatch.
	model.clipWatch = clipwatch.Start(ctx)
	program := tea.NewProgram(model, tea.WithContext(ctx))
	_, err := program.Run()
	// The status the window spoke for its box is the window's to take back:
	// Bubble Tea resets what it set itself, and this is not among it. A
	// window that dies instead is cleared by termguard.
	model.clearProgramStatus(os.Stdout)
	if err != nil {
		return model.made(), err
	}
	// A window opened as an attach ends with whatever ended the attach, so the
	// command that opened it fails the way a plain attach fails. Everything
	// else ends with nothing to say. See Model.exit.
	return model.made(), model.exitErr
}
