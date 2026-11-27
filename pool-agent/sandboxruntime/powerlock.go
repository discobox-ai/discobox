package sandboxruntime

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// A sandbox's power lock and boot mark hold across the two agents of a
// handover (ADR 26-11-26-536 §4): a power operation the old agent still has in
// flight, or a boot it is still waiting out, is seen by the new one, which
// waits for it. Each is a file of the pool's lock tree (layout.PoolLocks),
// locked (handover.Acquire, Mark), in front of which this process keeps its own
// semaphore, so its own waiters queue without polling the file.

// powerSemaphore is this process's half of sandboxID's power lock.
func (r *sandboxCore) powerSemaphore(sandboxID string) chan struct{} {
	actual, _ := r.powerLocks.LoadOrStore(sandboxID, make(chan struct{}, 1))
	semaphore, _ := actual.(chan struct{})
	return semaphore
}

func (r *sandboxCore) powerLockPath(sandboxID string) string {
	return filepath.Join(r.root.PoolLocks(r.projectID, r.poolID), sandboxID+".power")
}

func (r *sandboxCore) bootMarkPath(sandboxID string) string {
	return filepath.Join(r.root.PoolLocks(r.projectID, r.poolID), sandboxID+".boot")
}

// lockPower waits, until ctx ends, to hold sandboxID's power lock, and
// returns what lets go of it.
func (r *sandboxCore) lockPower(ctx context.Context, sandboxID string) (func(), error) {
	semaphore := r.powerSemaphore(sandboxID)
	select {
	case semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	lock, err := handover.Acquire(ctx, r.powerLockPath(sandboxID))
	if err != nil {
		<-semaphore
		return nil, fmt.Errorf("sandbox %s's power lock: %w", sandboxID, err)
	}
	return func() {
		_ = lock.Release()
		<-semaphore
	}, nil
}

// forgetLocks removes sandboxID's power lock and boot mark, as its removal
// ends, holding its power lock; a waiter for its lock locks a new file
// (handover.Remove). A later power operation on the removed sandbox (a reaper,
// an update, that waiter) makes its lock file again: on a pool container it
// lasts until the boot ends, and on a host root it stays an unlocked file
// (layout.PoolLocks).
func (r *sandboxCore) forgetLocks(sandboxID string) {
	for _, path := range []string{r.powerLockPath(sandboxID), r.bootMarkPath(sandboxID)} {
		if err := handover.Remove(path); err != nil {
			slog.Warn("remove a removed sandbox's lock", "sandboxID", sandboxID, "error", err)
		}
	}
}

// tryLockPower holds sandboxID's power lock if nothing holds it now, in this
// process or another, and reports whether it does. A lock file it cannot make
// or lock is an error, never "held elsewhere": its caller would otherwise
// step aside from the sandbox for good.
func (r *sandboxCore) tryLockPower(sandboxID string) (func(), bool, error) {
	semaphore := r.powerSemaphore(sandboxID)
	select {
	case semaphore <- struct{}{}:
	default:
		return nil, false, nil
	}
	lock, held, err := handover.TryAcquire(r.powerLockPath(sandboxID))
	if err != nil || !held {
		<-semaphore
		if err != nil {
			return nil, false, fmt.Errorf("sandbox %s's power lock: %w", sandboxID, err)
		}
		return nil, false, nil
	}
	return func() {
		_ = lock.Release()
		<-semaphore
	}, true, nil
}
