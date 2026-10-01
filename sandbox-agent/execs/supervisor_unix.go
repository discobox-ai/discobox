//go:build !windows

package execs

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// lockFile takes an exclusive flock on file, waiting for it when wait is set
// and reporting false rather than waiting when it is not.
//
// It is flock and must stay flock. A flock belongs to the open file
// description, so it passes to the shim with the descriptor and is released
// only when the last process holding that description exits — which is what
// makes it a shim's lifetime. A POSIX record lock (fcntl) belongs to a process
// instead: the supervisor closing its own copy would release it.
func lockFile(file *os.File, wait bool) (bool, error) {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(file.Fd()), how)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !wait && errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

// shimSysProcAttr starts a shim as the leader of a session of its own, so
// nothing the agent's own session or process group is sent reaches it: a shim
// outlives the agent that started it.
func shimSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// inheritLifetime passes file to the shim and returns the descriptor number
// the shim will find it at.
func inheritLifetime(cmd *exec.Cmd, file *os.File) string {
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	return strconv.Itoa(2 + len(cmd.ExtraFiles))
}

// holdLifetime marks the inherited descriptor close-on-exec. It arrives
// without the flag — that is how it was inherited — and everything the shim
// starts would otherwise inherit it in turn.
func holdLifetime(fd uintptr) error {
	syscall.CloseOnExec(int(fd))
	return nil
}

func terminateProcess(pid int) error {
	return translateKillError(syscall.Kill(pid, syscall.SIGTERM))
}

func killProcess(pid int) error {
	return translateKillError(syscall.Kill(pid, syscall.SIGKILL))
}

func translateKillError(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
