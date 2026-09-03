//go:build linux

package childproc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// reaperSweep is how often the reaper looks without being signaled.
//
// SIGCHLD is the wakeup that matters, but the loop stops as soon as it peeks a
// child this process owns (reapExited), so an orphan behind that child waits
// for the next one. In a busy agent the next one is milliseconds away; the
// sweep is what bounds the quiet case.
const reaperSweep = 30 * time.Second

// StartReaper collects the children nothing in this process is waiting for, and
// returns the function that stops it.
//
// Only PID 1 has this job: orphans re-parent to init, and everywhere else every
// child has an owner. managed names the pids that are this process's own
// long-running children rather than orphans — the systemd namespace child,
// which is signaled rather than waited for — so the log can say which is
// which.
func StartReaper(ctx context.Context, logger *slog.Logger, managed map[int]string) func() {
	if os.Getpid() != 1 {
		return func() {}
	}
	reaperCtx, cancel := context.WithCancel(ctx)
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigCh, syscall.SIGCHLD)
	go func() {
		defer close(done)
		sweep := time.NewTicker(reaperSweep)
		defer sweep.Stop()
		reapExited(logger, managed)
		for {
			select {
			case <-reaperCtx.Done():
				reapExited(logger, managed)
				return
			case <-sigCh:
				reapExited(logger, managed)
			case <-sweep.C:
				reapExited(logger, managed)
			}
		}
	}()
	return func() {
		cancel()
		signal.Stop(sigCh)
		<-done
	}
}

// reapExited collects every exited child this process does not own.
//
// Each round asks the kernel which child is waitable without collecting it, so
// that a child os/exec is about to wait for can be left where it is. That is
// the whole design: wait4(-1) would have taken it, and its owner would be told
// its command had no exit status at all (ADR 0087).
func reapExited(logger *slog.Logger, managed map[int]string) {
	for {
		pid, ok, err := peekExitedChild()
		if err != nil {
			logger.Warn("failed to inspect exited child processes", "error", err)
			return
		}
		if !ok {
			return
		}
		var status syscall.WaitStatus
		reaped := reapUnowned(pid, func(pid int) bool {
			var usage syscall.Rusage
			got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, &usage)
			return err == nil && got == pid
		})
		if !reaped {
			// Either a child whose owner is about to wait for it, or one that
			// went away between the peek and the claim. The peek would keep
			// naming the same pid until its owner collects it, so stop here and
			// let the next signal or sweep pick up whatever is behind it.
			return
		}
		if name := managed[pid]; name != "" {
			logger.Info("managed child exited", "name", name, "pid", pid, "status", waitStatus(status))
			continue
		}
		logger.Info("reaped orphaned child process", "pid", pid, "status", waitStatus(status))
	}
}

// peekExitedChild names an exited child without collecting it, or reports that
// there is none. WNOWAIT is what makes the answer a question rather than an
// act: the child stays waitable, for us or for its owner.
func peekExitedChild() (int, bool, error) {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_ALL, 0, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		switch {
		case err == nil:
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ECHILD):
			return 0, false, nil
		default:
			return 0, false, err
		}
		break
	}
	// WNOHANG with nothing waitable returns success having written nothing,
	// which is why the struct is zeroed on every call rather than reused.
	if info.Signo == 0 {
		return 0, false, nil
	}
	return siginfoPid(&info), true, nil
}

// siPidOffset is where si_pid sits in a SIGCHLD siginfo_t: after the three
// leading ints, padded to the union's alignment — nothing on 32-bit Linux, four
// bytes on 64-bit. x/sys/unix models everything past si_code as opaque bytes,
// so the field is read at its ABI offset rather than by name.
const siPidOffset = 3*4 + (unsafe.Sizeof(uintptr(0)) - 4)

func siginfoPid(info *unix.Siginfo) int {
	return int(*(*int32)(unsafe.Add(unsafe.Pointer(info), siPidOffset)))
}

func waitStatus(status syscall.WaitStatus) string {
	switch {
	case status.Exited():
		return "exited:" + strconv.Itoa(status.ExitStatus())
	case status.Signaled():
		return "signaled:" + status.Signal().String()
	case status.Stopped():
		return "stopped:" + status.StopSignal().String()
	default:
		return "unknown"
	}
}

// reapUnowned calls reap for pid unless this process owns it, holding the
// registry across both so no child can be started or released in between. It
// reports whether reap ran and said it collected the child.
//
// A pid we own is left alone rather than waited for: its owner's Wait is the
// only thing entitled to its exit status.
func reapUnowned(pid int, reap func(int) bool) bool {
	children.mu.Lock()
	defer children.mu.Unlock()
	if _, ours := children.owned[pid]; ours {
		return false
	}
	return reap(pid)
}
