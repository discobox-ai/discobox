//go:build unix

package cli

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// A tool that catches Ctrl-C and carries on still ends with its own status,
// and its directory still goes.
func TestDeliverToolScriptKeepsAToolThatCatchesInterrupt(t *testing.T) {
	tmp := t.TempDir()
	body := "#!/bin/sh\ntrap 'echo caught' INT\nkill -INT 0\nsleep 0.2\nexit 7\n"
	//nolint:gosec // G204: the script under test, with the test's own fixed arguments.
	cmd := exec.CommandContext(t.Context(), "sh", "-c", deliverToolScript, "sh", "catch.sh", body)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	// Its own process group, so the tool's `kill -INT 0` reaches the shell
	// and the tool and nothing of the test's.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 || !strings.Contains(string(out), "caught") {
		t.Fatalf("output = %q, err = %v; want the tool's own exit 7", out, err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("the delivered script was left behind: %v", entries)
	}
}
