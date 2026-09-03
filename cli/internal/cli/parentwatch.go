package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/localpty"
)

// parentPollInterval is how often an invocation that was spawned on a pane's
// pty checks that the process which spawned it is still there.
//
// There is no event to wait on. Linux has PR_SET_PDEATHSIG and Windows has job
// objects, but macOS — where the launcher is most used — has neither, and a
// lifeline pipe would have to be threaded through both platforms' start paths,
// one of which builds its own CreateProcess around a pseudoconsole attribute
// list. Asking every couple of seconds costs nothing measurable and is the same
// three lines on every platform.
const parentPollInterval = 2 * time.Second

// parentExitGrace is how long the command gets to unwind after its context is
// canceled before the process leaves anyway.
//
// Canceling is the polite half and it is usually enough: it unwinds the attach,
// restores the terminal, closes the sockets. But this exists because a command
// got stuck in a loop nobody was watching, and a watchdog that can only ask
// nicely reproduces the bug it was added for.
const parentExitGrace = 5 * time.Second

// parentWatchExit ends the process when canceling was not enough. It is a
// variable so a test can watch the watchdog give up without the test binary
// being what exits.
var parentWatchExit = os.Exit

// watchParentProcess ends this invocation once the process that spawned it is
// gone.
//
// It applies only to a child started by localpty — the launcher's panes — which
// is what sets ParentPIDEnv. An invocation the user started themselves is
// theirs to end, and one backgrounded from a shell (`discobox ... &`, then the
// shell exits) is reparented exactly the way an abandoned pane child is: the
// two are indistinguishable from in here, so the spawner has to say which it
// is.
//
// The returned context is the one the command should run under. Where there is
// no parent to watch it is the one passed in, unchanged.
func watchParentProcess(ctx context.Context, notify io.Writer) context.Context {
	pid, ok := parentProcessToWatch()
	if !ok {
		return ctx
	}
	return watchProcess(ctx, pid, parentPollInterval, parentExitGrace, notify)
}

// watchProcess is watchParentProcess with the policy — which process, how often
// to ask, how long it may take to go — handed to it, so what the watchdog does
// can be tested without waiting out the intervals a real one uses.
func watchProcess(ctx context.Context, pid int, poll, grace time.Duration, notify io.Writer) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(poll):
			}
			if parentProcessAlive(pid) {
				continue
			}
			if notify != nil {
				fmt.Fprintln(notify, "discobox: the window that started this command is gone; exiting")
			}
			cancel()
			time.Sleep(grace)
			parentWatchExit(1)
			return
		}
	}()
	return ctx
}

// parentProcessToWatch is the process this invocation was told not to outlive.
//
// The variable is consumed, not merely read. It names one process's child, and
// everything this command goes on to start inherits this environment: the
// editor a harness edit opens, and — the one that matters — the server the
// autolaunch forks, which is a singleton meant to outlive every CLI on the
// machine. A grandchild that kept the variable would be watching a process that
// is not its parent, find it "gone" on the first poll, and end itself two
// seconds in.
//
// An unparseable or nonsensical value is read as "nobody said": this decides
// whether a safety net is strung, and refusing to run a command over a mangled
// environment variable would be a worse answer than running it without one. It
// is cleared either way, so a mangled value does not travel further than the
// process that was handed it.
func parentProcessToWatch() (int, bool) {
	value := os.Getenv(localpty.ParentPIDEnv)
	_ = os.Unsetenv(localpty.ParentPIDEnv)
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
}
