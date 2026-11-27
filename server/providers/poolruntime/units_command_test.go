package poolruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A box whose bundle is from before slots has no units step: its agent and
// proxy are restarted after the apply when the placement moved, as they were
// then, and not otherwise, since its apply restarted what moved bundles move.
func TestABundleFromBeforeSlotsRestartsOnAMovedPlacementAlone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the units command runs in a Linux box")
	}
	if _, err := os.Stat("/usr/local/bin/discobox-pool-agent"); err == nil {
		t.Skip("this host has a pool agent installed, whose units step would run")
	}
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	fake := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(fake), 0o755); err != nil { //nolint:gosec // G306: an executable of the test's own
		t.Fatal(err)
	}
	run := func(placement string) string {
		t.Helper()
		_ = os.Remove(calls)
		cmd := exec.CommandContext(context.Background(), "sh", "-ec", unitsCommand, "units", placement)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("units command with a %s placement: %v\n%s", placement, err, out)
		}
		data, _ := os.ReadFile(calls)
		return strings.TrimSpace(string(data))
	}
	if got := run("moved"); got != "try-restart discobox-pool-agent.service discobox-proxy.service" {
		t.Fatalf("a moved placement ran systemctl %q, want its old units restarted", got)
	}
	if got := run("kept"); got != "" {
		t.Fatalf("a kept placement ran systemctl %q, want nothing", got)
	}
}
