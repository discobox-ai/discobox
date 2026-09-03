//go:build linux

package childproc

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// spin runs the reaper the way the signal loop does — as fast as children
// appear — and returns the function that stops it. The race this package exists
// to remove only shows up when the two are actually concurrent, so the test for
// it is the race.
func spin(t *testing.T) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			reapExited(quietLogger(), nil)
			time.Sleep(50 * time.Microsecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// The bug this package was written for: a reaper calling wait4(-1) collects the
// children os/exec is waiting for, and their owners are told the command has no
// exit status at all — over output that arrived and a command that ran. With
// the reaper peeking first, every run reports its own result.
func TestTheReaperLeavesChildrenThisProcessOwns(t *testing.T) {
	spin(t)
	for i := range 200 {
		out, err := CombinedOutput(exec.CommandContext(t.Context(), "/bin/echo", "ok"))
		if err != nil {
			t.Fatalf("run %d: %v (output %q)", i, err, string(out))
		}
		if string(out) != "ok\n" {
			t.Fatalf("run %d output = %q, want the command's own", i, string(out))
		}
	}
}

// A command that fails still reports how: the exit status is the caller's, and
// a reaper that took it would turn every failure into the same unusable error.
func TestAFailedCommandKeepsItsExitStatus(t *testing.T) {
	spin(t)
	for i := range 100 {
		var exit *exec.ExitError
		err := Run(exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 3"))
		if !errors.As(err, &exit) {
			t.Fatalf("run %d: err = %v, want an exit error", i, err)
		}
		if exit.ExitCode() != 3 {
			t.Fatalf("run %d: exit code = %d, want 3", i, exit.ExitCode())
		}
	}
}

// The reaper still has its job: a child nothing in this process waits for is
// collected rather than left as a zombie forever.
func TestTheReaperCollectsAnOrphanedChild(t *testing.T) {
	// Started outside childproc, which is what an orphan looks like from here:
	// no owner, so nobody is coming to wait for it — and nothing to cancel it,
	// since a context that ends with the test would go looking for a pid this
	// reaper has already collected.
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	deadline := time.Now().Add(5 * time.Second)
	for {
		reapExited(quietLogger(), nil)
		// A zombie still answers signal 0; only a collected child is gone.
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d was never reaped", pid)
		}
		time.Sleep(time.Millisecond)
	}
}

// The peek names the child that exited — the reap that follows is by pid — and
// leaves it waitable, which is what lets its owner have the exit status.
func TestThePeekNamesTheExitedChildAndLeavesIt(t *testing.T) {
	// Anything an earlier test left behind would be peeked first.
	reapExited(quietLogger(), nil)

	child, err := Start(exec.CommandContext(t.Context(), "/bin/true"))
	if err != nil {
		t.Fatal(err)
	}
	pid := child.cmd.Process.Pid

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok, err := peekExitedChild()
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if ok {
			if got != pid {
				t.Fatalf("peek = %d, want the exited child %d", got, pid)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no exited child was ever peeked for pid %d", pid)
		}
		time.Sleep(time.Millisecond)
	}
	// WNOWAIT: the peek must leave the child waitable for its owner.
	if err := child.Wait(); err != nil {
		t.Fatalf("wait after peek: %v", err)
	}
}
