// Package processhelper runs child processes behind a small stdio proxy helper.
package processhelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const entryArg = "__discobox_process_helper"

const defaultGracePeriod = 2 * time.Second

// CommandOptions describes a child process to launch through the helper entry.
type CommandOptions struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
	Grace   time.Duration
}

// CommandContext returns a command that re-execs the current binary as a helper.
//
// The returned command's stdin is the child stdin stream and parent liveness
// monitor. If the parent exits, the pipe closes, and the helper terminates the
// child after giving it a short grace period.
func CommandContext(ctx context.Context, opts CommandOptions) (*exec.Cmd, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return nil, fmt.Errorf("command is required")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	grace := opts.Grace
	if grace <= 0 {
		grace = defaultGracePeriod
	}
	args := []string{entryArg, "--grace-ms", strconv.FormatInt(grace.Milliseconds(), 10), "--", opts.Command}
	args = append(args, opts.Args...)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	configureHelperCommand(cmd)
	return cmd, nil
}

// HandleEntry runs the helper when args begin with the helper entry argument.
// Call this at the top of main before ordinary CLI parsing.
func HandleEntry(args []string, stdin io.Reader, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] != entryArg {
		return false, 0
	}
	code := runEntry(args[1:], stdin, stdout, stderr)
	return true, code
}

func runEntry(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	grace := defaultGracePeriod
	if len(args) >= 2 && args[0] == "--grace-ms" {
		ms, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || ms < 0 {
			fmt.Fprintf(stderr, "invalid process helper grace period %q\n", args[1])
			return 2
		}
		grace = time.Duration(ms) * time.Millisecond
		args = args[2:]
	}
	if len(args) == 0 || args[0] != "--" || len(args) < 2 {
		fmt.Fprintln(stderr, "invalid process helper invocation")
		return 2
	}
	childArgs := args[1:]
	//nolint:gosec // The helper only re-launches commands supplied by the already-running parent process.
	cmd := exec.Command(childArgs[0], childArgs[1:]...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureChildCommand(cmd)
	childStdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(stderr, "open child stdin: %v\n", err)
		return 126
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "start child: %v\n", err)
		return 126
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	copyCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(childStdin, stdin)
		_ = childStdin.Close()
		copyCh <- err
	}()

	select {
	case err := <-waitCh:
		return exitCode(err)
	case err := <-copyCh:
		if err != nil && !errors.Is(err, os.ErrClosed) {
			fmt.Fprintf(stderr, "copy child stdin: %v\n", err)
		}
		return terminateAndWait(cmd, waitCh, grace)
	}
}

func terminateAndWait(cmd *exec.Cmd, waitCh <-chan error, grace time.Duration) int {
	if cmd.Process != nil {
		_ = terminateProcess(cmd.Process)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waitCh:
		return exitCode(err)
	case <-timer.C:
		if cmd.Process != nil {
			_ = killProcess(cmd.Process)
		}
	}
	select {
	case err := <-waitCh:
		return exitCode(err)
	case <-time.After(grace):
		return 124
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
