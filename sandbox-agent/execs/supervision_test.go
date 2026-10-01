//go:build !windows

package execs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain lets this test binary be its own exec shim. Both unit managers
// start a shim by running the agent's own executable with "exec-shim", and
// under `go test` that executable is this binary — so the contract below runs
// real shims, in processes of their own, exactly as a sandbox does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "exec-shim" {
		os.Exit(runTestShim(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runTestShim is the agent's exec-shim entry point without the transcript
// store, which is the agent's to open and no part of supervision.
func runTestShim(args []string) int {
	parsed, err := ParseShimArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if parsed.Lifetime != nil {
		if err := HoldLifetime(*parsed.Lifetime); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := RunShim(ctx, parsed.Config); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestSupervisorPassesTheUnitManagerContract(t *testing.T) {
	testUnitManagerContract(t, func(_ *testing.T, runtimeDir string) UnitManager {
		return NewSupervisor(filepath.Join(runtimeDir, supervisorDirName))
	})
}

// testUnitManagerContract is what every unit manager must do for the exec
// manager above it, run through that manager against real shims: start,
// stop, the shim's exit status, and convergence across an agent restart.
// newUnits is called once per agent lifetime, with the runtime directory both
// lifetimes share.
func testUnitManagerContract(t *testing.T, newUnits func(t *testing.T, runtimeDir string) UnitManager) {
	t.Run("ExitStatus", func(t *testing.T) {
		agent := startContractAgent(t, newUnits, contractRuntimeDir(t), newContractAudit())
		exec := agent.run(t, "exit 7")
		// A command that exits non-zero reads failed, and either way the run is
		// over and carries the code the shim recorded.
		got := agent.waitFor(t, exec.ID, "the shim's exit status", settled)
		if got.ExitCode == nil || *got.ExitCode != 7 {
			t.Fatalf("exit code = %v, want 7: the shim's write is the exit status", got.ExitCode)
		}
	})

	t.Run("Stop", func(t *testing.T) {
		agent := startContractAgent(t, newUnits, contractRuntimeDir(t), newContractAudit())
		exec := agent.run(t, "sleep 600")
		running := agent.waitFor(t, exec.ID, "running", func(exec Exec) bool {
			return exec.Status == StatusRunning && exec.PID > 0
		})
		stopped, err := agent.manager.Stop(context.Background(), exec.ID)
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
		if stopped.Status != StatusExited || !stopped.Stopped {
			t.Fatalf("stopped exec = %s (stopped %v), want exited and stopped", stopped.Status, stopped.Stopped)
		}
		status, err := agent.units.Status(context.Background(), exec.Unit)
		if err != nil {
			t.Fatalf("unit status: %v", err)
		}
		if status.Loaded {
			t.Fatalf("unit %s is still loaded after a stop", exec.Unit)
		}
		// The command, not only the shim: stopping a unit ends what it ran.
		waitUntil(t, "the stopped command to exit", func() bool {
			return syscall.Kill(int(running.PID), 0) != nil
		})
		// And the stop is the last word: what the unit going away reports
		// afterwards does not turn a requested stop into a lost exec.
		time.Sleep(200 * time.Millisecond)
		if got, _ := agent.manager.Get(exec.ID); got.Status != StatusExited || !got.Stopped {
			t.Fatalf("exec after the unit went away = %s (stopped %v), want exited and stopped", got.Status, got.Stopped)
		}
	})

	t.Run("ConvergesAfterAgentRestart", func(t *testing.T) {
		runtimeDir := contractRuntimeDir(t)
		audit := newContractAudit()
		release := filepath.Join(t.TempDir(), "release")
		first := startContractAgent(t, newUnits, runtimeDir, audit)
		survivor := first.run(t, "sleep 600")
		finisher := first.run(t, fmt.Sprintf("while [ ! -e %q ]; do sleep 0.05; done; exit 3", release))
		for _, exec := range []Exec{survivor, finisher} {
			first.waitFor(t, exec.ID, "running", func(exec Exec) bool { return exec.Status == StatusRunning })
		}
		// The agent goes away. Its shims do not.
		first.stop()

		second := startContractAgent(t, newUnits, runtimeDir, audit)
		if got, _ := second.manager.Get(survivor.ID); got.Status != StatusRunning {
			t.Fatalf("an exec still running across the restart reads %s", got.Status)
		}

		// An ordinary exit, which the shim writes.
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		got := second.waitFor(t, finisher.ID, "the exit after the restart", settled)
		if got.ExitCode == nil || *got.ExitCode != 3 {
			t.Fatalf("exit code = %v, want 3", got.ExitCode)
		}

		// An end the shim could not write: only the unit manager can report it.
		status, err := second.units.Status(context.Background(), survivor.Unit)
		if err != nil || !status.Loaded || status.PID <= 0 {
			t.Fatalf("surviving unit = %+v, %v; want a loaded unit with its shim's pid", status, err)
		}
		if err := syscall.Kill(int(status.PID), syscall.SIGKILL); err != nil {
			t.Fatalf("kill the shim: %v", err)
		}
		second.waitFor(t, survivor.ID, "the killed shim's exec to read lost", func(exec Exec) bool {
			return exec.Status == StatusLost
		})
	})
}

// contractAgent is one lifetime of an agent: a unit manager, the exec manager
// over it, and the watcher converging the two.
type contractAgent struct {
	units   UnitManager
	manager *Manager
	audit   *contractAudit
	stop    func()
}

func startContractAgent(t *testing.T, newUnits func(*testing.T, string) UnitManager, runtimeDir string, audit *contractAudit) *contractAgent {
	t.Helper()
	units := newUnits(t, runtimeDir)
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: t.TempDir(),
		RuntimeDir:  runtimeDir,
		Units:       units,
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	watcher := NewWatcher(WatcherConfig{Manager: manager, Logger: slog.New(slog.DiscardHandler)})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()
	agent := &contractAgent{units: units, manager: manager, audit: audit}
	var stopped bool
	agent.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
	}
	t.Cleanup(agent.stop)
	// Whatever a test leaves running is stopped, through the unit manager
	// whichever lifetime it was started under.
	t.Cleanup(func() {
		for _, exec := range manager.List() {
			_ = units.Stop(context.Background(), exec.Unit)
		}
	})
	return agent
}

// run starts a shell command as an exec, the way the API does: create, then
// start.
func (a *contractAgent) run(t *testing.T, script string) Exec {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exec, err := a.manager.Create(ctx, CreateRequest{Command: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := a.manager.Start(ctx, exec.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	return exec
}

// waitFor waits on what the agent last *observed* of an exec, never asking the
// manager: a read refreshes the exec itself, and what this contract is about
// is that the agent converges with nobody asking (ADR 0115).
func (a *contractAgent) waitFor(t *testing.T, id, what string, done func(Exec) bool) Exec {
	t.Helper()
	var last Exec
	waitUntilSaying(t, what, func() bool {
		exec, ok := a.audit.latest(id)
		last = exec
		return ok && done(exec)
	}, func() string {
		return fmt.Sprintf("last read %s (exit code %v, error %q)", last.Status, last.ExitCode, last.Error)
	})
	return last
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	waitUntilSaying(t, what, done, func() string { return "" })
}

func waitUntilSaying(t *testing.T, what string, done func() bool, state func() string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; %s", what, state())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// contractAudit is the agent's store as the contract needs it: durable
// records that outlive an agent lifetime, and the latest status each exec was
// observed at. The watcher writes it from its own goroutine.
type contractAudit struct {
	mu       sync.Mutex
	records  map[string]Exec
	observed map[string]Exec
}

func newContractAudit() *contractAudit {
	return &contractAudit{records: map[string]Exec{}, observed: map[string]Exec{}}
}

func (a *contractAudit) latest(id string) (Exec, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exec, ok := a.observed[id]
	return cloneExec(exec), ok
}

func (a *contractAudit) RecordExecEvent(context.Context, string, string, string, map[string]any) error {
	return nil
}

func (a *contractAudit) ObserveExec(_ context.Context, exec Exec) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observed[exec.ID] = cloneExec(exec)
	return nil
}

func (a *contractAudit) SaveExecRecord(_ context.Context, exec Exec) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.records[exec.ID]; !ok {
		a.records[exec.ID] = cloneExec(exec)
	}
	return nil
}

func (a *contractAudit) LoadExecRecords(context.Context) ([]Exec, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Exec, 0, len(a.records))
	for _, exec := range a.records {
		out = append(out, cloneExec(exec))
	}
	return out, nil
}

func (a *contractAudit) DeleteExecRecord(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.records, id)
	delete(a.observed, id)
	return nil
}

// contractRuntimeDir is short on purpose: a shim's socket lives in it, and a
// Unix socket path has a length limit t.TempDir's nested names can exceed.
func contractRuntimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
