package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/pkg/layout"
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

// The lock tree's two files per sandbox: its power lock and its boot mark.
const (
	powerLockSuffix = ".power"
	bootMarkSuffix  = ".boot"
)

func (r *sandboxCore) powerLockPath(sandboxID string) string {
	return filepath.Join(r.root.PoolLocks(r.projectID, r.poolID), sandboxID+powerLockSuffix)
}

func (r *sandboxCore) bootMarkPath(sandboxID string) string {
	return filepath.Join(r.root.PoolLocks(r.projectID, r.poolID), sandboxID+bootMarkSuffix)
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
// an update, that waiter) makes its lock file again, which reapLockFiles
// reaps, as it does a boot mark left below.
//
// The boot mark stays while a boot still holds it, in this process or
// another: removing it would have a probe of the path find no boot while that
// boot runs. No boot begins meanwhile, since one begins only under the power
// lock the removal holds.
func (r *sandboxCore) forgetLocks(sandboxID string) {
	paths := []string{r.powerLockPath(sandboxID)}
	if !r.bootHeld(sandboxID) {
		paths = append(paths, r.bootMarkPath(sandboxID))
	}
	for _, path := range paths {
		if err := handover.Remove(path); err != nil {
			slog.Warn("remove a removed sandbox's lock", "sandboxID", sandboxID, "error", err)
		}
	}
}

// bootHeld reports whether a boot of sandboxID still holds its mark, in this
// process or another. One that cannot be read is held.
func (r *sandboxCore) bootHeld(sandboxID string) bool {
	if _, ok := r.booting.Load(sandboxID); ok {
		return true
	}
	held, err := r.bootMarked(sandboxID)
	return err != nil || held
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

// reapLockFiles removes the lock files of sandboxes the pool no longer has:
// no box among live, and no tree. The reapers and convergence lock every
// sandbox they look at, orphans included, and only a removal that succeeds
// removes its files, so without this they would pile up in a lock tree that
// outlives a boot (a host root's, under its state). Each is removed holding
// its power lock, so one held is skipped; a locker re-checks that the path
// still names the file it locked, so a file removed under it never splits a
// lock.
func (r *sandboxCore) reapLockFiles(logger *slog.Logger, live []string) {
	entries, err := os.ReadDir(r.root.PoolLocks(r.projectID, r.poolID))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		logger.Warn("list the pool's lock files", "error", err)
		return
	}
	alive := make(map[string]bool, len(live))
	for _, id := range live {
		alive[id] = true
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		sandboxID, ok := strings.CutSuffix(entry.Name(), powerLockSuffix)
		if !ok {
			sandboxID, ok = strings.CutSuffix(entry.Name(), bootMarkSuffix)
		}
		if !ok || seen[sandboxID] || alive[sandboxID] || layout.IsSnapshotID(sandboxID) {
			continue
		}
		seen[sandboxID] = true
		if r.hasTree(sandboxID) {
			continue
		}
		unlock, locked, err := r.tryLockPower(sandboxID)
		if err != nil {
			logger.Warn("lock a removed sandbox to reap its lock files", "sandboxID", sandboxID, "error", err)
			continue
		}
		if !locked {
			continue
		}
		// Looked at again under the lock: a create that held it meanwhile
		// made the tree.
		if !r.hasTree(sandboxID) {
			r.forgetLocks(sandboxID)
		}
		unlock()
	}
}

// hasTree reports whether sandboxID has a tree in this pool; one that
// cannot be looked at is taken to have one.
func (r *sandboxCore) hasTree(sandboxID string) bool {
	_, err := os.Stat(r.root.Sandbox(r.projectID, r.poolID, sandboxID))
	return !errors.Is(err, os.ErrNotExist)
}
