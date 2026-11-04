package endpoint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
