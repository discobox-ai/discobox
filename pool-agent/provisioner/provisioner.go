// Package provisioner provisions a disco-vm machine: it places a tree in the
// machine with disco-vm's copy and runs one command there with its exec (ADR
// 26-10-23-551 §3). The VM provider runs it once per pool machine, the pool
// agent once per sandbox machine, and the server's machine seam for a local
// driver once per sandbox; each brings its own tree and command.
package provisioner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

// outputLimit is how much of each of a command's streams a Result keeps: the
// end of it, which is where a failing command says why.
const outputLimit = 64 << 10

// Plan is what a machine is provisioned with.
type Plan struct {
	// Dir is the guest directory Tree is unpacked into. An entry's path is
	// relative to it, so a tree of absolute guest paths is placed at "/".
	Dir string
	// Tree is a tar stream. It may hold secrets (a key, a credential):
	// nothing of its contents is put in an error.
	Tree io.Reader
	// Command runs once the tree is placed. It must not ask for a TTY, which
	// would merge its stderr into its stdout. Its argv is named in errors, so
	// a secret goes in the tree, never in the command.
	Command machine.ExecRequest
}

// Result is what the command reported: its exit code and the last
// outputLimit bytes of each of its streams.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// ExitError is a command that ran and exited non-zero. Its message names the
// command and its exit code alone; what the command wrote is in the Result
// returned beside it, for the caller to report.
type ExitError struct {
	Argv     []string
	ExitCode int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s exited with status %d", strings.Join(e.Argv, " "), e.ExitCode)
}

// Provision places plan's tree in the machine and runs its command. A placed
// file is overwritten rather than refused, so Provision is as idempotent as
// its command is: running it twice with a command that is a no-op the second
// time leaves the machine as the first left it.
//
// A command that exits non-zero returns its Result with an *ExitError. A
// command that never reported an exit code — ctx ended while it ran, or the
// exec stream broke — returns what it wrote until then, with ExitCode -1 and
// that error, since a hung command's output is where it says what it waits
// on. A tree that fails to place runs nothing, and returns no Result.
func Provision(ctx context.Context, guest machine.Guest, plan Plan) (Result, error) {
	switch {
	case plan.Dir == "":
		return Result{}, errors.New("provision a machine: no directory to place the tree in")
	case plan.Tree == nil:
		return Result{}, errors.New("provision a machine: no tree to place")
	case len(plan.Command.Argv) == 0:
		return Result{}, errors.New("provision a machine: no command to run")
	case plan.Command.TTY:
		return Result{}, errors.New("provision a machine: the command cannot run on a TTY")
	}
	if err := guest.CopyTo(ctx, plan.Dir, plan.Tree); err != nil {
		return Result{}, fmt.Errorf("place the tree in %s: %w", plan.Dir, err)
	}
	var stdout, stderr tailBuffer
	code, err := machine.Run(ctx, guest, plan.Command, nil, &stdout, &stderr)
	result := Result{ExitCode: code, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		result.ExitCode = -1
		return result, fmt.Errorf("run %s: %w", strings.Join(plan.Command.Argv, " "), err)
	}
	if code != 0 {
		return result, &ExitError{Argv: plan.Command.Argv, ExitCode: code}
	}
	return result, nil
}

// tailBuffer keeps the last outputLimit bytes written to it.
type tailBuffer struct {
	buf bytes.Buffer
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= outputLimit {
		b.buf.Reset()
		p = p[len(p)-outputLimit:]
	} else if over := b.buf.Len() + len(p) - outputLimit; over > 0 {
		b.buf.Next(over)
	}
	b.buf.Write(p)
	return n, nil
}

func (b *tailBuffer) Bytes() []byte { return bytes.Clone(b.buf.Bytes()) }
