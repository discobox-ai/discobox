package endpoint

import (
	"context"
	"errors"
	"fmt"
	"net"
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
		// A main build sorts between the dot releases around it, and by its
		// commit count among other main builds.
		"stable is older than main":   {server: "v0.5.1", expected: "v0.5.2-main.900.gabcdef0", older: true},
		"main is older than the next": {server: "v0.5.2-main.900.gabcdef0", expected: "v0.5.2", older: true},
		"an older main build":         {server: "v0.5.2-main.9.g1234567", expected: "v0.5.2-main.10.gabcdef0", older: true},
		"a newer main build stays":    {server: "v0.5.2-main.900.gabcdef0", expected: "v0.5.1"},
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
	if _, err := waitReady(context.Background(), opts, time.Now().Add(20*time.Millisecond)); err != nil {
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

// A server upgrading pools on a loaded host can be slow to answer its probe,
// slower than the probe's own timeout, several times in a row. That is a
// server still running, not one that stopped: the wait goes on, and ends in
// its ready answer.
func TestWaitReadyKeepsWaitingOnAServerSlowToAnswer(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch n := probes.Add(1); {
		case n == 1:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"upgrading_pools","poolsUpgrading":1}`))
		case n <= 1+probesBeforeGone:
			// Busy: answers, but after the probe has given up.
			time.Sleep(60 * time.Millisecond)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"upgrading_pools","poolsUpgrading":1}`))
		default:
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		}
	}))
	defer server.Close()
	opts := LaunchOptions{Endpoint: server.URL, ProbeTimeout: 20 * time.Millisecond, UpgradeTimeout: time.Minute, ProbeInterval: time.Millisecond}
	if _, err := waitReady(context.Background(), opts, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("waitReady() = %v, want the wait to go on through slow answers", err)
	}
}

// A server whose listener is gone has stopped, and the wait ends with it
// rather than at the deadline. The error names the step it was on, and a
// server upgrading pools reports that step as its status, not as a phase.
func TestWaitReadyEndsWhenTheListenerIsGone(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"upgrading_pools","poolsUpgrading":2}`))
	}))
	go func() {
		for probes.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		server.Close()
	}()
	// A probe timeout longer than Windows takes to refuse a closed loopback
	// port (about two seconds), so the refusal is what each probe sees there.
	opts := LaunchOptions{Endpoint: server.URL, LogPath: filepath.Join(t.TempDir(), "server.log"), ProbeTimeout: 5 * time.Second, UpgradeTimeout: time.Minute, ProbeInterval: time.Millisecond}
	start := time.Now()
	_, err := waitReady(context.Background(), opts, time.Now().Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "stopped while starting") {
		t.Fatalf("waitReady() = %v, want a server that stopped", err)
	}
	if !strings.Contains(err.Error(), "upgrading pools (2 left)") {
		t.Fatalf("error %q does not name the step it stopped on", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("waited %s, want the wait to end with the server", elapsed)
	}
}

// Every caller enters the wait on an answer, so a server gone by the wait's
// first probe is reported as stopped, not waited out to the deadline and then
// called running.
func TestWaitReadyReportsAServerGoneBeforeItsFirstProbe(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	opts := LaunchOptions{Endpoint: server.URL, LogPath: filepath.Join(t.TempDir(), "server.log"), ProbeTimeout: 5 * time.Second, ProbeInterval: time.Millisecond}
	start := time.Now()
	_, err := waitReady(context.Background(), opts, time.Now().Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "stopped while starting") {
		t.Fatalf("waitReady() = %v, want a server that stopped", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("waited %s, want the wait to end with the server", elapsed)
	}
}

// A server still answering when the wait runs out has not stopped: the error
// says it is still running and what it was waiting on.
func TestWaitReadyDeadlineSaysTheServerIsStillRunning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"upgrading_pools","poolsUpgrading":1}`))
	}))
	defer server.Close()
	opts := LaunchOptions{Endpoint: server.URL, LogPath: filepath.Join(t.TempDir(), "server.log"), UpgradeTimeout: 50 * time.Millisecond, ProbeInterval: time.Millisecond}
	_, err := waitReady(context.Background(), opts, time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("waitReady() = nil, want the deadline to end the wait")
	}
	for _, want := range []string{"still running", "did not finish starting", "upgrading pools (1 left)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error %q says a running server stopped", err)
	}
}

// A server that is up but too busy to answer within the probe timeout is a
// server, not an absent one: a second launch beside it would ask it to shut
// down. EnsureRunning must wait on it and never resolve a command to start.
func TestEnsureRunningWaitsOnAServerSlowerThanTheProbe(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Busy for the first few probes, each answered after its timeout.
		if probes.Add(1) <= 3 {
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()
	var resolved atomic.Bool
	opts := LaunchOptions{
		Endpoint: server.URL, LockPath: filepath.Join(t.TempDir(), "launch.lock"),
		Command: func(context.Context) (Command, error) {
			resolved.Store(true)
			return Command{}, fmt.Errorf("a command was resolved beside a running server")
		},
		ProbeTimeout: 20 * time.Millisecond, ProbeInterval: time.Millisecond, ReadyTimeout: 10 * time.Second,
	}
	started, err := EnsureRunning(context.Background(), opts)
	if resolved.Load() || started || err != nil {
		t.Fatalf("EnsureRunning() = %v, %v (command resolved: %v); want a wait on the running server", started, err, resolved.Load())
	}
}

// The probe under the launch lock can find a server the first probe did not:
// one another CLI started while this one waited for the lock. Whether it
// answers "starting" or is too busy to answer in time, it is waited on, and
// then nothing more: launching after the wait would start a second server
// beside it, which asks the first to shut down.
func TestEnsureRunningUnderTheLockWaitsOnAServerThatAppeared(t *testing.T) {
	for name, handler := range map[string]func(n int32, w http.ResponseWriter){
		"starting": func(n int32, w http.ResponseWriter) {
			if n <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"starting","phase":"migrating"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		},
		"busy": func(n int32, w http.ResponseWriter) {
			if n <= 2 {
				time.Sleep(time.Second)
			}
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			// An address nothing listens on yet, for the first probe to find
			// empty.
			reserved, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := reserved.Addr().String()
			_ = reserved.Close()

			lockPath := filepath.Join(t.TempDir(), "launch.lock")
			unlock, err := acquireLaunchLock(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			waiting := make(chan struct{})
			var resolved atomic.Bool
			opts := LaunchOptions{
				Endpoint: "http://" + addr, LockPath: lockPath,
				Command: func(context.Context) (Command, error) {
					resolved.Store(true)
					return Command{}, fmt.Errorf("a command was resolved beside a running server")
				},
				// The default probe timeout: on Windows a closed loopback port
				// is refused only after it, and must still read as empty.
				ProbeInterval: time.Millisecond, ReadyTimeout: time.Minute,
				OnProgress: func(status health.Status) {
					if status.Phase == "waiting for the startup lock" {
						close(waiting)
					}
				},
			}
			type result struct {
				started bool
				err     error
			}
			done := make(chan result, 1)
			go func() {
				started, err := EnsureRunning(context.Background(), opts)
				done <- result{started, err}
			}()
			select {
			case <-waiting:
			case r := <-done:
				t.Fatalf("EnsureRunning() = %v, %v before it waited for the lock", r.started, r.err)
			}

			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			var probes atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				handler(probes.Add(1), w)
			}))
			_ = server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			unlock()

			r := <-done
			if resolved.Load() || r.started || r.err != nil {
				t.Fatalf("EnsureRunning() = %v, %v (command resolved: %v); want a wait on the server that appeared", r.started, r.err, resolved.Load())
			}
		})
	}
}

// A busy server is waited on until it answers, and its answer is then judged
// like any other: one older than this CLI is still replaced. Here the
// replacement's command is resolved, which is the first step of replacing it.
func TestEnsureRunningReplacesAnOlderServerItWaitedOn(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) <= 2 {
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`{"status":"ready","version":"v0.5.0"}`))
	}))
	defer server.Close()
	errResolved := fmt.Errorf("the replacement's command was resolved")
	opts := LaunchOptions{
		Endpoint: server.URL, LockPath: filepath.Join(t.TempDir(), "launch.lock"),
		Command:         func(context.Context) (Command, error) { return Command{}, errResolved },
		ExpectedVersion: "v0.5.1",
		ProbeTimeout:    20 * time.Millisecond, ProbeInterval: time.Millisecond, ReadyTimeout: 10 * time.Second,
	}
	if _, err := EnsureRunning(context.Background(), opts); !errors.Is(err, errResolved) {
		t.Fatalf("EnsureRunning() = %v, want the older server it waited on replaced", err)
	}
}

// A server that is listening and never answers is waited on, not replaced.
// The wait says so on the progress line, once, and ends at the deadline with
// an error that says the server is listening and not answering — not that it
// was starting, which it never said.
func TestWaitReadyOnASilentServerSaysItIsNotAnswering(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)
	var reported []health.Status
	opts := LaunchOptions{
		Endpoint: server.URL, LogPath: filepath.Join(t.TempDir(), "server.log"),
		ProbeTimeout: 10 * time.Millisecond, ProbeInterval: time.Millisecond,
		OnProgress: func(status health.Status) { reported = append(reported, status) },
	}
	_, err := waitReady(context.Background(), opts, time.Now().Add(200*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "is listening but has not answered") {
		t.Fatalf("waitReady() = %v, want a server listening and not answering", err)
	}
	if len(reported) != 1 || !reported[0].Waiting() || reported[0].Phase != "waiting for the server to answer" {
		t.Fatalf("progress = %+v, want one line saying it waits for the server to answer", reported)
	}
}

// A slow probe draws "waiting for the server to answer" over the step the
// server last reported. When it answers again with that same step, the line
// goes back to it rather than staying on the wait: on a loaded host a pool
// upgrade answers late now and then for minutes.
//
// The probe timeout is one a loaded runner's answer meets, and the slow probe
// never answers: its handler waits until the client gives up and closes the
// connection, so the timeout is the only thing that ends it (and what lets
// server.Close return). A fast probe that ran past a short timeout on a
// loaded Windows runner went unreported, and the redraw with it (#1637).
func TestWaitReadyRedrawsTheStepAfterASlowProbe(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch probes.Add(1) {
		case 1, 3:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"upgrading_pools","poolsUpgrading":1}`))
		case 2:
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		}
	}))
	defer server.Close()
	var lines []string
	opts := LaunchOptions{
		Endpoint: server.URL, ProbeTimeout: 2 * time.Second, UpgradeTimeout: time.Minute, ProbeInterval: time.Millisecond,
		OnProgress: func(status health.Status) { lines = append(lines, status.Status+"/"+status.Phase) },
	}
	if _, err := waitReady(context.Background(), opts, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("waitReady() = %v", err)
	}
	want := []string{
		health.StatusUpgradingPools + "/",
		health.StatusStarting + "/waiting for the server to answer",
		health.StatusUpgradingPools + "/",
		health.StatusReady + "/",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("progress = %q, want %q", lines, want)
	}
}
