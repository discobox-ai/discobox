package tui

import (
	"context"
	"io"
)

// interactExec adapts a terminal-owning action to tea.ExecCommand, so Bubble
// Tea releases the terminal — the alternate screen, raw mode, its own input
// reader, the renderer — for as long as the action runs, and takes it back when
// the action returns. The action therefore runs on the screen the window was
// started from, and the window comes back over the top of it.
//
// The streams are the real terminal's, handed over by the runtime: an attach
// wants a TTY on both ends, and a pager wants one to page onto.
type interactExec struct {
	ctx    context.Context
	ds     DataSource
	action Interaction
	ids    []string

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (c *interactExec) SetStdin(r io.Reader)  { c.stdin = r }
func (c *interactExec) SetStdout(w io.Writer) { c.stdout = w }
func (c *interactExec) SetStderr(w io.Writer) { c.stderr = w }

func (c *interactExec) Run() error {
	return c.ds.Interact(c.ctx, c.action, c.ids, c.stdin, c.stdout, c.stderr)
}
