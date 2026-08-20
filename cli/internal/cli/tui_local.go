package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/obot-platform/discobox/cli/internal/localpty"
	"github.com/obot-platform/discobox/cli/internal/tui"
)

// The commands that act on a discobox from this machine — apply — are not
// terminals in the discobox. They run here, over the API.
//
// To draw one in a pane it is given a terminal of its own. The command runs as
// a child process on a local pty sized to the pane, with that pty as its
// controlling terminal, and the pane draws what comes back. Where the pty comes
// from is localpty's problem, and differs by platform; see ADR 0065.
//
// The controlling terminal is the part that matters: anything the command
// starts that reads its keys from the terminal reads them from the pty, and so
// from the pane, rather than from the real terminal out from under the window.
//
// Running the command rather than reimplementing it is the point: what a pane
// shows is `disco apply`, with its own flag defaults and rendering, and not a
// second answer that drifts from the one a shell gives.

// localCommand is a command running on a pty of its own, presented as a
// terminal a pane can draw.
type localCommand struct {
	localpty.PTY
	events chan tui.TerminalEvent
}

// openLocalCommand starts one of this CLI's own commands on a pty.
func (d *apiDataSource) openLocalCommand(ctx context.Context, action tui.Interaction, sandboxID string, cols, rows int) (tui.Terminal, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find this program: %w", err)
	}

	args := d.app.globalFlags()
	args = append(args, string(action), sandboxID)
	pty, err := localpty.Start(ctx, localpty.Command{
		Path: self,
		Args: args,
		// The token goes through the environment rather than the argument list,
		// which every process on the machine can read.
		Env: append(os.Environ(), "DISCOBOX_TOKEN="+d.app.token),
	}, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", action, err)
	}
	return &localCommand{PTY: pty, events: make(chan tui.TerminalEvent)}, nil
}

// globalFlags are the flags this invocation was given, to hand to a child so it
// talks to the same server, project and directory.
func (a *App) globalFlags() []string {
	args := []string{"--server", a.serverURL, "--project", a.projectID}
	if a.source != "" {
		args = append(args, "--chdir", a.source)
	}
	if a.noStart {
		args = append(args, "--no-start")
	}
	return args
}

// Events never fires: there is no connection under a local command to lose.
func (c *localCommand) Events() <-chan tui.TerminalEvent { return c.events }

// ExitStatus passes on how the command ended, which is what lets a finished
// pane say "failed" rather than "finished". It is forwarded by hand because the
// embedded PTY is an interface, and the one it holds is only sometimes an
// ExitReporter.
func (c *localCommand) ExitStatus() (int, bool) {
	reporter, ok := c.PTY.(localpty.ExitReporter)
	if !ok {
		return 0, false
	}
	return reporter.ExitStatus()
}

var _ tui.Terminal = (*localCommand)(nil)
