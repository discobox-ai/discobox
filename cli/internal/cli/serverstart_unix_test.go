//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/health"
	"github.com/discobox-ai/x/shorttmp"
)

// Starting a server is the slowest thing the CLI ever does on the user's
// behalf, and the wiring that narrates it is one field on a struct — which is
// exactly the kind of thing that goes missing without anything failing to
// build. This asserts the narration reaches the user, not merely that the
// callback exists.
func TestEnsureLocalServerNarratesAStartingServer(t *testing.T) {
	socket := filepath.Join(shorttmp.Dir(t), "s.sock")
	endpointURL := "unix://" + socket
	listener, _, cleanup, err := endpoint.Listen(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var probes atomic.Int32
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if probes.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				encodeStatus(t, w, health.Status{Status: health.StatusStarting, Phase: "migrating the database"})
				return
			}
			encodeStatus(t, w, health.Status{Status: health.StatusReady})
		}),
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	var out bytes.Buffer
	_, app := newRootCommand()
	app.serverURL, app.errOut = endpointURL, &out
	if err := app.ensureLocalServer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "migrating the database") {
		t.Fatalf("nothing said what the server was doing: %q", out.String())
	}
	// Nothing was launched here, so there is no background process to warn
	// about — saying so would be a lie about the user's machine.
	if strings.Contains(out.String(), "started the discobox server") {
		t.Fatalf("claimed to have started a server that was already running: %q", out.String())
	}
}

// --quiet means the command's output is the only output.
func TestEnsureLocalServerStaysQuietWhenAsked(t *testing.T) {
	socket := filepath.Join(shorttmp.Dir(t), "s.sock")
	endpointURL := "unix://" + socket
	listener, _, cleanup, err := endpoint.Listen(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			encodeStatus(t, w, health.Status{Status: health.StatusStarting, Phase: "migrating the database"})
		}),
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	var out bytes.Buffer
	_, app := newRootCommand()
	app.serverURL, app.errOut, app.quiet = endpointURL, &out, true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.ensureLocalServer(ctx)
	if out.Len() != 0 {
		t.Fatalf("quiet run wrote %q", out.String())
	}
}

func encodeStatus(t *testing.T, w http.ResponseWriter, status health.Status) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(status); err != nil {
		t.Errorf("encode status: %v", err)
	}
}

// An invocation gets one autolaunch, at its start, and no more.
//
// Everything that retries — a terminal attach reconnecting, a watch loop — asks
// for a client on every pass, and each ask used to be another chance to launch a
// server. A command that outlived its server then spent the rest of its life
// spawning replacements that lost the race for the singleton lock and exited.
func TestAnInvocationAutolaunchesAtMostOnce(t *testing.T) {
	socket := filepath.Join(shorttmp.Dir(t), "s.sock")
	endpointURL := "unix://" + socket
	listener, _, cleanup, err := endpoint.Listen(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var probes atomic.Int32
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			probes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			encodeStatus(t, w, health.Status{Status: health.StatusReady})
		}),
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	_, app := newRootCommand()
	app.serverURL, app.quiet = endpointURL, true

	if _, _, err := app.httpClientWithAutoStart(true); err != nil {
		t.Fatal(err)
	}
	first := probes.Load()
	if first == 0 {
		t.Fatal("the first client should have checked whether a server was there")
	}

	// The passes a retry loop makes. None of them is a startup.
	for range 5 {
		if _, _, err := app.httpClientWithAutoStart(true); err != nil {
			t.Fatal(err)
		}
	}
	if got := probes.Load(); got != first {
		t.Fatalf("the launch path ran again for a later client: %d probes, want the %d from startup", got, first)
	}
}
