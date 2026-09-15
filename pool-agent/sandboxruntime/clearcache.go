package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
	"github.com/discobox-ai/discobox/pool-agent/imagereap"
)

// Clearing a pool's caches is the agent's own operation from end to end: it is
// the only component that can see which sandboxes use them and the only one that
// can reach them. The control plane asks and waits for the answer; it
// orchestrates nothing.
//
// The pool cache is bind-mounted whole into every sandbox, and the build cache,
// registry and proxy cache all serve what sandboxes ask of them, so emptying any
// of them under a running sandbox pulls things out from under whatever is using
// them. Every running sandbox is therefore stopped first, and nothing may start
// one again until the caches are empty. Nothing is started afterwards either: a
// stopped sandbox comes back on its next use (ADR 0017 §12), which is when it has
// something to do.

// ClearCache stops every running sandbox on this pool, empties the pool's
// caches (see clearCaches), and returns the sandboxes it stopped.
//
// It answers only once the work is done, and a second request while one is
// running waits for that one instead of starting another. How far a clear
// belongs to the requests waiting on it depends on how far it has got:
//
//   - While it waits for starts already under way to finish, it has done
//     nothing yet, and that wait can be as long as a create's image pull. If
//     every waiting request gives up, the clear gives up too and reopens the
//     gate, rather than holding the pool's starts off for a result nobody is
//     waiting for.
//   - Once the gate has drained it commits, and from there the clear finishes
//     whether or not anyone is still waiting, rather than leaving a pool with
//     its sandboxes stopped and half its cache deleted.
func (r *DockerSandboxRuntime) ClearCache(ctx context.Context) ([]string, error) {
	for {
		r.clearMu.Lock()
		run := r.clearRun
		if run == nil {
			drainCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			run = &cacheClear{done: make(chan struct{}), cancel: cancel}
			r.clearRun = run
			go r.runCacheClear(drainCtx, run)
		}
		if run.abandoned {
			// Everyone waiting on that clear gave up before it committed, and it
			// is on its way out. Wait for it to go, then start one of our own.
			r.clearMu.Unlock()
			select {
			case <-run.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		run.waiters++
		r.clearMu.Unlock()
		select {
		case <-run.done:
			if run.err != nil {
				return nil, run.err
			}
			return slices.Clone(run.stopped), nil
		case <-ctx.Done():
			r.clearMu.Lock()
			run.waiters--
			if run.waiters == 0 && !run.committed {
				run.abandoned = true
				run.cancel()
			}
			r.clearMu.Unlock()
			return nil, ctx.Err()
		}
	}
}

// cacheClear is one clear and the requests waiting on it. Every field but done,
// stopped and err is guarded by DockerSandboxRuntime.clearMu; stopped and err
// are written once, before done is closed.
type cacheClear struct {
	done   chan struct{}
	cancel context.CancelFunc
	// waiters counts the requests still waiting for the result.
	waiters int
	// committed is set once the gate has drained and the clear goes ahead
	// regardless of waiters; abandoned once the last waiter left before that.
	// At most one of them is ever set.
	committed bool
	abandoned bool
	stopped   []string
	err       error
}

func (r *DockerSandboxRuntime) runCacheClear(drainCtx context.Context, run *cacheClear) {
	defer run.cancel()
	run.stopped, run.err = r.clearCache(drainCtx, run)
	r.clearMu.Lock()
	r.clearRun = nil
	r.clearMu.Unlock()
	close(run.done)
}

func (r *DockerSandboxRuntime) clearCache(drainCtx context.Context, run *cacheClear) ([]string, error) {
	// Closed before the sandboxes are listed, and drained of every start already
	// under way, so the list below is final: a sandbox a create or start was
	// bringing up is up by the time it is read, and none can come up after.
	reopen, err := r.starts.close(drainCtx)
	if err != nil {
		return nil, err
	}
	defer reopen()
	// The commit is decided under the same lock the last waiter abandons under,
	// so a clear either sees that it was abandoned or is committed before
	// anyone can abandon it.
	r.clearMu.Lock()
	if run.abandoned {
		r.clearMu.Unlock()
		return nil, context.Canceled
	}
	run.committed = true
	r.clearMu.Unlock()
	ctx := context.WithoutCancel(drainCtx)

	sandboxes, err := r.ListSandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		stopped []string
		errs    []error
	)
	for _, sb := range sandboxes {
		if sb.Status != StatusRunning {
			continue
		}
		wg.Add(1)
		go func(sandboxID string) {
			defer wg.Done()
			wasRunning, err := r.stopIfRunning(ctx, sandboxID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("stop sandbox %s: %w", sandboxID, err))
				return
			}
			if wasRunning {
				stopped = append(stopped, sandboxID)
			}
		}(sb.SandboxID)
	}
	wg.Wait()
	// A sandbox that would not stop may still be using the caches, and deleting
	// under it is exactly what stopping was for. Leave them alone and say which
	// one; the sandboxes that did stop stay stopped.
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := r.clearCaches(ctx); err != nil {
		return nil, err
	}
	slices.Sort(stopped)
	return stopped, nil
}

// clearCaches empties every cache the pool holds, once nothing that uses them
// is running. Each is cleared the way its owner can survive while it keeps
// serving, and a failure in one does not stop the others: they are independent,
// and a partial clear is still disk reclaimed.
//
//   - The pool cache is only ever read by sandboxes, and they are stopped.
//   - BuildKit's cache is pruned through buildkitd, which holds its store open.
//   - The proxy's response cache is emptied underneath the running proxy, which
//     treats an entry whose file is gone as a miss and forgets it.
//   - The pool registry's storage is emptied underneath the running registry,
//     which keeps no descriptor cache that could go on vouching for a blob.
//   - Unused Discobox images are removed from the daemon once they are older
//     than clearImageFloor. Stopped sandboxes still use theirs, and the newest
//     image of each repository stays.
//
// The two that ask another process to do the work are bounded (buildPruneTimeout
// and imageReclaimTimeout): the start gate is closed for as long as this runs,
// and a buildkitd or daemon that stops answering must not hold every start on
// the pool off with it.
func (r *DockerSandboxRuntime) clearCaches(ctx context.Context) error {
	var errs []error
	for _, dir := range []struct{ name, path string }{
		{"pool cache", r.poolCacheRoot()},
		{"proxy response cache", resolve(layout.ProxyCache(r.projectID, r.poolID))},
		{"pool registry", resolve(buildkitagent.RegistryRoot(r.projectID, r.poolID))},
	} {
		if err := emptyDir(dir.path); err != nil {
			errs = append(errs, fmt.Errorf("clear %s: %w", dir.name, err))
		}
	}
	pruneCtx, cancelPrune := context.WithTimeout(ctx, buildPruneTimeout)
	defer cancelPrune()
	if err := buildkitagent.PruneBuildCache(pruneCtx); err != nil {
		errs = append(errs, err)
	}
	reclaimCtx, cancelReclaim := context.WithTimeout(ctx, imageReclaimTimeout)
	defer cancelReclaim()
	if _, err := imagereap.Reclaim(reclaimCtx, r.client, imagereap.Options{Retention: clearImageFloor}); err != nil {
		errs = append(errs, fmt.Errorf("remove unused images: %w", err))
	}
	return errors.Join(errs...)
}

// clearImageFloor is how recently an image must have arrived for a clear to
// leave it, where an ordinary pass keeps it for a day.
//
// Unused is not the same as unwanted for a moment after a pull. A create pulls
// its image, then prepares volumes and clones sources, and only then creates the
// container that makes the image in use. This pool's own creates are drained by
// the start gate before a clear starts, but on the local Docker backend the
// daemon is shared, and another pool's create in that window would lose the
// image it had just pulled and fail on "No such image". An hour covers that
// window even behind a slow clone, and what a clear leaves behind is only what
// arrived in the hour before it.
const clearImageFloor = time.Hour

// buildPruneTimeout and imageReclaimTimeout bound the steps of a clear that
// wait on another process. A prune walks every cache record and can be slow on
// a large store; an image pass matches the bound dockerworker gives its own.
const (
	buildPruneTimeout   = 10 * time.Minute
	imageReclaimTimeout = 2 * time.Minute
)

// stopIfRunning stops a sandbox the way an explicit stop would, having looked
// again under its lock: the list it was chosen from is a moment old, and
// publishing `stopping` for a sandbox that is already down would leave it
// reported as stopping until the next complete sync.
func (r *DockerSandboxRuntime) stopIfRunning(ctx context.Context, sandboxID string) (bool, error) {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	sb, err := r.GetSandbox(ctx, sandboxID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if sb.Status != StatusRunning {
		return false, nil
	}
	return true, r.stopLocked(ctx, sandboxID)
}

// emptyDir removes everything under root and keeps root itself. The directory
// is the bind source every sandbox container was created with, and only a
// create makes it, so a stopped container whose source had gone would fail its
// next start.
func emptyDir(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// startGate is what holds sandboxes down while the caches are cleared.
//
// Every operation that can start a container passes through it for its whole
// duration, and a clear closes it: the clear waits for the operations already
// inside to finish, and operations that arrive while it is closed wait for it to
// reopen. Waiting rather than refusing is deliberate — an on-demand start or a
// create that lands during a clear is a request that is simply late, not one
// that is wrong, and it proceeds once the caches are empty.
//
// The gate is entered before the sandbox's own power lock and never while
// holding it, and a clear takes power locks only once the gate is drained, so
// the two cannot wait on each other.
type startGate struct {
	mu     sync.Mutex
	active int
	// closed is non-nil while a clear holds the gate, and is closed when it
	// reopens.
	closed chan struct{}
	// drained is closed when the last operation inside leaves a closed gate.
	drained chan struct{}
}

// enter admits one start-capable operation, waiting while the gate is closed.
// The returned func leaves the gate.
func (g *startGate) enter(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if g.closed == nil {
			g.active++
			g.mu.Unlock()
			return g.leave, nil
		}
		closed := g.closed
		g.mu.Unlock()
		select {
		case <-closed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (g *startGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active == 0 && g.drained != nil {
		close(g.drained)
		g.drained = nil
	}
}

// close shuts the gate and returns once nothing is inside it, with a func that
// reopens it. If ctx ends first the gate is reopened and ctx's error returned.
// Only one closer may hold the gate at a time; ClearCache runs one clear at a
// time to ensure it.
func (g *startGate) close(ctx context.Context) (func(), error) {
	g.mu.Lock()
	closed := make(chan struct{})
	g.closed = closed
	var drained chan struct{}
	if g.active > 0 {
		drained = make(chan struct{})
		g.drained = drained
	}
	g.mu.Unlock()
	reopen := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.closed = nil
		g.drained = nil
		close(closed)
	}
	if drained != nil {
		select {
		case <-drained:
		case <-ctx.Done():
			reopen()
			return nil, ctx.Err()
		}
	}
	return reopen, nil
}
