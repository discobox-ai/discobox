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

// signalProcessGroup carries a wire signal name by Windows's nearest
// mechanism, as windowsDeliveries maps it: a Windows process has no signals,
// so a request to end it ends it, and a suspend or resume suspends or resumes
// its threads. The Delivery says which, for the exec's record.
func signalProcessGroup(cmd *exec.Cmd, name string) (Delivery, error) {
	delivery := deliveryFor(windowsDeliveries, name)
	if cmd == nil || cmd.Process == nil {
		return delivery, nil
	}
	switch delivery.Delivered {
	case "TerminateProcess":
		return delivery, cmd.Process.Kill()
	case "NtSuspendProcess", "NtResumeProcess":
		return delivery, callNtProcess(delivery.Delivered, cmd.Process.Pid)
	default:
		return delivery, nil
	}
}

var ntdll = windows.NewLazySystemDLL("ntdll.dll")

// callNtProcess calls ntdll's NtSuspendProcess or NtResumeProcess on pid:
// undocumented but long-stable, and the one call that stops or starts every
// thread of a process at once, which is the nearest thing to SIGSTOP and
// SIGCONT Windows has.
func callNtProcess(procedure string, pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("%s: open process %d: %w", procedure, pid, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	status, _, _ := ntdll.NewProc(procedure).Call(uintptr(handle))
	if status != 0 {
		return fmt.Errorf("%s: process %d: NTSTATUS 0x%08x", procedure, pid, status)
	}
	return nil
}

// exitCodeFromState reports the process's exit status. Windows has no signal
// exit convention to translate, so this is the raw code.
func exitCodeFromState(state *os.ProcessState) int64 {
	return int64(state.ExitCode())
}
