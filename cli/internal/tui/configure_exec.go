package tui

import (
	"context"
	"io"
)

// configureExec adapts a harness configure run to tea.ExecCommand so the root
// can suspend the TUI (releasing the alternate screen) and hand the real
// terminal to the harness's interactive configure flow, resuming when it exits.
type configureExec struct {
	ctx       context.Context
	ds        DataSource
	harnessID string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
}

func (c *configureExec) SetStdin(r io.Reader)  { c.stdin = r }
func (c *configureExec) SetStdout(w io.Writer) { c.stdout = w }
func (c *configureExec) SetStderr(w io.Writer) { c.stderr = w }

func (c *configureExec) Run() error {
	return c.ds.ConfigureHarness(c.ctx, c.harnessID, c.stdin, c.stdout, c.stderr)
}
