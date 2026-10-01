package resources

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// NewSampler is darwin's: ps and the kernel's process table.
func NewSampler() Sampler {
	return psSampler{ps: runPS, kernel: kernelProcs}
}

// psPath is where macOS ships ps, named absolutely so a sandbox user's PATH
// cannot put another program in its place.
const psPath = "/bin/ps"

// psArgs lists every process with its counters, the command line last because
// it is the one column that may hold spaces. -ww keeps ps from cutting the
// command line to a terminal's width.
var psArgs = []string{"-axww", "-o", "pid=,ppid=,time=,utime=,rss=,vsz=,args="}

// runPS runs ps in the C locale, so a CPU time's decimal point is a point.
func runPS(ctx context.Context) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, psPath, psArgs...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ps: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// kernelProcs reads every process's command and start time from kern.proc.all.
// The start time is wall-clock there; it is made ticks since boot against
// kern.boottime, which is what StartTicks means.
func kernelProcs() (map[int]kernelProc, error) {
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return nil, fmt.Errorf("read kern.boottime: %w", err)
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("read kern.proc.all: %w", err)
	}
	bootUsec := boot.Nano() / 1000
	out := make(map[int]kernelProc, len(procs))
	for i := range procs {
		proc := &procs[i].Proc
		startUsec := proc.P_starttime.Nano() / 1000
		out[int(proc.P_pid)] = kernelProc{
			comm:       unix.ByteSliceToString(proc.P_comm[:]),
			startTicks: uint64(max(startUsec-bootUsec, 0)) / (1_000_000 / clockTicksPerSecond),
		}
	}
	return out, nil
}
