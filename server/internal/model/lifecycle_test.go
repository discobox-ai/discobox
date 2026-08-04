package model_test

import (
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
)

// StateChangedAt anchors "how long has this been true", which timeouts derive
// their deadlines from. Re-asserting the current state must not move it, or a
// resource observed often enough would never time out — and under ADR 0017 §10
// a sandbox's state is re-asserted on every complete sync, which is far more
// often than the old reconcile-driven writes.
func TestSetStateStampsOnlyOnChange(t *testing.T) {
	var lifecycle model.ResourceLifecycle

	lifecycle.SetState(model.SandboxStatePending)
	first := lifecycle.StateChangedAt
	if first.IsZero() {
		t.Fatal("entering a state did not stamp StateChangedAt")
	}

	// Re-asserting the same state is what a repeated observation does.
	time.Sleep(time.Millisecond)
	lifecycle.SetState(model.SandboxStatePending)
	if !lifecycle.StateChangedAt.Equal(first) {
		t.Fatalf("re-asserting the same state restamped the anchor: %v then %v", first, lifecycle.StateChangedAt)
	}

	time.Sleep(time.Millisecond)
	lifecycle.SetState(model.SandboxStateRunning)
	if !lifecycle.StateChangedAt.After(first) {
		t.Fatalf("a real state change did not restamp the anchor: %v then %v", first, lifecycle.StateChangedAt)
	}
}

// RecordFailure is the one failure path (ADR 0017 §4). It records the state the
// caller chose rather than forcing a terminal one, which is what lets a pool
// land in `offline` and a sandbox in `failed` through the same call.
func TestRecordFailureKeepsTheCallersState(t *testing.T) {
	var sandbox model.ResourceLifecycle
	sandbox.SetState(model.SandboxStateRunning)
	before := sandbox.StateChangedAt
	time.Sleep(time.Millisecond)

	sandbox.RecordFailure(model.SandboxStateFailed, "boom")
	if sandbox.State != model.SandboxStateFailed {
		t.Fatalf("state = %q, want failed", sandbox.State)
	}
	if sandbox.ErrorMessage == nil || *sandbox.ErrorMessage != "boom" {
		t.Fatalf("error message = %v, want boom", sandbox.ErrorMessage)
	}
	if !sandbox.StateChangedAt.After(before) {
		t.Fatal("RecordFailure did not stamp StateChangedAt")
	}

	var pool model.ResourceLifecycle
	pool.RecordFailure(model.PoolStateOffline, "host unreachable")
	if pool.State != model.PoolStateOffline {
		t.Fatalf("state = %q, want offline: a failure must not force a terminal state", pool.State)
	}
}

// Accepting intent says what should be; it must not disturb what is. A stale
// observation is still the most recent one until something observes otherwise.
func TestRecordIntentLeavesStateAlone(t *testing.T) {
	var lifecycle model.ResourceLifecycle
	lifecycle.SetState(model.SandboxStateRunning)
	lifecycle.RecordFailure(model.SandboxStateRunning, "transient")

	lifecycle.RecordIntent(model.DesiredStateDeleted)

	if lifecycle.DesiredState != model.DesiredStateDeleted {
		t.Fatalf("desired state = %q, want deleted", lifecycle.DesiredState)
	}
	if lifecycle.State != model.SandboxStateRunning {
		t.Fatalf("state = %q, want running: accepting intent is not an observation", lifecycle.State)
	}
	if lifecycle.ErrorMessage != nil {
		t.Fatal("accepted intent did not clear the error from the previous generation")
	}
}

// Converged is the whole of what the orchestrator knows (ADR 0017 §1).
func TestConvergedComparesGenerations(t *testing.T) {
	var lifecycle model.ResourceLifecycle
	if !lifecycle.Converged() {
		t.Fatal("a fresh lifecycle should be converged")
	}
	lifecycle.IncrementGeneration()
	if lifecycle.Converged() {
		t.Fatal("new intent should leave the resource unconverged")
	}
	lifecycle.ObservedGeneration = lifecycle.Generation
	if !lifecycle.Converged() {
		t.Fatal("a reconciler that finished should leave the resource converged")
	}
}
