//go:build windows

package procio

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// processHandle is this agent's own handle on the process, opened at Start
// with the rights a suspend and resume need. It is held for the process's
// whole lifetime because a PID is not: once os/exec's Wait returns it closes
// its handle, and Windows may give the PID to another process, which a call
// that reopened the PID would then suspend.
type processHandle struct{ h windows.Handle }

func holdProcess(p *os.Process) (processHandle, error) {
	h, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(p.Pid))
	if err != nil {
		return processHandle{}, fmt.Errorf("open process %d: %w", p.Pid, err)
	}
	return processHandle{h: h}, nil
}

func (p processHandle) release() {
	if p.h != 0 {
		_ = windows.CloseHandle(p.h)
	}
}

// signalProcessGroup carries a wire signal name by Windows's nearest
// mechanism, as windowsDeliveries maps it: a Windows process has no signals,
// so a request to end it ends it, and a suspend or resume suspends or resumes
// its threads. The Delivery says which, for the exec's record.
func signalProcessGroup(cmd *exec.Cmd, handle processHandle, name string) (Delivery, error) {
	delivery := deliveryFor(windowsDeliveries, name)
	if cmd == nil || cmd.Process == nil {
		return delivery, nil
	}
	switch delivery.Delivered {
	case "TerminateProcess":
		return delivery, cmd.Process.Kill()
	case "NtSuspendProcess", "NtResumeProcess":
		return delivery, callNtProcess(delivery.Delivered, handle)
	default:
		return delivery, nil
	}
}

var ntdll = windows.NewLazySystemDLL("ntdll.dll")

// callNtProcess calls ntdll's NtSuspendProcess or NtResumeProcess on the
// process's held handle: undocumented but long-stable, and the one call that
// stops or starts every thread of a process at once, which is the nearest
// thing to SIGSTOP and SIGCONT Windows has.
func callNtProcess(procedure string, handle processHandle) error {
	if handle.h == 0 {
		return fmt.Errorf("%s: the process has been released", procedure)
	}
	status, _, _ := ntdll.NewProc(procedure).Call(uintptr(handle.h))
	if status != 0 {
		return fmt.Errorf("%s: NTSTATUS 0x%08x", procedure, status)
	}
	return nil
}

// exitCodeFromState reports the process's exit status. Windows has no signal
// exit convention to translate, so this is the raw code.
func exitCodeFromState(state *os.ProcessState) int64 {
	return int64(state.ExitCode())
}
