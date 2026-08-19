//go:build !windows

package localpty

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// start opens a pty pair and starts the command on it.
//
// creack/pty owns the part worth owning: the open/grant/unlock/ptsname sequence
// differs per operating system, and StartWithSize is what sets Setsid and
// Setctty — the difference between a terminal and a *controlling* terminal, and
// the whole reason the command is started this way.
func start(ctx context.Context, cmd Command, cols, rows int) (PTY, error) {
	cols, rows = size(cols, rows)

	//nolint:gosec // the program is this one and the arguments are its own
	command := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	command.Env = cmd.Env
	command.Dir = cmd.Dir

	//nolint:gosec // sizes are terminal dimensions
	tty, err := pty.StartWithSize(command, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	p := &unixPTY{tty: tty, cmd: command, waited: make(chan struct{})}
	go p.wait()
	return p, nil
}

// unixPTY is the pty master, with the command running on the other side of it.
type unixPTY struct {
	tty *os.File
	cmd *exec.Cmd

	// waited is closed once the command has been reaped and code is its exit
	// status. One goroutine owns the reaping: os/exec permits a single Wait,
	// and both a command that ends on its own and one the pane closed need the
	// answer.
	waited chan struct{}
	code   int

	closeOnce sync.Once
}

// wait reaps the command and records how it went.
func (p *unixPTY) wait() {
	defer close(p.waited)
	err := p.cmd.Wait()
	switch {
	case p.cmd.ProcessState != nil:
		// -1 for a command killed by a signal, which is what closing the pane
		// does to one that was still running.
		p.code = p.cmd.ProcessState.ExitCode()
	case err != nil:
		p.code = -1
	}
}

// ExitStatus is the command's exit code, once there is one.
func (p *unixPTY) ExitStatus() (int, bool) {
	select {
	case <-p.waited:
		return p.code, true
	case <-time.After(exitGrace):
		return 0, false
	}
}

// Read returns io.EOF where the pty reports EIO.
//
// On Linux, reading a pty master after the last slave descriptor is closed
// fails with EIO rather than returning end of file. That is what a command
// exiting looks like from this side, and reporting it as an error would put
// "read /dev/ptmx: input/output error" on screen every time one finished
// normally.
func (p *unixPTY) Read(b []byte) (int, error) {
	n, err := p.tty.Read(b)
	if err != nil && errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (p *unixPTY) Write(b []byte) (int, error) { return p.tty.Write(b) }

// Resize sets the pty's size, which is what sends the child its SIGWINCH.
func (p *unixPTY) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	//nolint:gosec // sizes are terminal dimensions
	return pty.Setsize(p.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close ends the command and releases the pty. Closing the pty is what unblocks
// a pending Read; the process is signaled first so a command waiting on a key
// does not outlive the pane it was drawn in.
func (p *unixPTY) Close() error {
	p.closeOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.tty.Close()
		<-p.waited
	})
	return nil
}

var (
	_ PTY          = (*unixPTY)(nil)
	_ ExitReporter = (*unixPTY)(nil)
)
