package sandboxruntime

import (
	"context"
	"testing"
	"time"
)

// watchProgress installs a sink for the duration of the test and returns what
// it collected. WatchSandboxProgress holds the sink until its context ends, so
// it runs in its own goroutine the way the agent runs it.
func watchProgress(t *testing.T, runtime *DockerSandboxRuntime) (<-chan SandboxProgressObservation, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	observed := make(chan SandboxProgressObservation, 8)
	go runtime.WatchSandboxProgress(ctx, func(_ context.Context, observation SandboxProgressObservation) error {
		observed <- observation
		return nil
	})
	// WatchSandboxProgress stores the sink and then holds it, so publishing
	// before it has been scheduled would be dropped as "nobody is listening"
	// and the test would be waiting on a report that was never going to come.
	for range 1000 {
		if publish, _ := runtime.progressPublisher.Load().(func(context.Context, SandboxProgressObservation) error); publish != nil {
			return observed, cancel
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the progress sink was never installed")
	return observed, cancel
}

// Every phase reports, not only the one with byte counts. A client that can say
// "creating the container" is not looking at a hang, which is the whole point
// of naming the phases the pull is only one of (ADR 0060).
func TestPublishSandboxPhaseNamesTheWork(t *testing.T) {
	runtime := &DockerSandboxRuntime{}
	observed, stop := watchProgress(t, runtime)
	defer stop()

	runtime.PublishSandboxPhase(t.Context(), "sbx_1", PhaseCreatingContainer)

	select {
	case got := <-observed:
		if got.SandboxID != "sbx_1" || got.Phase != PhaseCreatingContainer {
			t.Fatalf("observed %+v, want the creating-container phase for sbx_1", got)
		}
		if got.Pull != nil {
			t.Fatalf("observed a pull on a phase that has nothing to measure: %+v", got.Pull)
		}
	case <-time.After(time.Second):
		t.Fatal("no progress was published")
	}
}

// The pull carries its phase like every other report. Its byte counts are a
// refinement of the phase rather than a report of their own, so a client reads
// the phase first and never has to infer one from which fields are set.
func TestPublishSandboxPullProgressCarriesItsPhase(t *testing.T) {
	runtime := &DockerSandboxRuntime{}
	observed, stop := watchProgress(t, runtime)
	defer stop()

	runtime.PublishSandboxPullProgress(t.Context(), "sbx_1", PullProgress{Image: "example:latest", Layers: 3})

	select {
	case got := <-observed:
		if got.Phase != PhasePullingImage {
			t.Fatalf("phase = %q, want %q", got.Phase, PhasePullingImage)
		}
		if got.Pull == nil || got.Pull.Image != "example:latest" {
			t.Fatalf("observed %+v, want the pull it refines", got.Pull)
		}
	case <-time.After(time.Second):
		t.Fatal("no progress was published")
	}
}

// A report that cannot say what it is about is worth less than no report: it
// would land on the sandbox record and leave a client rendering an empty line
// where its last known phase used to be.
func TestPublishSandboxProgressRefusesAPhaselessReport(t *testing.T) {
	runtime := &DockerSandboxRuntime{}
	observed, stop := watchProgress(t, runtime)
	defer stop()

	runtime.PublishSandboxProgress(t.Context(), SandboxProgressObservation{SandboxID: "sbx_1"})
	runtime.PublishSandboxPhase(t.Context(), "sbx_1", PhaseStartingContainer)

	select {
	case got := <-observed:
		if got.Phase != PhaseStartingContainer {
			t.Fatalf("phase = %q, want the phaseless report to have been dropped", got.Phase)
		}
	case <-time.After(time.Second):
		t.Fatal("no progress was published")
	}
}
