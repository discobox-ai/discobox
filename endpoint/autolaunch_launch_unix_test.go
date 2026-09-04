//go:build !windows

package endpoint

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
//
// It is also reported as soon as the process is gone rather than at the start
// timeout. A server that refuses to start — a Windows host with no WSL
// Containers, a project with no harness — exits in milliseconds with its
// explanation already in the log, and spending ten more seconds on it delays
// every command that autolaunches after it.
func TestEnsureRunningReportsWhyTheServerDied(t *testing.T) {
	socket := testSocketPath(t)
	// No systemd-run or systemctl on this PATH, so the launch is the direct
	// one: a server started as a user service belongs to the service manager
	// and this process has no handle on it, which is a different path with a
	// different report. The command below is absolute and runs regardless.
	t.Setenv("PATH", t.TempDir())
	opts := LaunchOptions{
		Endpoint: "unix://" + socket,
		LogPath:  filepath.Join(t.TempDir(), "server.log"),
		Command:  "/bin/sh",
		Args:     []string{"-c", `echo 'unknown command "server" for "discobox"' >&2; exit 1`},
		// Generous, as it is in production. The point of the test is that this
		// is not what bounds the wait.
		StartTimeout:  30 * time.Second,
		ProbeInterval: 20 * time.Millisecond,
		ProbeTimeout:  100 * time.Millisecond,
	}

	start := time.Now()
	_, err := EnsureRunning(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("error %q does not say the server exited", err)
	}
	if !strings.Contains(err.Error(), `unknown command "server"`) {
		t.Fatalf("error %q does not carry what the server said", err)
	}
	if !strings.Contains(err.Error(), opts.LogPath) {
		t.Fatalf("error %q does not say where the rest of the log is", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("waited %s, want the wait to end with the process rather than at the start timeout", elapsed)
	}
}

// The log outlives the launch that wrote it: a server that died and was
// restarted by the next command would otherwise take its own explanation with
// it. Each launch is still reported on its own, from its banner down.
func TestEnsureRunningKeepsEarlierLaunchesInTheLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "server.log")
	launch := func(message string) {
		opts := LaunchOptions{
			Endpoint:      "unix://" + testSocketPath(t),
			LogPath:       logPath,
			Command:       "/bin/sh",
			Args:          []string{"-c", "echo " + message + " >&2; exit 1"},
			StartTimeout:  500 * time.Millisecond,
			ProbeInterval: 20 * time.Millisecond,
			ProbeTimeout:  100 * time.Millisecond,
		}
		_, err := EnsureRunning(context.Background(), opts)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), message) {
			t.Fatalf("error %q does not carry this launch's output", err)
		}
		// The launch before this one is not what failed now, so it is not what
		// the error reports.
		if message == "second-launch" && strings.Contains(err.Error(), "first-launch") {
			t.Fatalf("error %q carries an earlier launch's output", err)
		}
	}
	launch("first-launch")
	launch("second-launch")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first-launch", "second-launch"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("log %q lost %q", data, want)
		}
	}
	if got := strings.Count(string(data), serverLogBanner); got != 2 {
		t.Fatalf("log has %d launch banners, want 2", got)
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
		LogPath:       filepath.Join(t.TempDir(), "server.log"),
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

// The other half of waiting one out: a server that answers "starting" and then
// stops answering has exited, because a server holds its listeners from before
// it opens the database until the process ends. Waiting out the ready deadline
// for a process that is gone is five minutes of a status line saying the step
// it died on — which is exactly what a server that refuses to start without a
// harness would have looked like.
func TestEnsureRunningReportsAServerThatDiedWhileStarting(t *testing.T) {
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
			w.WriteHeader(http.StatusServiceUnavailable)
			writeStatus(t, w, health.Status{
				Status: health.StatusStarting,
				Phase:  "checking the built-in harnesses",
			})
			probes.Add(1)
		}),
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	opts := LaunchOptions{
		Endpoint:      endpointURL,
		LogPath:       filepath.Join(t.TempDir(), "server.log"),
		Command:       "/bin/sh",
		Args:          []string{"-c", "exit 7"}, // must never run
		ProbeInterval: 10 * time.Millisecond,
		ProbeTimeout:  time.Second,
		// Generous, as it is in production. The point of the test is that this
		// is not what bounds the wait.
		ReadyTimeout: 30 * time.Second,
	}
	// Answer a few probes, then stop listening the way an exiting process does.
	go func() {
		for probes.Load() < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		_ = server.Close()
		cleanup()
	}()

	start := time.Now()
	_, err = EnsureRunning(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error for a server that stopped while starting")
	}
	if !strings.Contains(err.Error(), "stopped while starting") {
		t.Fatalf("error %q does not say the server stopped while starting", err)
	}
	if !strings.Contains(err.Error(), "checking the built-in harnesses") {
		t.Fatalf("error %q does not name the step it stopped on", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("waited %s, want the wait to end with the server rather than at the ready deadline", elapsed)
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
		LogPath:      filepath.Join(t.TempDir(), "server.log"),
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

func TestEnsureRunningReplacesAnOlderServer(t *testing.T) {
	socket := testSocketPath(t)
	endpointURL := "unix://" + socket
	listener, _, cleanup, err := Listen(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var oldServer *http.Server
	oldServer = &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz":
				w.WriteHeader(http.StatusServiceUnavailable)
				writeStatus(t, w, health.Status{Status: health.StatusStarting, Phase: "stuck", Version: "v0.5.0"})
			case "/shutdown":
				w.WriteHeader(http.StatusAccepted)
				go func() { _ = oldServer.Close() }()
			default:
				http.NotFound(w, r)
			}
		}),
	}
	go func() { _ = oldServer.Serve(listener) }()

	t.Setenv("PATH", t.TempDir()) // force the directly launched process path
	opts := LaunchOptions{
		Endpoint: endpointURL,
		LogPath:  filepath.Join(t.TempDir(), "server.log"),
		Command:  os.Args[0],
		Args:     []string{"-test.run=^TestAutolaunchReplacementHelper$"},
		Env: []string{
			"DISCOBOX_TEST_SERVER_ENDPOINT=" + endpointURL,
			"DISCOBOX_TEST_SERVER_DELAY=150ms",
		},
		ExpectedVersion: "v0.5.1",
		// The replacement can wait behind the old server's data-directory lock
		// after its listener is gone. Prove that transition gets ReadyTimeout,
		// not this ordinary never-bound-child deadline.
		StartTimeout:  50 * time.Millisecond,
		ReadyTimeout:  5 * time.Second,
		ProbeInterval: 20 * time.Millisecond,
		ProbeTimeout:  time.Second,
	}
	started, err := EnsureRunning(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("EnsureRunning did not replace the older server")
	}

	baseURL, client, err := HTTPClient(endpointURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/shutdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

// TestAutolaunchReplacementHelper is re-executed as the replacement server by
// TestEnsureRunningReplacesAnOlderServer.
func TestAutolaunchReplacementHelper(t *testing.T) {
	endpointURL := os.Getenv("DISCOBOX_TEST_SERVER_ENDPOINT")
	if endpointURL == "" {
		t.Skip("helper process")
	}
	if delay, err := time.ParseDuration(os.Getenv("DISCOBOX_TEST_SERVER_DELAY")); err == nil {
		time.Sleep(delay)
	}
	listener, _, cleanup, err := Listen(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	done := make(chan struct{})
	var server *http.Server
	server = &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz":
				writeStatus(t, w, health.Status{Status: health.StatusReady, Version: "v0.5.1"})
			case "/shutdown":
				w.WriteHeader(http.StatusAccepted)
				go func() {
					_ = server.Close()
					close(done)
				}()
			default:
				http.NotFound(w, r)
			}
		}),
	}
	go func() { _ = server.Serve(listener) }()
	<-done
}

func writeStatus(t *testing.T, w http.ResponseWriter, status health.Status) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(status); err != nil {
		t.Errorf("encode status: %v", err)
	}
}
