package sandboxcreate

import (
	"context"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// awaitClient answers each read from the script, one entry per read, holding on
// the last. It is only ever asked for the sandbox, because the wait is over
// before anything is pushed.
type awaitClient struct {
	runtimes []apimodel.SandboxRuntime
	reads    int
}

func (c *awaitClient) GetSandbox(context.Context, apiclientgen.GetSandboxParams) (apiclientgen.GetSandboxRes, error) {
	runtime := c.runtimes[min(c.reads, len(c.runtimes)-1)]
	c.reads++
	return &apimodel.Sandbox{ID: "sbx_1", Runtime: runtime}, nil
}

func (c *awaitClient) CompleteSandboxSourcePush(context.Context, *apimodel.CompleteSandboxSourcePushBody, apiclientgen.CompleteSandboxSourcePushParams) (apiclientgen.CompleteSandboxSourcePushRes, error) {
	panic("the wait does not push")
}

// pulling is a sandbox mid-pull, reporting as of now.
func pulling(current, total int64) apimodel.SandboxRuntime {
	runtime := recentProgress(apiclientgen.SandboxProvisionPhasePullingImage)
	runtime.ProvisionProgress = apiclientgen.NewOptSandboxProvisionProgress(apimodel.SandboxProvisionProgress{
		Phase: apiclientgen.SandboxProvisionPhasePullingImage,
		Pull: apiclientgen.NewOptSandboxPullProgress(apimodel.SandboxPullProgress{
			Image:   "ghcr.io/discobox-ai/discobox-vm:v2",
			Current: apiclientgen.NewOptInt64(current),
			Total:   apiclientgen.NewOptInt64(total),
		}),
	})
	return runtime
}

// The wait for a discobox to park says what it is waiting for. Everything that
// happens before a sandbox can accept a push — the pull above all — is recorded
// on the sandbox this loop is already reading, and a pull is the reason this
// step is the long one (ADR 0060).
func TestAwaitSourceRequestedNarratesTheProvisioningItWaitsOn(t *testing.T) {
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{
		pulling(100*1024*1024, 400*1024*1024),
		// The same counts again: an unchanged line is not reported twice, so a
		// caller that renders every report does not flicker.
		pulling(100*1024*1024, 400*1024*1024),
		pulling(300*1024*1024, 400*1024*1024),
		recentProgress(apiclientgen.SandboxProvisionPhaseCreatingContainer),
		{State: apiclientgen.SandboxRuntimeStateAwaitingSource},
	}}

	var reported []Step
	report := Report(func(step Step) { reported = append(reported, step) })
	if err := awaitSourceRequested(t.Context(), client, "project-1", "sbx_1", report); err != nil {
		t.Fatalf("await: %v", err)
	}

	want := []Step{
		"pulling discobox-vm:v2 — 100.0 MiB of 400.0 MiB",
		"pulling discobox-vm:v2 — 300.0 MiB of 400.0 MiB",
		"creating the container",
	}
	if len(reported) != len(want) {
		t.Fatalf("reported %q, want %q", reported, want)
	}
	for i, line := range want {
		if reported[i] != line {
			t.Fatalf("reported[%d] = %q, want %q", i, reported[i], line)
		}
	}
}

// A discobox that is already parked reports nothing: the step the caller put on
// the line before this was called is still what is happening, and replacing it
// with a phase read off the record would say the same thing in other words.
func TestAwaitSourceRequestedSaysNothingAboutASandboxAlreadyParked(t *testing.T) {
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{
		{State: apiclientgen.SandboxRuntimeStateAwaitingSource},
	}}

	report := Report(func(step Step) { t.Errorf("reported %q for a discobox that was already waiting", step) })
	if err := awaitSourceRequested(t.Context(), client, "project-1", "sbx_1", report); err != nil {
		t.Fatalf("await: %v", err)
	}
	if client.reads != 1 {
		t.Fatalf("read the discobox %d times, want one", client.reads)
	}
}

// A nil Report is what a non-interactive caller passes, and the wait still
// waits. Nothing here may depend on someone listening.
func TestAwaitSourceRequestedToleratesNoReport(t *testing.T) {
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{
		recentProgress(apiclientgen.SandboxProvisionPhaseCreatingContainer),
		{State: apiclientgen.SandboxRuntimeStateAwaitingSource},
	}}

	start := time.Now()
	if err := awaitSourceRequested(t.Context(), client, "project-1", "sbx_1", nil); err != nil {
		t.Fatalf("await: %v", err)
	}
	if elapsed := time.Since(start); elapsed < awaitSourcePollInterval {
		t.Fatalf("returned after %s, want a wait that paced itself", elapsed)
	}
}
