package sandboxruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

func TestQueuedExplicitStartChecksAdmissionAtExecution(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(map[bool]string{false: "docker", true: "machine"}[machine], func(t *testing.T) {
			var epoch atomic.Int64
			epoch.Store(1)
			checked := make(chan struct{}, 1)
			authorize := func(_ context.Context, _ string, want int64) error {
				checked <- struct{}{}
				if epoch.Load() != want {
					return errors.New("superseded")
				}
				return nil
			}
			req := &workerapimodel.PoolSandboxOperationRequest{}
			req.StartEpoch.SetTo(1)
			var start func() error
			var lock interface {
				Lock()
				Unlock()
			}
			if machine {
				r := &MachineSandboxRuntime{sandboxCore: sandboxCore{authorizeStart: authorize}}
				lock = r.sandboxLock("box")
				start = func() error { return r.StartSandbox(context.Background(), "box", req) }
			} else {
				r := &DockerSandboxRuntime{sandboxCore: sandboxCore{authorizeStart: authorize}}
				lock = r.sandboxLock("box")
				start = func() error { return r.StartSandbox(context.Background(), "box", req) }
			}
			lock.Lock()
			done := make(chan error, 1)
			go func() { done <- start() }()

			select {
			case <-checked:
				t.Error("authorized before acquiring power lock")
			case <-time.After(20 * time.Millisecond):
			}
			epoch.Store(2) // A later stop admitted, even if its transport never answered.
			lock.Unlock()
			if err := <-done; !errors.Is(err, ErrStartNotAuthorized) {
				t.Fatalf("late start = %v", err)
			}
		})
	}
}

func TestFencedStartFailsClosedAndUnfencedWakeDoesNotCallBack(t *testing.T) {
	core := sandboxCore{}
	req := &workerapimodel.PoolSandboxOperationRequest{}
	req.StartEpoch.SetTo(7)
	if err := core.authorizeExplicitStart(context.Background(), "box", req); err == nil {
		t.Fatal("missing validator allowed fenced start")
	}
	core.authorizeStart = func(context.Context, string, int64) error { t.Fatal("unfenced wake asked for admission"); return nil }
	if err := core.authorizeExplicitStart(context.Background(), "box", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessfulSuspendCompletesBeforeAnotherServerCanStart(t *testing.T) {
	r, _ := newPausingRuntime(t, "running")
	var epoch atomic.Int64
	epoch.Store(1)
	completing := make(chan struct{})
	allow := make(chan struct{})
	r.completeSuspend = func(context.Context, string, string, int64) error {
		close(completing)
		<-allow
		epoch.Add(1)
		return nil
	}
	r.authorizeStart = func(_ context.Context, _ string, want int64) error {
		if epoch.Load() != want {
			return errors.New("superseded")
		}
		return nil
	}
	suspend := &workerapimodel.PoolSandboxOperationRequest{}
	suspend.SuspendId.SetTo("suspend-1")
	suspend.SuspendEpoch.SetTo(1)
	suspended := make(chan error, 1)
	go func() { suspended <- r.SuspendSandbox(context.Background(), "sbx_1", suspend) }()
	<-completing
	start := &workerapimodel.PoolSandboxOperationRequest{}
	start.StartEpoch.SetTo(1)
	started := make(chan error, 1)
	go func() { started <- r.StartSandbox(context.Background(), "sbx_1", start) }()
	select {
	case err := <-started:
		t.Fatalf("start escaped completion lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allow)
	if err := <-suspended; err != nil {
		t.Fatal(err)
	}
	if err := <-started; !errors.Is(err, ErrStartNotAuthorized) {
		t.Fatalf("stale start: %v", err)
	}
}

func TestLostSuspendCompletionSurvivesAgentRestart(t *testing.T) {
	r, _ := newPausingRuntime(t, "running")
	r.completeSuspend = func(context.Context, string, string, int64) error { return errors.New("response lost") }
	req := &workerapimodel.PoolSandboxOperationRequest{}
	req.SuspendId.SetTo("suspend-lost")
	req.SuspendEpoch.SetTo(1)
	if err := r.SuspendSandbox(context.Background(), "sbx_1", req); err == nil {
		t.Fatal("lost callback succeeded")
	}
	// A fresh core at the same durable root must finish the receipt before
	// checking a queued start, even though the original agent is gone.
	fresh := sandboxCore{root: r.root, projectID: r.projectID, poolID: r.poolID}
	completed := false
	fresh.completeSuspend = func(_ context.Context, _ string, id string, _ int64) error {
		if id != "suspend-lost" {
			t.Error(id)
		}
		completed = true
		return nil
	}
	fresh.authorizeStart = func(context.Context, string, int64) error {
		if !completed {
			t.Error("authorized before suspend completion")
		}
		return errors.New("superseded")
	}
	start := &workerapimodel.PoolSandboxOperationRequest{}
	start.StartEpoch.SetTo(1)
	if err := fresh.authorizeExplicitStart(context.Background(), "sbx_1", start); !errors.Is(err, ErrStartNotAuthorized) {
		t.Fatal(err)
	}
}

func TestPreparedSuspendThatNeverFrozeDoesNotComplete(t *testing.T) {
	r, _ := newPausingRuntime(t, "running")
	path := filepath.Join(r.sandboxRoot("sbx_1"), suspendCompletionFile)
	if err := writeSuspendCompletion(path, suspendCompletion{RequestID: "not-frozen", Epoch: 4}); err != nil {
		t.Fatal(err)
	}
	r.completeSuspend = func(context.Context, string, string, int64) error {
		t.Error("recorded an unperformed freeze")
		return nil
	}
	if err := r.finishSuspend(context.Background(), "sbx_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared receipt remains: %v", err)
	}
}

func TestSharedStopDrainsPreparedFreezeBeforeChangingItsEvidence(t *testing.T) {
	r, daemon := newPausingRuntime(t, "paused")
	path := filepath.Join(r.sandboxRoot("sbx_1"), suspendCompletionFile)
	if err := writeSuspendCompletion(path, suspendCompletion{RequestID: "frozen-before-crash", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	completed := false
	r.completeSuspend = func(context.Context, string, string, int64) error {
		if len(daemon.took()) != 0 {
			t.Error("stop changed suspension evidence before completing receipt")
		}
		completed = true
		return nil
	}
	lock := r.sandboxLock("sbx_1")
	lock.Lock()
	defer lock.Unlock()
	if err := r.stopLocked(context.Background(), "sbx_1"); err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("shared stop discarded successful freeze")
	}
}
