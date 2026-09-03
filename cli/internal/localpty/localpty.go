// Package localpty runs one of this CLI's own commands on a pseudo-terminal of
// its own, for the launcher to draw in a pane. See ADR 0065.
//
// The pty is the point. It is what gives the child a controlling terminal, so
// anything the command starts that reads its keys from the terminal — git, a
// pager, a credential prompt — reads them from the pane rather than from the
// real terminal out from under the window. It is also what makes the child
// believe it is on a terminal at all, so what it prints is what a shell would
// show.
//
// The two platforms do not share a mechanism. Unix opens a pty pair and hands
// the slave to a child started through os/exec; Windows creates a pseudoconsole
// and a pair of pipes, and must call CreateProcess itself because a
// pseudoconsole is attached through a thread attribute that os/exec cannot
// carry. What they share is this interface, which is everything the pane needs
// and nothing else.
package localpty

import (
	"context"
	"io"
	"os"
	"strconv"
	"time"
)

// PTY is a command running on a pseudo-terminal of its own.
//
// Reads are the command's output, writes are its keys, and both end when the
// command does: a Read after it exits reports io.EOF rather than the platform's
// own word for a terminal whose other side has gone.
//
// It is deliberately not an *os.File. Windows reads and writes through two
// separate pipes, so there is no single file to be, and the one consumer —
// the launcher's pane — needs no more than this.
type PTY interface {
	io.ReadWriteCloser

	// Resize sets the terminal size, which is what tells the command its window
	// changed. Sizes that are not positive are ignored rather than refused: a
	// window mid-layout is not an error.
	Resize(cols, rows int) error
}

// ExitReporter is a PTY that can say how its command ended.
//
// It is a second interface rather than part of PTY because what draws these
// also draws terminals that are not local commands — a session inside a
// discobox ends when the session ends, which is not a result anyone ran for.
// Both implementations here are one.
type ExitReporter interface {
	// ExitStatus is the command's exit code, and whether it has ended.
	//
	// It is asked at end of file, which is the output stopping rather than the
	// process being reaped: those are two events, and the caller is between
	// them. So it waits briefly for the second rather than answering that a
	// command that has plainly finished is still running.
	ExitStatus() (code int, done bool)
}

// exitGrace is how long ExitStatus waits for that second event. It is the gap
// between a command's last byte and its exit being collected here, which is
// microseconds when it happens at all.
const exitGrace = 250 * time.Millisecond

// Command is a program to run on a pty.
//
// It is not an *exec.Cmd because the Windows implementation cannot use os/exec
// at all (see the package doc), and a struct that borrowed exec.Cmd's shape
// would be promising fields it does not honor. These four are what the launcher
// needs and what both platforms can deliver.
type Command struct {
	// Path is the program, as an absolute path.
	Path string
	// Args are the arguments after the program name — exec.Cmd's Args without
	// its leading argv[0], not os.Args.
	Args []string
	// Env is the child's complete environment, in os/exec's KEY=VALUE shape.
	// Empty inherits this process's, as exec.Cmd's nil Env does.
	Env []string
	// Dir is the working directory. Empty is this process's.
	Dir string
}

// ParentPIDEnv names the process a command started here must not outlive. The
// child reads it and ends itself once that process is gone.
//
// Both of the ways a pane ends its command — canceling ctx, closing the PTY —
// run inside this process, so neither happens if this process dies without
// unwinding: a crash, a SIGKILL, a terminal closed out from under a window that
// did not get to its deferred Close. The child is started with Setsid and a pty
// of its own, which is the point of this package and also means nothing the
// operating system does on its own reaches it — no SIGHUP from the real
// terminal, no process group to be killed with. It is left running with a pty
// nobody holds, and on a command that retries (a terminal attach reconnecting)
// it stays that way indefinitely. This is the lifeline that ends it.
const ParentPIDEnv = "DISCOBOX_PARENT_PID"

// Start runs cmd on a pty sized to cols by rows, with the pty as the command's
// controlling terminal.
//
// Canceling ctx ends the command, as it does for exec.CommandContext. So does
// closing the returned PTY, which is the launcher's way of dismissing a pane
// whose command is still running. Neither survives this process dying
// abruptly, which is what ParentPIDEnv is for; it is set here rather than by
// each caller so that a child started on a pane's pty cannot be given one
// without it.
//
// The error is worth showing a user: a platform that has no pseudo-terminal to
// give says so here, and it is the only place that knows why.
func Start(ctx context.Context, cmd Command, cols, rows int) (PTY, error) {
	cmd.Env = withParentPID(cmd.Env)
	return start(ctx, cmd, cols, rows)
}

// withParentPID adds this process's ID to a child environment.
//
// An empty Env means "inherit this process's" (see Command.Env), so it has to
// be materialized before anything can be added to it — appending to nil would
// hand the child that one variable and nothing else.
func withParentPID(env []string) []string {
	if len(env) == 0 {
		env = os.Environ()
	}
	return append(env, ParentPIDEnv+"="+strconv.Itoa(os.Getpid()))
}

// defaultSize is what a pty is opened at when the caller has not laid out yet.
// A terminal that starts at zero draws itself wrong before anything can correct
// it, and every platform's pty rejects or mangles a zero dimension in its own
// way.
const (
	defaultCols = 80
	defaultRows = 24
)

func size(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = defaultCols
	}
	if rows <= 0 {
		rows = defaultRows
	}
	return cols, rows
}
