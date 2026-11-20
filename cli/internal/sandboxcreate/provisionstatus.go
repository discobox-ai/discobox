package sandboxcreate

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/provisionprogress"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
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
	if Repairing(sandbox) {
		// Ahead of the state, which a repair leaves reading `failed` until it
		// lands: the failure it names is the one being repaired, and its
		// message went when the repair was recorded. The phase is the rebuild's
		// own and the more current of the two things to say, so it takes the
		// line once there is one.
		if phase := recordedPhase(runtime); phase != "" {
			return StepRepairing + ": " + phase
		}
		if cause := AutomaticRepairCause(sandbox); cause != "" {
			return StepRepairing + ": " + Step(cause)
		}
		return StepRepairing
	}
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
		if phase := recordedPhase(runtime); phase != "" {
			return phase
		}
		return "waiting for source delivery"
	}
	if provisioned(sandbox) {
		return ""
	}
	// A live agent still preparing has passed the pool's boot/source phases.
	// Its display state is the current report; a recent pool phase is past.
	if runtime.State == apiclientgen.SandboxRuntimeStateReady && runtime.Generation == runtime.ObservedGeneration &&
		runtime.RuntimeState.Or("") == apiclientgen.SandboxRuntimeRuntimeStateRunning && runtime.DisplayState.Or("") == apiclientgen.SandboxRuntimeDisplayStateStarting {
		if preparation := agentPreparation(runtime); preparation != "" {
			return preparation
		}
		return "preparing the discobox"
	}
	if phase := recordedPhase(runtime); phase != "" {
		return phase
	}
	// No phase to report. Either nothing hosts the sandbox yet, or the phase
	// has aged out, and in both cases the coarse answer is the honest one.
	if runtime.State == apiclientgen.SandboxRuntimeStatePending {
		// The truthful answer, and a thin one: the interesting half is what the
		// pool is busy doing instead, which lives on the pool. A caller holding
		// one refines this with the pool provisioning report.
		return StepWaitingForPool
	}
	switch runtime.RuntimeState.Or("") {
	case apiclientgen.SandboxRuntimeRuntimeStateStarting:
		return "starting the discobox"
	case apiclientgen.SandboxRuntimeRuntimeStateSuspended:
		return "resuming the discobox"
	}
	return "preparing the discobox"
}

// RecordedProvision is the provisioning a discobox's agent last reported
// (`runtime.agentStatus.provision`, its provision.json, ADR 26-10-16-423 §6),
// and false when it reported none. It is the one reader of that record:
// every string in it was written by the box, and is escaped where printed.
func RecordedProvision(runtime apimodel.SandboxRuntime) (provisionspec.Status, bool) {
	agent, ok := runtime.AgentStatus.Get()
	if !ok {
		return provisionspec.Status{}, false
	}
	raw, ok := agent["provision"]
	if !ok {
		return provisionspec.Status{}, false
	}
	var status provisionspec.Status
	if err := json.Unmarshal(raw, &status); err != nil {
		return provisionspec.Status{}, false
	}
	return status, true
}

// ProvisionFailure is why a discobox's provisioning failed, from its agent's
// report, or "" when it has not. It is the server's rule for a running
// discobox reading `error`: provisioning not ready, with an error. The text is
// the box's, unescaped; every caller escapes it for where it prints.
func ProvisionFailure(runtime apimodel.SandboxRuntime) string {
	status, ok := RecordedProvision(runtime)
	if !ok || status.Ready || strings.TrimSpace(status.Error) == "" {
		return ""
	}
	return "discobox preparation failed: " + strings.TrimSpace(status.Error)
}

// agentPreparation describes the readiness gate from the agent's own report.
// Pending precedes the feature plan, while the agent loads its pinned bundles.
// Async features do not delay a harness, and must not appear to hold attach up.
func agentPreparation(runtime apimodel.SandboxRuntime) Step {
	status, ok := RecordedProvision(runtime)
	if !ok {
		return ""
	}
	if status.Ready {
		return "connecting to the terminal"
	}
	if failed := ProvisionFailure(runtime); failed != "" {
		return Step(OneLine(failed))
	}
	if status.State == provisionspec.StatePending && len(status.Features) == 0 {
		return "loading provisioning bundles"
	}
	var active []string
	required := 0
	for _, name := range slices.Sorted(maps.Keys(status.Features)) {
		feature := status.Features[name]
		if feature.Async || feature.State == provisionspec.StateOff {
			continue
		}
		required++
		if feature.State == provisionspec.StateReady {
			continue
		}
		if feature.State == provisionspec.StateProvisioning && feature.Step != "" {
			item := OneLine(name)
			if feature.Step != "" {
				item += " / " + OneLine(feature.Step)
			}
			active = append(active, item)
		}
	}
	if required == 0 {
		return ""
	}
	activity := "preparing features"
	if len(active) > 0 {
		activity = "provisioning " + strings.Join(active, ", ")
	}
	return Step(activity)
}

// StepRepairing is what a discobox says while a repair of it has been recorded
// and has not landed (ADR 26-10-12-711 §8).
const StepRepairing Step = "repairing the discobox"

// Repairing reports whether a repair of the sandbox, typed or automatic, is in
// flight.
//
// It is the server's displayState and nothing derived from it. A repair leaves
// the existence state reading `failed` until it lands, so every wait that takes
// `failed` for an answer asks this first: that failure is the one being
// repaired, not the wait's result.
func Repairing(sandbox *apimodel.Sandbox) bool {
	return sandbox != nil && sandbox.Runtime.DisplayState.Or("") == apiclientgen.SandboxRuntimeDisplayStateRepairing
}

// AutomaticRepairCause says why a repair nobody typed is running: what was
// being done to the discobox when it was found failed with no container, and
// the failure the repair answers. It is "" for a typed repair, and for a
// sandbox no repair is in flight on.
//
// The record outlives the repair (it is replaced by the next one), so its
// generation is what says the repair in flight is the one it describes.
func AutomaticRepairCause(sandbox *apimodel.Sandbox) string {
	if !Repairing(sandbox) {
		return ""
	}
	repair, ok := sandbox.Runtime.AutomaticRepair.Get()
	if !ok || repair.Generation != sandbox.Runtime.Generation {
		return ""
	}
	cause := "it had failed with no container"
	if trigger := OneLine(repair.Trigger); trigger != "" {
		cause = trigger + " found it failed with no container"
	}
	if failure := OneLine(repair.Failure.Or("")); failure != "" {
		cause += " (" + failure + ")"
	}
	return cause
}

// OneLine makes a recorded message fit to show on one row of a terminal: its
// white space is folded, and every rune a terminal would act on rather than
// show — a control, ESC among them, or an invisible format character such as a
// bidirectional override — is written as its Go escape.
//
// A status has one line, and a failure is whatever the thing that failed
// printed: a container runtime's error can quote an image name or a path as it
// was given. Shown raw, a sequence in it is an instruction to the reader's
// terminal.
func OneLine(value string) string {
	var b strings.Builder
	for _, r := range strings.Join(strings.Fields(value), " ") {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteRune(r)
		b.WriteString(quoted[1 : len(quoted)-1])
	}
	return b.String()
}

// provisioned reports whether the sandbox has nothing left to provision: the
// reconciler has finished the intent it is acting on, that intent settled as
// ready, and the container is up.
//
// The server projects blocking agent provisioning into displayState. Older
// servers omit that field; retain their lifecycle/runtime reading.
func provisioned(sandbox *apimodel.Sandbox) bool {
	runtime := sandbox.Runtime
	return runtime.DisplayState.Or(apiclientgen.SandboxRuntimeDisplayStateRunning) == apiclientgen.SandboxRuntimeDisplayStateRunning &&
		runtime.Generation == runtime.ObservedGeneration &&
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
		if time.Since(observed) > provisionprogress.ProvisionProgressFresh {
			return ""
		}
	}
	if pull, ok := progress.Pull.Get(); ok && progress.Phase == apiclientgen.SandboxProvisionPhasePullingImage {
		return Step(provisionprogress.ImagePull(pull, "pulling"))
	}
	if clone, ok := progress.Clone.Get(); ok {
		switch progress.Phase {
		case apiclientgen.SandboxProvisionPhaseFetchingSource:
			return cloneLine(clone, "pool fetching ")
		case apiclientgen.SandboxProvisionPhaseMaterializingSource:
			return cloneLine(clone, "sandbox checking out ")
		}
	}
	switch progress.Phase {
	case apiclientgen.SandboxProvisionPhasePullingImage:
		return "pulling the discobox image"
	case apiclientgen.SandboxProvisionPhasePreparingVolumes:
		return "preparing the discobox's storage"
	case apiclientgen.SandboxProvisionPhaseFetchingSource:
		return "pool fetching source"
	case apiclientgen.SandboxProvisionPhaseMaterializingSource:
		// The sandbox clones every source itself, from the remote or from the
		// pool's origin for one delivered from the client's machine.
		return "sandbox checking out source"
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

// cloneLine renders a pool fetch or sandbox checkout: which source — the remote it
// comes from, which is where the time goes on a slow clone — and how far in.
//
// Unlike the pull's, the object counts are a percentage: git knows each
// stage's total before the stage starts, so the ratio only moves forward.
// The bytes are what git has shown, which it starts showing only once the
// clone is slow enough to need a throughput.
func cloneLine(clone apimodel.SandboxCloneProgress, operation string) Step {
	line := operation + provisionprogress.DisplaySafe(clone.Source)
	objects, total, received := clone.Objects.Or(0), clone.ObjectsTotal.Or(0), clone.Bytes.Or(0)
	percent := func() string {
		return fmt.Sprintf("%d%%", objects*100/total)
	}
	switch stage, _ := clone.Stage.Get(); stage {
	case apiclientgen.CloneProgressStageCounting:
		line += " — waiting for the remote to pack it"
	case apiclientgen.CloneProgressStageReceiving:
		var parts []string
		if received > 0 {
			parts = append(parts, provisionprogress.Bytes(received))
		}
		if total > 0 {
			parts = append(parts, fmt.Sprintf("%s of %d objects", percent(), total))
		}
		if len(parts) > 0 {
			line += " — " + strings.Join(parts, ", ")
		}
	case apiclientgen.CloneProgressStageResolving:
		line += " — "
		if received > 0 {
			line += provisionprogress.Bytes(received) + " received, "
		}
		line += "resolving deltas"
		if total > 0 {
			line += " " + percent()
		}
	}
	return Step(line)
}

// StepWaitingForPool is what a sandbox says while no pool has taken it yet.
//
// A named constant because it is the one status a caller acts on rather than
// merely prints: it is the cue to go and ask the pool what it is doing, and
// comparing against a string literal in two packages is how those two drift.
const StepWaitingForPool Step = "waiting for the pool agent to become ready"

// PoolReader is the one call a status needs beyond the sandbox itself.
type PoolReader interface {
	GetPool(context.Context, apiclientgen.GetPoolParams) (apiclientgen.GetPoolRes, error)
}

// Status is what a client waiting on a discobox should say it is waiting for.
//
// ProvisionStatus answers from the sandbox alone, and for the longest stretch
// of a cold start its answer is StepWaitingForPool — true, and nearly useless,
// because those minutes are going into work only the pool records. So when that
// is the answer, the pool is asked what it is doing instead.
//
// Every caller narrating a wait goes through this rather than through
// ProvisionStatus. Refining at the call site is what left the launcher and the
// create path still saying "waiting for a pool to take it" after the wait that
// prompted this had been taught better: one of three call sites had it.
func Status(ctx context.Context, pools PoolReader, projectID string, sandbox *apimodel.Sandbox) Step {
	step := ProvisionStatus(sandbox)
	if step != StepWaitingForPool || pools == nil || sandbox == nil {
		return step
	}
	poolID := sandbox.PoolId.Or("")
	if poolID == "" {
		return step
	}
	res, err := pools.GetPool(ctx, apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: poolID})
	if err != nil {
		// An unreadable pool says nothing rather than something wrong: the read
		// can fail for reasons with no bearing on provisioning, and the wait
		// itself is entitled to report those.
		return step
	}
	pool, ok := res.(*apimodel.Pool)
	if !ok {
		return step
	}
	if poolStep := Step(provisionprogress.PoolStatus(pool)); poolStep != "" {
		return poolStep
	}
	return step
}
