package dockercache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// maxScanBytes bounds how much build output is retained for failure
// classification. The signatures we match are emitted next to the failure at
// the end of a build, so a tail is sufficient and a long build cannot grow
// this without bound.
const maxScanBytes = 64 << 10

// lockRetryDelay is how long to wait before re-running a build whose cache
// export lost the index.json flock. BuildKit holds that lock only for the
// final index write, so the contention window is short.
const lockRetryDelay = 2 * time.Second

// Run executes a user's docker command line and returns the exit code to use.
//
// Commands this shim did not modify are exec'd directly, so the overwhelmingly
// common case (every non-build docker command) keeps the original process,
// stdio, and signal behavior with no wrapper in the data path at all. Only a
// build we added cache flags to is run as a child, because only then is there
// something worth retrying.
func Run(args []string, home string) int {
	a := Rewrite(args, home)
	if !a.Injected {
		return execDirect(a.Argv)
	}

	code, out := runRelayed(a.Argv)
	if code == 0 {
		return code
	}

	// Retry only what this shim caused. A failing Dockerfile must never be
	// run twice: every action below is triggered by a BuildKit infrastructure
	// signature, not by a nonzero exit alone.
	switch classify(out) {
	case retryPrune:
		notice("build cache is corrupt; pruning and retrying")
		_ = execQuiet(RealDocker, "builder", "prune", "-f")
	case retryWithoutExport:
		notice("cache export failed; retrying without it")
		a.Argv = stripCacheTo(a.Argv)
	case retryBackoff:
		notice("another build is writing the shared cache; retrying")
		time.Sleep(lockRetryDelay)
	default:
		return code
	}

	code, _ = runRelayed(a.Argv)
	return code
}

// execDirect replaces this process with the real docker CLI.
func execDirect(argv []string) int {
	//nolint:gosec // Handing this process's own argv to the real docker CLI is the entire point of the shim.
	if err := syscall.Exec(argv[0], argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "discobox-docker: exec %s: %v\n", argv[0], err)
		return 127
	}
	return 0 // unreachable: Exec only returns on failure
}

// runRelayed runs argv, returning its exit code and a tail of its stderr.
//
// stdout is inherited as-is rather than relayed: `docker build -q` writes the
// image ID there and callers capture it, so those bytes must stay exactly what
// docker wrote, on their own stream. Only stderr is intercepted, which is
// where BuildKit writes progress and errors.
func runRelayed(argv []string) (int, string) {
	// context.Background(): the shim is a one-shot CLI process with nothing to
	// cancel it, and the child owns the terminal until the user's build ends.
	//nolint:gosec // Running this process's own argv as the real docker CLI is the entire point of the shim.
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout

	tail := &tailBuffer{max: maxScanBytes}

	// buildx picks its progress renderer from whether *stderr* is a terminal.
	// Handing the child a pipe would silently downgrade every build to plain
	// output, so when the real stderr is a terminal the child gets a pty
	// instead and sees a terminal too.
	var relayDone chan struct{}
	if size, err := pty.GetsizeFull(os.Stderr); err == nil {
		master, slave, openErr := pty.Open()
		if openErr != nil {
			return runPiped(cmd, tail)
		}
		_ = pty.Setsize(master, size)
		cmd.Stderr = slave

		// Deliberately no Setsid/Setctty: the child keeps this process's
		// controlling terminal and process group, so a user's Ctrl-C reaches
		// the build directly and needs no forwarding. The pty supplies
		// terminal *semantics* on fd 2 only.
		if err := cmd.Start(); err != nil {
			_ = master.Close()
			_ = slave.Close()
			fmt.Fprintf(os.Stderr, "discobox-docker: %v\n", err)
			return 1, ""
		}
		// The parent must drop its handle on the slave, or reads from the
		// master never end when the child exits.
		_ = slave.Close()

		stopResize := watchResize(master)

		relayDone = make(chan struct{})
		go func() {
			defer close(relayDone)
			// A closed pty master reports EIO rather than EOF on Linux; the
			// copy simply ends either way.
			_, _ = io.Copy(io.MultiWriter(os.Stderr, tail), master)
		}()

		code := wait(cmd)
		_ = master.Close()
		<-relayDone
		stopResize()
		return code, tail.String()
	}

	return runPiped(cmd, tail)
}

// runPiped is the non-terminal path: stderr is already not a tty, so progress
// is plain regardless and a pipe changes nothing the user would see.
func runPiped(cmd *exec.Cmd, tail *tailBuffer) (int, string) {
	r, w, err := os.Pipe()
	if err != nil {
		cmd.Stderr = os.Stderr
		return wait(start(cmd)), ""
	}
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		fmt.Fprintf(os.Stderr, "discobox-docker: %v\n", err)
		return 1, ""
	}
	_ = w.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.MultiWriter(os.Stderr, tail), r)
	}()
	code := wait(cmd)
	<-done
	_ = r.Close()
	return code, tail.String()
}

func start(cmd *exec.Cmd) *exec.Cmd {
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "discobox-docker: %v\n", err)
	}
	return cmd
}

// wait returns the child's exit code, mapping death-by-signal to the shell
// convention so a Ctrl-C'd build is not reported as success.
func wait(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "discobox-docker: %v\n", err)
	return 1
}

func execQuiet(name string, args ...string) error {
	// context.Background(): the shim is a one-shot CLI process with nothing to
	// cancel it, and a prune that is interrupted halfway leaves the builder
	// state this is trying to repair in a worse shape than before.
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

func notice(msg string) {
	fmt.Fprintf(os.Stderr, "discobox-docker: %s\n", msg)
}

type retryAction int

const (
	retryNone retryAction = iota
	// retryPrune covers corruption of the daemon's own BuildKit state. It is
	// not fixable by touching the exported cache directory, which is why the
	// output is the only available signal.
	retryPrune
	// retryWithoutExport covers a build that succeeded but could not write
	// the cache, typically a full cache volume.
	retryWithoutExport
	// retryBackoff covers losing the index.json flock to a concurrent build
	// in another sandbox sharing this cache.
	retryBackoff
)

// corruptionSignatures identify a poisoned BuildKit state in dockerd, which a
// `builder prune` clears.
var corruptionSignatures = []string{
	"parent snapshot",
	"failed to resume the status from path",
	"failed to get state for id",
	"content digest sha256:",
}

func classify(out string) retryAction {
	// Lock contention is checked first: it is specific, and its message can
	// appear alongside generic export failures.
	if strings.Contains(out, "could not lock") {
		return retryBackoff
	}
	for _, s := range corruptionSignatures {
		if strings.Contains(out, s) {
			return retryPrune
		}
	}
	if strings.Contains(out, "no space left on device") ||
		strings.Contains(out, "exporting cache to client directory") {
		return retryWithoutExport
	}
	return retryNone
}

// stripCacheTo removes the injected export flag, in both separate-value and
// inline forms, so a retry can still import cache while not writing it.
func stripCacheTo(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--cache-to" {
			i++ // also drop its value
			continue
		}
		if strings.HasPrefix(argv[i], "--cache-to=") {
			continue
		}
		out = append(out, argv[i])
	}
	return out
}

// tailBuffer retains the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
