package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"

	"github.com/obot-platform/discobox/cli/internal/tui"
)

// The commands that read a discobox and print — diff and status — are not
// terminals in the discobox. They run here: resolving the base over the exec
// API, pulling the patch, rendering it, and paging it with your own pager.
//
// To draw one in a pane it is given a terminal of its own. `disco diff` runs as
// a child process on a local pty sized to the pane, with that pty as its
// controlling terminal, and the pane draws what comes back.
//
// The controlling terminal is the part that matters. A pager reads its keys from
// /dev/tty rather than from stdin, which is where the patch is arriving — so
// without one, `less` would take its keys from the real terminal, out from under
// the window that is drawing it. With one, /dev/tty is the pty, and the pager is
// simply an application in the pane like any other.
//
// Running the command rather than reimplementing it is the point: what a pane
// shows is `disco diff`, with its rendering, its base resolution and its pager,
// and not a second answer that drifts from the one a shell gives.

// localCommand is a command running on a pty of its own, presented as a
// terminal a pane can draw.
type localCommand struct {
	tty    *os.File
	cmd    *exec.Cmd
	events chan tui.TerminalEvent

	closeOnce sync.Once
}

// openLocalCommand starts one of this CLI's own commands on a pty.
func (d *apiDataSource) openLocalCommand(ctx context.Context, action tui.Interaction, sandboxID string, cols, rows int) (tui.Terminal, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find this program: %w", err)
	}

	args := d.app.globalFlags()
	args = append(args, string(action), sandboxID)
	//nolint:gosec // the program is this one, and the arguments are its own flags
	command := exec.CommandContext(ctx, self, args...)
	// The token goes through the environment rather than the argument list,
	// which every process on the machine can read.
	command.Env = append(os.Environ(), "DISCOBOX_TOKEN="+d.app.token)

	term, err := startOnPTY(command, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", action, err)
	}
	return term, nil
}

// startOnPTY runs a command on a pty of its own, sized to the pane it is going
// into, and presents it as a terminal.
//
// pty.StartWithSize is what makes the pty the command's controlling terminal,
// which is the point: a pager reads its keys from /dev/tty, and without one it
// would take them from the real terminal instead of from the pane.
func startOnPTY(command *exec.Cmd, cols, rows int) (tui.Terminal, error) {
	//nolint:gosec // sizes are terminal dimensions
	tty, err := pty.StartWithSize(command, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &localCommand{tty: tty, cmd: command, events: make(chan tui.TerminalEvent)}, nil
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

// Read returns io.EOF where the pty reports EIO.
//
// On Linux, reading a pty master after the last slave descriptor is closed
// fails with EIO rather than returning end of file. That is what a command
// exiting looks like from this side, and reporting it as an error would put
// "read /dev/ptmx: input/output error" on screen every time one finished
// normally.
func (c *localCommand) Read(p []byte) (int, error) {
	n, err := c.tty.Read(p)
	if err != nil && errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}
func (c *localCommand) Write(p []byte) (int, error) { return c.tty.Write(p) }

// Resize sets the pty's size, which is what sends the child its SIGWINCH.
func (c *localCommand) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	//nolint:gosec // sizes are terminal dimensions
	return pty.Setsize(c.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close ends the command and releases the pty. Closing the pty is what unblocks
// a pending Read; the process is signaled first so a pager waiting on a key
// does not outlive the pane it was drawn in.
func (c *localCommand) Close() error {
	c.closeOnce.Do(func() {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.tty.Close()
		_ = c.cmd.Wait()
	})
	return nil
}

// Events never fires: there is no connection under a local command to lose.
func (c *localCommand) Events() <-chan tui.TerminalEvent { return c.events }

var _ tui.Terminal = (*localCommand)(nil)
