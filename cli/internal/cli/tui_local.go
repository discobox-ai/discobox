package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/discobox-ai/discobox/cli/internal/localpty"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// localCommand is a command running on a pty of its own, presented as a
// terminal a pane can draw.
type localCommand struct {
	localpty.PTY
	events chan tui.TerminalEvent
}

func (d *apiDataSource) openLocalHarnessConfigure(ctx context.Context, harnessID string, cols, rows int) (tui.Terminal, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find this program: %w", err)
	}
	args := append(d.app.globalFlags(), "admin", "harness", "configure", harnessID)
	pty, err := localpty.Start(ctx, localpty.Command{Path: self, Args: args, Env: append(os.Environ(), "DISCOBOX_TOKEN="+d.app.token)}, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("start harness configure: %w", err)
	}
	return &localCommand{PTY: pty, events: make(chan tui.TerminalEvent)}, nil
}

// globalFlags are the flags this invocation was given, to hand to a child so it
// talks to the same server and project.
func (a *App) globalFlags() []string {
	args := []string{"--server", a.serverURL, "--project", a.projectID}
	if a.autoStart != autoStartServerAuto {
		args = append(args, "--auto-start-server="+string(a.autoStart))
	}
	return args
}

// Events never fires: there is no connection under a local command to lose.
// Repaint has nothing to ask for. A local command has exactly one client — the
// pane that started it — so its pty is the size that pane last set, and there
// is no other attacher whose size could have displaced it. The Ctrl-L that
// called this reaches the command anyway.
func (c *localCommand) Repaint() error { return nil }

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
