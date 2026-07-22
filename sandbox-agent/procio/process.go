// Package procio runs a process and owns its input and output, whether that is
// a PTY or a set of pipes.
//
// It exists as its own package because the details here are subtle in ways that
// are invisible at the call site and expensive to discover in production: which
// end of a pipe each side owns, what a signal name means for a process group
// the kernel treats as orphaned, and what exit status a signal death should
// report. Isolating them makes each one testable with a real process and
// nothing else — no sockets, no attach protocol, no sandbox.
package procio

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// Options describes a process to start. The caller supplies a fully resolved
// environment and SysProcAttr; resolving a sandbox user into credentials is not
// this package's business.
type Options struct {
	Command []string
	Dir     string
	// Env is the complete environment, as exec.Cmd expects it.
	Env         []string
	SysProcAttr *syscall.SysProcAttr
	// TTY starts the process on a pseudo-terminal instead of pipes. Its stdout
	// and stderr are merged by the kernel onto the PTY, so Stderr is nil.
	TTY     bool
	Winsize *pty.Winsize
}

// Process is a started process and the descriptors the parent holds for it.
type Process struct {
	cmd    *exec.Cmd
	tty    *os.File
	stdin  io.WriteCloser
	stdout *os.File
	stderr *os.File
}

// Start launches the process.
func Start(opts Options) (*Process, error) {
	if len(opts.Command) == 0 {
		return nil, os.ErrInvalid
	}
	// The process's lifetime belongs to its owner, which stops it explicitly;
	// it is deliberately not tied to a request context.
	cmd := exec.CommandContext(context.Background(), opts.Command[0], opts.Command[1:]...) //nolint:gosec // the command is caller-supplied by design.
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.SysProcAttr = opts.SysProcAttr

	if opts.TTY {
		return startTTY(cmd, opts.Winsize)
	}
	return startPipes(cmd)
}

func startTTY(cmd *exec.Cmd, winsize *pty.Winsize) (*Process, error) {
	tty, err := pty.StartWithSize(cmd, winsize)
	if err != nil {
		return nil, err
	}
	// One device carries both directions, and the kernel has already merged
	// stdout and stderr onto it before the parent can see them.
	return &Process{cmd: cmd, tty: tty, stdin: tty, stdout: tty}, nil
}

func startPipes(cmd *exec.Cmd) (*Process, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own the output pipes rather than using cmd.StdoutPipe/StderrPipe. Wait
	// closes the pipes it created as soon as the process exits — the os/exec
	// docs call it incorrect to Wait while reads are outstanding — and the
	// owner of a Process waits in parallel with draining it. Pipes created here
	// are never touched by Wait, so a reader always drains to EOF instead of
	// racing a fast command's output away.
	stdout, stdoutIn, err := os.Pipe()
	if err != nil {
		closeAll(stdin)
		return nil, err
	}
	stderr, stderrIn, err := os.Pipe()
	if err != nil {
		closeAll(stdin, stdout, stdoutIn)
		return nil, err
	}
	cmd.Stdout = stdoutIn
	cmd.Stderr = stderrIn
	if err := cmd.Start(); err != nil {
		closeAll(stdin, stdout, stdoutIn, stderr, stderrIn)
		return nil, err
	}
	// The child holds its own descriptors now; drop the parent's copies of the
	// write ends so the readers see EOF when the process exits.
	closeAll(stdoutIn, stderrIn)
	return &Process{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// PID is the process id, which is also its process group id: every process
// starts in a new session (see the caller's SysProcAttr).
func (p *Process) PID() int64 { return int64(p.cmd.Process.Pid) }

// TTY is the PTY master, or nil for a pipe process.
func (p *Process) TTY() *os.File { return p.tty }

// Stdout is the process's output: the PTY for a TTY process, the stdout pipe
// otherwise. It reaches EOF when the process exits.
func (p *Process) Stdout() io.Reader { return p.stdout }

// Stderr is the process's error output, or nil for a TTY process, which has
// none to give: the kernel merged it onto the PTY.
func (p *Process) Stderr() io.Reader {
	if p.stderr == nil {
		return nil
	}
	return p.stderr
}

// WriteInput writes to the process's stdin.
func (p *Process) WriteInput(payload []byte) (int, error) { return p.stdin.Write(payload) }

// CloseInput closes stdin so a command reading to EOF terminates. It is a no-op
// for a TTY process, whose input side is the terminal itself and must stay open.
func (p *Process) CloseInput() {
	if p.tty != nil || p.stdin == nil {
		return
	}
	_ = p.stdin.Close()
}

// Close releases every descriptor the parent holds.
func (p *Process) Close() {
	if p.tty != nil {
		closeAll(p.tty)
		return
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	closeAll(p.stdout, p.stderr)
}

// Status is how a process ended.
type Status struct {
	// ExitCode follows the shell convention: the process's own status, or
	// 128+signum when a signal killed it.
	ExitCode int64
	// Err is the error Wait reported, if any. A non-zero exit is an error here
	// as well as a code.
	Err error
}

// Wait blocks until the process exits.
func (p *Process) Wait() Status {
	err := p.cmd.Wait()
	status := Status{Err: err}
	if state := p.cmd.ProcessState; state != nil {
		status.ExitCode = exitCodeFromState(state)
	}
	return status
}

// Signal delivers a named signal to the process group.
func (p *Process) Signal(name string) error { return signalProcessGroup(p.cmd, name) }

// Terminate asks the process group to stop.
func (p *Process) Terminate() { terminateProcessGroup(p.cmd) }

// Resize sets the PTY size. It is a no-op for a pipe process.
func (p *Process) Resize(rows, cols uint16) error {
	if p.tty == nil {
		return nil
	}
	return pty.Setsize(p.tty, &pty.Winsize{Rows: rows, Cols: cols})
}

func closeAll(files ...io.Closer) {
	for _, file := range files {
		if file == nil {
			continue
		}
		_ = file.Close()
	}
}
