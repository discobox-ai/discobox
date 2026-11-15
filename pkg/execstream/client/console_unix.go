//go:build !windows

package client

import (
	"os"
	"os/signal"
	"syscall"
)

// proxiedSignals are the signals handled here and delivered to the remote
// process rather than acted on locally. They matter only when the terminal is
// not in raw mode: a raw terminal passes Ctrl-C and Ctrl-Z through as the bytes
// 0x03 and 0x1a, and the remote line discipline turns those into signals for
// the remote foreground job — which is why an attached terminal never sees them
// here at all.
func proxiedSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP}
}

func (c *OSConsole) NotifySignals(ch chan<- os.Signal) { signal.Notify(ch, proxiedSignals()...) }

func (c *OSConsole) StopSignals(ch chan<- os.Signal) { signal.Stop(ch) }

// IsSuspendSignal reports whether sig means "stop this job" (Ctrl-Z). Suspend is
// handled rather than merely forwarded: the remote job stops *and* this process
// stops, so the user gets their shell back and fg resumes both, exactly as a
// local command behaves.
func (c *OSConsole) IsSuspendSignal(sig os.Signal) bool { return sig == syscall.SIGTSTP }

func (c *OSConsole) SignalName(sig os.Signal) (string, bool) {
	switch sig {
	case os.Interrupt:
		return "INT", true
	case syscall.SIGTERM:
		return "TERM", true
	case syscall.SIGHUP:
		return "HUP", true
	case syscall.SIGQUIT:
		return "QUIT", true
	case syscall.SIGTSTP:
		return "TSTP", true
	case syscall.SIGCONT:
		return "CONT", true
	default:
		return "", false
	}
}

// Suspend stops this process and returns once SIGCONT resumes it.
//
// It stops with SIGSTOP rather than by restoring SIGTSTP's default disposition
// and re-raising it. That dance does not work: a Go process that has notified
// SIGTSTP keeps a handler installed, and the re-raised signal was measured to
// come back to the handler instead of stopping — once per suspend under job
// control, and in a livelock without it. SIGSTOP can be neither caught nor
// ignored, and unlike SIGTSTP it is never discarded for an orphaned process
// group (a script or CI run), so it stops in every environment.
//
// Only this process is stopped, not the whole process group: where the group is
// shared — a script that did not set up job control — stopping it would take
// the script down with us, and where it is not shared (an interactive shell's
// foreground job) the two are equivalent.
func (c *OSConsole) Suspend() {
	// Arm the resume watch before stopping; SIGCONT is what fg delivers.
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGSTOP)
	<-cont
}
