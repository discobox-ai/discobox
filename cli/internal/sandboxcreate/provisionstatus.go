package sandboxcreate

import (
	"fmt"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// Reading what a discobox that is not ready yet is being made to do, off the
// discobox's own record (ADR 0060).
//
// Provisioning is the pool agent's work, not this client's, so nothing here is
// an observation this process could make: the phase and the pull counts are
// recorded on the sandbox as they happen, and a client that wants to narrate a
// wait reads them. Rendering lives in this package rather than in a frontend
// for the same reason the Step constants do — two spellings of one phase is a
// difference users would read as a difference in behavior — and because both
// waits that narrate provisioning are reached from here: the wait for a
// discobox to park ready for its source, and the wait for an attach.

// ProvisionProgressFresh bounds how old a recorded phase may be and still be
// reported as what is happening now.
//
// The record is the last report and is never cleared, so a sandbox that
// finished provisioning yesterday still carries the phase it finished in. Work
// that is genuinely underway reports continuously — the pull reports twice a
// second, and every other phase is published as it is entered — so a phase that
// has not been restated in this long is describing the past.
const ProvisionProgressFresh = 30 * time.Second

// ProvisionStatus is one line saying what a sandbox that is not ready yet is
// doing, or "" when there is nothing left for a client to wait on.
//
// The order is most specific first: a recorded phase beats an inference from
// state, because the phase is an observation from the process doing the work
// and the state is what the control plane has settled on.
func ProvisionStatus(sandbox *apimodel.Sandbox) Step {
	if sandbox == nil {
		return ""
	}
	runtime := sandbox.Runtime
	switch runtime.State {
	case apiclientgen.SandboxRuntimeStateFailed:
		if message := strings.TrimSpace(runtime.ErrorMessage.Or("")); message != "" {
			return "the discobox failed: " + Step(message)
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
	if provisioned(sandbox) {
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

// provisioned reports whether the sandbox has nothing left to provision: the
// reconciler has finished the intent it is acting on, that intent settled as
// ready, and the container is up.
//
// A converged, ready, running sandbox is one whose remaining wait is inside
// it — the harness install and the terminal launch, which no channel reports
// upward (ADR 0060) — so the sandbox itself has nothing left to say.
func provisioned(sandbox *apimodel.Sandbox) bool {
	runtime := sandbox.Runtime
	return runtime.Generation == runtime.ObservedGeneration &&
		runtime.State == apiclientgen.SandboxRuntimeStateReady &&
		runtime.RuntimeState.Or("") == apiclientgen.SandboxRuntimeRuntimeStateRunning
}

// recordedPhase renders the pool agent's last provisioning report, or "" when
// there is none or it is too old to be describing the present.
func recordedPhase(runtime apimodel.SandboxRuntime) Step {
	progress, ok := runtime.ProvisionProgress.Get()
	if !ok {
		return ""
	}
	if observed, ok := runtime.ProvisionProgressAt.Get(); ok {
		if time.Since(observed) > ProvisionProgressFresh {
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
		return Step(strings.ReplaceAll(phase, "_", " "))
	}
	return ""
}

// pullLine renders an image pull with both of its ratios.
//
// Neither ratio is progress toward a fixed target — both totals grow while the
// manifest is walked — so this reports them as the pair of counts they are and
// never as a percentage, which would visibly go backwards.
func pullLine(pull apimodel.SandboxPullProgress) Step {
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
	return Step(line)
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
