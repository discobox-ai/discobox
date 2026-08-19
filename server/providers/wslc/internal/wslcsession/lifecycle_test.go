//go:build windows

package wslcsession

import (
	"sync"
	"sync/atomic"
	"testing"
)

// A session that is closed must leave nothing holding its VM.
//
// The VM is torn down when its last reference is released, so a guest-process
// reference the session hands out and never takes back keeps the VM running
// for the life of the process -- under a name the next CreateSession then
// collides with, and with no handle left to close it by. A conn left open at
// Close is the ordinary way that happens: net/http retires idle connections on
// its own schedule, and the wait for one is unbounded.
func TestCloseReleasesAProcessWhoseConnWasNeverClosed(t *testing.T) {
	fake := useFakeManager(t)

	session, err := NewSession(Options{DisplayName: "discobox-pool_test"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := session.StartProcess("/bin/sh", []string{"/bin/sh", "-c", "true"}); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fake.releasedProcesses(); got != 1 {
		t.Fatalf("released %d process references, want 1 -- an unreleased one holds the VM", got)
	}
}

// A conn closed the ordinary way releases its reference once, and teardown must
// not release it a second time: over-releasing a COM object frees memory the
// process may still be using.
func TestClosingAConnReleasesExactlyOnce(t *testing.T) {
	fake := useFakeManager(t)

	session, err := NewSession(Options{DisplayName: "discobox-pool_test"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	conn, err := session.StartProcess("/bin/sh", []string{"/bin/sh", "-c", "true"})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close: %v", err)
	}
	if got := fake.releasedProcesses(); got != 1 {
		t.Fatalf("released %d after closing the conn, want 1", got)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fake.releasedProcesses(); got != 1 {
		t.Fatalf("released %d after teardown, want the conn's single release", got)
	}
}

// Closing a conn after the session has gone must not leak the reference: the
// call arrives too late to reach the COM thread, so teardown is what has to
// have released it.
func TestCloseThenConnCloseStillLeavesNothingHeld(t *testing.T) {
	fake := useFakeManager(t)

	session, err := NewSession(Options{DisplayName: "discobox-pool_test"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	conn, err := session.StartProcess("/bin/sh", []string{"/bin/sh", "-c", "true"})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close: %v", err)
	}
	if got := fake.releasedProcesses(); got != 1 {
		t.Fatalf("released %d, want exactly 1", got)
	}
}

// The session and manager themselves are still released, and the VM terminated,
// alongside the process references.
func TestCloseTerminatesAndReleasesTheSession(t *testing.T) {
	fake := useFakeManager(t)

	session, err := NewSession(Options{DisplayName: "discobox-pool_test"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.session.terminated.Load() != 1 || fake.session.released.Load() != 1 || fake.released.Load() != 1 {
		t.Fatalf("terminate=%d sessionRelease=%d managerRelease=%d, want 1 each",
			fake.session.terminated.Load(), fake.session.released.Load(), fake.released.Load())
	}
}

// useFakeManager stands a fake in for the COM session manager for one test.
func useFakeManager(t *testing.T) *fakeManager {
	t.Helper()
	fake := &fakeManager{session: &fakeSession{}}
	previous := activateManager
	activateManager = func() (sessionManager, error) { return fake, nil }
	t.Cleanup(func() { activateManager = previous })
	return fake
}

type fakeManager struct {
	session  *fakeSession
	released atomic.Int32
}

func (m *fakeManager) CreateSession(Options) (wslcSession, error) { return m.session, nil }
func (m *fakeManager) Release()                                   { m.released.Add(1) }

func (m *fakeManager) releasedProcesses() int {
	m.session.mu.Lock()
	defer m.session.mu.Unlock()
	total := 0
	for _, p := range m.session.processes {
		total += int(p.released.Load())
	}
	return total
}

type fakeSession struct {
	mu         sync.Mutex
	processes  []*fakeProcess
	terminated atomic.Int32
	released   atomic.Int32
}

func (s *fakeSession) GetDisplayName() (string, error) { return "discobox-pool_test", nil }
func (s *fakeSession) Terminate() error                { s.terminated.Add(1); return nil }
func (s *fakeSession) Release()                        { s.released.Add(1) }
func (s *fakeSession) CreateVolume(VolumeOptions) error {
	return nil
}

func (s *fakeSession) MountWindowsFolder(string, string, bool) error { return nil }

func (s *fakeSession) CreateRootNamespaceProcess(string, []string, bool) (wslcProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	process := &fakeProcess{}
	s.processes = append(s.processes, process)
	return process, nil
}

type fakeProcess struct {
	released atomic.Int32
}

// The handle is never read from or written to by these tests; only the
// reference's lifetime is under test.
func (p *fakeProcess) GetStdHandle(int32) (socketHandle, error) { return 0, nil }
func (p *fakeProcess) Release()                                 { p.released.Add(1) }
