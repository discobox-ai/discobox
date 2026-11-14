package endpoint

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/health"
)

func TestOlderServer(t *testing.T) {
	tests := map[string]struct {
		server   string
		expected string
		older    bool
	}{
		"older release":       {server: "v0.5.0", expected: "v0.5.1", older: true},
		"same release":        {server: "v0.5.1", expected: "v0.5.1"},
		"newer server":        {server: "v0.6.0", expected: "v0.5.1"},
		"unversioned server":  {expected: "v0.5.1"},
		"development client":  {server: "v0.5.0", expected: "accd52c7"},
		"prerelease is older": {server: "v0.5.1-rc.1", expected: "v0.5.1", older: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := olderServer(health.Status{Version: test.server}, test.expected)
			if got != test.older {
				t.Fatalf("olderServer(%q, %q) = %v, want %v", test.server, test.expected, got, test.older)
			}
		})
	}
}

func TestSystemdUnitNameIsStableAndScoped(t *testing.T) {
	opts := LaunchOptions{Endpoint: "unix:///tmp/discobox/server.sock"}

	got := userServiceUnitName(opts, Command{Path: "/usr/local/bin/discobox-server"})
	if got != "discobox-server-30ad8514897f671d" {
		t.Fatalf("userServiceUnitName() = %q", got)
	}
	// The name a machine's unit had when the CLI ran the server in-process.
	// An upgrade must not leave a second unit for the same endpoint.
	if was := userServiceUnitName(opts, Command{Path: "/usr/local/bin/discobox", Args: []string{"admin", "server"}}); was != got {
		t.Fatalf("the unit is now %q, and used to be %q", got, was)
	}
}

func TestSystemdUnitNameSanitizesCommandName(t *testing.T) {
	got := systemdUnitName("Discobox Dev!", "abc123")
	if got != "discobox-dev-server-abc123" {
		t.Fatalf("systemdUnitName() = %q", got)
	}
}

func TestEnsureRunningOlderStartingServerGetsReadinessAllowance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"starting","phase":"migrating","version":"v0.5.0"}`))
	}))
	defer server.Close()
	opts := LaunchOptions{Endpoint: server.URL, LockPath: filepath.Join(t.TempDir(), "launch.lock"), Command: func(context.Context) (Command, error) { return Command{Path: "unused-server"}, nil }, ExpectedVersion: "v0.5.1", ReadyTimeout: 50 * time.Millisecond, ProbeInterval: 5 * time.Millisecond}
	_, err := EnsureRunning(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "did not finish starting") || !strings.Contains(err.Error(), "migrating") {
		t.Fatalf("starting older server: %v", err)
	}
}

// A server upgrading pools has started and its API serves, but a command
// waits for it as for one starting: it answers 503 until its pools are on its
// agent. The wait is progress, reported with the count, not an error.
func TestEnsureRunningWaitsOutAServerUpgradingPools(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n := probes.Add(1); n < 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"status":"upgrading_pools","poolsUpgrading":%d}`, 4-n)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()
	var reported []health.Status
	opts := LaunchOptions{
		Endpoint: server.URL, LockPath: filepath.Join(t.TempDir(), "launch.lock"),
		Command:      func(context.Context) (Command, error) { return Command{Path: "unused-server"}, nil },
		ReadyTimeout: time.Minute, ProbeInterval: time.Millisecond,
		OnProgress: func(status health.Status) { reported = append(reported, status) },
	}
	started, err := EnsureRunning(context.Background(), opts)
	if err != nil || started {
		t.Fatalf("EnsureRunning() = %v, %v; want a wait on the running server", started, err)
	}
	// Each count is reported as it drops: the line says how many are left.
	// (The first probe, which decides to wait, reports nothing, as for a
	// server starting.)
	var counts []int
	for _, status := range reported {
		if status.UpgradingPools() {
			counts = append(counts, status.PoolsUpgrading)
		}
	}
	if !slices.Equal(counts, []int{2, 1}) || reported[len(reported)-1].Status != health.StatusReady {
		t.Fatalf("progress = %+v, want 2, then 1 pools upgrading, then ready", reported)
	}
}

// Upgrading pools has its own deadline from when it is first seen, so a start
// that used up its own does not cut off the server's bounded wait for its
// pools. Here the server upgrades for well past the deadline waitReady was
// given, and is waited out.
func TestWaitReadyGivesUpgradingPoolsItsOwnDeadline(t *testing.T) {
	settleAt := time.Now().Add(300 * time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if time.Now().Before(settleAt) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"upgrading_pools","poolsUpgrading":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()
	opts := LaunchOptions{Endpoint: server.URL, UpgradeTimeout: time.Minute, ProbeInterval: 5 * time.Millisecond}
	if err := waitReady(context.Background(), opts, time.Now().Add(20*time.Millisecond)); err != nil {
		t.Fatalf("waitReady() = %v, want the upgrade waited out past the start's deadline", err)
	}
}

// The client's own deadline outlasts the server's bound, so a pool that never
// upgrades ends in the server's ready answer, not the client's timeout.
func TestUpgradeTimeoutOutlastsTheServersBound(t *testing.T) {
	if got := (LaunchOptions{}).upgradeTimeout(); got <= health.PoolUpgradeWait {
		t.Fatalf("upgradeTimeout() = %s, want more than the server's %s", got, health.PoolUpgradeWait)
	}
}
