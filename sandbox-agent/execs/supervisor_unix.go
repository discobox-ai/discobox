//go:build !windows

package execs

import (
	"encoding/json"
	"errors"
	"fmt"
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

// holdLifetime marks the inherited descriptor close-on-exec — it arrives
// without the flag, that being how it was inherited, and everything the shim
// starts would otherwise inherit it in turn — and records the shim's own pid
// and identity in the unit it locks.
//
// The supervisor records them too, once the shim has started, but the agent
// can die in between; a unit whose lock is held and whose file names no shim
// is one nothing could stop. Each writes a whole record in one write
// (writeUnitState), so whichever lands last leaves a whole record. An
// identity that cannot be read is recorded as none — the shim still runs, and
// Stop then has only the lock to go on — rather than failing the exec. The
// descriptor is used raw, never wrapped in an os.File, whose finalizer would
// close it — and release the lock — once collected.
func holdLifetime(fd uintptr) error {
	syscall.CloseOnExec(int(fd))
	buf := make([]byte, unitRecordSize)
	n, err := unix.Pread(int(fd), buf, 0)
	if err != nil {
		return err
	}
	if !json.Valid(buf[:n]) {
		// Not yet a whole record: the supervisor is writing it, and will
		// finish with the pid in it.
		return nil
	}
	var state unitState
	if err := json.Unmarshal(buf[:n], &state); err != nil {
		return err
	}
	if state.PID != 0 {
		return nil
	}
	pid := os.Getpid()
	identity, _ := processIdentity(pid)
	state.PID, state.Identity = pid, identity
	return writeUnitState(int(fd), state)
}

// writeUnitState replaces the record in a unit's lifetime file with one
// write of exactly unitRecordSize bytes, the record padded with spaces, which
// JSON allows after a value. Every record is the same size, so no write is ever
// followed by a truncate, and two writers — the agent and the shim — cannot
// tear each other's record: the file always holds one whole record or the
// other.
func writeUnitState(fd int, state unitState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > unitRecordSize {
		return fmt.Errorf("unit record is %d bytes, more than %d", len(data), unitRecordSize)
	}
	record := make([]byte, unitRecordSize)
	copy(record, data)
	for i := len(data); i < len(record); i++ {
		record[i] = ' '
	}
	_, err = unix.Pwrite(fd, record, 0)
	return err
}

func terminateProcess(pid int) error {
	return translateKillError(syscall.Kill(pid, syscall.SIGTERM))
}

func killProcess(pid int) error {
	return translateKillError(syscall.Kill(pid, syscall.SIGKILL))
}

// processInfo is what the supervisor needs to know of a process it did not
// start: its identity — what the kernel says about when it started, exactly,
// which is how a pid is told apart from a later process that reused the
// number — and whether it has exited and waits only to be reaped, which no
// signal can end.
type processInfo struct {
	identity string
	exited   bool
}

// processIdentity is the identity of the live process pid.
func processIdentity(pid int) (string, error) {
	info, err := inspectProcess(pid)
	if err != nil {
		return "", err
	}
	if info.exited {
		return "", os.ErrProcessDone
	}
	return info.identity, nil
}

// isCommand reports whether pid is still the command a shim recorded with
// identity — alive, leading its own session, and the very process the shim
// started rather than a later one that reused the number. An unrecorded
// identity matches nothing.
func isCommand(pid int, identity string) bool {
	if identity == "" {
		return false
	}
	if current, err := processIdentity(pid); err != nil || current != identity {
		return false
	}
	sid, err := unix.Getsid(pid)
	return err == nil && sid == pid
}

// member is a live process in a session, as it was when the session was
// listed.
type member struct {
	pid      int
	identity string
}

// sessionMembers lists the live processes in session sid. Every exec command
// leads a session of its own (agentSysProcAttr), and what it starts stays in
// that session unless it leaves on purpose, so the session — not the process
// group — is the nearest thing to the control group systemd ends a unit with:
// an interactive shell puts each job in a group of its own.
func sessionMembers(sid int) ([]member, error) {
	pids, err := processes()
	if err != nil {
		return nil, err
	}
	var out []member
	for _, pid := range pids {
		if in, err := unix.Getsid(pid); err != nil || in != sid {
			continue
		}
		if identity, err := processIdentity(pid); err == nil {
			out = append(out, member{pid: pid, identity: identity})
		}
	}
	return out, nil
}

// signal sends sig to m if it is still the process that was listed, in the
// session it was listed in. A member that exited since, and whose number was
// taken, is not the one this was meant for. What is left is the instant
// between the check and kill(2): Linux could close it with a pidfd, darwin —
// the platform the Supervisor exists for — has nothing to close it with, and
// a pid that is reused within that instant has gone all the way round.
func (m member) signal(sid int, sig syscall.Signal) {
	if in, err := unix.Getsid(m.pid); err != nil || in != sid {
		return
	}
	if identity, err := processIdentity(m.pid); err != nil || identity != m.identity {
		return
	}
	_ = syscall.Kill(m.pid, sig)
}

// sessionStillOurs reports whether session sid is still the one led by the
// command whose identity the shim recorded. While a live process holds the
// number it must be that command; once the leader has gone, what is left in
// the session is taken to be its own — a session id stays its leader's for as
// long as anything in the session is alive, so a number reused by a new
// leader would have found the old session empty.
//
// That last step holds only for a caller that knows the command was alive a
// moment ago: the shim, stopping the command it ran. A newer session can lose
// its own leader too, so a caller looking at a pid recorded any length of time
// ago — the supervisor collecting a shim that died while the agent was down —
// requires a live, matching leader instead (isCommand).
func sessionStillOurs(sid int, identity string) bool {
	if info, err := inspectProcess(sid); err == nil && !info.exited {
		return isCommand(sid, identity)
	}
	return identity != ""
}

// signalSession sends sig to every live process in the session of the command
// with identity, and to nothing if that number is now someone else's.
func signalSession(sid int, identity string, sig syscall.Signal) error {
	if !sessionStillOurs(sid, identity) {
		return nil
	}
	members, err := sessionMembers(sid)
	if err != nil {
		return err
	}
	for _, m := range members {
		m.signal(sid, sig)
	}
	return nil
}

// askSessionToStop asks everything in the session of the command with
// identity to end, and asks nothing if that number is now someone else's:
// SIGTERM to all of it, and for a terminal SIGHUP with it, then SIGCONT.
//
// A terminal's session gets SIGHUP because a stopped terminal is a terminal
// that went away, and SIGHUP is how a program is told that. An interactive
// shell ignores SIGTERM and exits on SIGHUP, so without it every terminal's
// stop — a delete, a relaunch, a revive — would sit out the shim's whole grace
// before the kill. It is what systemd's SendSIGHUP= sends beside the SIGTERM
// for the same reason. SIGCONT follows, as systemd sends it: a stopped process
// — a Ctrl-Z'd editor, a harness's suspended child — holds the others pending
// and would meet the SIGKILL without ever having seen them.
func askSessionToStop(sid int, identity string, terminal bool) error {
	signals := []syscall.Signal{syscall.SIGTERM}
	if terminal {
		signals = append(signals, syscall.SIGHUP)
	}
	signals = append(signals, syscall.SIGCONT)
	for _, sig := range signals {
		if err := signalSession(sid, identity, sig); err != nil {
			return err
		}
	}
	return nil
}

// endSession kills everything in the session of the command with identity,
// and nothing if that number is now someone else's.
func endSession(sid int, identity string) error {
	if !sessionStillOurs(sid, identity) {
		return nil
	}
	return killSession(sid)
}

// killSession SIGKILLs session sid until nothing in it is left alive. It goes
// round more than once because a member may fork between being listed and
// being killed, and a killed process takes a moment to become a zombie. What
// it cannot end in that time — a process in uninterruptible sleep holds its
// SIGKILL until it wakes — it reports, naming how many.
func killSession(sid int) error {
	for range 20 {
		members, err := sessionMembers(sid)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		for _, m := range members {
			m.signal(sid, syscall.SIGKILL)
		}
		time.Sleep(10 * time.Millisecond)
	}
	members, err := sessionMembers(sid)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	return fmt.Errorf("session %d still has %d live processes after SIGKILL", sid, len(members))
}

func translateKillError(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
