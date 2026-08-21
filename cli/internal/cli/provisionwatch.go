package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// Narrating the wait for a sandbox that is not ready yet (ADR 0060).
//
// An attach blocks until every tier below it reports ready (ADR 0039), and the
// wait can legitimately run for minutes behind an image pull. Nothing is sent
// on the attach connection while that happens — the socket has not been
// upgraded yet, because the control plane is still waiting to proxy it — so the
// only way a client can say what it is waiting for is to read the sandbox and
// report what the pool agent recorded there.

const (
	// provisionPollInterval is how often a client waiting on a sandbox re-reads
	// it. It is display only: nothing waits on this loop, and the attach it
	// runs beside is already correct without it.
	//
	// This is not the poll ADR 0039 removed. That one gated readiness — the
	// client could not attach until it completed — and cost a round trip per
	// second of provisioning for an answer the server already knew. This one
	// starts after the attach is issued, ends when the attach connects, and its
	// worst failure is a status line that updates late.
	provisionPollInterval = 500 * time.Millisecond

	// provisionProgressFresh bounds how old a recorded phase may be and still
	// be reported as what is happening now.
	//
	// The record is the last report and is never cleared, so a sandbox that
	// finished provisioning yesterday still carries the phase it finished in.
	// Work that is genuinely underway reports continuously — the pull reports
	// twice a second, and every other phase is published as it is entered — so
	// a phase that has not been restated in this long is describing the past.
	provisionProgressFresh = 30 * time.Second
)

// watchProvisioning reports what a sandbox is being made to do, until ctx ends.
//
// It only ever calls report with a line that differs from the last one, so a
// caller can render every call and a phase that persists does not flicker.
// Nothing is reported for a sandbox that is already usable: there is no
// provisioning left to describe, and saying so would overwrite whatever the
// caller has on the line.
func (a *App) watchProvisioning(ctx context.Context, projectID, sandboxID string, report func(string)) {
	if report == nil {
		return
	}
	last := ""
	for {
		// The wait is measured before it is described, so an attach onto a
		// discobox that is already up reads nothing at all: it connects inside
		// the first interval and this returns having asked the server nothing.
		// A wait worth narrating lasts seconds, and half of one costs it
		// nothing.
		select {
		case <-ctx.Done():
			return
		case <-time.After(provisionPollInterval):
		}
		if sandbox, ok := a.sandboxSnapshot(ctx, projectID, sandboxID); ok {
			// An unreadable sandbox says nothing rather than saying something
			// wrong. The read can fail for reasons that have no bearing on
			// provisioning — a momentarily unavailable server above all — and
			// the attach is the thing entitled to report those.
			if line := provisionStatus(sandbox); line != "" && line != last {
				last = line
				report(line)
			}
		}
	}
}

// provisionStatus is one line saying what a sandbox that is not ready yet is
// doing, or "" when there is nothing left for this client to wait on.
//
// The order is most specific first: a recorded phase beats an inference from
// state, because the phase is an observation from the process doing the work
// and the state is what the control plane has settled on.
func provisionStatus(sandbox *apimodel.Sandbox) string {
	if sandbox == nil {
		return ""
	}
	runtime := sandbox.Runtime
	switch runtime.State {
	case apiclientgen.SandboxRuntimeStateFailed:
		if message := strings.TrimSpace(runtime.ErrorMessage.Or("")); message != "" {
			return "the discobox failed: " + message
		}
		return "the discobox failed"
	case apiclientgen.SandboxRuntimeStateArchived:
		// Archived is answered rather than waited on (ADR 0022 §5), so an
		// attach against one fails fast and this never has time to matter. It
		// is named anyway so the line is never silently wrong.
		return "the discobox is archived"
	case apiclientgen.SandboxRuntimeStateAwaitingSource:
		return "waiting for its source to be pushed"
	}
	if usable(sandbox) {
		return ""
	}
	if phase := recordedPhase(runtime); phase != "" {
		return phase
	}
	// No phase to report. Either nothing hosts the sandbox yet, or the phase
	// has aged out, and in both cases the coarse answer is the honest one.
	if runtime.State == apiclientgen.SandboxRuntimeStatePending {
		return "waiting for a pool to take it"
	}
	if runtime.RuntimeState.Or("") == apiclientgen.SandboxRuntimeRuntimeStateStarting {
		return "starting the discobox"
	}
	return "preparing the discobox"
}

// usable reports whether the sandbox has nothing left to provision: the
// reconciler has finished the intent it is acting on, that intent settled as
// ready, and the container is up.
//
// A converged, ready, running sandbox is one whose remaining wait is inside
// it — the harness install and the terminal launch, which no channel reports
// upward (ADR 0060) — so the sandbox itself has nothing left to say.
func usable(sandbox *apimodel.Sandbox) bool {
	runtime := sandbox.Runtime
	return runtime.Generation == runtime.ObservedGeneration &&
		runtime.State == apiclientgen.SandboxRuntimeStateReady &&
		runtime.RuntimeState.Or("") == apiclientgen.SandboxRuntimeRuntimeStateRunning
}

// recordedPhase renders the pool agent's last provisioning report, or "" when
// there is none or it is too old to be describing the present.
func recordedPhase(runtime apimodel.SandboxRuntime) string {
	progress, ok := runtime.ProvisionProgress.Get()
	if !ok {
		return ""
	}
	if observed, ok := runtime.ProvisionProgressAt.Get(); ok {
		if time.Since(observed) > provisionProgressFresh {
			return ""
		}
	}
	if pull, ok := progress.Pull.Get(); ok && progress.Phase == apiclientgen.SandboxProvisionPhasePullingImage {
		return pullLine(pull)
	}
	switch progress.Phase {
	case apiclientgen.SandboxProvisionPhasePullingImage:
		return "pulling the discobox image"
	case apiclientgen.SandboxProvisionPhasePreparingVolumes:
		return "preparing the discobox's storage"
	case apiclientgen.SandboxProvisionPhaseMaterializingSource:
		return "unpacking the source into the discobox"
	case apiclientgen.SandboxProvisionPhaseCreatingContainer:
		return "creating the container"
	case apiclientgen.SandboxProvisionPhaseStartingContainer:
		return "starting the container"
	case apiclientgen.SandboxProvisionPhaseWaitingForAgent:
		return "waiting for the discobox to come up"
	}
	// A phase this build does not know is still worth reporting: the server is
	// newer than this CLI, and its own word for what it is doing beats a blank
	// line.
	if phase := strings.TrimSpace(string(progress.Phase)); phase != "" {
		return strings.ReplaceAll(phase, "_", " ")
	}
	return ""
}

// pullLine renders an image pull with both of its ratios.
//
// Neither ratio is progress toward a fixed target — both totals grow while the
// manifest is walked — so this reports them as the pair of counts they are and
// never as a percentage, which would visibly go backwards.
func pullLine(pull apimodel.SandboxPullProgress) string {
	line := "pulling " + imageLabel(pull.Image)
	current, total := pull.Current.Or(0), pull.Total.Or(0)
	switch {
	case total > 0:
		line += fmt.Sprintf(" — %s of %s", humanBytes(current), humanBytes(total))
	case current > 0:
		line += fmt.Sprintf(" — %s", humanBytes(current))
	}
	if layers := pull.Layers.Or(0); layers > 0 {
		line += fmt.Sprintf(", %d/%d layers", pull.LayersComplete.Or(0), layers)
	}
	return line
}

// imageLabel shortens an image reference to the part that identifies it. A
// status line has one line, and the registry host and namespace are the same
// for every image a given deployment pulls.
func imageLabel(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return "the discobox image"
	}
	if slash := strings.LastIndex(image, "/"); slash >= 0 && slash+1 < len(image) {
		return image[slash+1:]
	}
	return image
}

// humanBytes renders a byte count the way a download reports one.
func humanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exponent := float64(bytes)/unit, 0
	for value >= unit && exponent < 3 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[exponent])
}
