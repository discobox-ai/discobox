//go:build !windows

package procio

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// processHandle is nothing on a POSIX platform: a signal goes to the process
// group by its id, and the caller's own Wait is what reaps it.
type processHandle struct{}

func holdProcess(*os.Process) (processHandle, error) { return processHandle{}, nil }

func (processHandle) release() {}

// posixSignals are the signals posixDeliveries delivers, by name.
var posixSignals = map[string]syscall.Signal{
	"SIGINT":  syscall.SIGINT,
	"SIGTERM": syscall.SIGTERM,
	"SIGKILL": syscall.SIGKILL,
	"SIGHUP":  syscall.SIGHUP,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGSTOP": syscall.SIGSTOP,
	"SIGCONT": syscall.SIGCONT,
}

// signalProcessGroup delivers a wire signal name, as posixDeliveries maps it,
// to a process group.
//
// A signal that ends a program (interrupt, terminate, hang up, quit, kill)
// goes, for a process on a PTY, to the terminal's foreground group
// (ForegroundGroup), where a local terminal's signal lands: the process an exec
// starts is often an interactive shell (a primary terminal's `bash -l`), whose
// own group is the shell alone, which ignores SIGTERM and survives SIGINT,
// while the harness on the screen runs as a job in a group of its own. The
// group the PTY names is read at the moment of the signal; between a job
// exiting and its shell taking the terminal back it can name a group that has
// just gone, and a signal that finds no such group goes to the process's own
// instead, as if the shell were already in front. A number reused within that
// window is the one group this can reach that the terminal no longer holds.
//
// Suspend and resume (SIGSTOP, SIGCONT) stay on the process's own group.
// Stopping a shell's job hands the terminal back to the shell, and only the
// shell's own `fg` gives it back: a SIGCONT from outside would resume the job
// in the background, where its next read of the terminal stops it again.
//
// Without a PTY, or when it cannot say, every signal goes to the process's own
// group: every process here starts in a new session, so that group reaches the
// command and anything it spawned without job control.
func signalProcessGroup(cmd *exec.Cmd, _ processHandle, tty *os.File, name string) (Delivery, error) {
	delivery := deliveryFor(posixDeliveries, name)
	sig, ok := posixSignals[delivery.Delivered]
	if !ok || cmd == nil || cmd.Process == nil {
		return delivery, nil
	}
	own := cmd.Process.Pid
	if sig != syscall.SIGSTOP && sig != syscall.SIGCONT {
		if foreground := ForegroundGroup(tty); foreground > 0 && foreground != own {
			if err := syscall.Kill(-foreground, sig); !errors.Is(err, syscall.ESRCH) {
				return delivery, err
			}
		}
	}
	return delivery, syscall.Kill(-own, sig)
}

// exitCodeFromState reports the exit status a shell would report. Go's ExitCode
// returns -1 for a process killed by a signal, which loses which signal it was
// and reads as a generic failure; the shell convention of 128+signum keeps it,
// so an interrupted command exits 130 as it does locally.
func exitCodeFromState(state *os.ProcessState) int64 {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return int64(128 + status.Signal())
	}
	return int64(state.ExitCode())
}
