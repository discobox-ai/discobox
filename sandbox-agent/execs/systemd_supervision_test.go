//go:build linux

package execs

import (
	"os"
	"testing"
)

// The same contract the Supervisor passes, against the sandbox's own systemd.
// Starting a transient unit needs root as well as the bus, so it runs where
// the agent itself would — in a sandbox, as root — and skips elsewhere.
func TestSystemdRunnerPassesTheUnitManagerContract(t *testing.T) {
	requireSystemBus(t)
	if os.Geteuid() != 0 {
		t.Skip("starting a transient unit needs root")
	}
	testUnitManagerContract(t, func(t *testing.T, _ string) UnitManager {
		runner := NewSystemdRunner()
		t.Cleanup(runner.Close)
		return runner
	})
}
