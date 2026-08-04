package sandboxruntime

import (
	"context"
	"sync"

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
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if sb.Status == StatusRunning {
		return nil
	}
	return r.startLocked(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) startLocked(ctx context.Context, sandboxID string) error {
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
