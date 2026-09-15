package sandboxruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEmptyDirRemovesContentsAndKeepsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(filepath.Join(root, "1000", "go", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "1000", "go", "pkg", "mod"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "top"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := emptyDir(root); err != nil {
		t.Fatalf("emptyDir: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("root must survive: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("root still holds %d entries", len(entries))
	}
}

func TestEmptyDirMissingRootIsEmpty(t *testing.T) {
	if err := emptyDir(filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Fatalf("a pool that never cached anything has nothing to clear: %v", err)
	}
}

func TestStartGateCloseWaitsForOperationsInside(t *testing.T) {
	var gate startGate
	leave, err := gate.enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan func())
	go func() {
		reopen, err := gate.close(context.Background())
		if err != nil {
			t.Error(err)
		}
		closed <- reopen
	}()
	select {
	case <-closed:
		t.Fatal("close returned while an operation was still inside")
	case <-time.After(50 * time.Millisecond):
	}
	leave()
	select {
	case reopen := <-closed:
		reopen()
	case <-time.After(time.Second):
		t.Fatal("close did not return once the gate drained")
	}
}

func TestStartGateHoldsArrivalsUntilReopened(t *testing.T) {
	var gate startGate
	reopen, err := gate.close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan func())
	go func() {
		leave, err := gate.enter(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		entered <- leave
	}()
	select {
	case <-entered:
		t.Fatal("an operation entered a closed gate")
	case <-time.After(50 * time.Millisecond):
	}
	reopen()
	select {
	case leave := <-entered:
		leave()
	case <-time.After(time.Second):
		t.Fatal("the waiting operation was not admitted once the gate reopened")
	}
	// The gate is reusable: a second clear drains and reopens the same way.
	reopen, err = gate.close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reopen()
}

func TestStartGateWaitHonorsContext(t *testing.T) {
	var gate startGate
	reopen, err := gate.close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reopen()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.enter(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("enter on a closed gate with a canceled context = %v, want context.Canceled", err)
	}
}

func TestStartGateCloseGivesUpWhenItsContextEnds(t *testing.T) {
	var gate startGate
	leave, err := gate.enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := gate.close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close past its deadline = %v, want context.DeadlineExceeded", err)
	}
	// The gate is open again: a second operation gets in while the first is
	// still inside.
	enterCtx, cancelEnter := context.WithTimeout(context.Background(), time.Second)
	defer cancelEnter()
	second, err := gate.enter(enterCtx)
	if err != nil {
		t.Fatalf("enter after an abandoned close: %v", err)
	}
	second()
}

// A clear that everyone stopped waiting for before the gate drained has done
// nothing, so it must not go on holding the pool's starts off.
func TestClearCacheAbandonedWhileDrainingReopensTheGate(t *testing.T) {
	r := &DockerSandboxRuntime{}
	leave, err := r.starts.enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer leave()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.ClearCache(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ClearCache past its deadline = %v, want context.DeadlineExceeded", err)
	}

	enterCtx, cancelEnter := context.WithTimeout(context.Background(), time.Second)
	defer cancelEnter()
	second, err := r.starts.enter(enterCtx)
	if err != nil {
		t.Fatalf("a start after the abandoned clear: %v", err)
	}
	second()

	// The abandoned run winds down and clears itself away, so a later request
	// starts a clear of its own rather than joining the one that gave up.
	r.clearMu.Lock()
	run := r.clearRun
	r.clearMu.Unlock()
	if run != nil {
		select {
		case <-run.done:
		case <-time.After(time.Second):
			t.Fatal("the abandoned clear never finished")
		}
	}
	r.clearMu.Lock()
	defer r.clearMu.Unlock()
	if r.clearRun != nil {
		t.Fatal("the abandoned clear is still the run a new request would join")
	}
}

func TestMemoryRuntimeClearCacheStopsRunningSandboxes(t *testing.T) {
	r := NewMemorySandboxRuntime()
	now := time.Now()
	r.sandboxes["sbx_b"] = &Sandbox{SandboxID: "sbx_b", Status: StatusRunning, StartedAt: &now}
	r.sandboxes["sbx_a"] = &Sandbox{SandboxID: "sbx_a", Status: StatusRunning, StartedAt: &now}
	r.sandboxes["sbx_c"] = &Sandbox{SandboxID: "sbx_c", Status: StatusStopped}
	stopped, err := r.ClearCache(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 2 || stopped[0] != "sbx_a" || stopped[1] != "sbx_b" {
		t.Fatalf("stopped = %v, want [sbx_a sbx_b]", stopped)
	}
	for id, sb := range r.sandboxes {
		if sb.Status != StatusStopped {
			t.Fatalf("%s is %s after a clear", id, sb.Status)
		}
	}
}
