package execs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingUnits answers unit queries with a status the test chooses, and counts
// how often it was asked. It is what proves a listing costs one query for the
// whole set rather than one per exec.
type countingUnits struct {
	fakeUnitManager

	mu          sync.Mutex
	statusCalls int
	listCalls   int
	units       map[string]UnitStatus
	changes     chan string
	// watchErr makes Watch fail, standing in for a sandbox with no dbus-daemon.
	watchErr error
}

func newCountingUnits() *countingUnits {
	return &countingUnits{units: map[string]UnitStatus{}, changes: make(chan string, 8)}
}

func (c *countingUnits) setUnit(status UnitStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.units[status.Unit] = status
}

func (c *countingUnits) Status(_ context.Context, unit string) (UnitStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusCalls++
	if status, ok := c.units[unitBaseName(unit)]; ok {
		return status, nil
	}
	return UnitStatus{Unit: unitBaseName(unit)}, nil
}

func (c *countingUnits) List(context.Context) ([]UnitStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	out := make([]UnitStatus, 0, len(c.units))
	for _, status := range c.units {
		out = append(out, status)
	}
	return out, nil
}

func (c *countingUnits) Watch(context.Context) (<-chan string, error) {
	if c.watchErr != nil {
		return nil, c.watchErr
	}
	return c.changes, nil
}

func (c *countingUnits) counts() (status int, list int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusCalls, c.listCalls
}

// A listing joins every exec with one query for all of their units. Asking per
// exec as well is what made a listing cost 3N+1 systemd queries.
func TestListQueriesEveryUnitInOneCall(t *testing.T) {
	units := newCountingUnits()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	for range 3 {
		created, err := manager.Create(ctx, CreateRequest{Command: []string{"sleep", "600"}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		units.setUnit(UnitStatus{Unit: created.Unit, Loaded: true, Active: true, Status: StatusRunning})
	}
	beforeStatus, beforeList := units.counts()

	if got := len(manager.List()); got != 3 {
		t.Fatalf("listed %d execs, want 3", got)
	}

	afterStatus, afterList := units.counts()
	if afterList-beforeList != 1 {
		t.Errorf("unit list calls = %d, want exactly 1 for the whole listing", afterList-beforeList)
	}
	if afterStatus != beforeStatus {
		t.Errorf("per-unit status calls = %d, want none: the listing already read every unit", afterStatus-beforeStatus)
	}
}

// The shim writes its own exit into the runtime file, and that write is the
// notification (ADR 0115 §1). Nothing polls and nothing asks: the watcher
// converges the exec because the file changed.
func TestWatcherObservesShimRuntimeWrite(t *testing.T) {
	units := newCountingUnits()
	observed := newObservingAudit()
	dir := t.TempDir()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  dir,
		Units:       units,
		Audit:       observed,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	created, err := manager.Create(ctx, CreateRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	units.setUnit(UnitStatus{Unit: created.Unit, Loaded: true, Active: true, Status: StatusRunning})

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewWatcher(WatcherConfig{Manager: manager}).Run(ctx)
	}()

	// The shim's write: the command is over, and this is the only record of its
	// exit status — systemd collects the transient unit as it dies.
	exited := created
	exited.Status = StatusExited
	code := int64(7)
	exited.ExitCode = &code
	at := time.Now().UTC()
	exited.ExitedAt = &at
	// Repeated rather than written once: Run establishes its watch on its own
	// goroutine, and a write that lands first would produce no event and no
	// second chance.
	stop := pump(t, func() {
		_ = writeRuntime(filepath.Join(dir, created.ID+".json"), exited)
	})
	defer stop()

	if got := observed.await(t, created.ID, StatusExited); got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("observed exit code = %v, want 7", got.ExitCode)
	}
	cancel()
	<-done
}

// The shim cannot report an end it did not survive — killed, out of memory, or
// a unit lost to a reboot. systemd's unit change is the only notification for
// that, and it is what demotes the exec to lost (ADR 0115 §2).
func TestWatcherObservesUnitChangeAsLost(t *testing.T) {
	units := newCountingUnits()
	observed := newObservingAudit()
	dir := t.TempDir()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  dir,
		Units:       units,
		Audit:       observed,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	created, err := manager.Create(ctx, CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Running as far as anything on disk knows, with a shim that never wrote.
	running := created
	running.Status = StatusRunning
	startedAt := time.Now().UTC()
	running.StartedAt = &startedAt
	if err := writeRuntime(filepath.Join(dir, created.ID+".json"), running); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	// The unit is gone, which is what systemd reports once it has collected it.
	units.setUnit(UnitStatus{Unit: created.Unit, Loaded: false, Status: StatusExited})

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewWatcher(WatcherConfig{Manager: manager}).Run(ctx)
	}()

	stop := pump(t, func() {
		select {
		case units.changes <- created.Unit:
		default:
		}
	})
	defer stop()

	got := observed.await(t, created.ID, StatusLost)
	if got.Error == "" {
		t.Error("a lost exec was recorded with no reason")
	}
	cancel()
	<-done
}

// A refresh that changed nothing must not rewrite the runtime file: the
// directory is watched for the shim's writes, and rewriting it here would wake
// the watcher with this process's own echo.
func TestRefreshDoesNotRewriteAnUnchangedRuntimeFile(t *testing.T) {
	units := newCountingUnits()
	dir := t.TempDir()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  dir,
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	created, err := manager.Create(ctx, CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	path := filepath.Join(dir, created.ID+".json")
	// A started exec, so the unit is what its status follows: a starting one is
	// deliberately never demoted, and would never change here.
	running := created
	running.Status = StatusRunning
	startedAt := time.Now().UTC().Truncate(time.Second)
	running.StartedAt = &startedAt
	if err := writeRuntime(path, running); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	units.setUnit(UnitStatus{
		Unit:      created.Unit,
		Loaded:    true,
		Active:    true,
		Status:    StatusRunning,
		StartedAt: &startedAt,
	})

	// Settle: the first sweep is allowed to write whatever it learned.
	manager.Sweep(ctx)
	stale := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("age the runtime file: %v", err)
	}

	manager.Sweep(ctx)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat runtime file: %v", err)
	}
	if !info.ModTime().Equal(stale) {
		t.Fatalf("the runtime file was rewritten with nothing to change (mtime %v)", info.ModTime())
	}

	// A real change still lands: the unit is gone, so the exec is lost.
	units.setUnit(UnitStatus{Unit: created.Unit, Loaded: false, Status: StatusExited})
	manager.Sweep(ctx)
	if info, err := os.Stat(path); err != nil || info.ModTime().Equal(stale) {
		t.Fatalf("a changed exec did not rewrite its runtime file (err %v)", err)
	}
}

// A watcher that could not subscribe is degraded, not blind: it sweeps on an
// interval so a sandbox with no dbus-daemon still converges (ADR 0115 §3).
func TestWatcherWithoutASubscriptionSweeps(t *testing.T) {
	units := newCountingUnits()
	units.watchErr = errors.New("no dbus-daemon")
	observed := newObservingAudit()
	dir := t.TempDir()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  dir,
		Units:       units,
		Audit:       observed,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	created, err := manager.Create(ctx, CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	running := created
	running.Status = StatusRunning
	startedAt := time.Now().UTC()
	running.StartedAt = &startedAt
	if err := writeRuntime(filepath.Join(dir, created.ID+".json"), running); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	units.setUnit(UnitStatus{Unit: created.Unit, Loaded: false, Status: StatusExited})

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewWatcher(WatcherConfig{Manager: manager, FallbackInterval: 10 * time.Millisecond}).Run(ctx)
	}()

	// Nothing notifies: only the fallback sweep can find this.
	observed.await(t, created.ID, StatusLost)
	cancel()
	<-done
}

// pump repeats a stimulus until the returned stop is called. The watcher
// establishes its notifications on its own goroutine, so a test that delivers
// one exactly once is racing that setup.
func pump(t *testing.T, deliver func()) func() {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			deliver()
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
}

// The runtime file must not change when only the shim's live fields moved, or
// the watcher on that directory feeds itself: write, inotify, refresh, write.
// LastAccessedAt is time.Now() for as long as a client is attached, so for an
// attached terminal "changed" would otherwise be true on every single refresh.
func TestRuntimeFileIgnoresLiveShimFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ex_live.json")
	at := time.Now().UTC()
	exec := Exec{
		ID:        "ex_live",
		Status:    StatusRunning,
		Unit:      "discobox-exec-ex_live",
		Command:   []string{"sleep", "600"},
		CreatedAt: at,
		StartedAt: &at,
	}
	if err := writeRuntime(path, exec); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	stale := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("age the runtime file: %v", err)
	}

	// Exactly what a refresh of an attached exec produces: every live field
	// moved, nothing durable did.
	live := exec
	live.AttacherCount = 2
	live.Title = "claude ✳ working"
	moved := time.Now().UTC()
	live.TitleChangedAt = &moved
	live.LastAccessedAt = &moved
	if err := writeRuntimeIfChanged(path, live); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(stale) {
		t.Fatal("live shim fields rewrote the runtime file; the watcher would wake itself forever")
	}

	// And they are not in the file at all: they do not outlive the shim.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, field := range []string{"attacherCount", "title", "titleChangedAt", "lastAccessedAt"} {
		if strings.Contains(string(data), field) {
			t.Errorf("runtime file persists the live field %q", field)
		}
	}

	// A durable change still writes.
	live.Status = StatusExited
	if err := writeRuntimeIfChanged(path, live); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.ModTime().Equal(stale) {
		t.Fatalf("a durable change did not rewrite the runtime file (err %v)", err)
	}
}

// observingAudit captures every ObserveExec, which is how a test sees that
// convergence happened without asking the manager and converging it itself.
type observingAudit struct {
	mu       sync.Mutex
	seen     map[string]Exec
	recordCh chan struct{}
	records  map[string]Exec
}

func newObservingAudit() *observingAudit {
	return &observingAudit{
		seen:     map[string]Exec{},
		records:  map[string]Exec{},
		recordCh: make(chan struct{}, 64),
	}
}

func (a *observingAudit) RecordExecEvent(context.Context, string, string, string, map[string]any) error {
	return nil
}

func (a *observingAudit) ObserveExec(_ context.Context, exec Exec) error {
	a.mu.Lock()
	a.seen[exec.ID] = cloneExec(exec)
	a.mu.Unlock()
	select {
	case a.recordCh <- struct{}{}:
	default:
	}
	return nil
}

func (a *observingAudit) SaveExecRecord(_ context.Context, exec Exec) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.records[exec.ID]; !ok {
		a.records[exec.ID] = cloneExec(exec)
	}
	return nil
}

func (a *observingAudit) LoadExecRecords(context.Context) ([]Exec, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Exec, 0, len(a.records))
	for _, exec := range a.records {
		out = append(out, cloneExec(exec))
	}
	return out, nil
}

// await waits for the exec to be observed in the given status. The watcher runs
// on its own goroutine, so the test waits for the observation rather than
// guessing how long a notification takes.
func (a *observingAudit) await(t *testing.T, id string, want Status) Exec {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		a.mu.Lock()
		exec, ok := a.seen[id]
		a.mu.Unlock()
		if ok && exec.Status == want {
			return exec
		}
		select {
		case <-a.recordCh:
		case <-deadline:
			t.Fatalf("exec %s was never observed as %s (last seen %q)", id, want, exec.Status)
		}
	}
}
