package tui

import (
	"context"
	"io"
)

// terminalAttachExec adapts the CLI terminal attach flow to tea.ExecCommand so
// Bubble Tea releases the alternate screen while the sandbox owns the terminal.
type terminalAttachExec struct {
	ctx       context.Context
	ds        DataSource
	sandboxID string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
}

func (c *terminalAttachExec) SetStdin(r io.Reader)  { c.stdin = r }
func (c *terminalAttachExec) SetStdout(w io.Writer) { c.stdout = w }
func (c *terminalAttachExec) SetStderr(w io.Writer) { c.stderr = w }

func (c *terminalAttachExec) Run() error {
	return c.ds.AttachTerminal(c.ctx, c.sandboxID, c.stdin, c.stdout, c.stderr)
}
