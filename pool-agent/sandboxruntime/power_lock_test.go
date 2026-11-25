package sandboxruntime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
)

// A wait for the power lock ends with its context, without the lock, and a
// context already ended never takes it, even when it is free.
func TestPowerLockContextWaitIsAbandoned(t *testing.T) {
	lock := newPowerLock()
	lock.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := lock.LockContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LockContext on a held lock = %v, want the deadline", err)
	}
	lock.Unlock()

	if err := lock.LockContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LockContext with an ended context = %v, want the deadline", err)
	}
	if !lock.TryLock() {
		t.Fatal("an abandoned wait left the lock held")
	}
	lock.Unlock()

	if err := lock.LockContext(context.Background()); err != nil {
		t.Fatalf("LockContext on a free lock = %v", err)
	}
	lock.Unlock()
}

// An archive or delete queued behind a power operation that never finishes
// gives up when its request ends, and does nothing: the server stops waiting
// for its answer after a deadline (ADR 26-11-24-783 §3), and an archive marker
// written later, for a request nobody is waiting on, would be one more
// teardown run unasked for every retry the server sent.
func TestDockerTeardownsWaitForThePowerLockOnlyWhileTheirRequestLives(t *testing.T) {
	runtime := runningDaemon(t, "sbx_1")
	runtime.root = withTestRoot(t)
	root := runtime.sandboxRoot("sbx_1")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	lock := runtime.sandboxLock("sbx_1")
	lock.Lock() // a start that never finishes
	defer lock.Unlock()

	for name, teardown := range map[string]func(context.Context, string) error{
		"archive": runtime.ArchiveSandbox,
		"delete":  runtime.DeleteSandbox,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := teardown(ctx, "sbx_1")
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s behind a held power lock = %v, want its request's deadline", name, err)
		}
	}
	if sandboxIsArchived(root) {
		t.Fatal("an abandoned archive wrote its marker")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("an abandoned delete touched the tree: %v", err)
	}
}

// The machine runtime's teardowns give up on the lock as the Docker runtime's.
func TestMachineTeardownsWaitForThePowerLockOnlyWhileTheirRequestLives(t *testing.T) {
	f := newMachineFixture(t, nil)
	r := f.runtime
	if _, err := r.CreateSandbox(context.Background(), machineCreate("sbx_7")); err != nil {
		t.Fatal(err)
	}
	lock := r.sandboxLock("sbx_7")
	lock.Lock()
	defer lock.Unlock()

	for name, teardown := range map[string]func(context.Context, string) error{
		"archive": r.ArchiveSandbox,
		"delete":  r.DeleteSandbox,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := teardown(ctx, "sbx_7")
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s behind a held power lock = %v, want its request's deadline", name, err)
		}
	}
	if sandboxIsArchived(r.sandboxRoot("sbx_7")) {
		t.Fatal("an abandoned archive wrote its marker")
	}
	inst, err := f.owner.Instance(context.Background(), "sbx-7")
	if err != nil {
		t.Fatalf("an abandoned delete removed the machine: %v", err)
	}
	if state := r.engine.State(context.Background(), inst); state != engine.Running {
		t.Fatalf("machine is %s after abandoned teardowns, want running", state)
	}
}
