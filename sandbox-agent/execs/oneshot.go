package execs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OnceRequest is one command to run to completion, outside the exec machinery:
// nothing records it, nothing can attach to it, and no terminal is allocated.
// It lives in this package because running a process as the run user, with that
// user's environment, is this package's to know — the credentials, the
// environment defaults, and the working directory are the ones an exec gets.
type OnceRequest struct {
	// Command is the argv, run directly and never through a shell.
	Command []string
	// Env is the environment the command receives, complete: the caller merges
	// in whatever it wants the process to inherit.
	Env map[string]string
	// Dir is the working directory. Empty starts in the manager's default.
	Dir string
	// MaxOutput bounds what is read from stdout, in bytes.
	MaxOutput int
}

// RunOnce runs the request as the run user and returns its stdout. The command
// gets a TMPDIR of its own, removed when it ends.
//
// stdout is bounded: a command that will not stop talking is stopped at the
// limit and reported as having failed rather than being allowed to fill this
// process's memory. stderr is bounded the same way and, on a failure, is what
// the error says, since that is where a command explains itself.
func (m *Manager) RunOnce(ctx context.Context, req OnceRequest) ([]byte, error) {
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return nil, errors.New("a command to run is required")
	}
	if req.MaxOutput <= 0 {
		return nil, errors.New("a bound on the output is required")
	}
	user, err := m.ResolveUser(CreateRequest{})
	if err != nil {
		return nil, err
	}
	env := EnvWithRuntimeDefaults(MergeEnv(m.env, req.Env), user)
	dir := req.Dir
	if dir == "" {
		if dir, err = m.resolveWorkdir("", HomeDir(user, env)); err != nil {
			return nil, err
		}
	}
	attributes, err := AgentSysProcAttr(user)
	if err != nil {
		return nil, err
	}
	// The command's TMPDIR is a directory of its own, and this removes it
	// however the command ends. The command cannot be trusted to: a
	// cancellation kills its whole process group (below), so a wrapper's own
	// EXIT trap never runs, and whatever it staged — a CLI's state, a copy of
	// its account — would otherwise stay behind, one directory per canceled
	// run.
	scratch, err := os.MkdirTemp("", "discobox-once-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := chownToUser(scratch, user); err != nil {
		return nil, err
	}
	env["TMPDIR"] = scratch
	// Resolved against the environment the command will run in, not this
	// process's. They are not the same PATH — the agent is root and the
	// command runs as the run user — and a name is looked up where the
	// command's own PATH says, or the wrong binary answers to it.
	program, err := lookPath(req.Command[0], env["PATH"])
	if err != nil {
		return nil, err
	}

	//nolint:gosec // The command is the caller's by design, as every exec's is.
	cmd := exec.CommandContext(ctx, program, req.Command[1:]...)
	cmd.Env = envList(env)
	cmd.Dir = dir
	cmd.SysProcAttr = attributes
	// Killing the child alone is not enough to end this. Its output is read
	// into memory rather than through a pipe this process closes, so the wait
	// returns only once every writer is gone — and the command is a wrapper
	// script that starts an agent CLI that starts helpers of its own. Whatever
	// it left behind would hold the wait open long past the deadline, and the
	// caller waiting on that wait would hold whatever it is holding. So the
	// session is killed, and the wait gives up shortly after either way.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killGroup(cmd.Process.Pid)
	}
	cmd.WaitDelay = oneShotWaitDelay
	stdout := &boundedBuffer{limit: req.MaxOutput}
	stderr := &boundedBuffer{limit: oneShotMaxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, &OnceFailure{Program: req.Command[0], Err: err, Stderr: oneLine(stderr.buf.String())}
	}
	if stdout.over {
		return nil, fmt.Errorf("%s printed more than the %d bytes that may be read", req.Command[0], req.MaxOutput)
	}
	return stdout.buf.Bytes(), nil
}

// boundedBuffer keeps what a command printed up to a limit and remembers that
// there was more. Writes past the limit are accepted and dropped rather than
// refused: a command told its output is going nowhere dies of a broken pipe,
// and what is wanted here is the command's own exit status, not that one.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	switch room := b.limit - b.buf.Len(); {
	case room <= 0:
		b.over = b.over || len(p) > 0
	case len(p) > room:
		b.buf.Write(p[:room])
		b.over = true
	default:
		b.buf.Write(p)
	}
	return len(p), nil
}

// lookPath resolves a program name against a PATH of its own.
//
// A name carrying a separator is a path already and is taken as one. Anything
// else is looked for in the PATH's absolute entries only: a relative entry
// would be read here against this process's directory and then run against the
// command's, which are not the same place — that mismatch is the whole of what
// os/exec's ErrDot exists to refuse.
//
// The executable bit is a filter, not a permission: this process is root and
// the command runs as the run user, so what it finds is what would be tried,
// not what will be allowed.
func lookPath(program, path string) (string, error) {
	if strings.ContainsAny(program, `/\`) {
		return program, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, program)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s is not on this command's PATH", program)
}

// OnceFailure is a command that did not succeed.
//
// What it printed on stderr is kept out of the message on purpose. A one-shot
// runs with whatever is in the sandbox's environment, which for the judge is
// the project's own harness credential, and a CLI that fails to authenticate
// prints back what it tried. The message names the command and how it failed;
// a caller that has somewhere safe to put the detail reads Stderr itself.
type OnceFailure struct {
	Program string
	Err     error
	Stderr  string
}

func (e *OnceFailure) Error() string { return e.Program + ": " + e.Err.Error() }

func (e *OnceFailure) Unwrap() error { return e.Err }

// oneShotMaxStderr bounds what a failing command's explanation may be. It is
// small on purpose: what is wanted is the first line of a failure, not a log.
const oneShotMaxStderr = 4 << 10

// oneShotWaitDelay is how long a killed command's output is still read before
// this process stops waiting on it. Long enough for the kill to land, short
// enough that a wedged wrapper cannot hold the judge open.
const oneShotWaitDelay = 2 * time.Second

// oneLine flattens a command's complaint into something an error can carry.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// envList is a process environment in the form exec.Cmd takes.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	return out
}
