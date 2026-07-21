//go:build !windows

package procio

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// signalProcessGroup maps a wire signal name onto the process group. The group
// is the process's own: every process here starts in a new session, so
// signaling the group reaches the command and anything it spawned.
func signalProcessGroup(cmd *exec.Cmd, name string) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	trimmed := strings.TrimSpace(strings.ToUpper(name))
	trimmed = strings.TrimPrefix(trimmed, "SIG")
	switch trimmed {
	case "INT":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	case "TERM":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	case "KILL":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	case "HUP":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	case "QUIT":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGQUIT)
	case "TSTP":
		// SIGSTOP, not SIGTSTP. Starting in a new session makes the process
		// group orphaned by definition — no member has a parent in the same
		// session — and the kernel discards SIGTSTP, SIGTTIN, and SIGTTOU sent
		// to an orphaned group. SIGTSTP here would silently do nothing; SIGSTOP
		// is never discarded, so a suspend from a client always lands.
		//
		// This is a client asking to stop a whole process. Ctrl-Z typed into a
		// TTY process is a byte, not a signal: the line discipline delivers
		// SIGTSTP to the foreground job, a child group of the shell that is not
		// orphaned, which stops normally with its handler intact.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGSTOP)
	case "CONT":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGCONT)
	default:
		return nil
	}
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
