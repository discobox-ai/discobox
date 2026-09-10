package libkrun

import "syscall"

// launcherSysProcAttr arms the kernel's half of the launcher's lifetime.
//
// PR_SET_PDEATHSIG survives everything this process does afterwards, including
// being SIGKILLed, which is what the watchdog pipe cannot claim: a pipe is
// closed by an orderly exit, and this covers the disorderly one. Both are
// needed, and together they are why no VM outlives its server (ADR 0062 §9).
//
// Deliberately not Setsid. The VM is this process's to own now, so it should
// also take the signal that ends an interactive server.
func launcherSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
