// Package execstest is a stand-in for systemd and the exec shim, for tests of
// the layers built on execs.Manager — services, monitors — that need execs to
// start, run and stop without a real process behind them.
package execstest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// Units stands in for systemd, and for the shim systemd would start.
//
// The shim half is what matters: an exec is started by dialing the socket its
// shim listens on, so a fake that only records the launch leaves every start
// timing out. This binds that socket and answers the two requests the exec
// layer makes of it — start and status — from a record it keeps, which is
// exactly the shim's role.
type Units struct {
	Starts []execs.StartRequest
	Stops  []string
	// Exit, when set, is what a launched process does instead of running: it is
	// how a test says "this exec ended on its own".
	Exit *int64

	mu    sync.Mutex
	shims map[string]*fakeShim
}

// NewUnits is a Units whose shims are closed when the test ends.
func NewUnits(t testing.TB) *Units {
	units := &Units{shims: map[string]*fakeShim{}}
	t.Cleanup(units.closeAll)
	return units
}

func (f *Units) Start(_ context.Context, req execs.StartRequest) (execs.StartResult, error) {
	f.mu.Lock()
	f.Starts = append(f.Starts, req)
	f.mu.Unlock()
	// The record the shim publishes starts from the one the exec layer wrote
	// before launching, so identity and metadata are the real ones.
	record := readExecRecord(req.RuntimePath)
	now := time.Now().UTC()
	record.Unit = req.Unit
	record.StartedAt = &now
	record.PID = 4242
	if f.Exit != nil {
		record.Status = execs.StatusExited
		record.ExitCode = f.Exit
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

func (f *Units) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	f.Stops = append(f.Stops, unit)
	defer f.mu.Unlock()
	if shim, ok := f.shims[unit]; ok {
		shim.close()
		delete(f.shims, unit)
	}
	return nil
}

func (f *Units) Status(_ context.Context, unit string) (execs.UnitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, live := f.shims[unit]
	return execs.UnitStatus{Unit: unit, Loaded: true, Active: live}, nil
}

func (f *Units) List(context.Context) ([]execs.UnitStatus, error) { return nil, nil }

// Watch reports no unit changes: these tests drive the manager directly rather
// than through the watcher.
func (f *Units) Watch(ctx context.Context) (<-chan string, error) {
	ch := make(chan string)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (f *Units) closeAll() {
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
