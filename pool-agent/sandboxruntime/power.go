package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
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

// sandboxLock returns the mutex guarding power operations for one sandbox.
func (r *DockerSandboxRuntime) sandboxLock(sandboxID string) *sync.Mutex {
	actual, _ := r.powerLocks.LoadOrStore(sandboxID, &sync.Mutex{})
	lock, _ := actual.(*sync.Mutex)
	return lock
}

func (r *DockerSandboxRuntime) StartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	leave, err := r.starts.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	return r.startLocked(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) StopSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	return r.stopLocked(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) RestartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	leave, err := r.starts.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return err
	}
	return r.startLocked(ctx, sandboxID)
}

// EnsureSandboxRunning starts a sandbox that is not running, and is a no-op for
// one that already is. It is what the sandbox-directed routes call so that a
// stopped sandbox comes up on first use (ADR 0017 §12).
//
// Nothing about it is a special path: it takes the same lock and calls the same
// start as an explicit instruction, so an implicitly started sandbox reports
// starting and then running exactly like any other.
func (r *DockerSandboxRuntime) EnsureSandboxRunning(ctx context.Context, sandboxID string, awaitContainer bool) error {
	// A sandbox whose container is not there yet is waited for, not failed,
	// when the caller is one that waits (ADR 0039 tier 2). This tier is the
	// only one that can see the container, and a rebuild — repair, or a
	// recreate after runtime loss — leaves a window where this pool holds the
	// sandbox's tree and its container is between removal and recreation.
	//
	// It cannot see whether a rebuild is actually coming, so the wait is only
	// for the routes the control plane itself waits on. Everything else is
	// asked on a cadence or by a command that wants an answer, and a failed
	// sandbox whose container is simply gone held each of those for the whole
	// wait before saying so.
	//
	// The wait is outside the lock: the create it is waiting for takes the same
	// lock, so holding it here would wait for something it was blocking.
	current, err := r.sandboxContainer(ctx, sandboxID, awaitContainer)
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
	if current != nil && current.Status == StatusRunning {
		waited, err := r.awaitBoot(ctx, sandboxID)
		if err != nil || !waited {
			return err
		}
	}
	for {
		booting, err := r.startUnlessRunning(ctx, sandboxID)
		if errors.Is(err, errContainerRetired) && awaitContainer {
			// The start retired a container from before the bootstrap named
			// the pool's key; the control plane rebuilds it, and the caller
			// waits for that like any other rebuild.
			if _, err := r.sandboxContainer(ctx, sandboxID, true); err != nil {
				return err
			}
			continue
		}
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
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	if sb.Status == StatusRunning {
		return r.SandboxBooting(sandboxID), nil
	}
	return false, r.startLocked(ctx, sandboxID)
}

// sandboxContainerWaitTimeout bounds the wait for a container that is being
// rebuilt. It is strictly shorter than the control plane's own wait, so a
// container that never appears is reported by this tier — the only one that can
// see it — rather than by a deadline two levels out (ADR 0039).
const (
	sandboxContainerWaitTimeout  = 90 * time.Second
	sandboxContainerPollInterval = 250 * time.Millisecond
)

// sandboxContainer returns as soon as the sandbox has a container, and — when
// asked to await one — waits for one only when this pool is holding the
// sandbox's tree.
//
// An id whose tree is not here is not late, it is wrong: waiting on it would
// turn a prompt error into a minute and a half of silence on every route
// autoStart wraps. An archived sandbox is not waited on either — its container
// is gone by intent (ADR 0022 §5) — and is left to the archive answer its
// caller gives.
//
// It returns the sandbox as last read, or nil when it stopped waiting without
// one.
func (r *DockerSandboxRuntime) sandboxContainer(ctx context.Context, sandboxID string, await bool) (*Sandbox, error) {
	deadline := time.Now()
	if await {
		deadline = deadline.Add(sandboxContainerWaitTimeout)
	}
	for {
		sb, err := r.GetSandbox(ctx, sandboxID)
		if err == nil {
			return sb, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if r.isArchived(sandboxID) || !r.hostsSandbox(sandboxID) {
			return nil, nil
		}
		if !time.Now().Before(deadline) {
			// The tree is here and the container is not: the sandbox exists,
			// and repair is what gives it one.
			return nil, ErrNoContainer
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sandboxContainerPollInterval):
		}
	}
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
// It does not wait for a rebuild. That wait belongs to the route the control
// plane itself waits on (see EnsureSandboxRunning); a power instruction is a
// command that wants an answer, and for a sandbox nothing is rebuilding the
// wait would only delay the one it gets.
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
	if sb.Status == StatusRunning {
		return nil
	}
	if err := r.retireContainerWithoutRuntimeConfig(ctx, sb); err != nil {
		return err
	}
	// Announce the transition before making it. The Docker event only arrives
	// once the container is up, and waitForSandboxAgent can take a while after
	// that, so without this nobody could ever observe `starting`.
	r.publishSandboxState(ctx, sandboxID, StateStarting)
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
// waiting on the older one wakes when it ends and looks again.
//
// A sandbox deleted or archived mid-boot reads as booting until the wait gives
// up, at most sandboxAgentReadyTimeout. Nothing routes on the mark for a
// sandbox that is gone.
func (r *DockerSandboxRuntime) beginBoot(sandboxID string) *sandboxBoot {
	boot := &sandboxBoot{done: make(chan struct{})}
	r.booting.Store(sandboxID, boot)
	return boot
}

func (r *DockerSandboxRuntime) endBoot(sandboxID string, boot *sandboxBoot, err error) {
	boot.err = err
	r.booting.CompareAndDelete(sandboxID, boot)
	close(boot.done)
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
	if _, err := r.client.ContainerRemove(ctx, sb.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("retire sandbox container: %w", err)
	}
	// The control plane learns a container is gone from a complete sync's
	// omission, so one goes now rather than at the next interval: the rebuild
	// starts while an on-demand start is still waiting for it.
	if publish, _ := r.statePublisher.Load().(func(context.Context, SandboxStateBatch) error); publish != nil {
		r.publishCompleteSync(ctx, slog.Default(), publish)
	}
	return errContainerRetired
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
// its own sandboxAgentReadyTimeout, and the caller only waits on it for as long
// as its own context allows.
func (r *DockerSandboxRuntime) finishBoot(ctx context.Context, sandboxID string, boot *sandboxBoot) error {
	go func() {
		detached := context.WithoutCancel(ctx)
		err := r.waitForSandboxAgent(detached, sandboxID)
		if err == nil {
			err = logRuntimeConfigFailure(detached, sandboxID, r.deliverRuntimeConfig(detached, sandboxID, nil))
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
// waited looks at the sandbox again.
func (r *DockerSandboxRuntime) awaitBoot(ctx context.Context, sandboxID string) (bool, error) {
	value, ok := r.booting.Load(sandboxID)
	if !ok {
		return false, nil
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

// SandboxBooting reports whether the sandbox's container is up but its sandbox
// agent has not answered yet.
func (r *DockerSandboxRuntime) SandboxBooting(sandboxID string) bool {
	_, ok := r.booting.Load(sandboxID)
	return ok
}

func (r *DockerSandboxRuntime) stopLocked(ctx context.Context, sandboxID string) error {
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
