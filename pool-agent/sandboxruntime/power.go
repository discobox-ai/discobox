package sandboxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// Power operations are instructions (ADR 0017 §9). They act and return whether
// the instruction was accepted; the resulting state is published by the state
// reporter, which is watching the same containers these calls manipulate.
//
// Every power operation for one sandbox serializes on that sandbox's lock. That
// is what makes on-demand start safe: ten concurrent requests to a stopped
// sandbox produce one start, and an explicit stop cannot interleave with an
// auto-start half way through. The lock is per sandbox, never global, so
// unrelated sandboxes do not queue behind each other.
//
// Every operation that can start a container also enters the pool's start gate
// before taking that lock, which is how a cache clear keeps the pool's
// sandboxes down while it works (see clearcache.go). startLocked must only be
// reached from inside the gate.

func (r *DockerSandboxRuntime) StartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error {
	leave, err := r.starts.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.authorizeExplicitStart(ctx, sandboxID, req); err != nil {
		return err
	}
	return r.startLocked(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) StopSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	return r.stopLocked(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) RestartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	leave, err := r.starts.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return err
	}
	return r.startLocked(ctx, sandboxID)
}

// SuspendSandbox freezes a running sandbox whole with `docker pause`, under its
// power lock (ADR 26-10-16-420 §3). A person asked to keep what it holds, so a
// pause that fails is returned rather than turned into a stop.
//
// A sandbox still booting is refused like a stopped one: its boot waits for an
// agent the freeze would keep from ever answering.
func (r *DockerSandboxRuntime) SuspendSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	switch {
	case sb.Status == StatusSuspended:
		return r.suspendWithCompletion(ctx, sandboxID, req, func() error { return nil })
	case sb.Status != StatusRunning:
		return ErrNotRunning
	case r.SandboxBooting(sandboxID):
		return fmt.Errorf("%w: it is still starting", ErrNotRunning)
	}
	return r.suspendWithCompletion(ctx, sandboxID, req, func() error { return r.pauseLocked(ctx, sb) })
}

// pauseLocked freezes the sandbox's container. The pause event reports it
// suspended. The caller holds the sandbox's power lock.
func (r *DockerSandboxRuntime) pauseLocked(ctx context.Context, sb *Sandbox) error {
	if _, err := r.client.ContainerPause(ctx, sb.ID, client.ContainerPauseOptions{}); err != nil {
		return fmt.Errorf("suspend sandbox %s: %w", sb.SandboxID, err)
	}
	return nil
}

// resumeLocked thaws a suspended sandbox (ADR 26-10-16-420 §4): the unpause
// event reports it running, and nothing in it boots. What changed while it was
// frozen is then delivered — its runtime-config document, and to its agent the
// news that it resumed, which sets aside the wakes now due so the one that
// brought it back is not returned by its next power-down. Neither is the
// resume's to fail: the status poll converges the document, and an agent from
// before the resumed route has no wakes to set aside. The caller holds the
// sandbox's power lock.
//
// Both run detached from the caller, as a boot's wait does (finishBoot), each
// bounded by its own timeout: a use that thawed the sandbox and then went away
// must not leave its due wake in place, where the next sleep's confirmation
// would hand it back and undo that sleep at once. They stay under the lock, so
// no power-down can confirm before the wakes are set aside. Neither is retried:
// the status poll converges the document, and a resumed call that fails anyway
// costs at most the next sleep, undone by the wake it hands back.
func (r *DockerSandboxRuntime) resumeLocked(ctx context.Context, sb *Sandbox) error {
	if _, err := r.client.ContainerUnpause(ctx, sb.ID, client.ContainerUnpauseOptions{}); err != nil {
		return fmt.Errorf("resume sandbox %s: %w", sb.SandboxID, err)
	}
	slog.InfoContext(ctx, "resumed suspended sandbox", "sandboxId", sb.SandboxID)
	r.afterThaw(ctx, sb.SandboxID)
	return nil
}

// afterThaw delivers what changed while a sandbox was frozen, a container or
// a machine (ADR 26-10-16-420 §4, ADR 26-11-21-395 §5): its runtime-config
// document, and to its agent the news that it resumed, which sets aside the
// wakes now due and counts the thaw as a start for its idle clock. Both run
// detached from the caller; see resumeLocked for why, and why neither is the
// thaw's to fail. The caller holds the sandbox's power lock.
func (r *sandboxCore) afterThaw(ctx context.Context, sandboxID string) {
	ctx = context.WithoutCancel(ctx)
	if err := logRuntimeConfigFailure(ctx, sandboxID, r.deliverRuntimeConfig(ctx, sandboxID, nil)); err != nil {
		slog.WarnContext(ctx, "deliver runtime config to a resumed sandbox", "sandboxId", sandboxID, "error", err)
	}
	if err := r.tellResumed(ctx, sandboxID); err != nil {
		slog.WarnContext(ctx, "tell a resumed sandbox it resumed", "sandboxId", sandboxID, "error", err)
	}
}

// PowerDownSandbox carries out a power-down the sandbox asked for, on what the
// sandbox confirms under the power lock rather than on what its last status
// said: that status is a poll and a push old, and a client may have attached
// since (ADR 26-10-16-337 §5). A confirmed idle stops the sandbox. A confirmed
// sleep freezes it (ADR 26-10-16-420 §2), and stops it if the freeze fails:
// the agent asked to go down, and a stop is the power-down every runtime has.
//
// A sleep whose stop then fails leaves the sandbox up with its request
// consumed; its idle stop puts it down in time.
func (r *DockerSandboxRuntime) PowerDownSandbox(ctx context.Context, sandboxID string, reason PowerDownReason) (Wake, error) {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return Wake{}, err
	}
	defer unlock()
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return Wake{}, err
	}
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return Wake{}, err
	}
	if sb.Status != StatusRunning {
		return Wake{}, ErrPowerDownRefused
	}
	wake, err := r.confirmPowerDown(ctx, sandboxID, reason)
	if err != nil {
		return Wake{}, err
	}
	slog.Default().Info("powering sandbox down on its own request", "sandboxId", sandboxID, "reason", reason, "wakeAt", wake.At)
	if reason == PowerDownSleep {
		err := r.pauseLocked(ctx, sb)
		if err == nil {
			return wake, nil
		}
		slog.Default().Warn("could not freeze a sleeping sandbox; stopping it", "sandboxId", sandboxID, "error", err)
	}
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return Wake{}, err
	}
	return wake, nil
}

// confirmPowerDown asks the sandbox agent to confirm reason, with a token the
// pool signs for that alone.
func (r *sandboxCore) confirmPowerDown(ctx context.Context, sandboxID string, reason PowerDownReason) (Wake, error) {
	status, data, err := r.callAutostop(ctx, sandboxID, "confirm", map[string]string{"reason": string(reason)})
	if err != nil {
		return Wake{}, fmt.Errorf("confirm power-down with sandbox %s: %w", sandboxID, err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusConflict:
		return Wake{}, ErrPowerDownRefused
	default:
		return Wake{}, fmt.Errorf("confirm power-down with sandbox %s: status %d: %s", sandboxID, status, strings.TrimSpace(string(data)))
	}
	var confirmed struct {
		WakeAt *time.Time `json:"wakeAt"`
		Wake   string     `json:"wake"`
	}
	if err := json.Unmarshal(data, &confirmed); err != nil {
		return Wake{}, fmt.Errorf("decode sandbox %s's confirmation: %w", sandboxID, err)
	}
	if confirmed.WakeAt == nil {
		return Wake{}, nil
	}
	return Wake{At: confirmed.WakeAt.UTC(), Name: confirmed.Wake}, nil
}

// tellResumed tells the sandbox agent its sandbox was thawed. An agent from
// before the route answers 404, and has nothing to do on a resume.
func (r *sandboxCore) tellResumed(ctx context.Context, sandboxID string) error {
	status, data, err := r.callAutostop(ctx, sandboxID, "resumed", struct{}{})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		return fmt.Errorf("status %d: %s", status, strings.TrimSpace(string(data)))
	}
	return nil
}

// callAutostop posts body to one of the sandbox agent's pool-only autostop
// routes, with a token the pool signs for that scope alone, and returns the
// answer's status and body.
func (r *sandboxCore) callAutostop(ctx context.Context, sandboxID, route string, body any) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, runtimeConfigCallTimeout)
	defer cancel()
	token, err := r.poolToken(sandboxID, sandboxconfig.AutostopConfirmScope)
	if err != nil {
		return 0, nil, err
	}
	dial, err := r.boxes.SandboxDialer(ctx, sandboxID, SandboxAgentPort)
	if err != nil {
		return 0, nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	target := HTTPURL(SandboxAgentPort, fmt.Sprintf("/api/projects/%s/sandboxes/%s/autostop/%s", r.projectID, sandboxID, route))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The connection is no credential: the sandbox agent decides on this token
	// alone, whatever carried it (ADR 0126 §5).
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: dial.Transport()}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// EnsureSandboxRunning starts a sandbox that is not running, thaws one that is
// suspended, and is a no-op for one that already runs. It is what the sandbox-directed routes call so that a
// stopped sandbox comes up on first use (ADR 0017 §12), and what the control
// plane calls before it forwards an action into one (ADR 26-10-12-711 §2).
//
// Nothing about it is a special path: it takes the same lock and calls the same
// start as an explicit instruction, so an implicitly started sandbox reports
// starting and then running exactly like any other.
func (r *DockerSandboxRuntime) EnsureSandboxRunning(ctx context.Context, sandboxID string) error {
	// A sandbox whose container is not there is answered at once, and says
	// which kind of absence it is (powerTarget). This tier is the only one
	// that can see the container, and it cannot see whether a rebuild is
	// coming: the control plane can, and is the one that waits for it
	// (ADR 26-10-12-711 §6). Waiting here held every caller of a failed
	// sandbox whose container was simply gone for the whole wait.
	current, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	// A sandbox that is already up needs no start, so its traffic does not
	// wait on the start gate while a cache clear holds it. A clear stopping it
	// a moment after this look is the same as any stop landing just after a
	// request, which the proxy already has to answer.
	//
	// Running is Docker's word, though, and Docker says it the moment the
	// container starts, well before the sandbox agent inside it answers. A
	// start or create under way is still booting it, and the request waits for
	// that boot to end; proxying on the first look sends it to an agent that is
	// not listening yet, and it comes back a bare 502. A boot can end without
	// the sandbox up, so one that was waited on is followed by a second look
	// the ordinary way, which starts the sandbox if it went down.
	if current.Status == StatusRunning {
		waited, err := r.awaitBoot(ctx, sandboxID)
		if err != nil || !waited {
			return err
		}
	}
	for {
		// A start that retires a container from before the bootstrap named
		// the pool's key answers that there is none (errContainerRetired),
		// like any other: the control plane rebuilds it.
		booting, err := r.startUnlessRunning(ctx, sandboxID)
		if err != nil || !booting {
			return err
		}
		// Running, but on a boot an earlier request began and left: a boot
		// outlives its requester. It is waited out here, with neither the gate
		// nor the lock held, so a stop or a clear is not held up behind it,
		// and then the sandbox is looked at again.
		if _, err := r.awaitBoot(ctx, sandboxID); err != nil {
			return err
		}
	}
}

// startUnlessRunning starts the sandbox unless it is already running, inside
// the start gate and under the sandbox's power lock. It reports whether it
// found the sandbox running on a boot still under way, which its caller waits
// out once both are released.
func (r *DockerSandboxRuntime) startUnlessRunning(ctx context.Context, sandboxID string) (bool, error) {
	leave, err := r.starts.enter(ctx)
	if err != nil {
		return false, err
	}
	defer leave()
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	defer unlock()
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	if sb.Status == StatusRunning {
		return r.SandboxBooting(sandboxID), nil
	}
	return false, r.startLocked(ctx, sandboxID)
}

// powerTarget returns the container a power operation acts on, and says why
// there is none when there is not.
//
// No container is three different answers, and only this tier can tell them
// apart. An archived sandbox has none by intent (ADR 0022 §5). One whose tree
// this pool holds is a sandbox being rebuilt or one that needs repair — it is
// not missing, and calling it missing sent an explicit start back as a bare
// "sandbox not found" while an attach to the same sandbox answered that it
// needed repair. Only an id whose tree is not here is not found.
//
// It does not wait for a rebuild. That wait is the control plane's, which
// knows whether one is coming (ADR 26-10-12-711 §6); here it would only delay
// the answer for a sandbox nothing is rebuilding.
func (r *DockerSandboxRuntime) powerTarget(ctx context.Context, sandboxID string) (*Sandbox, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return sb, err
	}
	switch {
	case r.isArchived(sandboxID):
		return nil, ErrArchived
	case r.hostsSandbox(sandboxID):
		return nil, ErrNoContainer
	}
	return nil, err
}

func (r *DockerSandboxRuntime) startLocked(ctx context.Context, sandboxID string) error {
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	if err := r.harnessStartAllowed(sandboxID); err != nil {
		return err
	}
	// Archiving removes the container, so reaching here with a marked tree means
	// a container survived a partial archive. Starting it would silently undo the
	// archive and put the sandbox back beyond the reach of its retention policy.
	if r.isArchived(sandboxID) {
		return ErrArchived
	}
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	switch sb.Status {
	case StatusRunning:
		return nil
	case StatusSuspended:
		return r.resumeLocked(ctx, sb)
	}
	if err := r.retireContainerWithoutRuntimeConfig(ctx, sb); err != nil {
		return err
	}
	// Announce the transition before making it. The Docker event only arrives
	// once the container is up, and waitForSandboxAgent can take a while after
	// that, so without this nobody could ever observe `starting`.
	r.publishSandboxState(ctx, sandboxID, StateStarting)
	// Every start of a delivered sandbox begins with its agent, units and
	// pins in place (ADR 26-10-16-423 §7): a pin moved while it was down is
	// what it boots on.
	if err := r.placeAgent(ctx, sandboxID, sb.ID); err != nil {
		r.publishSandboxState(ctx, sandboxID, StateStopped)
		return err
	}
	boot := r.beginBoot(sandboxID)
	if _, err := r.client.ContainerStart(ctx, sb.ID, client.ContainerStartOptions{}); err != nil {
		r.publishSandboxState(ctx, sandboxID, StateStopped)
		r.endBoot(sandboxID, boot, err)
		return err
	}
	return r.finishBoot(ctx, sandboxID, boot)
}

// sandboxBoot is one start of a sandbox's container, from just before
// ContainerStart until its sandbox agent answers or the wait for it gives up.
// err is written once, before done is closed.
type sandboxBoot struct {
	done chan struct{}
	err  error
	// mark is the sandbox's boot mark on disk (bootMarkPath), which another
	// process of the pool sees, held by the newest boot alone: a newer boot
	// takes it over from the one it replaces, and the newest releases it as
	// it ends. Nil on an older boot, and on one whose mark could not be
	// taken, which leaves the mark this process's alone. Under bootMu.
	mark *handover.Lock
}

// beginBoot marks a sandbox as booting. It is called before ContainerStart, so
// there is no moment Docker reports the container running and the mark is not
// there. Every beginBoot is ended by endBoot, directly when the container did
// not start and through finishBoot when it did.
//
// Boots of one sandbox can overlap. Both paths that start a container hold its
// power lock to do it, but a boot's wait outlives that lock (see finishBoot), so
// a restart or a replacing create can begin a new boot while the old one is
// still waiting. The newer boot replaces the older in the map, and endBoot
// removes only its own, so the mark lasts until the newest boot ends; a caller
// waiting on the older one wakes when it ends and looks again. The mark on
// disk follows the map: the newer boot takes it over, and it is let go of
// when the newest boot ends, so another process sees what this one does.
//
// A sandbox deleted or archived mid-boot reads as booting until the wait sees
// its box gone, or at most until sandboxAgentReadyTimeout. Nothing routes on
// the mark for a sandbox that is gone.
func (r *sandboxCore) beginBoot(sandboxID string) *sandboxBoot {
	r.bootMu.Lock()
	defer r.bootMu.Unlock()
	boot := &sandboxBoot{done: make(chan struct{})}
	if value, ok := r.booting.Load(sandboxID); ok {
		if older, ok := value.(*sandboxBoot); ok {
			boot.mark, older.mark = older.mark, nil
		}
	}
	if boot.mark == nil {
		// A mark waits only behind the file locked alone, which nothing
		// does to a boot mark; the wait is bounded all the same, since every
		// sandbox's boots begin under bootMu.
		ctx, cancel := context.WithTimeout(context.Background(), bootMarkWait)
		mark, err := handover.Mark(ctx, r.bootMarkPath(sandboxID))
		cancel()
		if err != nil {
			slog.Warn("sandbox boot mark not held on disk", "sandboxID", sandboxID, "error", err)
		}
		boot.mark = mark
	}
	r.booting.Store(sandboxID, boot)
	return boot
}

func (r *sandboxCore) endBoot(sandboxID string, boot *sandboxBoot, err error) {
	r.bootMu.Lock()
	boot.err = err
	if r.booting.CompareAndDelete(sandboxID, boot) {
		_ = boot.mark.Release()
		boot.mark = nil
	}
	r.bootMu.Unlock()
	close(boot.done)
}

// stopRefusedSandbox stops a container whose agent refused the pool's
// runtime-config delivery outright. It runs inside the boot, which the caller
// holding the sandbox's power lock is waiting on, so it acts on the container
// directly rather than through stopLocked.
func (r *DockerSandboxRuntime) stopRefusedSandbox(ctx context.Context, sandboxID string, refusal error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return
	}
	slog.WarnContext(ctx, "stopping a sandbox that refuses the pool's runtime config", "sandboxId", sandboxID, "error", refusal)
	r.publishSandboxState(ctx, sandboxID, StateStopping)
	timeout := sandboxStopTimeoutSeconds
	if _, err := r.client.ContainerStop(ctx, sb.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
		slog.WarnContext(ctx, "stop a sandbox that refuses the pool's runtime config", "sandboxId", sandboxID, "error", err)
	}
}

// errContainerRetired is a start that found the sandbox's container built from
// a bootstrap with no pool key — a container from before the pool delivered
// runtime config — and removed it, so the control plane rebuilds it onto one
// that does (ADR 26-10-08-127). It is a no-container answer: the sandbox is
// being rebuilt.
var errContainerRetired = fmt.Errorf("%w: its container was built before its pool delivered runtime config, and is being rebuilt", ErrNoContainer)

// retireContainerWithoutRuntimeConfig removes a stopped container that cannot
// take the pool's runtime-config documents, and returns errContainerRetired.
//
// Such a container runs on the files staged for it before this pool stopped
// staging any, so nothing it is told after — a rotated secret, a changed idle
// timeout — reaches it. Its next start is where it moves: the durable tree is
// in the pool-host binds, so removing the container loses nothing, the state
// channel reports it gone, and the control plane's ensure recreates it from the
// spec it holds, on the bootstrap that names the pool's key.
func (r *DockerSandboxRuntime) retireContainerWithoutRuntimeConfig(ctx context.Context, sb *Sandbox) error {
	// A runtime with no key builds no container that takes runtime config, so
	// it has nothing to retire them for.
	if r.poolPublicKey() == "" {
		return nil
	}
	takes, err := r.containerTakesRuntimeConfig(ctx, sb.ID)
	if err != nil || takes {
		return err
	}
	slog.InfoContext(ctx, "retiring a sandbox container built before its pool delivered runtime config; the control plane rebuilds it", "sandboxId", sb.SandboxID)
	if err := r.removeForRebuild(ctx, sb); err != nil {
		return err
	}
	return errContainerRetired
}

// removeForRebuild removes a sandbox's container so the control plane
// recreates it from the spec it holds: the durable tree is in the pool-host
// binds, so removing the container loses nothing. The caller holds the
// sandbox's power lock.
func (r *DockerSandboxRuntime) removeForRebuild(ctx context.Context, sb *Sandbox) error {
	// Kept for the container the rebuild makes (ADR 26-11-14-302 §3).
	if err := r.stashWorkingRoot(ctx, sb.SandboxID, sb.ID); err != nil {
		return err
	}
	if _, err := r.client.ContainerRemove(ctx, sb.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove sandbox container for a rebuild: %w", err)
	}
	// The control plane learns a container is gone from a complete sync's
	// omission, so one goes now rather than at the next interval: the rebuild
	// starts while an on-demand start is still waiting for it.
	if publish, _ := r.statePublisher.Load().(func(context.Context, SandboxStateBatch) error); publish != nil {
		r.publishCompleteSync(ctx, slog.Default(), publish)
	}
	return nil
}

// sandboxAgentReadyTimeout bounds a boot's wait for its sandbox agent, once its
// box runs. It is a backstop for an agent that will never answer from a box
// that keeps running — systemd, not the agent, is PID 1, so an agent that
// fails leaves its box up — and not a measure of a slow boot: a box that
// exits or is removed ends the wait at once, whatever is left of it.
//
// It is generous on purpose. Every create waits here, so giving up on a box
// still booting fails the create while the box goes on to come up by itself.
// A container's agent answers in about a second on an idle host, and was seen
// taking minutes on one with five runnable threads for every CPU;
// a VM's provisions the sandbox user before it listens (ADR 26-10-09-143 §1).
const sandboxAgentReadyTimeout = 5 * time.Minute

// agentReadyBound is this runtime's sandboxAgentReadyTimeout.
func (r *sandboxCore) agentReadyBound() time.Duration {
	if r.agentReadyTimeout > 0 {
		return r.agentReadyTimeout
	}
	return sandboxAgentReadyTimeout
}

// agentWaitRestateInterval is how often a boot's wait for its sandbox agent
// restates PhaseWaitingForAgent (restateWaitingForAgent).
var agentWaitRestateInterval = 10 * time.Second

// agentWaitRestateTimeout bounds one restatement. It is a report to the
// control plane over a client with no timeout of its own, so a slow or wedged
// control plane must not hold the wait past an agent that has come up, or run
// it into its bound and fail the create.
const agentWaitRestateTimeout = 3 * time.Second

// restateWaitingForAgent restates PhaseWaitingForAgent every
// agentWaitRestateInterval for as long as a boot runs: finishBoot starts it
// before the wait for the sandbox agent, and stops it once the runtime-config
// document is delivered, before the boot ends. stop returns once no
// restatement is in flight, so none lands after what is reported next.
//
// The restatement is what keeps a slow boot from reading as a stalled one, as
// the settle wait's restating keeps a long clone current. Nothing else about
// the sandbox moves while its agent comes up, and both of the things watching
// it give up on a phase that goes unrestated: the attach wait's stall budget
// (two minutes, ADR 0039) and a client's freshness for a recorded phase
// (thirty seconds, ADR 0060).
//
// It lasts exactly as long as the boot, which is the observation: the boot
// runs detached from any request, its wait ends on the box exiting or being
// removed and at agentReadyBound, and its delivery at runtimeConfigCallTimeout,
// so it cannot keep an attach waiting on a box that is not coming up. It runs
// beside the boot rather than between the wait's passes because a starved
// agent is slow at everything: one /healthz was seen to take 56 s, and the
// delivery to it another 40 s after it first answered.
func (r *sandboxCore) restateWaitingForAgent(ctx context.Context, sandboxID string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(agentWaitRestateInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reportCtx, cancelReport := context.WithTimeout(ctx, agentWaitRestateTimeout)
				r.publishSandboxPhase(reportCtx, sandboxID, PhaseWaitingForAgent)
				cancelReport()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// finishBoot waits for the sandbox agent of a container that has just started,
// delivers it the sandbox's runtime-config document, and ends the boot when
// that is done or the wait gives up.
//
// Every start ends here — create, start, restart, and an on-demand start — so
// every boot is given the pool's current view of the sandbox: its secrets, its
// proxy credential, the pool's idle timeout as it is now, and its sources'
// delivery (ADR 0126 §3). A delivery that does not land is the status poll's to
// repair and does not fail the boot; an agent with no intake does, since nothing
// the pool could do would make that sandbox usable (ADR 26-10-08-127 §7).
//
// The wait belongs to the boot, not to the request that began it: that request
// going away — a client that reconnects, an attach canceled mid-start — must
// not end the mark while the agent is still not listening, or every request
// behind it would be proxied into a 502. So the wait runs detached, bounded by
// its own agentReadyBound, and the caller only waits on it for as long as its
// own context allows.
func (r *sandboxCore) finishBoot(ctx context.Context, sandboxID string, boot *sandboxBoot) error {
	go func() {
		detached := context.WithoutCancel(ctx)
		stopRestating := r.restateWaitingForAgent(detached, sandboxID)
		err := r.boxes.waitForSandboxAgent(detached, sandboxID)
		refused := false
		if err == nil {
			err = logRuntimeConfigFailure(detached, sandboxID, r.deliverRuntimeConfig(detached, sandboxID, nil))
			refused = err != nil
		}
		// Before anything else is reported of the sandbox: a restatement still
		// in flight must not land after it.
		stopRestating()
		if refused {
			// The agent will never take this pool's document. Left running,
			// the container would read as a healthy sandbox to the next
			// request once this boot's mark is gone; stopped, every start
			// meets the refusal again and says so.
			r.boxes.stopRefusedSandbox(detached, sandboxID, err)
		}
		r.endBoot(sandboxID, boot, err)
	}()
	select {
	case <-boot.done:
		return boot.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitBoot waits out a boot of the sandbox under way, and reports whether
// there was one. How that boot ended is its starter's to report; a caller that
// waited looks at the sandbox again. A boot the other agent of a handover
// began is waited out by its mark on disk.
func (r *sandboxCore) awaitBoot(ctx context.Context, sandboxID string) (bool, error) {
	value, ok := r.booting.Load(sandboxID)
	if !ok {
		held, err := r.bootMarked(sandboxID)
		if err != nil {
			return true, err
		}
		if !held {
			return false, nil
		}
		return true, handover.AwaitFree(ctx, r.bootMarkPath(sandboxID))
	}
	boot, ok := value.(*sandboxBoot)
	if !ok {
		return false, nil
	}
	select {
	case <-boot.done:
		return true, nil
	case <-ctx.Done():
		return true, ctx.Err()
	}
}

// AwaitSandboxBoot waits out a boot of the sandbox under way, for a caller
// that must not start it (requireRunning). How the boot ended is its
// starter's to report.
func (r *sandboxCore) AwaitSandboxBoot(ctx context.Context, sandboxID string) error {
	_, err := r.awaitBoot(ctx, sandboxID)
	return err
}

// SandboxBooting reports whether the sandbox's container is up but its sandbox
// agent has not answered yet.
func (r *sandboxCore) SandboxBooting(sandboxID string) bool {
	if _, ok := r.booting.Load(sandboxID); ok {
		return true
	}
	held, err := r.bootMarked(sandboxID)
	if err != nil {
		if r.warnBootMark(sandboxID) {
			slog.Warn("read a sandbox's boot mark; reading it as booting", "sandboxID", sandboxID, "error", err)
		}
		return true
	}
	return held
}

// bootMarkWait bounds beginBoot's wait to take a boot mark.
const bootMarkWait = 5 * time.Second

// bootMarkWarnEvery is how often an unreadable boot mark is logged, per
// sandbox: SandboxBooting asks on the request path.
const bootMarkWarnEvery = time.Minute

// warnBootMark reports whether an unreadable boot mark of sandboxID is due a
// warning, at most once per bootMarkWarnEvery.
func (r *sandboxCore) warnBootMark(sandboxID string) bool {
	now := time.Now()
	if last, ok := r.bootMarkWarned.Load(sandboxID); ok {
		if at, ok := last.(time.Time); ok && now.Sub(at) < bootMarkWarnEvery {
			return false
		}
	}
	r.bootMarkWarned.Store(sandboxID, now)
	return true
}

// bootMarked reports whether a process of the pool holds the sandbox's boot
// mark. A mark that cannot be read is an error, which its callers read as a
// boot under way (SandboxBooting) or fail with (awaitBoot): never as a boot
// ended, which would let this process act on a box the other agent is
// booting.
func (r *sandboxCore) bootMarked(sandboxID string) (bool, error) {
	held, err := handover.Held(r.bootMarkPath(sandboxID))
	if err != nil {
		return false, fmt.Errorf("sandbox %s's boot mark: %w", sandboxID, err)
	}
	return held, nil
}

func (r *DockerSandboxRuntime) stopLocked(ctx context.Context, sandboxID string) error {
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	r.publishSandboxState(ctx, sandboxID, StateStopping)
	timeout := sandboxStopTimeoutSeconds
	_, err = r.client.ContainerStop(ctx, sb.ID, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

const sandboxStopTimeoutSeconds = 10

var ErrStartNotAuthorized = errors.New("explicit start is no longer authorized")

// Called with the runtime power lock held, after queues and the start gate.
// A lost HTTP response can leave an old request queued here after the server
// admits a stop. The callback observes that stop even if it never reached us.
func (r *sandboxCore) authorizeExplicitStart(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if req == nil || !req.StartEpoch.IsSet() {
		return nil
	}
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return fmt.Errorf("%w: %w", ErrStartNotAuthorized, err)
	}
	if r.authorizeStart == nil {
		return fmt.Errorf("explicit start authorization is unavailable")
	}
	if err := r.authorizeStart(ctx, sandboxID, req.StartEpoch.Value); err != nil {
		return fmt.Errorf("%w: %w", ErrStartNotAuthorized, err)
	}
	return nil
}
