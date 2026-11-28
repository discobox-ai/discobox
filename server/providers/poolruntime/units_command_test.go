package poolruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
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

// A units step that refused because a slot still drains failed nothing: it
// is ErrPoolSlotDraining, which the reconciler asks again on. One whose last
// handover's new agent never took over is ErrPoolTakeOverStalled, and any
// other exit is the step's failure as before.
func TestAUnitsStepRefusedForADrainIsTryLater(t *testing.T) {
	result := provisioner.Result{Stderr: []byte("a slot still drains from the last handover")}
	draining := unitsError(pooloutbound.Resolved{}, result, &provisioner.ExitError{Argv: []string{"sh"}, ExitCode: bundle.UnitsDrainingExitCode})
	if !errors.Is(draining, sandbox.ErrPoolSlotDraining) {
		t.Fatalf("a refusal for a drain = %v, want ErrPoolSlotDraining", draining)
	}
	stalled := unitsError(pooloutbound.Resolved{}, provisioner.Result{}, &provisioner.ExitError{Argv: []string{"sh"}, ExitCode: bundle.UnitsTakeOverStalledExitCode})
	if !errors.Is(stalled, sandbox.ErrPoolTakeOverStalled) || errors.Is(stalled, sandbox.ErrPoolSlotDraining) {
		t.Fatalf("a handover whose new agent never took over = %v, want ErrPoolTakeOverStalled", stalled)
	}
	failed := unitsError(pooloutbound.Resolved{}, provisioner.Result{}, &provisioner.ExitError{Argv: []string{"sh"}, ExitCode: 1})
	if errors.Is(failed, sandbox.ErrPoolSlotDraining) || errors.Is(failed, sandbox.ErrPoolTakeOverStalled) || failed == nil {
		t.Fatalf("a failed units step = %v, want a failure", failed)
	}
}

// The maker runs the units step asking it to report a refusal for a drain.
func TestTheMakerAsksTheUnitsStepToReportADrain(t *testing.T) {
	if !strings.Contains(unitsCommand, `exec "$step" `+bundle.UnitsReportDraining) {
		t.Fatalf("the units command does not pass %s to the step:\n%s", bundle.UnitsReportDraining, unitsCommand)
	}
}

// A maker returns the units step's word on a handover as it is: a drain as
// "try later", a stalled takeover as a recorded failure that replaces
// nothing. Any other failure is left to the maker.
func TestAHandOverFailureIsNeverKept(t *testing.T) {
	draining := fmt.Errorf("start the pool's agent: %w", sandbox.ErrPoolSlotDraining)
	if got := HandOverFailure(draining); !errors.Is(got, sandbox.ErrPoolSlotDraining) || errors.Is(got, sandbox.ErrPoolConfigApply) {
		t.Fatalf("a drain = %v, want it as it is", got)
	}
	stalled := fmt.Errorf("start the pool's agent: %w", sandbox.ErrPoolTakeOverStalled)
	if got := HandOverFailure(stalled); !errors.Is(got, sandbox.ErrPoolConfigApply) || !errors.Is(got, sandbox.ErrPoolTakeOverStalled) {
		t.Fatalf("a stalled takeover = %v, want it under ErrPoolConfigApply", got)
	}
	if got := HandOverFailure(errors.New("registry unreachable")); got != nil {
		t.Fatalf("another failure = %v, want it left to the maker", got)
	}
}

// The units step's failure is redacted as the apply's is: it runs with the
// outbound routing's environment, whose proxy credentials it may echo, and
// stays the drain or stall it was.
func TestAUnitsStepFailureIsRedacted(t *testing.T) {
	const secret = "s3cret"
	proxyURL := "http://pool:" + secret + "@proxy.example:3128"
	outbound := pooloutbound.Resolved{HTTP: proxyURL}
	result := provisioner.Result{Stderr: []byte("units: dial " + proxyURL + ": refused")}
	err := unitsError(outbound, result, &provisioner.ExitError{Argv: []string{"sh"}, ExitCode: bundle.UnitsTakeOverStalledExitCode})
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the units step's failure carries the proxy's credential: %v", err)
	}
	if !errors.Is(err, sandbox.ErrPoolTakeOverStalled) {
		t.Fatalf("a redacted stall = %v, want ErrPoolTakeOverStalled still", err)
	}
}
