//go:build !windows

package execs

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// lockFile takes a flock on file — exclusive or shared — waiting for it when
// wait is set and reporting false rather than waiting when it is not.
//
// It is flock and must stay flock. A flock belongs to the open file
// description, so it passes to the shim with the descriptor and is released
// only when the last process holding that description exits — which is what
// makes it a shim's lifetime. A POSIX record lock (fcntl) belongs to a process
// instead: the supervisor closing its own copy would release it.
func lockFile(file *os.File, exclusive, wait bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
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

// processInfo is what the supervisor needs to know of a process it did not
// start: when it started, which is how a pid is told apart from a later
// process that reused the number, and whether it has exited and waits only to
// be reaped, which no signal can end.
type processInfo struct {
	started time.Time
	exited  bool
}

// startTolerance is how far a process's start time, as the kernel reports it,
// may sit from the one the shim recorded for its command and still be that
// command. The shim records the time just after the process starts, and Linux
// reports boot time in whole seconds.
const startTolerance = 2 * time.Second

// isCommand reports whether pid is still the command that a shim recorded as
// starting at started — alive, leading its own session, and started then
// rather than some later process that reused the number.
func isCommand(pid int, started time.Time) bool {
	info, err := inspectProcess(pid)
	if err != nil || info.exited {
		return false
	}
	if sid, err := unix.Getsid(pid); err != nil || sid != pid {
		return false
	}
	return info.started.Sub(started).Abs() <= startTolerance
}

// sessionMembers lists the live processes in session sid. Every exec command
// leads a session of its own (agentSysProcAttr), and what it starts stays in
// that session unless it leaves on purpose, so the session — not the process
// group — is the nearest thing to the control group systemd ends a unit with:
// an interactive shell puts each job in a group of its own.
func sessionMembers(sid int) ([]int, error) {
	pids, err := processes()
	if err != nil {
		return nil, err
	}
	var out []int
	for _, pid := range pids {
		if member, err := unix.Getsid(pid); err != nil || member != sid {
			continue
		}
		if info, err := inspectProcess(pid); err != nil || info.exited {
			continue
		}
		out = append(out, pid)
	}
	return out, nil
}

// sessionStillOurs reports whether session sid is still the one a command
// that started at started leads. While a live process holds the number it must
// be that command; once the leader has gone, what is left in the session is
// taken to be its own — a session id stays its leader's for as long as
// anything in the session is alive, so a number reused by a new leader would
// have found the old session empty.
//
// That last step holds only for a caller that knows the command was alive a
// moment ago: the shim, stopping the command it ran. A newer session can lose
// its own leader too, so a caller looking at a pid recorded any length of time
// ago — the supervisor collecting a shim that died while the agent was down —
// requires a live, matching leader instead (isCommand).
func sessionStillOurs(sid int, started time.Time) bool {
	if info, err := inspectProcess(sid); err == nil && !info.exited {
		return isCommand(sid, started)
	}
	return true
}

// signalSession sends sig to every live process in the session of the command
// that started at started, and to nothing if that number is now someone
// else's.
func signalSession(sid int, started time.Time, sig syscall.Signal) error {
	if !sessionStillOurs(sid, started) {
		return nil
	}
	members, err := sessionMembers(sid)
	if err != nil {
		return err
	}
	for _, pid := range members {
		_ = syscall.Kill(pid, sig)
	}
	return nil
}

// askSessionToStop asks everything in the session of the command that
// started at started to end, and asks nothing if that number is now someone
// else's: SIGTERM to all of it, and for a terminal SIGHUP with it, then
// SIGCONT.
//
// A terminal's session gets SIGHUP because a stopped terminal is a terminal
// that went away, and SIGHUP is how a program is told that. An interactive
// shell ignores SIGTERM and exits on SIGHUP, so without it every terminal's
// stop — a delete, a relaunch, a revive — would sit out the shim's whole grace
// before the kill. It is what systemd's SendSIGHUP= sends beside the SIGTERM
// for the same reason. SIGCONT follows, as systemd sends it: a stopped process
// — a Ctrl-Z'd editor, a harness's suspended child — holds the others pending
// and would meet the SIGKILL without ever having seen them.
func askSessionToStop(sid int, started time.Time, terminal bool) error {
	signals := []syscall.Signal{syscall.SIGTERM}
	if terminal {
		signals = append(signals, syscall.SIGHUP)
	}
	signals = append(signals, syscall.SIGCONT)
	for _, sig := range signals {
		if err := signalSession(sid, started, sig); err != nil {
			return err
		}
	}
	return nil
}

// endSession kills everything in the session of the command that started at
// started, and nothing if that number is now someone else's.
func endSession(sid int, started time.Time) error {
	if !sessionStillOurs(sid, started) {
		return nil
	}
	return killSession(sid)
}

// killSession SIGKILLs session sid until nothing in it is left alive. It goes
// round more than once because a member may fork between being listed and
// being killed, and a killed process takes a moment to become a zombie.
func killSession(sid int) error {
	for range 20 {
		members, err := sessionMembers(sid)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		for _, pid := range members {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("session still has live processes")
}

func translateKillError(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
