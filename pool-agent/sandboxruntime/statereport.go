package sandboxruntime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// The state channel (ADR 0017 §10). The control plane holds no opinion about
// whether a sandbox is running, so this process is the only thing that knows —
// and it says so on its own schedule rather than in reply to anything.
//
// Two deliveries, and both are load-bearing:
//
//   - Deltas, driven by the Docker event stream, so a transition is visible in
//     roughly the time it takes to happen.
//   - A complete sync on startup and on an interval, listing every sandbox this
//     agent hosts. This is what makes the channel correct rather than merely
//     fast: a dropped delta heals at the next sync, where a delta-only channel
//     drifts permanently the first time a post fails. It is also how a sandbox
//     whose container died while the agent was down is ever noticed.
//
// The interval's arrival is enough to serve as pool liveness if the control
// plane wants one; nothing here needs a separate heartbeat.

const (
	// sandboxStateSyncInterval is how often the complete sync runs. It bounds
	// how stale the control plane's view can be when deltas are lost, so it is
	// also the honest upper bound on "this sandbox is running" being wrong.
	sandboxStateSyncInterval = 60 * time.Second
	// sandboxStateWatchBackoff is the pause before re-subscribing to a Docker
	// event stream that ended or errored.
	sandboxStateWatchBackoff = 5 * time.Second
)

// Observed sandbox states. These are the values a runtime can actually see, and
// they are a subset of the control plane's vocabulary: `pending` and
// `awaiting_source` are facts about a sandbox the control plane is still
// building, which this agent has no view of.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
)

// SandboxStateObservation is one observation about one sandbox.
type SandboxStateObservation struct {
	SandboxID string
	State     string
	Error     string
}

// SandboxStateBatch is one delivery on the channel.
type SandboxStateBatch struct {
	// Complete marks the periodic full sync: States is every sandbox this agent
	// hosts. A delta says nothing about sandboxes it does not mention.
	Complete   bool
	ReportedAt time.Time
	States     []SandboxStateObservation
}

// PublishSandboxState reports a single transition immediately.
//
// The power operations call this on their way into a start or a stop, which is
// the only way `starting` and `stopping` can be observed at all: by the time
// the Docker event arrives the transition is over, and a state nobody can
// report is a state that does not exist. Everything else is derived from the
// event stream.
func (r *DockerSandboxRuntime) PublishSandboxState(ctx context.Context, sandboxID, state string) {
	publish, _ := r.statePublisher.Load().(func(context.Context, SandboxStateBatch) error)
	if publish == nil || sandboxID == "" {
		return
	}
	batch := SandboxStateBatch{
		ReportedAt: time.Now().UTC(),
		States:     []SandboxStateObservation{{SandboxID: sandboxID, State: state}},
	}
	if err := publish(ctx, batch); err != nil {
		slog.DebugContext(ctx, "publish sandbox state", "sandboxID", sandboxID, "state", state, "error", err)
	}
}

// Provisioning phases. These name what this agent is doing to a sandbox that
// has no state transition to announce it, for a client that is waiting to
// attach and wants to know what it is waiting for (ADR 0060).
//
// They are not states and never become any: nothing branches on a phase, and
// the phase a sandbox was last in means nothing once it is up. That is the
// whole reason they ride the progress array rather than the state one.
//
// PhasePullingImage is the only phase with a denominator to report; the rest
// are named work. Reporting them anyway is the point — a client that can say
// "creating the container" is not looking at a hang.
const (
	PhasePullingImage        = "pulling_image"
	PhasePreparingVolumes    = "preparing_volumes"
	PhaseMaterializingSource = "materializing_source"
	PhaseCreatingContainer   = "creating_container"
	PhaseStartingContainer   = "starting_container"
	PhaseWaitingForAgent     = "waiting_for_agent"
)

// SandboxProgressObservation is one report of provisioning progress on one
// sandbox: work that is underway and has no state transition to announce it.
//
// It rides the state channel rather than getting one of its own because it is
// the same kind of fact — something only this process can see about a sandbox
// it hosts — and because a waiting client needs it interleaved with the state
// changes it sits between (ADR 0039).
type SandboxProgressObservation struct {
	SandboxID string
	// Phase is what is being done, and is always set: a report that cannot say
	// what it is about is worth less than no report at all.
	Phase string
	// Pull refines PhasePullingImage with how far in it is. No other phase
	// sets it.
	Pull *PullProgress
}

// PublishSandboxProgress reports provisioning progress immediately, if a
// watcher is running. It is best-effort in the same way PublishSandboxState is:
// progress that nobody is listening for is dropped rather than queued, and the
// complete sync remains the thing that makes the channel correct.
func (r *DockerSandboxRuntime) PublishSandboxProgress(ctx context.Context, observation SandboxProgressObservation) {
	publish, _ := r.progressPublisher.Load().(func(context.Context, SandboxProgressObservation) error)
	if publish == nil || strings.TrimSpace(observation.SandboxID) == "" || strings.TrimSpace(observation.Phase) == "" {
		return
	}
	if err := publish(ctx, observation); err != nil {
		slog.DebugContext(ctx, "publish sandbox progress", "sandboxId", observation.SandboxID, "error", err)
	}
}

// PublishSandboxPullProgress reports the image pull, the one phase that can say
// how far in it is.
func (r *DockerSandboxRuntime) PublishSandboxPullProgress(ctx context.Context, sandboxID string, pull PullProgress) {
	r.PublishSandboxProgress(ctx, SandboxProgressObservation{SandboxID: sandboxID, Phase: PhasePullingImage, Pull: &pull})
}

// PublishSandboxPhase reports a phase that has nothing to measure — which is
// every phase but the pull. It is the call the create path makes at each of its
// own boundaries.
func (r *DockerSandboxRuntime) PublishSandboxPhase(ctx context.Context, sandboxID, phase string) {
	r.PublishSandboxProgress(ctx, SandboxProgressObservation{SandboxID: sandboxID, Phase: phase})
}

// WatchSandboxProgress installs the progress sink for as long as ctx lives. It
// is separate from WatchSandboxStates because progress is reported by whoever
// is doing the work, not derived from the Docker event stream, so there is
// nothing here to watch — only a sink to hold.
func (r *DockerSandboxRuntime) WatchSandboxProgress(ctx context.Context, publish func(context.Context, SandboxProgressObservation) error) {
	if publish == nil {
		return
	}
	r.progressPublisher.Store(publish)
	defer r.progressPublisher.Store((func(context.Context, SandboxProgressObservation) error)(nil))
	<-ctx.Done()
}

// WatchSandboxStates runs the state channel until ctx ends.
func (r *DockerSandboxRuntime) WatchSandboxStates(ctx context.Context, logger *slog.Logger, publish func(context.Context, SandboxStateBatch) error) {
	if logger == nil {
		logger = slog.Default()
	}
	if publish == nil {
		return
	}
	r.statePublisher.Store(publish)
	defer r.statePublisher.Store((func(context.Context, SandboxStateBatch) error)(nil))

	for ctx.Err() == nil {
		if err := r.watchSandboxStateEvents(ctx, logger, publish); err != nil && ctx.Err() == nil {
			logger.Warn("watch sandbox state events", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sandboxStateWatchBackoff):
		}
	}
}

// watchSandboxStateEvents subscribes to this pool's container lifecycle events
// and publishes what it sees, with a complete sync on subscribe and on the
// interval. It returns when the stream ends so the caller can reconnect.
//
// The subscription's Since is captured before it opens and the first sync runs
// after, so a transition landing in that window is replayed by the daemon
// rather than lost between the two.
func (r *DockerSandboxRuntime) watchSandboxStateEvents(ctx context.Context, logger *slog.Logger, publish func(context.Context, SandboxStateBatch) error) error {
	since := time.Now()
	filters := client.Filters{}
	filters = filters.Add("type", string(events.ContainerEventType))
	// Death matters as much as removal, which is the gap this closes: watching
	// only `destroy` meant a container that stopped and stayed put produced no
	// signal at all, and the control plane went on reporting it as running.
	filters = filters.Add("event", string(events.ActionStart))
	filters = filters.Add("event", string(events.ActionDie))
	filters = filters.Add("event", string(events.ActionStop))
	filters = filters.Add("event", string(events.ActionDestroy))
	filters = filters.Add("label", sandboxLabelManaged+"=true")
	filters = filters.Add("label", sandboxLabelProject+"="+r.projectID)
	filters = filters.Add("label", sandboxLabelPool+"="+r.poolID)
	result := r.client.Events(ctx, client.EventsListOptions{
		Since:   fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
		Filters: filters,
	})

	r.publishCompleteSync(ctx, logger, publish)

	sync := time.NewTicker(sandboxStateSyncInterval)
	defer sync.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-result.Err:
			return err
		case message := <-result.Messages:
			observation, ok := observationFromEvent(message)
			if !ok {
				continue
			}
			batch := SandboxStateBatch{ReportedAt: time.Now().UTC(), States: []SandboxStateObservation{observation}}
			if err := publish(ctx, batch); err != nil && ctx.Err() == nil {
				logger.Warn("publish sandbox state delta", "sandboxID", observation.SandboxID, "error", err)
			}
		case <-sync.C:
			r.publishCompleteSync(ctx, logger, publish)
		}
	}
}

func (r *DockerSandboxRuntime) publishCompleteSync(ctx context.Context, logger *slog.Logger, publish func(context.Context, SandboxStateBatch) error) {
	sandboxes, err := r.ListSandboxes(ctx)
	if err != nil {
		// Publishing a sync we could not build would report every sandbox on
		// this pool as gone, so a failed listing must stay silent and wait for
		// the next tick.
		logger.Warn("list sandboxes for state sync", "error", err)
		return
	}
	states := make([]SandboxStateObservation, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if sb.SandboxID == "" {
			continue
		}
		states = append(states, SandboxStateObservation{
			SandboxID: sb.SandboxID,
			State:     stateFromStatus(sb.Status),
		})
	}
	batch := SandboxStateBatch{Complete: true, ReportedAt: time.Now().UTC(), States: states}
	if err := publish(ctx, batch); err != nil && ctx.Err() == nil {
		logger.Warn("publish sandbox state sync", "error", err)
	}
}

func observationFromEvent(message events.Message) (SandboxStateObservation, bool) {
	sandboxID := strings.TrimSpace(message.Actor.Attributes[sandboxLabelSandbox])
	if sandboxID == "" {
		return SandboxStateObservation{}, false
	}
	switch message.Action {
	case events.ActionStart:
		return SandboxStateObservation{SandboxID: sandboxID, State: StateRunning}, true
	case events.ActionDie, events.ActionStop, events.ActionDestroy:
		// A destroyed container is not running, which is all this channel
		// claims. Whether the sandbox should still exist is the control plane's
		// question, and its complete sync answers it by omission.
		return SandboxStateObservation{SandboxID: sandboxID, State: StateStopped}, true
	default:
		return SandboxStateObservation{}, false
	}
}

// stateFromStatus reduces a container status to the only distinction this
// channel can honestly make: running, or not.
//
// `failed` is deliberately absent. A container that has exited looks identical
// whether it was stopped on purpose or died on its own — a normal stop leaves
// exit code 130 or 143, which is indistinguishable from a crash — so reporting
// failure from an exit code turns every deliberate stop into an error. Whether
// something went wrong is a judgement about an operation, which belongs to the
// component that asked for it, not to the one watching containers.
func stateFromStatus(status Status) string {
	if status == StatusRunning {
		return StateRunning
	}
	return StateStopped
}
