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
			DesiredState:        model.SandboxDesiredStateRunning,
			Phase:               model.SandboxPhaseRunning,
			LastOperationStatus: model.SandboxOperationStatusSuccess,
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
	out, err := SandboxToAPI(sandbox)
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
		phase      string
		generation int64
		observed   int64
		want       string
	}{
		{name: "pending create", desired: model.SandboxDesiredStateRunning, phase: model.SandboxPhasePending, generation: 1, observed: 0, want: "starting"},
		{name: "stale running phase", desired: model.SandboxDesiredStateRunning, phase: model.SandboxPhaseRunning, generation: 2, observed: 1, want: "starting"},
		{name: "running", desired: model.SandboxDesiredStateRunning, phase: model.SandboxPhaseRunning, generation: 2, observed: 2, want: "running"},
		{name: "stop requested", desired: model.SandboxDesiredStateStopped, phase: model.SandboxPhaseRunning, generation: 3, observed: 2, want: "stopping"},
		{name: "stopped", desired: model.SandboxDesiredStateStopped, phase: model.SandboxPhaseStopped, generation: 3, observed: 3, want: "stopped"},
		{name: "delete requested", desired: model.SandboxDesiredStateDeleted, phase: model.SandboxPhaseDeleting, generation: 4, observed: 3, want: "deleting"},
		{name: "deleted", desired: model.SandboxDesiredStateDeleted, phase: model.SandboxPhaseDeleted, generation: 4, observed: 4, want: "deleted"},
		{name: "failed", desired: model.SandboxDesiredStateRunning, phase: model.SandboxPhaseFailed, generation: 2, observed: 1, want: "error"},
		{name: "invalid lifecycle", desired: "", phase: "", want: "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &model.Sandbox{ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:       tt.desired,
				Phase:              tt.phase,
				Generation:         tt.generation,
				ObservedGeneration: tt.observed,
			}}
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
			DesiredState:        model.SandboxDesiredStateStopped,
			Phase:               model.SandboxPhaseRunning,
			LastOperationStatus: model.SandboxOperationStatusPending,
			Generation:          2,
			ObservedGeneration:  1,
		},
	}
	out, err := SandboxToAPI(sandbox)
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	displayState, ok := out.Runtime.DisplayState.Get()
	if !ok {
		t.Fatal("displayState is missing")
	}
	if got := string(displayState); got != "stopping" {
		t.Fatalf("displayState = %q, want stopping", got)
	}
}
