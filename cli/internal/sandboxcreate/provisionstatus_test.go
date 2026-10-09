package sandboxcreate

import (
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// provisioning builds a sandbox whose runtime says what the test is about.
func provisioning(runtime apimodel.SandboxRuntime) *apimodel.Sandbox {
	return &apimodel.Sandbox{Runtime: runtime}
}

// recentProgress is a phase reported just now, which is what the pool agent
// does for as long as the work is underway.
func recentProgress(phase apiclientgen.SandboxProvisionPhase) apimodel.SandboxRuntime {
	return apimodel.SandboxRuntime{
		State: apiclientgen.SandboxRuntimeStatePending,
		ProvisionProgress: apiclientgen.NewOptSandboxProvisionProgress(
			apimodel.SandboxProvisionProgress{Phase: phase},
		),
		ProvisionProgressAt: apiclientgen.NewOptDateTime(time.Now()),
	}
}

// A waiting client says what the sandbox is doing, and the recorded phase beats
// every inference that could be drawn from state.
func TestProvisionStatusPrefersTheRecordedPhase(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase apiclientgen.SandboxProvisionPhase
		want  Step
	}{
		{"pull", apiclientgen.SandboxProvisionPhasePullingImage, "pulling the discobox image"},
		{"volumes", apiclientgen.SandboxProvisionPhasePreparingVolumes, "preparing the discobox's storage"},
		{"source", apiclientgen.SandboxProvisionPhaseMaterializingSource, "cloning the source into the discobox"},
		{"create", apiclientgen.SandboxProvisionPhaseCreatingContainer, "creating the container"},
		{"start", apiclientgen.SandboxProvisionPhaseStartingContainer, "starting the container"},
		{"agent", apiclientgen.SandboxProvisionPhaseWaitingForAgent, "waiting for the discobox to come up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProvisionStatus(provisioning(recentProgress(tc.phase))); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// A phase this build has never heard of is still the server's own word for what
// it is doing, and beats saying nothing. A CLI is routinely older than the
// control plane it talks to.
func TestProvisionStatusReportsAnUnknownPhase(t *testing.T) {
	got := ProvisionStatus(provisioning(recentProgress("warming_the_cache")))
	if got != "warming the cache" {
		t.Fatalf("status = %q, want the unknown phase spelled out", got)
	}
}

// The record is the last report and is never cleared, so a phase that stopped
// being restated is describing the past. Reporting it would leave a status line
// insisting on work that finished long ago.
func TestProvisionStatusIgnoresAStalePhase(t *testing.T) {
	runtime := recentProgress(apiclientgen.SandboxProvisionPhasePullingImage)
	runtime.ProvisionProgressAt = apiclientgen.NewOptDateTime(time.Now().Add(-2 * ProvisionProgressFresh))

	got := ProvisionStatus(provisioning(runtime))
	if got != "waiting for the pool agent to become ready" {
		t.Fatalf("status = %q, want the state-based answer once the phase has aged out", got)
	}
}

// Nothing is reported for a sandbox with nothing left to provision. The wait
// that remains is inside it — the harness install and the terminal launch — and
// no channel reports that upward (ADR 0060), so the caller keeps its own line
// rather than being handed a wrong one.
func TestProvisionStatusSaysNothingAboutAUsableSandbox(t *testing.T) {
	runtime := recentProgress(apiclientgen.SandboxProvisionPhaseWaitingForAgent)
	runtime.State = apiclientgen.SandboxRuntimeStateReady
	runtime.RuntimeState = apiclientgen.NewOptSandboxRuntimeRuntimeState(apiclientgen.SandboxRuntimeRuntimeStateRunning)
	runtime.Generation, runtime.ObservedGeneration = 3, 3

	if got := ProvisionStatus(provisioning(runtime)); got != "" {
		t.Fatalf("status = %q, want nothing for a sandbox that is already up", got)
	}
}

// An unconverged sandbox is not usable however ready its state reads: the
// reconciler is still acting on newer intent, which is exactly the window in
// which a pushed source is being materialized.
func TestProvisionStatusWaitsOnAnUnconvergedSandbox(t *testing.T) {
	runtime := apimodel.SandboxRuntime{
		State:              apiclientgen.SandboxRuntimeStateReady,
		RuntimeState:       apiclientgen.NewOptSandboxRuntimeRuntimeState(apiclientgen.SandboxRuntimeRuntimeStateRunning),
		Generation:         4,
		ObservedGeneration: 3,
	}
	if got := ProvisionStatus(provisioning(runtime)); got != "preparing the discobox" {
		t.Fatalf("status = %q, want the sandbox still treated as provisioning", got)
	}
}

// A failure is an answer, not a phase, and it carries the reason the reconciler
// recorded rather than a generic line.
func TestProvisionStatusReportsAFailureWithItsReason(t *testing.T) {
	runtime := apimodel.SandboxRuntime{
		State:        apiclientgen.SandboxRuntimeStateFailed,
		ErrorMessage: apiclientgen.NewOptString("pull image: not found"),
	}
	if got := ProvisionStatus(provisioning(runtime)); got != "the discobox failed: pull image: not found" {
		t.Fatalf("status = %q, want the recorded reason", got)
	}
}

// A sandbox parked waiting for its push says so. Nothing server-side can move
// it along — the client owes it the source (ADR 0039) — so this is the one
// phase a user can act on.
func TestProvisionStatusNamesTheSourcePush(t *testing.T) {
	runtime := apimodel.SandboxRuntime{State: apiclientgen.SandboxRuntimeStateAwaitingSource}
	if got := ProvisionStatus(provisioning(runtime)); got != "waiting for its source to be pushed" {
		t.Fatalf("status = %q, want the push named", got)
	}
}

// A sandbox cloning its source says which source and how far in, for as long
// as the pool restates it: a full clone of a large remote is often most of
// the wait, and "waiting for the pool agent" was what it said instead (#138).
func TestProvisionStatusNamesTheCloneAndHowFarItHasGot(t *testing.T) {
	const source = "github.com/discobox-ai/discobox"
	for _, tc := range []struct {
		name  string
		clone apimodel.SandboxCloneProgress
		want  Step
	}{
		{"not started", apimodel.SandboxCloneProgress{Source: source}, "cloning " + source},
		{"control characters", apimodel.SandboxCloneProgress{Source: "example.com/a\x1b[2Jb\u202e"}, `cloning example.com/a\x1b[2Jb\u202e`},
		{"counting", apimodel.SandboxCloneProgress{
			Source: source,
			Stage:  apiclientgen.NewOptCloneProgressStage(apiclientgen.CloneProgressStageCounting),
		}, "cloning " + source + " — waiting for the remote to pack it"},
		{"receiving", apimodel.SandboxCloneProgress{
			Source:       source,
			Stage:        apiclientgen.NewOptCloneProgressStage(apiclientgen.CloneProgressStageReceiving),
			Objects:      apiclientgen.NewOptInt64(17739),
			ObjectsTotal: apiclientgen.NewOptInt64(35477),
			Bytes:        apiclientgen.NewOptInt64(15623782),
		}, "cloning " + source + " — 14.9 MiB, 50% of 35477 objects"},
		{"receiving before a throughput", apimodel.SandboxCloneProgress{
			Source:       source,
			Stage:        apiclientgen.NewOptCloneProgressStage(apiclientgen.CloneProgressStageReceiving),
			Objects:      apiclientgen.NewOptInt64(3),
			ObjectsTotal: apiclientgen.NewOptInt64(4),
		}, "cloning " + source + " — 75% of 4 objects"},
		{"resolving", apimodel.SandboxCloneProgress{
			Source:       source,
			Stage:        apiclientgen.NewOptCloneProgressStage(apiclientgen.CloneProgressStageResolving),
			Objects:      apiclientgen.NewOptInt64(2576),
			ObjectsTotal: apiclientgen.NewOptInt64(25762),
			Bytes:        apiclientgen.NewOptInt64(28940697),
		}, "cloning " + source + " — 27.6 MiB received, resolving deltas 9%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := recentProgress(apiclientgen.SandboxProvisionPhaseMaterializingSource)
			progress := runtime.ProvisionProgress.Value
			progress.Clone = apiclientgen.NewOptSandboxCloneProgress(tc.clone)
			runtime.ProvisionProgress = apiclientgen.NewOptSandboxProvisionProgress(progress)
			if got := ProvisionStatus(provisioning(runtime)); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// The pull's numbers are reported as the pair of counts they are. Both totals
// grow while the manifest is walked, so a percentage would visibly go
// backwards.
func TestPullLineReportsBothRatiosAndNoPercentage(t *testing.T) {
	line := string(pullLine(apimodel.SandboxPullProgress{
		Image:          "ghcr.io/discobox-ai/discobox-harness-codex:latest",
		Current:        apiclientgen.NewOptInt64(150 * 1024 * 1024),
		Total:          apiclientgen.NewOptInt64(400 * 1024 * 1024),
		Layers:         apiclientgen.NewOptInt(9),
		LayersComplete: apiclientgen.NewOptInt(4),
	}))
	want := "pulling discobox-harness-codex:latest — 150.0 MiB of 400.0 MiB, 4/9 layers"
	if line != want {
		t.Fatalf("pull line = %q, want %q", line, want)
	}
}

// A pull that has not been told any layer sizes yet reports what it has rather
// than a ratio against zero.
func TestPullLineHandlesAnUnsizedPull(t *testing.T) {
	line := string(pullLine(apimodel.SandboxPullProgress{
		Image:   "discobox-shell:latest",
		Current: apiclientgen.NewOptInt64(2048),
	}))
	if line != "pulling discobox-shell:latest — 2.0 KiB" {
		t.Fatalf("pull line = %q", line)
	}
}
