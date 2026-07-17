package model_test

import (
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
)

// PhaseChangedAt anchors "how long has this been in this phase", which timeouts
// derive their deadlines from. Re-asserting the current phase must not move it,
// or a resource reconciled often enough would never time out.
func TestSetPhaseStampsOnlyOnChange(t *testing.T) {
	var lifecycle model.ResourceLifecycle

	lifecycle.SetPhase(model.SandboxPhasePending)
	first := lifecycle.PhaseChangedAt
	if first.IsZero() {
		t.Fatal("entering a phase did not stamp PhaseChangedAt")
	}

	// Re-asserting the same phase is what a converging reconcile does.
	time.Sleep(time.Millisecond)
	lifecycle.SetPhase(model.SandboxPhasePending)
	if !lifecycle.PhaseChangedAt.Equal(first) {
		t.Fatalf("re-asserting the same phase restamped the anchor: %v then %v", first, lifecycle.PhaseChangedAt)
	}

	time.Sleep(time.Millisecond)
	lifecycle.SetPhase(model.SandboxPhaseRunning)
	if !lifecycle.PhaseChangedAt.After(first) {
		t.Fatalf("a real phase change did not restamp the anchor: %v then %v", first, lifecycle.PhaseChangedAt)
	}
}

// The lifecycle helpers are the only phase writers, so each must anchor too.
func TestLifecycleTransitionsStampPhaseChangedAt(t *testing.T) {
	t.Run("BeginOperation", func(t *testing.T) {
		lifecycle := model.NewResourceLifecycle(model.SandboxCreateOperation)
		if lifecycle.PhaseChangedAt.IsZero() {
			t.Fatal("BeginOperation did not stamp PhaseChangedAt")
		}
	})

	t.Run("CompleteOperation", func(t *testing.T) {
		lifecycle := model.NewResourceLifecycle(model.SandboxCreateOperation)
		before := lifecycle.PhaseChangedAt
		time.Sleep(time.Millisecond)
		lifecycle.CompleteOperation(model.SandboxPhaseRunning, nil)
		if !lifecycle.PhaseChangedAt.After(before) {
			t.Fatal("CompleteOperation did not stamp PhaseChangedAt")
		}
	})

	t.Run("FailOperation", func(t *testing.T) {
		lifecycle := model.NewResourceLifecycle(model.SandboxCreateOperation)
		before := lifecycle.PhaseChangedAt
		time.Sleep(time.Millisecond)
		lifecycle.FailOperation("boom")
		if lifecycle.Phase != model.SandboxPhaseFailed {
			t.Fatalf("phase = %q, want failed", lifecycle.Phase)
		}
		if !lifecycle.PhaseChangedAt.After(before) {
			t.Fatal("FailOperation did not stamp PhaseChangedAt")
		}
	})
}
