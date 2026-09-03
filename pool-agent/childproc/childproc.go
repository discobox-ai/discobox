// Package childproc owns this process's children: it starts every subprocess
// the pool agent runs, and it reaps the ones nothing is waiting for.
//
// The two halves are one thing because they are one kernel resource. The pool
// agent is PID 1 in its container, so orphans re-parent to it and something has
// to collect them; it also runs git and chown through os/exec, and a reaper
// that calls wait4(-1) collects those too. The loser of that race gets no
// answer at all: the kernel has already released the child, so os/exec's Wait
// returns "waitid: no child processes" over output that arrived and a command
// that succeeded. A caller reading that as "the command failed" then acts on a
// fact that is not true — which is how a stolen `git remote get-url` turned
// into a sandbox stuck in error (ADR 0087).
//
// So a child is either owned — started here, and its owner will wait for it —
// or it is not, and the reaper takes it. Nothing else may start a subprocess in
// this process: an exec.Cmd started directly is a child the reaper cannot tell
// from an orphan.
package childproc

import (
	"bytes"
	"errors"
	"os/exec"
	"sync"
)

// children is the set of pids this process is waiting for. It is package state
// because what it describes is: the kernel keeps one child set per process, and
// a registry that could be instantiated twice would describe half of it.
var children = struct {
	mu    sync.Mutex
	owned map[int]uint64
	next  uint64
}{owned: map[int]uint64{}}

// Child is a started subprocess this process owns.
type Child struct {
	cmd   *exec.Cmd
	token uint64
}

// Start starts cmd and records it as owned, so the reaper leaves its exit
// status to the Wait below.
//
// The registry lock is held across the fork: a child that existed before it was
// recorded is exactly the window the reaper's ownership check has to be free
// of, and the cost is the length of one fork+exec.
func Start(cmd *exec.Cmd) (*Child, error) {
	children.mu.Lock()
	defer children.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	children.next++
	token := children.next
	children.owned[cmd.Process.Pid] = token
	return &Child{cmd: cmd, token: token}, nil
}

// Wait waits for the command and releases its record.
//
// The release is by token rather than by pid, because the kernel may hand the
// pid to the next child started here before this runs, and deleting that
// child's record would hand it to the reaper.
func (c *Child) Wait() error {
	err := c.cmd.Wait()
	children.mu.Lock()
	defer children.mu.Unlock()
	if c.cmd.Process != nil && children.owned[c.cmd.Process.Pid] == c.token {
		delete(children.owned, c.cmd.Process.Pid)
	}
	return err
}

// Run starts cmd and waits for it, the way exec.Cmd.Run does.
func Run(cmd *exec.Cmd) error {
	child, err := Start(cmd)
	if err != nil {
		return err
	}
	return child.Wait()
}

// CombinedOutput runs cmd and returns its interleaved stdout and stderr, the
// way exec.Cmd.CombinedOutput does.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("childproc: Stdout already set")
	}
	if cmd.Stderr != nil {
		return nil, errors.New("childproc: Stderr already set")
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := Run(cmd)
	return out.Bytes(), err
}
