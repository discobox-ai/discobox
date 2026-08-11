package services

import (
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

func TestSandboxToAPIIncludesRegisteredHarnessConfig(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:        "sb_1",
		ProjectID: "p1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.SandboxStateRunning,
		},
		HarnessConfig: &model.HarnessConfig{
			ID:         "ac_1",
			ProjectID:  "p1",
			Slug:       "codex",
			BuiltIn:    true,
			Configured: true,
			Name:       "Codex",
			RunCommand: []string{"codex"},
		},
	}
	out, err := SandboxToAPI(sandbox, SandboxImageTarget{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	harnessConfig, ok := out.HarnessConfig.Get()
	if !ok {
		t.Fatalf("expected embedded harness config")
	}
	if got := harnessConfig.RunCommand; len(got) != 1 || got[0] != "codex" {
		t.Fatalf("runCommand = %#v, want registered [codex]", got)
	}
}

func TestSandboxDisplayState(t *testing.T) {
	tests := []struct {
		name       string
		desired    string
		state      string
		errMessage string
		generation int64
		observed   int64
		want       string
	}{
		// Observed state carries the answer directly now: the pool agent
		// reports starting/stopping, so display no longer infers them from a
		// desired power state that no longer exists (ADR 0017 §7).
		{name: "pending create", desired: model.DesiredStatePresent, state: model.SandboxStatePending, generation: 1, want: "starting"},
		{name: "awaiting source", desired: model.DesiredStatePresent, state: model.SandboxStateAwaitingSource, generation: 1, observed: 1, want: "starting"},
		{name: "starting", desired: model.DesiredStatePresent, state: model.SandboxStateStarting, generation: 1, observed: 1, want: "starting"},
		{name: "running", desired: model.DesiredStatePresent, state: model.SandboxStateRunning, generation: 2, observed: 2, want: "running"},
		{name: "stopping", desired: model.DesiredStatePresent, state: model.SandboxStateStopping, generation: 2, observed: 2, want: "stopping"},
		{name: "stopped", desired: model.DesiredStatePresent, state: model.SandboxStateStopped, generation: 3, observed: 3, want: "stopped"},

		// A stopped sandbox that is still wanted reads "stopped", not
		// "starting". Nothing is bringing it back, and saying otherwise is the
		// exact lie this ADR removes.
		{name: "stopped and still present", desired: model.DesiredStatePresent, state: model.SandboxStateStopped, generation: 3, observed: 3, want: "stopped"},

		{name: "delete requested", desired: model.DesiredStateDeleted, state: model.SandboxStateRunning, generation: 4, observed: 3, want: "deleting"},
		{name: "deleted", desired: model.DesiredStateDeleted, state: model.SandboxStateDeleted, generation: 4, observed: 4, want: "deleted"},
		{name: "failed", desired: model.DesiredStatePresent, state: model.SandboxStateFailed, generation: 2, observed: 2, want: "error"},
		{name: "error recorded", desired: model.DesiredStatePresent, state: model.SandboxStateStopped, errMessage: "boom", generation: 2, observed: 2, want: "error"},
		{name: "invalid lifecycle", desired: "", state: "", want: "error"},
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
			sandbox := &model.Sandbox{ResourceLifecycle: lifecycle}
			if got := SandboxDisplayState(sandbox); got != tt.want {
				t.Fatalf("SandboxDisplayState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSandboxToAPIIncludesCalculatedDisplayState(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:              "sb_1",
		ProjectID:       "p1",
		CreatedByUserID: "u1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:       model.DesiredStatePresent,
			State:              model.SandboxStateRunning,
			Generation:         2,
			ObservedGeneration: 1,
		},
	}
	out, err := SandboxToAPI(sandbox, SandboxImageTarget{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	displayState, ok := out.Runtime.DisplayState.Get()
	if !ok {
		t.Fatal("displayState is missing")
	}
	// Observed state is the answer, even mid-generation: the sandbox is
	// running, and a pending spec change does not make that untrue.
	if got := string(displayState); got != "running" {
		t.Fatalf("displayState = %q, want running", got)
	}
}
