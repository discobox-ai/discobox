package services

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// fakeUnits stands in for systemd, and for the shim systemd would start.
//
// The shim half is what matters: an exec is started by dialing the socket its
// shim listens on, so a fake that only records the launch leaves every start
// timing out. This binds that socket and answers the two requests the exec
// layer makes of it — start and status — from a record it keeps, which is
// exactly the shim's role.
type fakeUnits struct {
	starts []execs.StartRequest
	stops  []string
	// exit, when set, is what a launched process does instead of running: it is
	// how a test says "this service ended on its own".
	exit *int64

	mu    sync.Mutex
	shims map[string]*fakeShim
}

func newFakeUnits(t *testing.T) *fakeUnits {
	units := &fakeUnits{shims: map[string]*fakeShim{}}
	t.Cleanup(units.closeAll)
	return units
}

func (f *fakeUnits) Start(_ context.Context, req execs.StartRequest) (execs.StartResult, error) {
	f.starts = append(f.starts, req)
	// The record the shim publishes starts from the one the exec layer wrote
	// before launching, so identity and metadata are the real ones.
	record := readExecRecord(req.RuntimePath)
	now := time.Now().UTC()
	record.Unit = req.Unit
	record.StartedAt = &now
	record.PID = 4242
	if f.exit != nil {
		record.Status = execs.StatusExited
		record.ExitCode = f.exit
		record.ExitedAt = &now
	} else {
		record.Status = execs.StatusRunning
	}
	shim, err := newFakeShim(req.SocketPath, record)
	if err != nil {
		return execs.StartResult{}, err
	}
	f.mu.Lock()
	// A relaunch reuses the socket path under a fresh unit generation, so
	// whatever was listening there is gone by the time this binds.
	if previous, ok := f.shims[req.Unit]; ok {
		previous.close()
	}
	f.shims[req.Unit] = shim
	f.mu.Unlock()
	return execs.StartResult{Unit: req.Unit, PID: 4242}, nil
}

func (f *fakeUnits) Stop(_ context.Context, unit string) error {
	f.stops = append(f.stops, unit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if shim, ok := f.shims[unit]; ok {
		shim.close()
		delete(f.shims, unit)
	}
	return nil
}

func (f *fakeUnits) Status(_ context.Context, unit string) (execs.UnitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, live := f.shims[unit]
	return execs.UnitStatus{Unit: unit, Loaded: true, Active: live}, nil
}

func (f *fakeUnits) List(context.Context) ([]execs.UnitStatus, error) { return nil, nil }

func (f *fakeUnits) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for unit, shim := range f.shims {
		shim.close()
		delete(f.shims, unit)
	}
}

// fakeShim serves the exec shim's local socket API: the record it holds is what
// start and status report.
type fakeShim struct {
	listener net.Listener
	server   *http.Server
}

func newFakeShim(socketPath string, record execs.Exec) (*fakeShim, error) {
	// A previous generation's socket file may still be on disk; the real shim
	// has it removed for it before it binds.
	_ = os.Remove(socketPath)
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "unix", socketPath)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(record)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	answer := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	mux.HandleFunc("/start", answer)
	mux.HandleFunc("/status", answer)
	shim := &fakeShim{listener: listener, server: &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}}
	go func() { _ = shim.server.Serve(listener) }()
	return shim, nil
}

func (s *fakeShim) close() {
	_ = s.server.Close()
	_ = s.listener.Close()
}

// readExecRecord reads the runtime file the exec layer wrote before launching.
func readExecRecord(path string) execs.Exec {
	var record execs.Exec
	data, err := os.ReadFile(path)
	if err != nil {
		return record
	}
	_ = json.Unmarshal(data, &record)
	return record
}

func newTestManager(t *testing.T) (*Manager, *fakeUnits, string) {
	t.Helper()
	root := t.TempDir()
	units := newFakeUnits(t)
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: root,
		RuntimeDir:  filepath.Join(t.TempDir(), "rt"),
		Env:         map[string]string{"PATH": "/usr/bin", "SHELL": "/bin/sh"},
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	manager, err := NewManager(ManagerConfig{Execs: execManager, Root: root})
	if err != nil {
		t.Fatalf("new service manager: %v", err)
	}
	return manager, units, root
}

// A service is an exec: a login shell running the declared script, on pipes,
// tagged with the service it runs (ADR 0068 §2, §3).
func TestStartCreatesAPipeExecTaggedWithItsService(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-discobox-api.sh", apiScript, 0o755)

	service, err := manager.Start(context.Background(), "discobox-api")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if service.Status != StatusRunning {
		t.Fatalf("status = %q, want running", service.Status)
	}
	if service.ExecID == "" {
		t.Fatal("a started service must report the exec running it")
	}
	if len(units.starts) != 1 {
		t.Fatalf("started %d units, want 1", len(units.starts))
	}
	start := units.starts[0]
	if start.TTY {
		t.Error("a service runs on pipes, not a PTY")
	}
	if start.Metadata[MetadataServiceID] != "discobox-api" {
		t.Errorf("metadata %s = %q", MetadataServiceID, start.Metadata[MetadataServiceID])
	}
	if start.Metadata[MetadataServiceName] != "Discobox API" {
		t.Errorf("metadata %s = %q", MetadataServiceName, start.Metadata[MetadataServiceName])
	}
	// The script runs through a login shell, so it gets the PATH and profile a
	// person typing the same command would get.
	if len(start.Command) != 3 || start.Command[1] != "-lc" {
		t.Fatalf("command = %v, want a login shell running the script", start.Command)
	}
	if want := filepath.Join(root, DirName, "10-discobox-api.sh"); !strings.Contains(start.Command[2], want) {
		t.Errorf("command line = %q, want it to run %q", start.Command[2], want)
	}
	// The path is quoted, so a checkout with a space in it still starts.
	if !strings.HasPrefix(start.Command[2], "'") {
		t.Errorf("command line = %q, want the script path quoted", start.Command[2])
	}
	// And it runs in the repository root it was declared in, so the relative
	// paths a service script reads its own repository through resolve.
	if start.Workdir != root {
		t.Errorf("workdir = %q, want the repository root %q", start.Workdir, root)
	}
}

// The workdir is named on the request rather than left to the exec default, so
// a service still runs where its declaration lives when the two differ.
func TestAServiceRunsInItsDeclaringRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	units := newFakeUnits(t)
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: root,
		// A sandbox whose execs start somewhere else entirely.
		DefaultWorkdir: t.TempDir(),
		RuntimeDir:     filepath.Join(t.TempDir(), "rt"),
		Env:            map[string]string{"PATH": "/usr/bin", "SHELL": "/bin/sh"},
		Units:          units,
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	manager, err := NewManager(ManagerConfig{Execs: execManager, Root: root})
	if err != nil {
		t.Fatalf("new service manager: %v", err)
	}
	writeService(t, root, "10-api.sh", apiScript, 0o755)

	if _, err := manager.Start(context.Background(), "api"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := units.starts[0].Workdir; got != root {
		t.Fatalf("workdir = %q, want the declaring root %q", got, root)
	}

	// And a restart resumes there: the workdir is the record's, not the
	// default's, so nothing re-derives it per run.
	if _, err := manager.Restart(context.Background(), "api"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := units.starts[1].Workdir; got != root {
		t.Fatalf("workdir after restart = %q, want %q", got, root)
	}
}

// Starting a running service changes nothing: the verb is idempotent, which is
// what lets boot's autostart and a client's start overlap harmlessly.
func TestStartIsIdempotentWhileRunning(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)

	first, err := manager.Start(context.Background(), "api")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	second, err := manager.Start(context.Background(), "api")
	if err != nil {
		t.Fatalf("start again: %v", err)
	}
	if first.ExecID != second.ExecID {
		t.Fatalf("exec id changed on a second start: %q then %q", first.ExecID, second.ExecID)
	}
	if len(units.starts) != 1 {
		t.Fatalf("started %d units, want 1: a running service must not be started twice", len(units.starts))
	}
}

// Stopping keeps the record, and the record says it was stopped rather than
// that it crashed.
func TestStopKeepsTheRecordAndSaysItWasStopped(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)

	started, err := manager.Start(context.Background(), "api")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	stopped, err := manager.Stop(context.Background(), "api")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", stopped.Status)
	}
	if stopped.ExecID != started.ExecID {
		t.Fatalf("stopping changed the exec id: %q then %q", started.ExecID, stopped.ExecID)
	}
	if len(units.stops) != 1 {
		t.Fatalf("stopped %d units, want 1", len(units.stops))
	}
	// And the listing agrees, which is what a second window would see.
	listed, err := manager.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Status != StatusStopped {
		t.Fatalf("listed = %+v, want one stopped service", listed)
	}
}

// Stopping something nobody ever started is the state being asked for, not an
// error.
func TestStopUnstartedServiceSucceeds(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)

	service, err := manager.Stop(context.Background(), "api")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if service.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", service.Status)
	}
	if len(units.stops) != 0 {
		t.Fatalf("stopped %d units, want none", len(units.stops))
	}
}

// A restart resumes the same identity, so the transcript and the workspace tab
// keyed on it survive (ADR 0038).
func TestRestartKeepsTheExecID(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)

	started, err := manager.Start(context.Background(), "api")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	restarted, err := manager.Restart(context.Background(), "api")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restarted.ExecID != started.ExecID {
		t.Fatalf("restart changed the exec id: %q then %q", started.ExecID, restarted.ExecID)
	}
	if restarted.Status != StatusRunning {
		t.Fatalf("status = %q, want running", restarted.Status)
	}
	if len(units.starts) != 2 {
		t.Fatalf("started %d units, want 2", len(units.starts))
	}
	// Each run gets its own transient unit generation, which is what keeps the
	// audit trail's unit references unambiguous.
	if units.starts[0].Unit == units.starts[1].Unit {
		t.Errorf("both runs used unit %q; a relaunch must take a fresh generation", units.starts[0].Unit)
	}
}

// Restarting a service that is not running just starts it, so restart is the
// verb that always ends with it up.
func TestRestartStartsAStoppedService(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)

	service, err := manager.Restart(context.Background(), "api")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if service.Status != StatusRunning {
		t.Fatalf("status = %q, want running", service.Status)
	}
	if len(units.stops) != 0 {
		t.Fatalf("stopped %d units, want none: nothing was running", len(units.stops))
	}
	if len(units.starts) != 1 {
		t.Fatalf("started %d units, want 1", len(units.starts))
	}
}

// Nothing restarts a service that ends on its own; its exit status is reported
// and it is left alone (ADR 0068 §4).
func TestAServiceThatExitsStaysExited(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int64
		want Status
	}{
		{"clean exit", 0, StatusExited},
		{"failure", 1, StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, units, root := newTestManager(t)
			units.exit = &tc.code
			writeService(t, root, "10-api.sh", apiScript, 0o755)

			if _, err := manager.Start(context.Background(), "api"); err != nil {
				t.Fatalf("start: %v", err)
			}
			service, err := manager.Get("api")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if service.Status != tc.want {
				t.Fatalf("status = %q, want %q", service.Status, tc.want)
			}
			if len(units.starts) != 1 {
				t.Fatalf("started %d units, want 1: nothing restarts a service that ended", len(units.starts))
			}
		})
	}
}

// A declaration that cannot run is refused rather than started, and says why.
func TestStartRefusesAnUnrunnableDeclaration(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o644)

	_, err := manager.Start(context.Background(), "api")
	if err == nil {
		t.Fatal("start succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
	if len(units.starts) != 0 {
		t.Fatalf("started %d units, want none", len(units.starts))
	}
}

func TestGetUnknownServiceIsNotFound(t *testing.T) {
	manager, _, _ := newTestManager(t)
	if _, err := manager.Get("nope"); err == nil {
		t.Fatal("get succeeded, want ErrNotFound")
	}
}

// Declarations are re-read on every listing, so a file added while the sandbox
// is up appears without restarting anything (ADR 0068 §5) — and appears
// stopped, because a file appearing must not start a program.
func TestListSeesDeclarationsAddedWhileRunning(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)
	if _, err := manager.Start(context.Background(), "api"); err != nil {
		t.Fatalf("start: %v", err)
	}

	writeService(t, root, "20-late.sh", "#!/bin/sh\n#---\n# name: Late\n#---\nexec sleep 1\n", 0o755)
	listed, err := manager.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d services, want 2", len(listed))
	}
	if listed[1].ID != "late" || listed[1].Status != StatusStopped {
		t.Fatalf("second service = %+v, want a stopped `late`", listed[1])
	}
	if len(units.starts) != 1 {
		t.Fatalf("started %d units, want 1: listing must not start anything", len(units.starts))
	}
}

// Boot starts everything runnable, and one broken declaration does not cost the
// others.
func TestEnsureStartedStartsEveryRunnableService(t *testing.T) {
	manager, units, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)
	writeService(t, root, "20-broken.sh", apiScript, 0o644)
	writeService(t, root, "30-web.sh", "#!/bin/sh\n#---\n# name: Web\n#---\nexec serve\n", 0o755)

	if err := manager.EnsureStarted(context.Background(), nil); err != nil {
		t.Fatalf("ensure started: %v", err)
	}
	if len(units.starts) != 2 {
		t.Fatalf("started %d units, want 2", len(units.starts))
	}
	listed, err := manager.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	statuses := map[string]Status{}
	for _, service := range listed {
		statuses[service.ID] = service.Status
	}
	if statuses["api"] != StatusRunning || statuses["web"] != StatusRunning {
		t.Errorf("statuses = %v, want api and web running", statuses)
	}
	if statuses["broken"] != StatusStopped {
		t.Errorf("broken = %q, want stopped", statuses["broken"])
	}
}

// A sandbox that declares nothing does no work at boot and reports nothing.
func TestEnsureStartedWithNoDeclarations(t *testing.T) {
	manager, units, _ := newTestManager(t)
	if err := manager.EnsureStarted(context.Background(), nil); err != nil {
		t.Fatalf("ensure started: %v", err)
	}
	if len(units.starts) != 0 {
		t.Fatalf("started %d units, want none", len(units.starts))
	}
}

func TestLogsForAServiceThatNeverRan(t *testing.T) {
	manager, _, root := newTestManager(t)
	writeService(t, root, "10-api.sh", apiScript, 0o755)

	entries, err := manager.Logs(context.Background(), "api")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want none", entries)
	}
}
