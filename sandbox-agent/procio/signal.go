package procio

import "strings"

// Delivery is what a signal a client asked for became on this platform.
//
// The attach protocol carries POSIX signal names, and not every platform can
// carry each one as itself: a Linux exec's orphaned process group discards
// SIGTSTP, and a Windows process has no signals at all. Where a name cannot be
// carried as asked, the platform's nearest real mechanism carries it instead
// and Reason says so, so the exec's record shows what actually happened rather
// than a request that silently did something else, or nothing (ADR 0145 §4).
type Delivery struct {
	// Requested is the signal asked for, as the protocol names it: "TSTP".
	Requested string
	// Delivered is the mechanism that carried it, named as the platform names
	// it: "SIGSTOP", "TerminateProcess". Empty when nothing could.
	Delivered string
	// Reason says why Delivered is not Requested, and is empty when the signal
	// was delivered as asked.
	Reason string
}

// Mapped reports whether the signal was carried by something other than
// itself, or by nothing.
func (d Delivery) Mapped() bool { return d.Reason != "" }

// posixDeliveries is how a POSIX platform carries each signal the protocol
// names. Every exec starts in a new session, so these are sent to its process
// group, which reaches the command and anything it spawned.
var posixDeliveries = map[string]Delivery{
	"INT":  {Delivered: "SIGINT"},
	"TERM": {Delivered: "SIGTERM"},
	"KILL": {Delivered: "SIGKILL"},
	"HUP":  {Delivered: "SIGHUP"},
	"QUIT": {Delivered: "SIGQUIT"},
	"CONT": {Delivered: "SIGCONT"},
	// SIGSTOP, not SIGTSTP. Starting in a new session makes the process group
	// orphaned by definition -- no member has a parent in the same session --
	// and the kernel discards SIGTSTP, SIGTTIN, and SIGTTOU sent to an orphaned
	// group. SIGTSTP here would silently do nothing; SIGSTOP is never
	// discarded, so a suspend from a client always lands.
	//
	// This is a client asking to stop a whole process. Ctrl-Z typed into a TTY
	// process is a byte, not a signal: the line discipline delivers SIGTSTP to
	// the foreground job, a child group of the shell that is not orphaned,
	// which stops normally with its handler intact.
	"TSTP": {Delivered: "SIGSTOP", Reason: "the exec's process group is orphaned, and the kernel discards SIGTSTP sent to one; SIGSTOP stops it instead"},
}

// windowsDeliveries is how Windows carries each signal the protocol names. A
// Windows process has no signals, so every one is mapped, each to the nearest
// thing the platform has: ending the process, or suspending and resuming its
// threads. None reaches the processes it started, which a process group would.
var windowsDeliveries = map[string]Delivery{
	"INT":  {Delivered: "TerminateProcess", Reason: windowsEnds("SIGINT")},
	"TERM": {Delivered: "TerminateProcess", Reason: windowsEnds("SIGTERM")},
	"KILL": {Delivered: "TerminateProcess", Reason: windowsEnds("SIGKILL")},
	"HUP":  {Delivered: "TerminateProcess", Reason: windowsEnds("SIGHUP")},
	"QUIT": {Delivered: "TerminateProcess", Reason: windowsEnds("SIGQUIT")},
	"TSTP": {Delivered: "NtSuspendProcess", Reason: "a Windows process has no SIGSTOP; its threads are suspended instead, and the processes it started are not"},
	"CONT": {Delivered: "NtResumeProcess", Reason: "a Windows process has no SIGCONT; its threads are resumed instead, and the processes it started are not"},
}

func windowsEnds(signal string) string {
	return "a Windows process has no " + signal + "; the process is ended instead, with no chance to handle it, and the processes it started are not"
}

// deliveryFor is how table carries the signal name, given as a client sent it.
// A name the protocol does not carry is delivered as nothing, and says so.
func deliveryFor(table map[string]Delivery, name string) Delivery {
	requested := strings.TrimPrefix(strings.TrimSpace(strings.ToUpper(name)), "SIG")
	delivery, ok := table[requested]
	if !ok {
		return Delivery{Requested: requested, Reason: "the attach protocol carries no signal of that name"}
	}
	delivery.Requested = requested
	return delivery
}
