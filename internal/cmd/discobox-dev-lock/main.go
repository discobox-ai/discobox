// discobox-dev-lock runs one development loop of a given name at a time per
// checkout, and refuses to start a second.
//
// `task dev` supervises discobox-server: when the process exits, watchnbuild
// starts a new one. A starting server asks whoever holds the data directory's
// singleton to shut down (ADR 0019), and the displaced server exits 0, which
// its own supervisor reads as a clean exit to restart. Two `task dev` in one
// checkout therefore never settle: each restart displaces the other every retry
// interval, forever, and neither terminal shows anything but a server that
// keeps shutting itself down. Nothing in either loop can detect it — from
// inside, being displaced looks exactly like the rebuild it is meant to look
// like.
//
// So the second loop is stopped before it starts, at the only point where the
// duplicate is still visible as a duplicate. The lock covers one checkout, not
// one machine: two checkouts sharing a data directory still displace each
// other, and are still told apart only by the server's own singleton.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/discobox-ai/discobox/internal/filelock"
)

// lockDir holds the loop locks, under the gitignored scratch directory the rest
// of the dev loop already writes to.
const lockDir = ".tmp/dev"

func main() {
	// The child is left to handle its own interrupt. A terminal delivers Ctrl-C
	// to every process in the foreground group, so watchnbuild already has the
	// signal and is draining on it; forwarding a second one would be read as
	// the "stop now" that kills the process group. Ignoring rather than dying
	// is what keeps the lock held for as long as the loop is actually running.
	signal.Ignore(os.Interrupt, syscall.SIGTERM)

	err := run(os.Args[1:], os.Stdout, os.Stderr)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-dev-lock: "+err.Error())
		os.Exit(1)
	}
}

// run takes the named lock and runs the command under it. The child's output
// streams are arguments so a test can run a child without its output landing in
// the test binary's own.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: discobox-dev-lock <name> <command> [args...]")
	}
	name, command := args[0], args[1:]

	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", lockDir, err)
	}
	path := filepath.Join(lockDir, name+".lock")
	lock, err := filelock.TryAcquire(path)
	if errors.Is(err, filelock.ErrBusy) {
		return fmt.Errorf("%s is already running in this checkout%s.\n"+
			"Stop it before starting another: two of them displace each other's server in a loop",
			name, describeHolder(path))
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	// context.Background(): the child's lifetime is the point. It is stopped by
	// the signal the terminal sends it, not by this process losing patience.
	child := exec.CommandContext(context.Background(), command[0], command[1:]...) //nolint:gosec // The command is this tool's own argv, written in Taskfile.yml.
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, stdout, stderr
	// The child's exit error is returned unwrapped: main turns it into this
	// process's exit code, so a supervised loop that fails is a failure here
	// too rather than a success with a message.
	return child.Run()
}

// describeHolder names the process holding the lock, when it can. Windows
// denies readers a locked file, so there this is empty and the message stands
// without a pid.
func describeHolder(path string) string {
	pid, ok := filelock.HolderPID(path)
	if !ok {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}
