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
			State:        model.SandboxStateReady,
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
	out, err := SandboxToAPI(sandbox, nil)
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
		name         string
		desired      string
		state        string
		runtimeState string
		errMessage   string
		generation   int64
		observed     int64
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
			sandbox := &model.Sandbox{ResourceLifecycle: lifecycle, RuntimeState: tt.runtimeState}
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
			State:              model.SandboxStateReady,
			Generation:         2,
			ObservedGeneration: 1,
		},
		RuntimeState: model.SandboxRuntimeStateRunning,
	}
	out, err := SandboxToAPI(sandbox, nil)
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	displayState, ok := out.Runtime.DisplayState.Get()
	if !ok {
		t.Fatal("displayState is missing")
	}
	// The observed runtime state is the answer, even mid-generation: the
	// sandbox is running, and a pending spec change does not make that untrue.
	if got := string(displayState); got != "running" {
		t.Fatalf("displayState = %q, want running", got)
	}
	runtimeState, ok := out.Runtime.RuntimeState.Get()
	if !ok {
		t.Fatal("runtimeState is missing")
	}
	if got := string(runtimeState); got != "running" {
		t.Fatalf("runtimeState = %q, want running", got)
	}
}

// A sandbox no agent has reported on serves no runtimeState at all. Empty is a
// member of the model's vocabulary and not of the API enum, so it is omitted
// rather than sent as "" (ADR 0034 §2).
func TestSandboxToAPIOmitsUnobservedRuntimeState(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:              "sb_1",
		ProjectID:       "p1",
		CreatedByUserID: "u1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:       model.DesiredStatePresent,
			State:              model.SandboxStateReady,
			Generation:         1,
			ObservedGeneration: 1,
		},
	}
	out, err := SandboxToAPI(sandbox, nil)
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	if _, ok := out.Runtime.RuntimeState.Get(); ok {
		t.Fatal("runtimeState is present for a sandbox nobody has reported on")
	}
	displayState, _ := out.Runtime.DisplayState.Get()
	if got := string(displayState); got != "starting" {
		t.Fatalf("displayState = %q, want starting", got)
	}
}
