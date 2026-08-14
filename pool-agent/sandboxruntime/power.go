package sandboxruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/moby/moby/client"

	workerapimodel "github.com/obot-platform/discobox/pool-agent/api/model"
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

// sandboxLock returns the mutex guarding power operations for one sandbox.
func (r *DockerSandboxRuntime) sandboxLock(sandboxID string) *sync.Mutex {
	actual, _ := r.powerLocks.LoadOrStore(sandboxID, &sync.Mutex{})
	lock, _ := actual.(*sync.Mutex)
	return lock
}

func (r *DockerSandboxRuntime) StartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
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
func (r *DockerSandboxRuntime) EnsureSandboxRunning(ctx context.Context, sandboxID string) error {
	// A sandbox whose container is not there yet is waited for, not failed
	// (ADR 0039 tier 2). This tier is the only one that can see the container,
	// and a rebuild — repair, or a recreate after runtime loss — leaves a
	// window where this pool holds the sandbox's tree and its container is
	// between removal and recreation. Falling through it produced "no
	// inspectable IP address" from the proxy, about a fact the caller could
	// not act on.
	//
	// The wait is outside the lock: the create it is waiting for takes the same
	// lock, so holding it here would wait for something it was blocking.
	if err := r.waitForSandboxContainer(ctx, sandboxID); err != nil {
		return err
	}
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		// An archived sandbox has no container, so the lookup fails first. The
		// archive check answers the more useful question, and only for a
		// sandbox this pool actually holds.
		if errors.Is(err, ErrNotFound) && r.SandboxIsArchived(sandboxID) {
			return ErrArchived
		}
		return err
	}
	if sb.Status == StatusRunning {
		return nil
	}
	return r.startLocked(ctx, sandboxID)
}

// sandboxContainerWaitTimeout bounds the wait for a container that is being
// rebuilt. It is strictly shorter than the control plane's own wait, so a
// container that never appears is reported by this tier — the only one that can
// see it — rather than by a deadline two levels out (ADR 0039).
const (
	sandboxContainerWaitTimeout  = 90 * time.Second
	sandboxContainerPollInterval = 250 * time.Millisecond
)

// waitForSandboxContainer returns as soon as the sandbox has a container, and
// waits for one only when this pool is holding the sandbox's tree.
//
// An id whose tree is not here is not late, it is wrong: waiting on it would
// turn a prompt error into a minute and a half of silence on every route
// autoStart wraps. An archived sandbox is not waited on either — its container
// is gone by intent (ADR 0022 §5) — and is left to the archive answer its
// caller gives.
func (r *DockerSandboxRuntime) waitForSandboxContainer(ctx context.Context, sandboxID string) error {
	deadline := time.Now().Add(sandboxContainerWaitTimeout)
	for {
		_, err := r.GetSandbox(ctx, sandboxID)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return err
		}
		if r.SandboxIsArchived(sandboxID) || !r.hostsSandbox(sandboxID) {
			return nil
		}
		if !time.Now().Before(deadline) {
			// The caller reports the container that never came back; there is
			// nothing more specific this tier can say about it.
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sandboxContainerPollInterval):
		}
	}
}

func (r *DockerSandboxRuntime) startLocked(ctx context.Context, sandboxID string) error {
	// Archiving removes the container, so reaching here with a marked tree means
	// a container survived a partial archive. Starting it would silently undo the
	// archive and put the sandbox back beyond the reach of its retention policy.
	if r.SandboxIsArchived(sandboxID) {
		return ErrArchived
	}
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if sb.Status == StatusRunning {
		return nil
	}
	// Announce the transition before making it. The Docker event only arrives
	// once the container is up, and waitForSandboxAgent can take a while after
	// that, so without this nobody could ever observe `starting`.
	r.PublishSandboxState(ctx, sandboxID, StateStarting)
	if _, err := r.client.ContainerStart(ctx, sb.ID, client.ContainerStartOptions{}); err != nil {
		r.PublishSandboxState(ctx, sandboxID, StateStopped)
		return err
	}
	return r.waitForSandboxAgent(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) stopLocked(ctx context.Context, sandboxID string) error {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	r.PublishSandboxState(ctx, sandboxID, StateStopping)
	timeout := sandboxStopTimeoutSeconds
	_, err = r.client.ContainerStop(ctx, sb.ID, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

const sandboxStopTimeoutSeconds = 10
