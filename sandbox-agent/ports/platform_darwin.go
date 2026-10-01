package ports

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// platformScanner is darwin's: lsof, reading the kernel's socket tables through
// the system's own tool.
func platformScanner() Scanner {
	return lsofScanner{run: runLsof, ephemeral: sysctlEphemeralRange}
}

// lsofPath is where macOS ships lsof, named absolutely so a sandbox user's PATH
// cannot put another program in its place.
const lsofPath = "/usr/sbin/lsof"

// runLsof runs lsof and returns what it printed. lsof exits 1 both when it
// fails and when nothing matched; only the second is silent, so a silent exit
// 1 is an empty table rather than an error.
func runLsof(ctx context.Context, args []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, lsofPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && stdout.Len() == 0 && stderr.Len() == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lsof: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// sysctlEphemeralRange is the range darwin autobinds from, which is not
// Linux's: IANA's 49152-65535 unless someone changed it.
func sysctlEphemeralRange() portRange {
	low, errLow := unix.SysctlUint32("net.inet.ip.portrange.first")
	high, errHigh := unix.SysctlUint32("net.inet.ip.portrange.last")
	if errLow != nil || errHigh != nil || low < 1 || high > 65535 || low > high {
		return darwinEphemeralRange
	}
	return portRange{low: int(low), high: int(high)}
}
