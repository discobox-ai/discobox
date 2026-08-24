//go:build !windows

package endpoint

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/health"
)

// A server that dies on startup used to be invisible: its output went nowhere,
// so the only symptom was a caller waiting out its timeout against a socket
// nothing had bound. That is exactly how `discobox server` — a command that no
// longer existed — failed for every user without saying so once.
func TestEnsureRunningReportsWhyTheServerDied(t *testing.T) {
	socket := testSocketPath(t)
	opts := LaunchOptions{
		Endpoint:      "unix://" + socket,
		Command:       "/bin/sh",
		Args:          []string{"-c", `echo 'unknown command "server" for "discobox"' >&2; exit 1`},
		StartTimeout:  500 * time.Millisecond,
		ProbeInterval: 20 * time.Millisecond,
		ProbeTimeout:  100 * time.Millisecond,
	}

	_, err := EnsureRunning(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "never answered") {
		t.Fatalf("error %q does not say the server never answered", err)
	}
	if !strings.Contains(err.Error(), `unknown command "server"`) {
		t.Fatalf("error %q does not carry what the server said", err)
	}
}

// A server that is still starting is waited on, not raced. Launching a second
// one alongside it is how a slow first run turned into two servers fighting
// over the same data directory.
func TestEnsureRunningWaitsOutAStartingServer(t *testing.T) {
	socket := testSocketPath(t)
	endpointURL := "unix://" + socket
	listener, _, cleanup, err := Listen(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var probes atomic.Int32
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Ready only once it has been asked a few times, so the wait is
			// exercised rather than satisfied by the first probe.
			if probes.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				writeStatus(t, w, health.Status{Status: health.StatusStarting, Phase: "migrating the database"})
				return
			}
			writeStatus(t, w, health.Status{Status: health.StatusReady})
		}),
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	var phases []string
	opts := LaunchOptions{
		Endpoint:      endpointURL,
		Command:       "/bin/sh",
		Args:          []string{"-c", "exit 7"}, // must never run
		ProbeInterval: 10 * time.Millisecond,
		ProbeTimeout:  time.Second,
		ReadyTimeout:  10 * time.Second,
		OnProgress:    func(s health.Status) { phases = append(phases, s.Phase) },
	}
	started, err := EnsureRunning(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// It waited on the server that was already there rather than starting one.
	if started {
		t.Fatal("EnsureRunning reported starting a server that was already running")
	}
	if len(phases) != 1 || phases[0] != "migrating the database" {
		t.Fatalf("phases = %v, want the starting phase reported exactly once", phases)
	}
}

// An already-ready server needs neither a launch nor a wait.
func TestEnsureRunningAcceptsAServerWithNoStatusBody(t *testing.T) {
	socket := testSocketPath(t)
	endpointURL := "unix://" + socket
	listener, _, cleanup, err := Listen(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		// No body at all: an older server, or a proxy answering the probe.
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	opts := LaunchOptions{
		Endpoint:     endpointURL,
		Command:      "/bin/sh",
		Args:         []string{"-c", "exit 7"}, // must never run
		ProbeTimeout: time.Second,
	}
	started, err := EnsureRunning(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("EnsureRunning reported starting a server that was already running")
	}
}

func writeStatus(t *testing.T, w http.ResponseWriter, status health.Status) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(status); err != nil {
		t.Errorf("encode status: %v", err)
	}
}
