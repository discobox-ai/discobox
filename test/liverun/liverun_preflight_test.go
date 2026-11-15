package liverun_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// A fresh server's project has no default harness, so a run given no
// --harness either runs claude-code or refuses before it makes anything.
func TestLiveRunWithNoDefaultHarness(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "no-default-harness"), "", 0o644)
	})
	if i := indexOf(r.calls, " run "); i >= 0 && strings.Contains(r.calls[i], "-H claude-code") {
		return
	}
	if !strings.Contains(r.report, "| FAIL | preflight |") || !strings.Contains(r.report, "harness") {
		t.Fatalf("exit %d, want the run to pass -H claude-code or preflight to refuse the missing default:\n%s", r.exit, r.report)
	}
	if indexOf(r.calls, "admin provider create") >= 0 {
		t.Fatalf("a provider was made for a run that cannot start its discobox:\n%s", strings.Join(r.calls, "\n"))
	}
}

// A server with no route to the provider's API must stop the run at
// preflight, naming the provider, before any pool is made: the pool would
// only fail later with "bootstrap stopped making progress".
func TestLiveRunStopsWhenTheServerCannotReachTheProvider(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "provider-unreachable"), "", 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with a provider the server cannot reach:\n%s", r.out)
	}
	if indexOf(r.calls, "admin pool create") >= 0 {
		t.Fatalf("a pool was made on a provider the server cannot reach:\n%s", strings.Join(r.calls, "\n"))
	}
	if !strings.Contains(r.report, "network is unreachable") {
		t.Fatalf("report does not name why the provider is unreachable:\n%s", r.report)
	}
}
