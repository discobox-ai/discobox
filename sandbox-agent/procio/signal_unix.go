//go:build !windows

package procio

import (
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

// signalProcessGroup delivers a wire signal name to the process group, as
// posixDeliveries maps it. The group is the process's own: every process here
// starts in a new session, so signaling the group reaches the command and
// anything it spawned.
func signalProcessGroup(cmd *exec.Cmd, _ processHandle, name string) (Delivery, error) {
	delivery := deliveryFor(posixDeliveries, name)
	sig, ok := posixSignals[delivery.Delivered]
	if !ok || cmd == nil || cmd.Process == nil {
		return delivery, nil
	}
	return delivery, syscall.Kill(-cmd.Process.Pid, sig)
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
