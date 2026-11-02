package model_test

import (
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// The display state combines the two state axes, existence first (ADR 0034
// §5); a sandbox event carries it (ADR 26-11-01-560 §2).
func TestSandboxDisplayState(t *testing.T) {
	tests := []struct {
		name         string
		desired      string
		state        string
		runtimeState string
		errMessage   string
		generation   int64
		observed     int64
		repair       int64
		want         string
	}{
		// Existence answers first, then the runtime axis fills in what the
		// container is doing (ADR 0034 §5).
		{name: "pending create", desired: model.DesiredStatePresent, state: model.SandboxStatePending, generation: 1, want: "starting"},
		{name: "awaiting source", desired: model.DesiredStatePresent, state: model.SandboxStateAwaitingSource, generation: 1, observed: 1, want: "starting"},
		{name: "starting", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateStarting, generation: 1, observed: 1, want: "starting"},
		{name: "running", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateRunning, generation: 2, observed: 2, want: "running"},
		{name: "stopping", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateStopping, generation: 2, observed: 2, want: "stopping"},
		{name: "stopped", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateStopped, generation: 3, observed: 3, want: "stopped"},

		// A stopped sandbox that is still wanted reads "stopped", not
		// "starting". Nothing is bringing it back, and saying otherwise is the
		// exact lie ADR 0017 removes.
		{name: "stopped and still present", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateStopped, generation: 3, observed: 3, want: "stopped"},

		// The two windows a single field could not express. A sandbox observed
		// running before its create finished is still starting: the caller
		// asked for a converged sandbox. One that converged before any agent
		// reported on it is starting too, and briefly — the create publishes
		// what it saw before returning (ADR 0034 §4).
		{name: "observed running before converging", desired: model.DesiredStatePresent, state: model.SandboxStatePending, runtimeState: model.SandboxRuntimeStateRunning, generation: 1, want: "starting"},
		{name: "converged before any observation", desired: model.DesiredStatePresent, state: model.SandboxStateReady, generation: 1, observed: 1, want: "starting"},

		{name: "delete requested", desired: model.DesiredStateDeleted, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateRunning, generation: 4, observed: 3, want: "deleting"},
		{name: "deleted", desired: model.DesiredStateDeleted, state: model.SandboxStateDeleted, generation: 4, observed: 4, want: "deleted"},
		{name: "archive requested", desired: model.DesiredStateArchived, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateRunning, generation: 4, observed: 3, want: "archiving"},
		// An archive leaves the last observation behind; existence outranks it.
		{name: "archived", desired: model.DesiredStateArchived, state: model.SandboxStateArchived, runtimeState: model.SandboxRuntimeStateRunning, generation: 4, observed: 4, want: "archived"},
		{name: "failed", desired: model.DesiredStatePresent, state: model.SandboxStateFailed, generation: 2, observed: 2, want: "error"},
		{name: "error recorded", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateStopped, errMessage: "boom", generation: 2, observed: 2, want: "error"},
		{name: "invalid lifecycle", desired: "", state: "", want: "error"},

		// A repair recorded and not yet landed answers before the failure it
		// is repairing: the row still reads `failed`, and "error" would name
		// the thing being fixed (ADR 26-10-12-711 §8). Once its generation is
		// observed it is whatever it became, and a later generation is not a
		// repair at all.
		{name: "repair of a failed sandbox in flight", desired: model.DesiredStatePresent, state: model.SandboxStateFailed, generation: 4, observed: 3, repair: 4, want: "repairing"},
		{name: "repair of a running sandbox in flight", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateRunning, generation: 4, observed: 3, repair: 4, want: "repairing"},
		{name: "repair landed", desired: model.DesiredStatePresent, state: model.SandboxStateReady, runtimeState: model.SandboxRuntimeStateStopped, generation: 4, observed: 4, repair: 4, want: "stopped"},
		{name: "repair failed", desired: model.DesiredStatePresent, state: model.SandboxStateFailed, errMessage: "boom", generation: 4, observed: 4, repair: 4, want: "error"},
		{name: "intent after a repair", desired: model.DesiredStatePresent, state: model.SandboxStateFailed, generation: 5, observed: 4, repair: 4, want: "error"},
		{name: "archive requested over a repair", desired: model.DesiredStateArchived, state: model.SandboxStateFailed, generation: 4, observed: 3, repair: 4, want: "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle := model.ResourceLifecycle{
				DesiredState:       tt.desired,
				State:              tt.state,
				Generation:         tt.generation,
				ObservedGeneration: tt.observed,
			}
			if tt.errMessage != "" {
				lifecycle.ErrorMessage = &tt.errMessage
			}
			sandbox := &model.Sandbox{ResourceLifecycle: lifecycle, RuntimeState: tt.runtimeState, RepairGeneration: tt.repair}
			if got := sandbox.DisplayState(); got != tt.want {
				t.Fatalf("DisplayState() = %q, want %q", got, tt.want)
			}
		})
	}
}
