package execs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// sZomb is a process that has exited and waits only to be reaped (SZOMB in
// XNU's sys/proc.h).
const sZomb = 5

// processes lists every process the kernel knows of.
func processes() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(procs))
	for _, proc := range procs {
		if pid := int(proc.Proc.P_pid); pid > 0 {
			out = append(out, pid)
		}
	}
	return out, nil
}

// inspectProcess reads a process's identity — the instant it started, to the
// microsecond, as the kernel recorded it, which no later process holding the
// same pid can share — and whether it has already exited and waits only to be
// reaped.
func inspectProcess(pid int) (processInfo, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processInfo{}, err
	}
	if int(proc.Proc.P_pid) != pid {
		return processInfo{}, unix.ESRCH
	}
	start := proc.Proc.P_starttime
	return processInfo{
		identity: fmt.Sprintf("%d.%06d", start.Sec, start.Usec),
		exited:   proc.Proc.P_stat == sZomb,
	}, nil
}
